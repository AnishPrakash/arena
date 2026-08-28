package core

import (
	"testing"
	"time"
)

// timeZero is the contest start passed to RankScore. ModeSpeed does not use the contest
// start in its penalty calculation, so the zero value is all these tests need.
var timeZero time.Time

// TestClassifyRun is the most important test in the codebase.
//
// Several failure conditions are simultaneously true for the same run: an OOM-killed
// process ALSO exits non-zero and dies on SIGKILL; a process killed by SIGXFSZ ALSO dies
// on a signal. Checking them in the wrong order silently produces wrong verdicts, which is
// how participants lose trust in a judge. Every case below pins one ordering decision.
func TestClassifyRun(t *testing.T) {
	lim := Limits{CPUms: 1000, WallMs: 3000, MemMB: 256, StdoutKB: 64}.Normalize()

	cases := []struct {
		name string
		out  ExecOutcome
		ok   bool // did the checker accept the output?
		want Verdict
	}{
		{
			name: "clean accept",
			out:  ExecOutcome{ExitCode: 0, CPUms: 120, WallMs: 200, MaxRSSKB: 4096},
			ok:   true, want: VerdictAC,
		},
		{
			name: "correct process, wrong output",
			out:  ExecOutcome{ExitCode: 0, CPUms: 120, WallMs: 200, MaxRSSKB: 4096},
			ok:   false, want: VerdictWA,
		},
		{
			// The single most important case. An OOM kill produces exit 137 and SIGKILL,
			// so an implementation that checks the exit code first reports RE for every
			// memory-limit violation.
			name: "oom kill outranks the non-zero exit it causes",
			out:  ExecOutcome{ExitCode: 137, Signal: 9, OOMKilled: true, MaxRSSKB: 262144},
			ok:   false, want: VerdictMLE,
		},
		{
			// Fallback path for sandbox backends that cannot report the cgroup's oom_kill
			// counter: peak RSS at or above the limit is treated as MLE.
			name: "rss at the limit without an oom flag still means MLE",
			out:  ExecOutcome{ExitCode: 1, MaxRSSKB: 256 * 1024},
			ok:   false, want: VerdictMLE,
		},
		{
			name: "busy loop hits RLIMIT_CPU (SIGXCPU)",
			out:  ExecOutcome{Signal: 24, CPUms: 1000, WallMs: 1050},
			ok:   false, want: VerdictTLE,
		},
		{
			// Burns no CPU at all, so RLIMIT_CPU can never fire. Only the wall-clock
			// watchdog catches this. A judge with a single timeout gets this wrong.
			name: "sleep burns no CPU but trips the wall timer",
			out:  ExecOutcome{Signal: 9, WallKill: true, CPUms: 2, WallMs: 3000},
			ok:   false, want: VerdictTLE,
		},
		{
			// SIGXFSZ (25) also kills the process on a signal, so OLE must outrank RE.
			name: "output flood outranks the signal death it causes",
			out:  ExecOutcome{Signal: 25, FSizeKill: true, StdoutLen: 90 * 1024},
			ok:   false, want: VerdictOLE,
		},
		{
			// Exited cleanly and produced correct output, but wrote past the cap.
			name: "stdout over the cap without SIGXFSZ",
			out:  ExecOutcome{ExitCode: 0, StdoutLen: 65 * 1024, CPUms: 10},
			ok:   true, want: VerdictOLE,
		},
		{
			name: "segfault",
			out:  ExecOutcome{Signal: 11, ExitCode: 139, CPUms: 5},
			ok:   false, want: VerdictRE,
		},
		{
			name: "non-zero exit",
			out:  ExecOutcome{ExitCode: 1, CPUms: 5},
			ok:   false, want: VerdictRE,
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			if got := ClassifyRun(c.out, lim, c.ok); got != c.want {
				t.Fatalf("got %s, want %s", got, c.want)
			}
		})
	}
}

// TestAggregateReportsFirstFailureNotWorst pins two reporting decisions:
//   - the submission verdict is the first non-AC test in INDEX order, because participants
//     expect "failed on test 2", not "the worst thing that happened anywhere";
//   - headline cpu/mem are the MAXIMUM across tests, because the max is a property of the
//     algorithm on its worst input, while the sum is a property of how many tests the
//     setter happened to write.
func TestAggregateReportsFirstFailureNotWorst(t *testing.T) {
	tests := []TestResult{
		{Index: 1, Verdict: VerdictAC, CPUms: 10, MemKB: 1000},
		{Index: 2, Verdict: VerdictWA, CPUms: 12, MemKB: 1100},
		{Index: 5, Verdict: VerdictMLE, CPUms: 40, MemKB: 90000},
	}
	v, cpu, _, mem, failed := Aggregate(tests)

	if v != VerdictWA {
		t.Fatalf("verdict = %s, want WA (first failure in index order, not the most severe)", v)
	}
	if failed != 2 {
		t.Fatalf("failedTest = %d, want 2", failed)
	}
	if cpu != 40 {
		t.Fatalf("cpu = %d, want the MAX across tests (40), not the sum", cpu)
	}
	if mem != 90000 {
		t.Fatalf("mem = %d, want the max across tests", mem)
	}
}

