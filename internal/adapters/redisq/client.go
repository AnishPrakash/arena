// file: internal/adapters/redisq/client.go
package redisq

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

func NewClient(ctx context.Context, addr string, db int) (*redis.Client, error) {
	rdb := redis.NewClient(&redis.Options{
		Addr: addr,
		DB:   db,
		// Sized for a burst: 200 idle-capable connections costs Redis almost nothing and
		// avoids connection-establishment latency spikes at contest start.
		PoolSize:        200,
		MinIdleConns:    10,
		ConnMaxIdleTime: 5 * time.Minute,
		DialTimeout:     3 * time.Second,
		ReadTimeout:     3 * time.Second,
		WriteTimeout:    3 * time.Second,
		MaxRetries:      3,
	})
	if err := rdb.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("redis ping %s: %w", addr, err)
	}
	return rdb, nil
}
