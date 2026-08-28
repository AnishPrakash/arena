// file: cmd/runner/main.go
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	_ "github.com/AnishPrakash/arena/internal/checker" // registers the built-in checkers

	"github.com/AnishPrakash/arena/internal/adapters/blob"
	"github.com/AnishPrakash/arena/internal/adapters/redisq"
	"github.com/AnishPrakash/arena/internal/adapters/sandbox"
	"github.com/AnishPrakash/arena/internal/config"
	"github.com/AnishPrakash/arena/internal/core"
	"github.com/AnishPrakash/arena/internal/judge"
	"github.com/AnishPrakash/arena/internal/langs"
	"github.com/AnishPrakash/arena/internal/obs"
	"github.com/AnishPrakash/arena/internal/perfcount"
	"github.com/AnishPrakash/arena/internal/ports"
)

var version = "dev"

type agent struct {
	cfg      config.Config
	log      *slog.Logger
	q        ports.Queue
	j        *judge.Judge
	http     *http.Client
	inFlight sync.Map // messageID -> struct{}, for heartbeats and for nack on SIGTERM
}

func main() {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	// Two contexts on purpose:
	//   rootCtx  cancels on SIGTERM and stops us CLAIMING new work
	//   workCtx  stays alive briefly so in-flight jobs can be nacked cleanly
	rootCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	rdb, err := redisq.NewClient(rootCtx, cfg.RedisAddr, cfg.RedisDB)
	fatal(log, err)
	defer rdb.Close()

	q, err := redisq.NewQueue(rootCtx, rdb, redisq.QueueOpts{
		Stream: cfg.Stream, Group: cfg.Group, LeaseTTL: cfg.LeaseTTL,
	})
	fatal(log, err)

	blobs, err := blob.NewLocal(cfg.BlobRoot)
	fatal(log, err)

	sb, err := sandbox.New(cfg.SandboxDriver, cfg.Env, cfg.RunnerID)
	fatal(log, err)
	defer sb.Close()

	registry, err := langs.LoadBuiltin()
	fatal(log, err)

	// Clear containers a previous incarnation of this runner left behind after a crash.
	if ds, ok := sb.(*sandbox.Docker); ok {
		if n, err := ds.SweepStale(rootCtx); err != nil {
			log.Warn("stale container sweep failed", "err", err)
		} else if n > 0 {
			log.Warn("removed containers orphaned by a previous crash", "count", n)
		}
	}

	// Pre-pull every image before serving traffic. Otherwise the first participant of the
	// contest waits for a 200 MB download while everyone behind them queues.
	for _, m := range registry.List() {
		if err := sb.Warm(rootCtx, m.Image, 1); err != nil {
			log.Warn("image pre-pull failed", "image", m.Image, "err", err)
		}
	}

	a := &agent{
		cfg: cfg, log: log, q: q,
		http: &http.Client{Timeout: 15 * time.Second},
		j: &judge.Judge{
			Sandbox: sb, Blobs: blobs, Langs: registry,
			BoxRoot: cfg.BoxRoot, CacheRoot: cfg.BoxRoot + "/testdata",
			RunnerID: cfg.RunnerID, CPUModel: cpuModel(),
		},
	}

	log.Info("runner starting", "id", cfg.RunnerID, "slots", cfg.RunnerSlots,
		"sandbox", cfg.SandboxDriver, "version", version, "cpu", a.j.CPUModel)

	// Probe once for hardware instruction counting. Retired instruction count is a
	// strictly better efficiency metric than CPU milliseconds — reproducible to ~0.1% and
	// immune to noisy neighbours and frequency scaling — but it needs a PMU the platform
	// often does not expose. Degrade deliberately and say so, rather than silently
	// reporting zeros. See ADR-009.
	if ok, why := perfcount.Available(); ok {
		log.Info("hardware instruction counting available")
	} else {
		log.Info("instruction counting unavailable, falling back to normalised CPU time",
			"reason", why)
	}

	a.loop(rootCtx)

	// ---- graceful drain ---------------------------------------------------
	// On a spot-instance preemption notice (AWS ~2 min, GCP ~30 s) we get SIGTERM. Nacking
	// in flight work makes it redeliverable within milliseconds instead of after a full
	// LEASE_TTL of silence. This is the single change that makes spot instances — the
	// biggest cost lever in the system — safe to use during a live contest.
	drainCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var ids []string
	a.inFlight.Range(func(k, _ any) bool { ids = append(ids, k.(string)); return true })
	if len(ids) > 0 {
		log.Warn("preempted: releasing in-flight jobs for immediate redelivery", "count", len(ids))
		_ = a.q.Nack(drainCtx, ids...)
	}
	log.Info("runner stopped")
}

