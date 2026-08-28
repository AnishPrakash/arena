// file: internal/core/scoring.go
package core

import "time"

// ScoreMode selects how the leaderboard ranks participants.
type ScoreMode string

const (
	// ModeSpeed ranks by problems solved, then by total measured execution cost.
	// This is the mode for a "speed-coding, algorithmic efficiency" event.
	ModeSpeed ScoreMode = "speed"

	// ModeICPC ranks by problems solved, then by time-to-solve plus a penalty per
	// rejected attempt. The classic contest formula.
	ModeICPC ScoreMode = "icpc"
)

// UserProblemStat is one participant's best state on one problem.
type UserProblemStat struct {
	Solved       bool
	Attempts     int // rejected attempts before the accepted one
	BestCPUms    int64
	BestMemKB    int64
	Instructions int64
	SolvedAt     time.Time
}

// zsetScale packs a two-level ordering into the single float64 that a Redis sorted set
// stores. float64 holds integers exactly up to 2^53 (~9.0e15), so with a 1e9 scale we can
// represent up to ~9,000,000 solved problems and a penalty below 1e9 without any loss of
// precision. Both bounds are comfortably beyond any real contest.
const zsetScale = 1e9

// RankScore returns the value to ZADD. Higher is better; read the board with ZREVRANGE.
//
// The trick is to make "problems solved" dominate every possible penalty by scaling it
// above the penalty's maximum, so a single sorted set gives an exact multi-key ordering
// with no post-sort in application code — which is what keeps the leaderboard O(log n)
// instead of a GROUP BY over the submissions table.
func RankScore(mode ScoreMode, stats map[string]UserProblemStat, contestStart time.Time) float64 {
	var solved int64
	var penalty int64

	for _, s := range stats {
		if !s.Solved {
			continue
		}
		solved++
		switch mode {
		case ModeSpeed:
			// Cost of the accepted solution. Prefer the deterministic instruction count
			// when the runner was able to measure it; fall back to CPU milliseconds.
			if s.Instructions > 0 {
				penalty += s.Instructions / 1_000_000 // millions of instructions
			} else {
				penalty += s.BestCPUms
			}
		case ModeICPC:
			penalty += int64(s.SolvedAt.Sub(contestStart).Minutes())
			penalty += int64(s.Attempts) * 20
		}
	}

	if penalty < 0 {
		penalty = 0
	}
	if penalty >= zsetScale {
		penalty = zsetScale - 1 // saturate rather than corrupt the ordering
	}
	return float64(solved)*zsetScale - float64(penalty)
}

// TestPoints computes a partial score for subtask-style problems. Contest mode ignores it
// (all-or-nothing), but having it here means adding subtasks later is a scoring-config
// change, not a judge change.
func TestPoints(tests []TestResult, perTest []TestRef) int {
	total := 0
	byIdx := make(map[int]int, len(perTest))
	for _, r := range perTest {
		byIdx[r.Index] = r.Points
	}
	for _, t := range tests {
		if t.Verdict.IsAccepted() {
			total += byIdx[t.Index]
		}
	}
	return total
}
