// file: internal/adapters/redisq/bus.go
package redisq

import (
	"context"

	"github.com/redis/go-redis/v9"
)

// Bus fans events out across API replicas.
//
// The problem it solves: an SSE connection is held open by ONE API pod, but the verdict
// arrives at whichever pod the runner happened to POST to. Without a broker, the
// participant's browser never hears about their own result. Redis pub/sub makes every pod
// see every event, so any pod can serve any stream — which is what keeps the API layer
// horizontally scalable and freely restartable.
//
// Pub/sub is fire-and-forget: a message published while nobody is subscribed is dropped.
// That is correct here, because the durable record is already in Postgres and the client
// re-reads state on reconnect. Never use this bus for anything that must not be lost.
type Bus struct{ rdb *redis.Client }

func NewBus(rdb *redis.Client) *Bus { return &Bus{rdb: rdb} }

func (b *Bus) Publish(ctx context.Context, topic string, payload []byte) error {
	return b.rdb.Publish(ctx, topic, payload).Err()
}

func (b *Bus) Subscribe(ctx context.Context, topic string) (<-chan []byte, func(), error) {
	ps := b.rdb.Subscribe(ctx, topic)
	if _, err := ps.Receive(ctx); err != nil { // confirm the subscription before returning
		_ = ps.Close()
		return nil, nil, err
	}
	out := make(chan []byte, 16)
	go func() {
		defer close(out)
		for msg := range ps.Channel() {
			select {
			case out <- []byte(msg.Payload):
			default:
				// Slow consumer: drop rather than block the shared pub/sub goroutine.
				// A dropped progress event is harmless; the client polls final state.
			}
		}
	}()
	return out, func() { _ = ps.Close() }, nil
}

// Topic helpers keep channel names in one place.
func TopicSubmission(id string) string  { return "arena:sub:" + id }
func TopicLeaderboard(id string) string { return "arena:lb:" + id }