// loop is the claim/execute cycle. Concurrency is capped by a slot semaphore whose size is
// the number of DEDICATED cores, never more: oversubscribing judging slots destroys timing
// determinism, which is the one property the brief explicitly demands.
func (a *agent) loop(ctx context.Context) {
	slots := make(chan int, a.cfg.RunnerSlots)
	for i := 0; i < a.cfg.RunnerSlots; i++ {
		slots <- i
	}
	var wg sync.WaitGroup

	go a.heartbeatLoop(ctx)
	go a.reclaimLoop(ctx)

	for {
		select {
		case <-ctx.Done():
			wg.Wait() // let running jobs finish or be cancelled before we drain
			return
		default:
		}

		// Block for at most 2 s: shorter than the redis client's 3 s read timeout, and
		// short enough that SIGTERM is noticed promptly.
		batch := len(slots)
		if batch == 0 {
			time.Sleep(50 * time.Millisecond)
			continue
		}
		deliveries, err := a.q.Consume(ctx, a.cfg.RunnerID, batch, 2*time.Second)
		if err != nil {
			if ctx.Err() != nil {
				continue
			}
			a.log.Error("consume", "err", err)
			time.Sleep(time.Second)
			continue
		}

		for _, d := range deliveries {
			slot := <-slots
			a.inFlight.Store(d.MessageID, struct{}{})
			obs.RunnerSlotsBusy.WithLabelValues(a.cfg.RunnerID).
				Set(float64(a.cfg.RunnerSlots - len(slots)))
			wg.Add(1)
			go func(d ports.Delivery, slot int) {
				defer wg.Done()
				defer func() {
					a.inFlight.Delete(d.MessageID)
					slots <- slot
				}()
				a.handle(ctx, d, slot)
			}(d, slot)
		}
	}
}

func (a *agent) handle(ctx context.Context, d ports.Delivery, slot int) {
	job := d.Job
	start := time.Now()

	if !job.EnqueuedAt.IsZero() {
		obs.QueueWait.Observe(time.Since(job.EnqueuedAt).Seconds())
	}
	a.markJudging(ctx, job)

	res, err := a.j.Run(ctx, job, slot)
	if err != nil {
		// An infrastructure failure. Do NOT ack: leaving the message unacked lets the
		// lease expire and another runner try. If it fails MaxAttempts times it is
		// dead-lettered and reported honestly as IE rather than being retried forever or,
		// worse, blamed on the participant's code.
		a.log.Error("judge failed", "submission", job.SubmissionID,
			"attempt", job.Attempt, "err", err)
		if job.Attempt >= a.cfg.MaxAttempts {
			res = core.SubmissionResult{
				SubmissionID: job.SubmissionID, Attempt: job.Attempt,
				RunnerID: a.cfg.RunnerID, Status: core.StatusFailed,
				Verdict: core.VerdictIE, FailedTest: -1,
				CompileLog: "the judge could not complete this submission; please resubmit",
			}
			a.report(ctx, res)
			_ = a.q.Ack(ctx, d.MessageID)
			obs.DeadLettered.Inc()
		}
		return
	}

	a.report(ctx, res)

	// Ack LAST. Everything before this point is safe to repeat; acking first would mean a
	// crash between ack and report silently loses a submission.
	if err := a.q.Ack(ctx, d.MessageID); err != nil {
		a.log.Error("ack", "err", err)
	}

	a.log.Info("judged",
		"submission", job.SubmissionID, "verdict", res.Verdict,
		"cpu_ms", res.CPUms, "mem_kb", res.MemKB,
		"tests", len(res.Tests), "took", time.Since(start).Round(time.Millisecond))
}

