// file: internal/api/reconciler.go
package api

import (
	"context"
	"time"

	"github.com/AnishPrakash/arena/internal/core"
	"github.com/AnishPrakash/arena/internal/obs"
)

// StartReconciler is the last line of defence against a lost submission.
//
// Layered recovery, from cheapest to most expensive:
//  1. Queue lease expiry  -> a runner died mid-job                 (Phase 3)
//  2. THIS reconciler     -> the MESSAGE itself was lost (Redis failover, a failed
//     XADD after the row was inserted, an operator FLUSHDB)
//  3. Manual admin replay -> everything else
//
// Without step 2 a participant can sit on "Queued" forever with no error anywhere, which
// is the worst possible failure mode during a timed contest: silent and invisible.
func (s *Server) StartReconciler(ctx context.Context, every, stuckAfter time.Duration) {
	t := time.NewTicker(every)
	go func() {
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.reconcileOnce(ctx, stuckAfter)
			}
		}
	}()
}

func (s *Server) reconcileOnce(ctx context.Context, stuckAfter time.Duration) {
	stuck, err := s.store.StuckSubmissions(ctx, stuckAfter, 100)
	if err != nil || len(stuck) == 0 {
		return
	}
	s.log.Warn("reconciler found stuck submissions", "count", len(stuck))

	for _, sub := range stuck {
		// Persist the increment BEFORE re-publishing. Without this the guard below is
		// unreachable and the reconciler re-enqueues the same submission every tick.
		attempt, err := s.store.BumpAttempt(ctx, sub.ID)
		if err != nil {
			s.log.Error("bump attempt", "submission", sub.ID, "err", err)
			continue
		}
		sub.Attempt = attempt

		if sub.Attempt >= s.cfg.MaxAttempts {
			// Give up honestly. An Internal Error the participant can see and report beats
			// a spinner that never resolves.
			_ = s.store.SaveResult(ctx, core.SubmissionResult{
				SubmissionID: sub.ID, Status: core.StatusFailed, Verdict: core.VerdictIE,
				FailedTest: -1, CompileLog: "judge could not complete this submission after " +
					"repeated attempts; please resubmit",
			})
			obs.DeadLettered.Inc()
			s.publishEvent(ctx, sub.ID, map[string]any{
				"id": sub.ID, "status": core.StatusFailed, "verdict": core.VerdictIE})
			continue
		}
		problem, err := s.store.GetProblem(ctx, sub.ProblemID)
		if err != nil {
			continue
		}
		tests, err := s.store.ListTestCases(ctx, problem.ID)
		if err != nil {
			continue
		}
		m, ok := s.langs.Get(sub.Language)
		if !ok {
			continue
		}
		_ = s.queue.Publish(ctx, core.JobSpec{
			SubmissionID: sub.ID, Attempt: sub.Attempt + 1,
			ContestID: sub.ContestID, ProblemID: sub.ProblemID, UserID: sub.UserID,
			Language: sub.Language, SourceRef: sub.SourceRef, SourceHash: sub.SourceHash,
			ImageDigest: m.Image, TestdataVersion: problem.TestdataVersion, Tests: tests,
			Limits: core.LimitSet{
				Compile: m.EffectiveCompileLimits(),
				Run:     m.ScaleRunLimits(problem.Limits),
			},
			Checker: problem.Checker, Policy: core.DefaultPolicy(), EnqueuedAt: time.Now(),
		})
		obs.LeaseReclaims.Inc()
	}
}

// StartQueueGauges keeps the autoscaling signal fresh in Prometheus.
func (s *Server) StartQueueGauges(ctx context.Context) {
	t := time.NewTicker(5 * time.Second)
	go func() {
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if pending, backlog, err := s.queue.Depth(ctx); err == nil {
					obs.QueuePending.Set(float64(pending))
					obs.QueueBacklog.Set(float64(backlog))
				}
			}
		}
	}()
}
