// file: internal/adapters/sandbox/docker.go
package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/AnishPrakash/arena/internal/core"
	"github.com/AnishPrakash/arena/internal/obs"
	"github.com/AnishPrakash/arena/internal/ports"
)

// Docker runs each execution in its own throwaway container.
//
// It shells out to the docker CLI rather than using the SDK. That is a deliberate
// trade-off: the CLI is one dependency fewer, every flag is copy-pasteable into a terminal
// when debugging at 2 a.m., and the ~15 ms of process spawn is dwarfed by container
// creation. If profiling ever shows it matters, swap in the SDK behind this same
// ports.Sandbox interface — no caller changes.
type Docker struct {
	// RunnerID labels every container this runner creates, so SweepStale can remove its
	// own orphans without touching containers belonging to another runner on the same host.
	RunnerID string

	warmMu sync.Mutex
	warm   map[string]string // poolKey -> pre-created container name
	seq    uint64
	seqMu  sync.Mutex
}

func NewDocker(runnerID string) *Docker {
	return &Docker{RunnerID: runnerID, warm: map[string]string{}}
}

func (d *Docker) next() uint64 {
	d.seqMu.Lock()
	defer d.seqMu.Unlock()
	d.seq++
	return d.seq
}

// Run executes one command in a fresh sandbox and returns raw metrics.
//
// It deliberately returns core.ExecOutcome and NOT a Verdict. Turning "exited 137 with
// oom_killed=true" into MLE is domain logic that lives in core.ClassifyRun, is pure, and
// is unit tested without Docker. Keeping the sandbox verdict-free is what lets the same
// classifier sit on top of gVisor or Firecracker later.
func (d *Docker) Run(ctx context.Context, spec ports.RunSpec) (core.ExecOutcome, error) {
	var out core.ExecOutcome

	metaHost := filepath.Join(spec.BoxDir, "meta.json")
	_ = os.Remove(metaHost)

	name := spec.Name
	if name == "" {
		name = fmt.Sprintf("arena-%d-%d", time.Now().UnixNano(), d.next())
	}

	// boxrun's wall deadline is the inner guard. The outer context deadline is deliberately
	// larger so that in the normal case boxrun reports a clean wall_kill rather than the
	// runner ripping the container away and losing the metrics.
	inner := spec.Limits.WallMs
	if inner <= 0 {
		inner = 10_000
	}
	outerDeadline := time.Duration(inner)*time.Millisecond + 15*time.Second

	boxCmd := append([]string{
		"/usr/local/bin/boxrun",
		"-meta", "/box/meta.json",
		"-cpu-ms", strconv.FormatInt(spec.Limits.CPUms, 10),
		"-wall-ms", strconv.FormatInt(inner, 10),
		"-fsize-kb", strconv.FormatInt(spec.Limits.StdoutKB, 10),
		"-stack-mb", strconv.FormatInt(spec.Limits.StackMB, 10),
		"-nofile", strconv.FormatInt(spec.Limits.OpenFiles, 10),
		"-stdout", "/box/stdout",
		"-stderr", "/box/stderr",
		"-workdir", "/box",
		"-no-aslr=" + strconv.FormatBool(spec.DisableASLR),
	}, boxStdin(spec)...)
	boxCmd = append(boxCmd, "--")
	boxCmd = append(boxCmd, spec.Cmd...)

	args := []string{
		"run", "--name", name,
		"--label", "arena.runner=" + d.RunnerID,
		"--workdir", "/box",
		"-v", spec.BoxDir + ":/box:rw",

		// ---- isolation ----------------------------------------------------
		"--read-only", // the rootfs is immutable; only /box is writable
		"--cap-drop=ALL",
		"--security-opt=no-new-privileges",
		"--user", "65534:65534", // never root, even inside the namespace

		// ---- resources ------------------------------------------------------
		// memory-swap EQUAL to memory disables swap entirely. Without this, Docker
		// defaults to 2x memory of swap, a leaking program swaps instead of OOMing, and
		// a clean MLE degrades into a 60-second wall timeout that also thrashes the node.
		"--memory", fmt.Sprintf("%dm", spec.Limits.MemMB),
		"--memory-swap", fmt.Sprintf("%dm", spec.Limits.MemMB),
		"--memory-swappiness", "0",
		"--pids-limit", strconv.FormatInt(spec.Limits.Pids, 10), // fork bombs
		"--ulimit", "core=0",
		"--cpus", "1.0",
	}

	// Pinning to a dedicated core is the largest single reduction in timing variance.
	// Slots are allocated one per physical core and never oversubscribed.
	if spec.CPUSet != "" {
		args = append(args, "--cpuset-cpus", spec.CPUSet)
	}

	// No network at all: no exfiltration, no downloading a solution mid-run, no using our
	// infrastructure to attack a third party.
	if !spec.Network {
		args = append(args, "--network", "none")
	}

	for _, e := range spec.Env {
		args = append(args, "-e", e)
	}
	args = append(args, spec.Image)
	args = append(args, boxCmd...)

	// A runner killed mid-judge leaves its container behind, and names are deterministic,
	// so the retry would collide with "container name is already in use" (exit 125) and
	// burn every attempt. Clear the name first; this is a no-op in the normal case.
	d.remove(name)

	runCtx, cancel := context.WithTimeout(ctx, outerDeadline)
	defer cancel()

	setupStart := time.Now()
	cmd := exec.CommandContext(runCtx, "docker", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	obs.SandboxSetup.Observe(time.Since(setupStart).Seconds())

	// Inspect BEFORE removing. OOMKilled comes from the cgroup's own accounting; it is the
	// only trustworthy way to distinguish MLE from RE, because an OOM-killed process also
	// exits non-zero and dies on SIGKILL.
	oom, inspectExit := d.inspect(context.Background(), name)
	d.remove(name)

	meta, metaErr := readMeta(metaHost)
	if metaErr != nil {
		// No meta.json means boxrun never ran or the container was destroyed before it
		// could write. That is OUR failure, not the participant's — surface it as an
		// error so the runner reports IE rather than blaming their code.
		if runErr != nil {
			return out, fmt.Errorf("sandbox: %v: %s", runErr, strings.TrimSpace(stderr.String()))
		}
		return out, fmt.Errorf("sandbox: no metrics produced: %w", metaErr)
	}

	out = core.ExecOutcome{
		ExitCode:  meta.ExitCode,
		Signal:    meta.Signal,
		CPUms:     meta.CPUms,
		WallMs:    meta.WallMs,
		MaxRSSKB:  meta.MaxRSSKB,
		OOMKilled: oom,
		WallKill:  meta.WallKill,
		FSizeKill: meta.FSizeKill,
		StdoutLen: meta.StdoutLen,
	}
	if oom {
		obs.OOMKills.Inc()
	}
	if meta.WallKill {
		obs.WallKills.Inc()
	}
	// If docker itself reported 137 but boxrun saw nothing, the kernel killed the
	// container out from under the supervisor — still an OOM.
	if inspectExit == 137 && !out.OOMKilled && out.MaxRSSKB == 0 {
		out.OOMKilled = true
	}
	return out, nil
}

func boxStdin(spec ports.RunSpec) []string {
	if spec.StdinPath == "" {
		return nil
	}
	return []string{"-stdin", spec.StdinPath}
}

type boxMeta struct {
	ExitCode  int   `json:"exit_code"`
	Signal    int   `json:"signal"`
	CPUms     int64 `json:"cpu_ms"`
	WallMs    int64 `json:"wall_ms"`
	MaxRSSKB  int64 `json:"max_rss_kb"`
	WallKill  bool  `json:"wall_kill"`
	CPUKill   bool  `json:"cpu_kill"`
	FSizeKill bool  `json:"fsize_kill"`
	StdoutLen int64 `json:"stdout_len"`
}

func readMeta(path string) (boxMeta, error) {
	var m boxMeta
	b, err := os.ReadFile(path)
	if err != nil {
		return m, err
	}
	return m, json.Unmarshal(b, &m)
}

// inspect reads the container's final state from Docker's own records.
func (d *Docker) inspect(ctx context.Context, name string) (oom bool, exitCode int) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "inspect",
		"--format", "{{.State.OOMKilled}} {{.State.ExitCode}}", name).Output()
	if err != nil {
		return false, 0
	}
	f := strings.Fields(strings.TrimSpace(string(out)))
	if len(f) != 2 {
		return false, 0
	}
	code, _ := strconv.Atoi(f[1])
	return f[0] == "true", code
}

