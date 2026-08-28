// file: internal/core/job.go
package core

import "time"

// JobSpec is the unit of work published to the queue.
//
// Design rule: a JobSpec is SELF-CONTAINED and IMMUTABLE. The runner never queries the
// database to find out what to do. Two consequences:
//  1. Runner nodes need no database credentials, so a sandbox escape lands somewhere
//     that cannot reach participant data.
//  2. Re-running an archived JobSpec reproduces the original verdict exactly, because
//     the image digest, testdata version and limits are all pinned in the message.
type JobSpec struct {
	SubmissionID string `json:"submission_id"`
	Attempt      int    `json:"attempt"` // 1-based; incremented on redelivery

	ContestID string `json:"contest_id"`
	ProblemID string `json:"problem_id"`
	UserID    string `json:"user_id"`

	Language   string `json:"language"`    // manifest id, e.g. "cpp20"
	SourceRef  string `json:"source_ref"`  // blob key
	SourceHash string `json:"source_hash"` // sha256 of the normalised source

	// ImageDigest pins the exact sandbox image. Recorded on the result so the run is
	// reproducible even after the tag moves.
	ImageDigest string `json:"image_digest"`

	// TestdataVersion is the content hash of the whole test set. If a setter fixes a bad
	// test case, this changes, old verdicts remain explainable, and the verdict cache is
	// invalidated automatically.
	TestdataVersion string    `json:"testdata_version"`
	Tests           []TestRef `json:"tests"`

	Limits  LimitSet      `json:"limits"`
	Checker CheckerConfig `json:"checker"`
	Policy  Policy        `json:"policy"`

	EnqueuedAt time.Time `json:"enqueued_at"`
}

// TestRef points at one test case in the blob store.
type TestRef struct {
	Index     int    `json:"idx"`
	InputRef  string `json:"input_ref"`
	OutputRef string `json:"output_ref"`
	IsSample  bool   `json:"is_sample"`
	Points    int    `json:"points"`
}

// CheckerConfig selects an output comparator from the registry. New comparison semantics
// are a new file implementing Checker plus one registry line — never an edit to the judge.
type CheckerConfig struct {
	Type    string  `json:"type"`              // "exact" | "token" | "float"
	Epsilon float64 `json:"epsilon,omitempty"` // for "float"
}

// Policy holds per-run switches that are not resource limits.
type Policy struct {
	// StopOnFirstFailure halves average judging work in contest mode. Turn it off for
	// practice mode, where participants want to see every failing case.
	StopOnFirstFailure bool `json:"stop_on_first_failure"`

	// Network is always false for participant code. It exists as a field so that a future
	// "API challenge" problem type can opt in explicitly and visibly.
	Network bool `json:"network"`

	// DisableASLR makes allocation layout — and therefore cache behaviour and timing —
	// reproducible across runs.
	DisableASLR bool `json:"disable_aslr"`
}

// DefaultPolicy is the contest default.
func DefaultPolicy() Policy {
	return Policy{StopOnFirstFailure: true, Network: false, DisableASLR: true}
}

// ExecOutcome is the raw, verdict-free result of running one process in the sandbox.
// The sandbox adapter produces this; only core turns it into a Verdict. That separation is
// what lets the same verdict logic sit on top of Docker, gVisor or a bare process.
type ExecOutcome struct {
	ExitCode  int   `json:"exit_code"`
	Signal    int   `json:"signal"` // 0 if the process exited normally
	CPUms     int64 `json:"cpu_ms"` // utime + stime from wait4
	WallMs    int64 `json:"wall_ms"`
	MaxRSSKB  int64 `json:"max_rss_kb"`
	OOMKilled bool  `json:"oom_killed"` // from the cgroup, NOT guessed from the exit code
	WallKill  bool  `json:"wall_kill"`  // supervisor's timer fired
	FSizeKill bool  `json:"fsize_kill"` // SIGXFSZ — output flood
	StdoutLen int64 `json:"stdout_len"`

	// Instructions is the retired-instruction count when a PMU is available (see §7b of
	// 01-ARCHITECTURE). Zero means "not measured", and callers must fall back to CPUms.
	Instructions int64 `json:"instructions,omitempty"`

	Stderr string `json:"stderr,omitempty"` // truncated; only surfaced for CE
}

