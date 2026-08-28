// file: internal/adapters/redisq/ratelimit.go
package redisq

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
)

// tokenBucket is a classic bucket implemented in Lua so refill and consume are atomic.
//
// Chosen over a fixed window because a fixed window lets a participant fire 2x the limit
// across a window boundary — precisely the burst that hurts, since it happens at contest
// start when everyone's windows are aligned.
//
// KEYS[1] = bucket key
// ARGV[1] = refill rate (tokens/sec)
// ARGV[2] = burst (bucket capacity)
// ARGV[3] = now (unix millis)
// ARGV[4] = cost
var tokenBucket = redis.NewScript(`
local rate     = tonumber(ARGV[1])
local burst    = tonumber(ARGV[2])
local now      = tonumber(ARGV[3])
local cost     = tonumber(ARGV[4])

local data = redis.call('HMGET', KEYS[1], 'tokens', 'ts')
local tokens = tonumber(data[1])
local ts     = tonumber(data[2])
if tokens == nil then tokens = burst; ts = now end

local delta = math.max(0, now - ts) / 1000.0
tokens = math.min(burst, tokens + delta * rate)

local allowed = 0
local retry_ms = 0
if tokens >= cost then
  tokens = tokens - cost
  allowed = 1
else
  retry_ms = math.ceil(((cost - tokens) / rate) * 1000)
end

redis.call('HSET', KEYS[1], 'tokens', tokens, 'ts', now)
-- Expire idle buckets so a one-off visitor does not occupy memory forever.
redis.call('PEXPIRE', KEYS[1], math.ceil((burst / rate) * 1000) + 60000)
return {allowed, retry_ms}
`)

type RateLimiter struct{ rdb *redis.Client }

func NewRateLimiter(rdb *redis.Client) *RateLimiter { return &RateLimiter{rdb: rdb} }

func (r *RateLimiter) Allow(ctx context.Context, key string, ratePerMin, burst int) (bool, time.Duration, error) {
	if ratePerMin <= 0 {
		return true, 0, nil
	}
	if burst <= 0 {
		burst = 1
	}
	res, err := tokenBucket.Run(ctx, r.rdb, []string{"arena:rl:" + key},
		float64(ratePerMin)/60.0, burst, time.Now().UnixMilli(), 1).Slice()
	if err != nil {
		// Fail OPEN. A Redis blip must not stop a contest; abuse is the lesser risk here,
		// and the one-in-flight lock below still bounds the damage. Make this a conscious,
		// documented choice rather than an accident.
		return true, 0, nil
	}
	allowed, _ := res[0].(int64)
	retryMs, _ := res[1].(int64)
	return allowed == 1, time.Duration(retryMs) * time.Millisecond, nil
}

// AcquireInFlight enforces "at most one judging job per user at a time".
//
// This is the anti-starvation rule most implementations miss. Rate limiting alone does not
// stop a participant with 12 queued submissions from occupying 12 of your judging slots in
// the final two minutes of a contest, while everyone else waits. A per-user lock caps any
// single participant's share of the fleet at one slot, no matter how fast they submit.
func (r *RateLimiter) AcquireInFlight(ctx context.Context, userID string, ttl time.Duration) (bool, error) {
	// The TTL is the safety net: if a runner dies without releasing, the lock frees itself.
	return r.rdb.SetNX(ctx, "arena:inflight:"+userID, "1", ttl).Result()
}

func (r *RateLimiter) ReleaseInFlight(ctx context.Context, userID string) error {
	return r.rdb.Del(ctx, "arena:inflight:"+userID).Err()
}