// heartbeatLoop keeps the lease alive on jobs we are still working on. Without it, a
// legitimate 20-test submission that takes longer than LEASE_TTL would be stolen by the
// reclaimer and judged twice.
func (a *agent) heartbeatLoop(ctx context.Context) {
	t := time.NewTicker(a.cfg.LeaseTTL / 3)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			var ids []string
			a.inFlight.Range(func(k, _ any) bool { ids = append(ids, k.(string)); return true })
			if len(ids) > 0 {
				_ = a.q.Heartbeat(ctx, a.cfg.RunnerID, ids...)
			}
		}
	}
}

// reclaimLoop takes over work abandoned by runners that died or were preempted. Every
// runner does this, so there is no special "coordinator" node to fail.
func (a *agent) reclaimLoop(ctx context.Context) {
	t := time.NewTicker(a.cfg.LeaseTTL / 2)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			live, dead, err := a.q.Reclaim(ctx, a.cfg.RunnerID, a.cfg.LeaseTTL, a.cfg.MaxAttempts)
			if err != nil {
				continue
			}
			for _, d := range dead {
				a.report(ctx, core.SubmissionResult{
					SubmissionID: d.Job.SubmissionID, Attempt: d.Job.Attempt,
					RunnerID: a.cfg.RunnerID, Status: core.StatusFailed,
					Verdict: core.VerdictIE, FailedTest: -1,
					CompileLog: "the judge could not complete this submission; please resubmit",
				})
				_ = a.q.Ack(ctx, d.MessageID)
				obs.DeadLettered.Inc()
			}
			if len(live) > 0 {
				obs.LeaseReclaims.Add(float64(len(live)))
				a.log.Warn("reclaimed abandoned jobs", "count", len(live))
			}
			// Reclaimed messages are now owned by this consumer and will be returned by
			// the next XREADGROUP of pending entries; the simplest correct behaviour is to
			// let the normal loop pick them up on its next pass.
		}
	}
}

// ---------------------------------------------------------------- API calls

func (a *agent) markJudging(ctx context.Context, job core.JobSpec) {
	body, _ := json.Marshal(map[string]any{"runner_id": a.cfg.RunnerID, "attempt": job.Attempt})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPatch,
		fmt.Sprintf("%s/internal/submissions/%s/judging", a.cfg.APIBase, job.SubmissionID),
		bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Runner-Token", a.cfg.RunnerToken)
	if resp, err := a.http.Do(req); err == nil {
		resp.Body.Close()
	}
}

// report posts the verdict, retrying briefly. A verdict that reaches nobody is a
// submission that never finishes, so this is worth a few retries — and it is safe to
// retry because the endpoint is idempotent.
func (a *agent) report(ctx context.Context, res core.SubmissionResult) {
	body, _ := json.Marshal(res)
	for attempt := 0; attempt < 4; attempt++ {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost,
			a.cfg.APIBase+"/internal/results", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Runner-Token", a.cfg.RunnerToken)
		resp, err := a.http.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode < 300 {
				return
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Duration(1<<attempt) * 250 * time.Millisecond): // 250ms, 500, 1s, 2s
		}
	}
	a.log.Error("could not report result; the reconciler will retry the submission",
		"submission", res.SubmissionID)
}

// cpuModel is recorded on every result so a timing anomaly can be traced to the hardware
// it ran on — the auditability half of the determinism story.
func cpuModel() string {
	out, err := exec.Command("sh", "-c",
		"grep -m1 'model name' /proc/cpuinfo | cut -d: -f2").Output()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(out))
}

func fatal(log *slog.Logger, err error) {
	if err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}