// remove is fire-and-forget with its own timeout. A leaked container is a leaked cgroup
// and eventually a leaked core, so this must never be skipped — but it must also never
// block judging if the daemon is briefly unresponsive.
func (d *Docker) remove(name string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = exec.CommandContext(ctx, "docker", "rm", "-f", "-v", name).Run()
}

// Warm pre-pulls images so the first submission of a contest does not pay a multi-hundred
// megabyte download while a participant watches.
//
// A fuller warm pool (pre-`docker create`d containers bound to each slot's box directory,
// started on demand and replenished in the background) takes container start from ~250 ms
// to ~50 ms. It is written up in DECISIONS.md as the next optimisation; the pre-pull below
// captures most of the benefit for a fraction of the complexity.
func (d *Docker) Warm(ctx context.Context, image string, _ int) error {
	// Already present locally? Nothing to pull. Locally-built tags (arena/*:local) have no
	// registry behind them, so an unconditional pull fails on every runner start and
	// makes a healthy system look broken in the logs.
	if exec.CommandContext(ctx, "docker", "image", "inspect", image).Run() == nil {
		return nil
	}
	pullCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	return exec.CommandContext(pullCtx, "docker", "pull", image).Run()
}

// SweepStale removes containers left behind by a previous incarnation of THIS runner —
// the ones a SIGKILL prevented it from cleaning up. Scoped by label so a co-located
// runner's live containers are never touched.
//
// Without this, an orphan keeps consuming CPU on a pinned core and holds a container name
// the retry needs.
func (d *Docker) SweepStale(ctx context.Context) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "ps", "-aq",
		"--filter", "label=arena.runner="+d.RunnerID).Output()
	if err != nil {
		return 0, err
	}
	ids := strings.Fields(string(out))
	for _, id := range ids {
		_ = exec.CommandContext(ctx, "docker", "rm", "-f", "-v", id).Run()
	}
	return len(ids), nil
}

func (d *Docker) Close() error { return nil }
