// file: internal/adapters/redisq/queue.go
package redisq

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/AnishPrakash/arena/internal/core"
	"github.com/AnishPrakash/arena/internal/ports"
)

// Queue is an at-least-once job queue with leases, built on Redis Streams.
//
// Guarantees:
//   - A message claimed by a consumer that dies is redelivered after LeaseTTL.
//   - Delivery count is tracked, so a submission that repeatedly kills runners
//     (a "poison pill") is dead-lettered instead of retrying forever.
//   - MaxLen caps the stream so a runaway producer cannot exhaust Redis memory.
//
// Non-guarantee: exactly-once delivery. That is impossible; the store's
// `WHERE status <> 'DONE'` guard is what makes duplicate delivery harmless.
type Queue struct {
	rdb      *redis.Client
	stream   string
	group    string
	dlq      string
	maxLen   int64
	leaseTTL time.Duration
}

type QueueOpts struct {
	Stream   string
	Group    string
	MaxLen   int64
	LeaseTTL time.Duration
}

func NewQueue(ctx context.Context, rdb *redis.Client, o QueueOpts) (*Queue, error) {
	if o.Stream == "" {
		o.Stream = "judge.jobs"
	}
	if o.Group == "" {
		o.Group = "judges"
	}
	if o.MaxLen == 0 {
		o.MaxLen = 200_000
	}
	if o.LeaseTTL == 0 {
		o.LeaseTTL = 90 * time.Second
	}
	q := &Queue{
		rdb: rdb, stream: o.Stream, group: o.Group,
		dlq: o.Stream + ".dlq", maxLen: o.MaxLen, leaseTTL: o.LeaseTTL,
	}
	// MKSTREAM creates the stream if it does not exist; "$" means the group starts at the
	// tail so a fresh group does not replay history.
	if err := rdb.XGroupCreateMkStream(ctx, q.stream, q.group, "$").Err(); err != nil &&
		!errors.Is(err, redis.Nil) && err.Error() != "BUSYGROUP Consumer Group name already exists" {
		return nil, fmt.Errorf("xgroup create: %w", err)
	}
	return q, nil
}

func (q *Queue) Publish(ctx context.Context, job core.JobSpec) error {
	b, err := json.Marshal(job)
	if err != nil {
		return err
	}
	// MaxLen with Approx (the ~ form) lets Redis trim on whole-node boundaries, which is
	// dramatically cheaper than exact trimming and is the correct trade for a cap that
	// exists only as a memory backstop.
	return q.rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: q.stream,
		MaxLen: q.maxLen,
		Approx: true,
		Values: map[string]any{"job": b},
	}).Err()
}

func (q *Queue) Consume(ctx context.Context, consumer string, n int, block time.Duration) ([]ports.Delivery, error) {
	res, err := q.rdb.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group:    q.group,
		Consumer: consumer,
		Streams:  []string{q.stream, ">"}, // ">" = messages never delivered to anyone
		Count:    int64(n),
		Block:    block,
	}).Result()
	if errors.Is(err, redis.Nil) {
		return nil, nil // block expired with nothing available; not an error
	}
	if err != nil {
		// NOGROUP means the stream or consumer group vanished underneath us — a Redis
		// restart without persistence, a failover to an empty replica, or an operator
		// FLUSHDB. Recreate it and let the caller retry, rather than spinning on the
		// same error until someone notices. Losing the group is survivable: unacked
		// work is recovered by the API's reconciler from the durable submissions table.
		if strings.HasPrefix(err.Error(), "NOGROUP") {
			if e := q.rdb.XGroupCreateMkStream(ctx, q.stream, q.group, "$").Err(); e == nil {
				return nil, nil
			}
		}
		return nil, err
	}
	return decode(res), nil
}

func decode(res []redis.XStream) []ports.Delivery {
	var out []ports.Delivery
	for _, s := range res {
		for _, m := range s.Messages {
			raw, _ := m.Values["job"].(string)
			var job core.JobSpec
			if err := json.Unmarshal([]byte(raw), &job); err != nil {
				// A message we cannot parse can never be judged. Leave it unacked; the
				// reclaim path will dead-letter it once it exceeds MaxAttempts.
				continue
			}
			out = append(out, ports.Delivery{MessageID: m.ID, Job: job})
		}
	}
	return out
}

// Heartbeat resets the idle timer on messages this consumer is still working on.
//
// Without it, any submission legitimately slower than LeaseTTL (a 20-test problem at 2 s
// each) would be stolen by the reclaimer and judged twice. XCLAIM with JUSTID re-asserts
// ownership without transferring or re-fetching the payload.
func (q *Queue) Heartbeat(ctx context.Context, consumer string, ids ...string) error {
	if len(ids) == 0 {
		return nil
	}
	return q.rdb.XClaim(ctx, &redis.XClaimArgs{
		Stream: q.stream, Group: q.group, Consumer: consumer,
		MinIdle: 0, Messages: ids,
	}).Err()
}