func TestAggregateIgnoresSkippedTests(t *testing.T) {
	// Early exit marks the remaining tests skipped; they must not contribute metrics or
	// mask the real failure.
	tests := []TestResult{
		{Index: 1, Verdict: VerdictAC, CPUms: 10, MemKB: 1000},
		{Index: 2, Verdict: VerdictRE, CPUms: 5, MemKB: 900},
		{Index: 3, Verdict: VerdictAC, CPUms: 9999, MemKB: 9999999, Skipped: true},
	}
	v, cpu, _, mem, failed := Aggregate(tests)
	if v != VerdictRE || failed != 2 {
		t.Fatalf("verdict=%s failed=%d, want RE on test 2", v, failed)
	}
	if cpu != 10 || mem != 1000 {
		t.Fatalf("skipped tests leaked into metrics: cpu=%d mem=%d", cpu, mem)
	}
}

func TestAggregateAllAccepted(t *testing.T) {
	tests := []TestResult{
		{Index: 1, Verdict: VerdictAC, CPUms: 10, MemKB: 1000},
		{Index: 2, Verdict: VerdictAC, CPUms: 30, MemKB: 2000},
	}
	v, cpu, _, mem, failed := Aggregate(tests)
	if v != VerdictAC {
		t.Fatalf("verdict = %s, want AC", v)
	}
	if failed != -1 {
		t.Fatalf("failedTest = %d, want -1 when accepted", failed)
	}
	if cpu != 30 || mem != 2000 {
		t.Fatalf("cpu=%d mem=%d, want the max of each", cpu, mem)
	}
}

func TestVerdictSeverityOrdering(t *testing.T) {
	// WorstOf must respect the documented precedence: IE > CE > OLE > MLE > TLE > RE > WA > AC
	ordered := []Verdict{VerdictAC, VerdictWA, VerdictRE, VerdictTLE, VerdictMLE, VerdictOLE, VerdictCE, VerdictIE}
	for i := 0; i < len(ordered); i++ {
		for j := i + 1; j < len(ordered); j++ {
			if got := WorstOf(ordered[i], ordered[j]); got != ordered[j] {
				t.Fatalf("WorstOf(%s, %s) = %s, want %s", ordered[i], ordered[j], got, ordered[j])
			}
			// Must be symmetric in its argument order.
			if got := WorstOf(ordered[j], ordered[i]); got != ordered[j] {
				t.Fatalf("WorstOf(%s, %s) = %s, want %s", ordered[j], ordered[i], got, ordered[j])
			}
		}
	}
	// An unknown verdict must never be treated as a pass.
	if WorstOf(VerdictAC, Verdict("NONSENSE")) == VerdictAC {
		t.Fatal("an unrecognised verdict must not be treated as accepted")
	}
}

func TestLimitsNormalizeAndScale(t *testing.T) {
	l := Limits{CPUms: 1000}.Normalize()
	if l.WallMs != 3000 {
		t.Fatalf("wall should default to 3x cpu, got %d", l.WallMs)
	}

	// A wall limit below the CPU limit can never be satisfied, so it must be corrected
	// rather than honoured.
	l2 := Limits{CPUms: 2000, WallMs: 500}.Normalize()
	if l2.WallMs < l2.CPUms {
		t.Fatalf("wall %d must not be below cpu %d", l2.WallMs, l2.CPUms)
	}

	// Normalize must be total: a zero value yields a usable envelope.
	z := Limits{}.Normalize()
	if z.CPUms <= 0 || z.WallMs <= 0 || z.MemMB <= 0 || z.Pids <= 0 || z.StdoutKB <= 0 {
		t.Fatalf("zero-value Limits did not normalise to something usable: %+v", z)
	}

	// Per-language fairness: Python gets 4x the time, Java 2.5x plus 128 MB of JVM
	// overhead, so an identical algorithm earns an identical verdict in either language.
	py := DefaultRunLimits().Scale(4.0, 0)
	if py.CPUms != 8000 {
		t.Fatalf("python cpu = %d, want 8000", py.CPUms)
	}
	jv := DefaultRunLimits().Scale(2.5, 128)
	if jv.MemMB != 384 {
		t.Fatalf("java mem = %d, want 384", jv.MemMB)
	}

	// A zero or negative multiplier must not zero out the limits.
	safe := DefaultRunLimits().Scale(0, 0)
	if safe.CPUms != DefaultRunLimits().CPUms {
		t.Fatalf("a zero multiplier must be treated as 1.0, got cpu=%d", safe.CPUms)
	}
}

