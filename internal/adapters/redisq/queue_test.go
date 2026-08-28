// file: internal/adapters/redisq/queue_test.go
//go:build integration

package redisq

import (
	"context"
	"testing"
	"time"

	"github.com/AnishPrakash/arena/internal/core"
)

func TestLeaseRedeliveryAfterConsumerDeath(t *testing.T) {
	ctx := context.Background()
	rdb, err := NewClient(ctx, "localhost:6379", 15) // db 15 = scratch
	if err != nil {
		t.Skip("redis unavailable")
	}
	rdb.FlushDB(ctx)

	q, err := NewQueue(ctx, rdb, QueueOpts{
		Stream: "t.jobs", Group: "t", LeaseTTL: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := q.Publish(ctx, core.JobSpec{SubmissionID: "s1", Attempt: 1}); err != nil {
		t.Fatal(err)
	}

	// Consumer A claims it, then "dies" — never acks.
	got, err := q.Consume(ctx, "runner-A", 10, 500*time.Millisecond)
	if err != nil || len(got) != 1 {
		t.Fatalf("consume: %v, n=%d", err, len(got))
	}

	// Consumer B sees nothing new: the message is owned by A.
	fresh, _ := q.Consume(ctx, "runner-B", 10, 100*time.Millisecond)
	if len(fresh) != 0 {
		t.Fatal("message must not be delivered twice while the lease is held")
	}

	// After the lease expires, B reclaims it with an incremented attempt count.
	time.Sleep(1200 * time.Millisecond)
	live, dead, err := q.Reclaim(ctx, "runner-B", time.Second, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(dead) != 0 {
		t.Fatalf("nothing should be dead-lettered yet, got %d", len(dead))
	}
	if len(live) != 1 || live[0].Job.SubmissionID != "s1" {
		t.Fatalf("expected s1 to be reclaimed, got %+v", live)
	}
	if live[0].Job.Attempt < 2 {
		t.Fatalf("attempt should have incremented, got %d", live[0].Job.Attempt)
	}
}

func TestPoisonPillIsDeadLettered(t *testing.T) {
	ctx := context.Background()
	rdb, err := NewClient(ctx, "localhost:6379", 15)
	if err != nil {
		t.Skip("redis unavailable")
	}
	rdb.FlushDB(ctx)
	q, _ := NewQueue(ctx, rdb, QueueOpts{Stream: "p.jobs", Group: "p", LeaseTTL: 50 * time.Millisecond})
	_ = q.Publish(ctx, core.JobSpec{SubmissionID: "boom"})

	// Simulate three runners each claiming it and dying.
	for i := 0; i < 3; i++ {
		_, _ = q.Consume(ctx, "r", 10, 100*time.Millisecond)
		time.Sleep(80 * time.Millisecond)
		_, _, _ = q.Reclaim(ctx, "r", 50*time.Millisecond, 3)
	}
	// XPENDING only returns messages idle longer than minIdle, and the last XCLAIM in
	// the loop above reset that timer to zero. Wait past the lease before the final
	// check, or the message is filtered out and the assertion sees an empty result.
	time.Sleep(80 * time.Millisecond)

	_, dead, _ := q.Reclaim(ctx, "r", 50*time.Millisecond, 3)
	if len(dead) == 0 {
		t.Fatal("a submission that repeatedly kills runners must be dead-lettered, not retried forever")
	}
	if dead[0].Job.SubmissionID != "boom" {
		t.Fatalf("wrong message dead-lettered: %+v", dead[0].Job)
	}
	// It must also be recorded on the DLQ stream, so an operator can see what was dropped
	// and why. Silently discarding a submission is worse than failing it loudly.
	if n, err := rdb.XLen(ctx, "p.jobs.dlq").Result(); err != nil || n == 0 {
		t.Fatalf("dead-lettered message must land on the DLQ stream (len=%d, err=%v)", n, err)
	}
}

func TestLeaderboardOrdering(t *testing.T) {
	ctx := context.Background()
	rdb, err := NewClient(ctx, "localhost:6379", 15)
	if err != nil {
		t.Skip("redis unavailable")
	}
	rdb.FlushDB(ctx)
	lb := NewLeaderboard(rdb)
	start := time.Now().Add(-time.Hour)

	// alice: 2 problems, slow.  bob: 1 problem, very fast.
	_ = lb.Apply(ctx, "c", "alice", "p1", core.UserProblemStat{Solved: true, BestCPUms: 900, SolvedAt: start.Add(time.Minute)}, core.ModeSpeed, start)
	_ = lb.Apply(ctx, "c", "alice", "p2", core.UserProblemStat{Solved: true, BestCPUms: 950, SolvedAt: start.Add(2 * time.Minute)}, core.ModeSpeed, start)
	_ = lb.Apply(ctx, "c", "bob", "p1", core.UserProblemStat{Solved: true, BestCPUms: 5, SolvedAt: start.Add(time.Minute)}, core.ModeSpeed, start)

	top, err := lb.Top(ctx, "c", 10)
	if err != nil || len(top) != 2 {
		t.Fatalf("top: %v %v", err, top)
	}
	if top[0].UserID != "alice" {
		t.Fatal("problems solved must dominate execution speed")
	}

	// A slower re-solve must not worsen an existing best.
	_ = lb.Apply(ctx, "c", "bob", "p1", core.UserProblemStat{Solved: true, BestCPUms: 800, SolvedAt: start.Add(3 * time.Minute)}, core.ModeSpeed, start)
	_, score, _ := lb.RankOf(ctx, "c", "bob")
	if score != 1e9-5 {
		t.Fatalf("bob's best (5ms) must be preserved, score=%v", score)
	}
}