// TestResult is one judged test case.
type TestResult struct {
	Index     int     `json:"idx"`
	Verdict   Verdict `json:"verdict"`
	CPUms     int64   `json:"cpu_ms"`
	WallMs    int64   `json:"wall_ms"`
	MemKB     int64   `json:"mem_kb"`
	ExitCode  int     `json:"exit_code"`
	Signal    int     `json:"signal"`
	Message   string  `json:"message,omitempty"`    // checker diagnostic, e.g. "line 3: expected 42 got 41"
	Skipped   bool    `json:"skipped,omitempty"`    // early exit
	SampleOut string  `json:"sample_out,omitempty"` // ONLY populated for IsSample tests
}

// SubmissionResult is what the runner reports back to the control plane.
type SubmissionResult struct {
	SubmissionID string       `json:"submission_id"`
	Attempt      int          `json:"attempt"`
	RunnerID     string       `json:"runner_id"`
	Verdict      Verdict      `json:"verdict"`
	Status       Status       `json:"status"`
	CPUms        int64        `json:"cpu_ms"` // max across tests — the algorithm's cost
	WallMs       int64        `json:"wall_ms"`
	MemKB        int64        `json:"mem_kb"` // max across tests
	Instructions int64        `json:"instructions,omitempty"`
	Score        int          `json:"score"`
	FailedTest   int          `json:"failed_test"` // -1 when accepted
	CompileLog   string       `json:"compile_log,omitempty"`
	Tests        []TestResult `json:"tests"`
	ImageDigest  string       `json:"image_digest"`
	CPUModel     string       `json:"cpu_model,omitempty"` // auditability of timings
	JudgedAt     time.Time    `json:"judged_at"`
}

// ClassifyRun turns a raw ExecOutcome into a Verdict, given the limits that were applied
// and whether the produced output was correct.
//
// This function is the single source of truth for "what went wrong", it is pure, and it is
// exhaustively unit tested. Every ordering decision here has a comment because every one
// of them is a bug someone has shipped.
func ClassifyRun(out ExecOutcome, lim Limits, outputCorrect bool) Verdict {
	// 1. Output flood first. SIGXFSZ also makes the process die on a signal, which would
	//    otherwise be misreported as RE.
	// >= not >: RLIMIT_FSIZE caps the file exactly AT the limit, so it can never exceed
	// it and a strict > is unreachable. This matters because CPython sets SIGXFSZ to
	// SIG_IGN at startup in order to raise OSError instead, so a Python output flood
	// arrives here with FSizeKill false and StdoutLen exactly equal to the cap. C++ has no
	// such handler and does die on signal 25.
	if out.FSizeKill || (lim.StdoutKB > 0 && out.StdoutLen >= lim.StdoutKB*1024) {
		return VerdictOLE
	}

	// 2. Memory before runtime error. An OOM-killed process is killed with SIGKILL and
	//    reports a non-zero exit; checking the exit code first turns every MLE into an RE.
	//    We trust the cgroup's oom_kill counter, and fall back to an RSS threshold for
	//    sandbox backends that do not expose it.
	if out.OOMKilled || (lim.MemMB > 0 && out.MaxRSSKB >= lim.MemMB*1024) {
		return VerdictMLE
	}

	// 3. Time. Two independent triggers: the CPU budget was consumed (RLIMIT_CPU, which
	//    manifests as SIGXCPU=24 or SIGKILL=9), or the wall timer fired.
	if out.WallKill || out.Signal == 24 /*SIGXCPU*/ ||
		(lim.CPUms > 0 && out.CPUms >= lim.CPUms) ||
		(lim.WallMs > 0 && out.WallMs >= lim.WallMs) {
		return VerdictTLE
	}

	// 4. Anything else abnormal is a genuine runtime error.
	if out.Signal != 0 || out.ExitCode != 0 {
		return VerdictRE
	}

	// 5. Finally, correctness.
	if !outputCorrect {
		return VerdictWA
	}
	return VerdictAC
}

// Aggregate folds per-test results into a submission verdict and its headline metrics.
// The submission verdict is the FIRST non-AC verdict in test index order, which is what
// participants expect ("your solution failed on test 7"), not the most severe one anywhere.
func Aggregate(tests []TestResult) (v Verdict, cpuMs, wallMs, memKB int64, failedTest int) {
	v = VerdictAC
	failedTest = -1
	for _, t := range tests {
		if t.Skipped {
			continue
		}
		if t.CPUms > cpuMs {
			cpuMs = t.CPUms
		}
		if t.WallMs > wallMs {
			wallMs = t.WallMs
		}
		if t.MemKB > memKB {
			memKB = t.MemKB
		}
		if !t.Verdict.IsAccepted() && failedTest == -1 {
			v = t.Verdict
			failedTest = t.Index
		}
	}
	return
}
