// file: internal/judge/judge.go
package judge

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/AnishPrakash/arena/internal/checker"
	"github.com/AnishPrakash/arena/internal/core"
	"github.com/AnishPrakash/arena/internal/langs"
	"github.com/AnishPrakash/arena/internal/ports"
)

// Judge turns one JobSpec into one SubmissionResult. It owns no state beyond its
// dependencies, so a runner can drive N of them concurrently, one per pinned core.
type Judge struct {
	Sandbox   ports.Sandbox
	Blobs     ports.BlobStore
	Langs     *langs.Registry
	BoxRoot   string // <BoxRoot>/slot-<n> is one judging workspace
	CacheRoot string // node-local test data cache, keyed by content hash
	RunnerID  string
	CPUModel  string
}

// Run judges a submission. It never returns an error for anything the PARTICIPANT did —
// a crash, a timeout, an unparseable program are all verdicts. An error return means OUR
// infrastructure failed, and the caller turns it into IE and lets the queue retry.
func (j *Judge) Run(ctx context.Context, job core.JobSpec, slot int) (core.SubmissionResult, error) {
	res := core.SubmissionResult{
		SubmissionID: job.SubmissionID,
		Attempt:      job.Attempt,
		RunnerID:     j.RunnerID,
		Status:       core.StatusDone,
		FailedTest:   -1,
		ImageDigest:  job.ImageDigest,
		CPUModel:     j.CPUModel,
		JudgedAt:     time.Now(),
	}

	manifest, ok := j.Langs.Get(job.Language)
	if !ok {
		return res, fmt.Errorf("unknown language %q", job.Language)
	}
	chk, err := checker.Get(job.Checker.Type)
	if err != nil {
		return res, err
	}

	// --- workspace -------------------------------------------------------
	// One box per slot, wiped between submissions. Ephemeral by construction: whatever a
	// submission leaves behind — files, memory, half-written state — is deleted, which is
	// why "memory leaks" cannot accumulate across submissions.
	slotDir := filepath.Join(j.BoxRoot, fmt.Sprintf("slot-%d", slot))
	boxDir := filepath.Join(slotDir, "box")
	stashDir := filepath.Join(slotDir, "stash")
	for _, d := range []string{boxDir, stashDir} {
		_ = os.RemoveAll(d)
		if err := os.MkdirAll(d, 0o777); err != nil {
			return res, fmt.Errorf("prepare workspace: %w", err)
		}
		// 0777 because the container runs as uid 65534 and must be able to write results
		// into the bind mount. The directory is on the runner's own scratch disk and is
		// destroyed after every submission.
		_ = os.Chmod(d, 0o777)
	}
	defer os.RemoveAll(boxDir)

	cpuset := fmt.Sprintf("%d", slot+1) // pinned core; slot 0 -> core 1, and so on

	// --- source ----------------------------------------------------------
	if err := j.Blobs.GetToFile(ctx, job.SourceRef, filepath.Join(boxDir, manifest.SourceFile)); err != nil {
		return res, fmt.Errorf("fetch source: %w", err)
	}

	// --- compile ONCE ------------------------------------------------------
	// Compiling inside the per-test loop is the single most common performance bug in a
	// homegrown judge: for a 12-test problem it multiplies the most expensive step by 12.
	if manifest.NeedsCompile() {
		out, err := j.Sandbox.Run(ctx, ports.RunSpec{
			Image: manifest.Image, Cmd: manifest.Compile, BoxDir: boxDir,
			Limits: job.Limits.Compile, Env: manifest.Env, CPUSet: cpuset,
			Network: false, DisableASLR: false,
			Name:    fmt.Sprintf("arena-c-%s", short(job.SubmissionID)),
		})
		if err != nil {
			return res, fmt.Errorf("compile sandbox: %w", err)
		}
		if out.ExitCode != 0 || out.Signal != 0 {
			res.Verdict = core.VerdictCE
			// Compiler stderr is safe to return: it describes the participant's own code.
			// Program STDOUT on hidden tests is not, and is never returned anywhere.
			res.CompileLog = sanitize(readCapped(filepath.Join(boxDir, "stderr"), 8000))
			if res.CompileLog == "" && out.WallKill {
				res.CompileLog = "compilation timed out"
			}
			return res, nil
		}
	}

	// Stash the built artefact OUTSIDE the bind mount, then restore it before each test.
	//
	// Why: the box is bind-mounted read-write and shared across this submission's tests, so
	// a participant's program could overwrite /box/prog on test 1 and run something else on
	// test 2. Restoring from a stash the sandbox cannot reach closes that hole for a copy
	// of a few hundred kilobytes.
	if err := copyTree(boxDir, stashDir); err != nil {
		return res, fmt.Errorf("stash artefacts: %w", err)
	}

	// --- tests -------------------------------------------------------------
	for _, tc := range job.Tests {
		select {
		case <-ctx.Done():
			return res, ctx.Err() // preemption: report nothing, let the queue redeliver
		default:
		}

		tr, err := j.runOne(ctx, job, manifest, chk, tc, boxDir, stashDir, cpuset)
		if err != nil {
			return res, err
		}
		res.Tests = append(res.Tests, tr)

		// Early exit. In contest mode a participant only needs to know the FIRST failing
		// test, and skipping the rest roughly halves average judging work — the cheapest
		// capacity increase available without touching infrastructure.
		if job.Policy.StopOnFirstFailure && !tr.Verdict.IsAccepted() {
			for _, rest := range job.Tests {
				if rest.Index > tc.Index {
					res.Tests = append(res.Tests, core.TestResult{
						Index: rest.Index, Verdict: core.VerdictAC, Skipped: true,
					})
				}
			}
			break
		}
	}

	res.Verdict, res.CPUms, res.WallMs, res.MemKB, res.FailedTest = core.Aggregate(res.Tests)
	res.Score = core.TestPoints(res.Tests, job.Tests)
	for _, t := range res.Tests {
		_ = t
	}
	return res, nil
}

