// file: internal/core/verdict.go
package core

// Verdict is the outcome of judging one test case, or of a whole submission.
type Verdict string

const (
	VerdictAC  Verdict = "AC"  // Accepted
	VerdictWA  Verdict = "WA"  // Wrong Answer
	VerdictTLE Verdict = "TLE" // Time Limit Exceeded
	VerdictMLE Verdict = "MLE" // Memory Limit Exceeded
	VerdictRE  Verdict = "RE"  // Runtime Error
	VerdictOLE Verdict = "OLE" // Output Limit Exceeded
	VerdictCE  Verdict = "CE"  // Compilation Error
	VerdictIE  Verdict = "IE"  // Internal Error (our fault, never hidden)
)

// severity orders verdicts so that the *most diagnostic* one wins when several are
// simultaneously true.
//
// Why this exact order matters: a program that is OOM-killed almost always ALSO exits
// non-zero. Naively checking the exit code first reports RE for every MLE, which is the
// classic wrong-verdict bug that makes participants stop trusting a judge. Likewise a
// program killed by SIGXFSZ (output flood) exits on a signal and would otherwise be
// reported as RE.
//
//	IE > CE > OLE > MLE > TLE > RE > WA > AC
func (v Verdict) severity() int {
	switch v {
	case VerdictIE:
		return 70
	case VerdictCE:
		return 60
	case VerdictOLE:
		return 50
	case VerdictMLE:
		return 40
	case VerdictTLE:
		return 30
	case VerdictRE:
		return 20
	case VerdictWA:
		return 10
	case VerdictAC:
		return 0
	default:
		return 70 // unknown is treated as an internal error, never as a pass
	}
}

// IsAccepted reports whether the verdict is a pass.
func (v Verdict) IsAccepted() bool { return v == VerdictAC }

// IsTerminalForSubmission reports whether encountering this verdict on one test means the
// remaining tests need not run in contest mode.
func (v Verdict) IsTerminalForSubmission() bool { return v != VerdictAC }

// WorstOf returns the more severe of two verdicts.
func WorstOf(a, b Verdict) Verdict {
	if b.severity() > a.severity() {
		return b
	}
	return a
}

// Human returns a display string.
func (v Verdict) Human() string {
	switch v {
	case VerdictAC:
		return "Accepted"
	case VerdictWA:
		return "Wrong Answer"
	case VerdictTLE:
		return "Time Limit Exceeded"
	case VerdictMLE:
		return "Memory Limit Exceeded"
	case VerdictRE:
		return "Runtime Error"
	case VerdictOLE:
		return "Output Limit Exceeded"
	case VerdictCE:
		return "Compilation Error"
	default:
		return "Internal Error"
	}
}

// Status is the lifecycle state of a submission, independent of its verdict.
type Status string

const (
	StatusQueued  Status = "QUEUED"
	StatusJudging Status = "JUDGING"
	StatusDone    Status = "DONE"
	StatusFailed  Status = "FAILED" // exhausted retries; verdict will be IE
)