func TestLimitsValidateRejectsFleetKillers(t *testing.T) {
	// A problem setter chooses limits; they do not get to choose limits that take the
	// judging fleet down.
	if err := DefaultRunLimits().Validate(); err != nil {
		t.Fatalf("default run limits must validate, got %v", err)
	}
	for _, bad := range []Limits{
		{CPUms: 60_000},
		{CPUms: 1000, WallMs: 120_000},
		{CPUms: 1000, MemMB: 8192},
		{CPUms: 1000, StdoutKB: 1 << 20},
		{CPUms: 1000, Pids: 100_000},
	} {
		if err := bad.Validate(); err == nil {
			t.Fatalf("Validate accepted an out-of-range envelope: %+v", bad)
		}
	}
}

func TestRankScoreOrdering(t *testing.T) {
	mk := func(solved int, cpu int64) map[string]UserProblemStat {
		m := map[string]UserProblemStat{}
		for i := 0; i < solved; i++ {
			m[string(rune('a'+i))] = UserProblemStat{Solved: true, BestCPUms: cpu}
		}
		return m
	}

	// Problems solved must dominate execution speed, whatever the penalty.
	if RankScore(ModeSpeed, mk(3, 5000), timeZero) <= RankScore(ModeSpeed, mk(2, 1), timeZero) {
		t.Fatal("solved count must dominate the time penalty")
	}

	// With equal solves, lower cost ranks higher.
	if RankScore(ModeSpeed, mk(2, 100), timeZero) <= RankScore(ModeSpeed, mk(2, 900), timeZero) {
		t.Fatal("with equal solves, lower cost must rank higher")
	}

	// Unsolved problems contribute nothing.
	withUnsolved := mk(2, 100)
	withUnsolved["z"] = UserProblemStat{Solved: false, BestCPUms: 1}
	if RankScore(ModeSpeed, withUnsolved, timeZero) != RankScore(ModeSpeed, mk(2, 100), timeZero) {
		t.Fatal("an unsolved problem must not change the score")
	}

	// Instruction count, when present, is preferred over wall-noisy CPU milliseconds.
	instr := map[string]UserProblemStat{
		"a": {Solved: true, BestCPUms: 9999, Instructions: 1_000_000},
	}
	cpuOnly := map[string]UserProblemStat{
		"a": {Solved: true, BestCPUms: 9999},
	}
	if RankScore(ModeSpeed, instr, timeZero) <= RankScore(ModeSpeed, cpuOnly, timeZero) {
		t.Fatal("instruction count should be used in preference to cpu_ms when available")
	}

	// A penalty larger than the packing scale must saturate, never wrap around and
	// corrupt the ordering.
	huge := map[string]UserProblemStat{
		"a": {Solved: true, BestCPUms: 5_000_000_000},
	}
	if RankScore(ModeSpeed, huge, timeZero) >= RankScore(ModeSpeed, mk(1, 1), timeZero) {
		t.Fatal("a saturating penalty must still rank below a fast solve")
	}
	if RankScore(ModeSpeed, huge, timeZero) <= RankScore(ModeSpeed, mk(0, 0), timeZero)-1e9 {
		t.Fatal("saturation must not push the score below the zero-solve floor")
	}
}

func TestRankScoreICPCUsesTimeAndAttempts(t *testing.T) {
	start := time.Date(2026, 8, 29, 10, 0, 0, 0, time.UTC)

	fast := map[string]UserProblemStat{
		"a": {Solved: true, SolvedAt: start.Add(10 * time.Minute)},
	}
	slow := map[string]UserProblemStat{
		"a": {Solved: true, SolvedAt: start.Add(90 * time.Minute)},
	}
	if RankScore(ModeICPC, fast, start) <= RankScore(ModeICPC, slow, start) {
		t.Fatal("in ICPC mode, solving earlier must rank higher")
	}

	clean := map[string]UserProblemStat{
		"a": {Solved: true, SolvedAt: start.Add(30 * time.Minute)},
	}
	penalised := map[string]UserProblemStat{
		"a": {Solved: true, SolvedAt: start.Add(30 * time.Minute), Attempts: 3},
	}
	if RankScore(ModeICPC, clean, start) <= RankScore(ModeICPC, penalised, start) {
		t.Fatal("rejected attempts must add penalty in ICPC mode")
	}
}

func TestTestPointsSumsOnlyAcceptedCases(t *testing.T) {
	refs := []TestRef{
		{Index: 1, Points: 30},
		{Index: 2, Points: 30},
		{Index: 3, Points: 40},
	}
	results := []TestResult{
		{Index: 1, Verdict: VerdictAC},
		{Index: 2, Verdict: VerdictWA},
		{Index: 3, Verdict: VerdictAC},
	}
	if got := TestPoints(results, refs); got != 70 {
		t.Fatalf("TestPoints = %d, want 70", got)
	}
}