func (j *Judge) runOne(ctx context.Context, job core.JobSpec, m *langs.Manifest,
	chk checker.Checker, tc core.TestRef, boxDir, stashDir, cpuset string) (core.TestResult, error) {

	// Restore a pristine box: the compiled artefact from the stash, nothing else.
	if err := resetBox(boxDir, stashDir); err != nil {
		return core.TestResult{}, err
	}

	inPath, expPath, err := j.testdata(ctx, job, tc)
	if err != nil {
		return core.TestResult{}, fmt.Errorf("fetch testdata: %w", err)
	}
	// Hard-link the input into the box when possible (same filesystem, zero copy); fall
	// back to a copy across devices. A 50 MB input copied 12 times per submission is real
	// disk bandwidth on a busy node.
	boxIn := filepath.Join(boxDir, "input")
	if err := os.Link(inPath, boxIn); err != nil {
		if err := copyFile(inPath, boxIn); err != nil {
			return core.TestResult{}, err
		}
	}
	_ = os.Chmod(boxIn, 0o644)

	out, err := j.Sandbox.Run(ctx, ports.RunSpec{
		Image: m.Image, Cmd: m.Run, BoxDir: boxDir,
		StdinPath: "/box/input", Limits: job.Limits.Run, Env: m.Env,
		CPUSet: cpuset, Network: job.Policy.Network, DisableASLR: job.Policy.DisableASLR,
		Name:   fmt.Sprintf("arena-r-%s-%d", short(job.SubmissionID), tc.Index),
	})
	if err != nil {
		return core.TestResult{}, fmt.Errorf("run sandbox: %w", err)
	}

	// Only check correctness if the process actually terminated cleanly; running a
	// comparison over the truncated output of a killed process wastes I/O and can produce
	// a confusing "wrong answer" message attached to a TLE.
	correct := false
	msg := ""
	if out.ExitCode == 0 && out.Signal == 0 && !out.WallKill && !out.OOMKilled {
		ok, m2, cerr := chk.Check(expPath, filepath.Join(boxDir, "stdout"), job.Checker)
		if cerr != nil {
			return core.TestResult{}, fmt.Errorf("checker: %w", cerr)
		}
		correct, msg = ok, m2
	}

	verdict := core.ClassifyRun(out, job.Limits.Run, correct)

	tr := core.TestResult{
		Index: tc.Index, Verdict: verdict,
		CPUms: out.CPUms, WallMs: out.WallMs, MemKB: out.MaxRSSKB,
		ExitCode: out.ExitCode, Signal: out.Signal,
	}
	if verdict == core.VerdictWA {
		tr.Message = msg
	}
	// SECURITY: the program's output is echoed back ONLY for sample tests, whose inputs the
	// participant already has. Echoing it for hidden tests would let anyone exfiltrate the
	// entire test set with `print(open('/box/input').read())`.
	if tc.IsSample {
		tr.SampleOut = sanitize(readCapped(filepath.Join(boxDir, "stdout"), 2000))
	}
	return tr, nil
}

// testdata fetches a test case into the node-local cache, keyed by the test data's content
// hash. The first submission of a contest pays the download; every subsequent one on that
// node reads from local disk. Because the key is a content hash, a corrected test case
// produces a new key and can never be served stale.
func (j *Judge) testdata(ctx context.Context, job core.JobSpec, tc core.TestRef) (in, exp string, err error) {
	dir := filepath.Join(j.CacheRoot, job.TestdataVersion, job.ProblemID)
	if err = os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	in = filepath.Join(dir, fmt.Sprintf("%d.in", tc.Index))
	exp = filepath.Join(dir, fmt.Sprintf("%d.out", tc.Index))
	if _, statErr := os.Stat(in); statErr != nil {
		if err = j.Blobs.GetToFile(ctx, tc.InputRef, in); err != nil {
			return
		}
	}
	if _, statErr := os.Stat(exp); statErr != nil {
		if err = j.Blobs.GetToFile(ctx, tc.OutputRef, exp); err != nil {
			return
		}
	}
	return
}

// ---------------------------------------------------------------- helpers

func resetBox(boxDir, stashDir string) error {
	entries, err := os.ReadDir(boxDir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		_ = os.RemoveAll(filepath.Join(boxDir, e.Name()))
	}
	return copyTree(stashDir, boxDir)
}

func copyTree(src, dst string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		s, d := filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())
		if e.IsDir() {
			if err := os.MkdirAll(d, 0o777); err != nil {
				return err
			}
			if err := copyTree(s, d); err != nil {
				return err
			}
			continue
		}
		// Skip artefacts of a previous run rather than restoring them.
		switch e.Name() {
		case "stdout", "stderr", "meta.json", "input":
			continue
		}
		if err := copyFile(s, d); err != nil {
			return err
		}
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	fi, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, fi.Mode())
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return os.Chmod(dst, fi.Mode())
}

func readCapped(path string, max int) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	b := make([]byte, max)
	n, _ := f.Read(b)
	s := string(b[:n])
	if fi, err := f.Stat(); err == nil && fi.Size() > int64(max) {
		s += "\n... [truncated]"
	}
	return s
}

// sanitize strips control characters and ANSI escape sequences before any program-produced
// text reaches a log, an API response or a browser. Without this a submission can inject
// terminal escapes into your logs or markup into a leaderboard page.
func sanitize(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\n' || r == '\t':
			b.WriteRune(r)
		case r == 0x1b: // ESC — drop, and with it every escape sequence it introduces
			continue
		case r < 0x20 || r == 0x7f:
			continue
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