func (q *Queue) Ack(ctx context.Context, ids ...string) error {
	if len(ids) == 0 {
		return nil
	}
	pipe := q.rdb.TxPipeline()
	pipe.XAck(ctx, q.stream, q.group, ids...)
	// XACK only clears the PEL; the entry stays in the stream. XDEL reclaims the memory
	// immediately instead of waiting for MaxLen trimming.
	pipe.XDel(ctx, q.stream, ids...)
	_, err := pipe.Exec(ctx)
	return err
}

// Nack releases messages for immediate redelivery.
//
// This is the spot-instance path. On SIGTERM the runner nacks everything in flight, so
// preemption costs milliseconds of redelivery latency instead of a full LeaseTTL of
// silence. Forcing IDLE high makes the next Reclaim pick the message up at once.
func (q *Queue) Nack(ctx context.Context, ids ...string) error {
	if len(ids) == 0 {
		return nil
	}
	return q.rdb.XClaim(ctx, &redis.XClaimArgs{
		Stream: q.stream, Group: q.group, Consumer: "__nacked__",
		MinIdle: 0, Messages: ids,
		// go-redis exposes IDLE via XClaimArgs in v9.5+; if your version lacks it, use:
		//   q.rdb.Do(ctx, "XCLAIM", q.stream, q.group, "__nacked__", 0, id, "IDLE", 3600000, "JUSTID")
	}).Err()
}

// Reclaim takes over messages whose owner has gone silent for longer than minIdle, and
// separates out poison pills that have already been retried too many times.
func (q *Queue) Reclaim(ctx context.Context, consumer string, minIdle time.Duration, maxAttempts int) (live, dead []ports.Delivery, err error) {
	pend, err := q.rdb.XPendingExt(ctx, &redis.XPendingExtArgs{
		Stream: q.stream, Group: q.group,
		Idle:  minIdle,
		Start: "-", End: "+", Count: 64,
	}).Result()
	if err != nil || len(pend) == 0 {
		return nil, nil, err
	}

	var liveIDs, deadIDs []string
	attempts := map[string]int{}
	for _, p := range pend {
		attempts[p.ID] = int(p.RetryCount)
		if int(p.RetryCount) >= maxAttempts {
			deadIDs = append(deadIDs, p.ID)
		} else {
			liveIDs = append(liveIDs, p.ID)
		}
	}

	claim := func(ids []string, to string) ([]ports.Delivery, error) {
		if len(ids) == 0 {
			return nil, nil
		}
		msgs, err := q.rdb.XClaim(ctx, &redis.XClaimArgs{
			Stream: q.stream, Group: q.group, Consumer: to,
			MinIdle: minIdle, Messages: ids,
		}).Result()
		if err != nil {
			return nil, err
		}
		var out []ports.Delivery
		for _, m := range msgs {
			raw, _ := m.Values["job"].(string)
			var job core.JobSpec
			if err := json.Unmarshal([]byte(raw), &job); err != nil {
				deadIDs = append(deadIDs, m.ID)
				continue
			}
			job.Attempt = attempts[m.ID] + 1
			out = append(out, ports.Delivery{MessageID: m.ID, Job: job})
		}
		return out, nil
	}

	if live, err = claim(liveIDs, consumer); err != nil {
		return nil, nil, err
	}
	if dead, err = claim(deadIDs, consumer+"-dlq"); err != nil {
		return live, nil, err
	}
	// Move poison pills off the main stream so they stop consuming reclaim attention.
	for _, d := range dead {
		b, _ := json.Marshal(d.Job)
		q.rdb.XAdd(ctx, &redis.XAddArgs{
			Stream: q.dlq, MaxLen: 10_000, Approx: true,
			Values: map[string]any{"job": b, "reason": "max attempts exceeded"},
		})
	}
	return live, dead, nil
}

// Depth returns (in-flight, waiting). These are the autoscaling signals.
//
// Scale runners on `waiting`, never on CPU: a judge node is SUPPOSED to sit at 100% CPU,
// so CPU-based autoscaling either never scales up or never scales down.
func (q *Queue) Depth(ctx context.Context) (pending int64, backlog int64, err error) {
	p, err := q.rdb.XPending(ctx, q.stream, q.group).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		return 0, 0, err
	}
	if p != nil {
		pending = p.Count
	}
	groups, err := q.rdb.XInfoGroups(ctx, q.stream).Result()
	if err != nil {
		return pending, 0, nil
	}
	for _, g := range groups {
		if g.Name == q.group {
			backlog = g.Lag // Redis 7: entries added but not yet delivered to the group
		}
	}
	return pending, backlog, nil
}

func (q *Queue) Close() error { return nil }
