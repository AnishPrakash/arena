// file: internal/api/handlers_internal.go
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/AnishPrakash/arena/internal/adapters/redisq"
	"github.com/AnishPrakash/arena/internal/core"
	"github.com/AnishPrakash/arena/internal/obs"
	"github.com/AnishPrakash/arena/internal/ports"
)

type markJudgingReq struct {
	RunnerID string `json:"runner_id"`
	Attempt  int    `json:"attempt"`
}

func (s *Server) handleMarkJudging(w http.ResponseWriter, r *http.Request) {
	var in markJudgingReq
	_ = json.NewDecoder(r.Body).Decode(&in)
	id := chi.URLParam(r, "id")
	if err := s.store.MarkJudging(r.Context(), id, in.RunnerID, in.Attempt); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	s.publishEvent(r.Context(), id, map[string]any{"status": core.StatusJudging, "id": id})
	w.WriteHeader(http.StatusNoContent)
}

// handleResult is the only way a verdict enters the system.
//
// Everything about it is idempotent on purpose: at-least-once queue delivery means this
// endpoint WILL occasionally be called twice for the same submission, and neither the
// database nor the leaderboard may double-count.
func (s *Server) handleResult(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var res core.SubmissionResult
	if err := json.NewDecoder(r.Body).Decode(&res); err != nil {
		writeErr(w, 400, "invalid json")
		return
	}
	if res.SubmissionID == "" {
		writeErr(w, 400, "submission_id required")
		return
	}
	if res.Status == "" {
		res.Status = core.StatusDone
	}
	res.JudgedAt = time.Now()

	// SaveResult is guarded by `status <> 'DONE'`, so a duplicate report is a no-op.
	if err := s.store.SaveResult(ctx, res); err != nil {
		writeErr(w, 500, err.Error())
		return
	}

	sub, err := s.store.GetSubmission(ctx, res.SubmissionID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	problem, err := s.store.GetProblem(ctx, sub.ProblemID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}

	obs.Verdicts.WithLabelValues(string(res.Verdict), sub.Language).Inc()
	if !sub.CreatedAt.IsZero() {
		obs.JudgeDuration.WithLabelValues(sub.Language).Observe(time.Since(sub.CreatedAt).Seconds())
	}

	s.afterVerdict(ctx, sub, res, problem)

	// The in-flight lock is released only now, when judging has genuinely finished. That
	// is what makes "one running job per user" a real fairness guarantee rather than a
	// rate limit with extra steps.
	_ = s.rl.ReleaseInFlight(ctx, sub.UserID)

	w.WriteHeader(http.StatusNoContent)
}

// afterVerdict performs every side effect of a finished submission. New side effects
// (Discord webhook, plagiarism scan, analytics) are added HERE or as bus subscribers -
// never inside the judging loop.
func (s *Server) afterVerdict(ctx context.Context, sub ports.Submission,
	res core.SubmissionResult, problem ports.Problem) {

	// 1. Populate the verdict cache for future identical submissions.
	if res.Verdict != core.VerdictIE {
		if m, ok := s.langs.Get(sub.Language); ok {
			img := res.ImageDigest
			if img == "" {
				img = m.Image
			}
			// Must mirror handleSubmit exactly, limits included, or writes and reads use
			// different keys and the cache silently never hits.
			key := VerdictCacheKey(sub.SourceHash, sub.Language, img, problem.TestdataVersion,
				core.LimitSet{
					Compile: m.EffectiveCompileLimits(),
					Run:     m.ScaleRunLimits(problem.Limits),
				})
			_ = s.store.PutCachedVerdict(ctx, key, res)
		}
	}

	// 2. Leaderboard.
	if res.Verdict == core.VerdictAC {
		if c, err := s.store.GetContest(ctx, problem.ContestID); err == nil {
			_ = s.board.Apply(ctx, problem.ContestID, sub.UserID, problem.ID,
				core.UserProblemStat{
					Solved: true, BestCPUms: res.CPUms, BestMemKB: res.MemKB,
					Instructions: res.Instructions, SolvedAt: res.JudgedAt,
				}, c.ScoringMode, c.StartsAt)
			s.publishEvent(ctx, redisq.TopicLeaderboard(problem.ContestID),
				map[string]any{"contest_id": problem.ContestID, "changed": true})
		}
	}

	// 3. Push to any SSE client watching this submission.
	s.publishEvent(ctx, sub.ID, map[string]any{
		"id": sub.ID, "status": core.StatusDone, "verdict": res.Verdict,
		"cpu_ms": res.CPUms, "mem_kb": res.MemKB, "failed_test": res.FailedTest,
		"compile_log": res.CompileLog,
	})
}
