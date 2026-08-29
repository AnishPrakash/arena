// file: internal/adapters/redisq/leaderboard.go
package redisq

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/AnishPrakash/arena/internal/core"
	"github.com/AnishPrakash/arena/internal/ports"
)

// applyScript updates one participant's per-problem best and recomputes their rank score
// ATOMICALLY.
//
// Why Lua: read-modify-write from Go would race two concurrent verdicts for the same user
// (very common — participants submit several problems in the same minute) and silently
// lose one. A Lua script runs single-threaded inside Redis, so the read, the comparison
// and the ZADD are one indivisible operation. This is one round trip instead of three,
// which also matters when the leaderboard updates thousands of times a minute.
//
// KEYS[1] = arena:lb:{contest}:stats:{user}   hash  field=problemID value="cpu|instr|attempts|minutes"
// KEYS[2] = arena:lb:{contest}:z              zset  member=userID score=rank score
// ARGV    = userID, problemID, cpuMs, instructions, attempts, minutes, mode, scale
var applyScript = redis.NewScript(`
local cur = redis.call('HGET', KEYS[1], ARGV[2])
local newcpu = tonumber(ARGV[3])
local write = true
if cur then
  local c = tonumber(string.match(cur, "^(%-?%d+)"))
  -- Keep the participant's BEST accepted run; a slower later AC must not worsen their rank.
  if c ~= nil and c <= newcpu then write = false end
end
if write then
  redis.call('HSET', KEYS[1], ARGV[2], ARGV[3]..'|'..ARGV[4]..'|'..ARGV[5]..'|'..ARGV[6])
end

local all = redis.call('HGETALL', KEYS[1])
local solved, penalty = 0, 0
for i = 2, #all, 2 do
  local cpu, instr, att, mins = string.match(all[i], "^(%-?%d+)|(%-?%d+)|(%-?%d+)|(%-?%d+)$")
  if cpu ~= nil then
    solved = solved + 1
    if ARGV[7] == 'speed' then
      local ins = tonumber(instr)
      if ins > 0 then
        penalty = penalty + math.floor(ins / 1000000)
      else
        penalty = penalty + tonumber(cpu)
      end
    else
      penalty = penalty + tonumber(mins) + 20 * tonumber(att)
    end
  end
end

local scale = tonumber(ARGV[8])
if penalty < 0 then penalty = 0 end
if penalty >= scale then penalty = scale - 1 end
local score = solved * scale - penalty
redis.call('ZADD', KEYS[2], score, ARGV[1])
return tostring(score)
`)

type Leaderboard struct {
	rdb *redis.Client
	ttl time.Duration
}

func NewLeaderboard(rdb *redis.Client) *Leaderboard {
	// 30 days: long enough that a contest never expires mid-event, short enough that old
	// contests do not accumulate in memory forever. Postgres remains the durable truth.
	return &Leaderboard{rdb: rdb, ttl: 30 * 24 * time.Hour}
}

func statsKey(contestID, userID string) string {
	return fmt.Sprintf("arena:lb:%s:stats:%s", contestID, userID)
}
func zKey(contestID string) string { return fmt.Sprintf("arena:lb:%s:z", contestID) }

const zsetScale = 1e9

func (l *Leaderboard) Apply(ctx context.Context, contestID, userID, problemID string,
	st core.UserProblemStat, mode core.ScoreMode, contestStart time.Time) error {

	if !st.Solved {
		return nil // only accepted submissions move the board
	}
	minutes := int64(0)
	if !st.SolvedAt.IsZero() && !contestStart.IsZero() {
		minutes = int64(st.SolvedAt.Sub(contestStart).Minutes())
		if minutes < 0 {
			minutes = 0
		}
	}
	sk, zk := statsKey(contestID, userID), zKey(contestID)
	if err := applyScript.Run(ctx, l.rdb, []string{sk, zk},
		userID, problemID, st.BestCPUms, st.Instructions, st.Attempts, minutes,
		string(mode), int64(zsetScale)).Err(); err != nil {
		return err
	}
	pipe := l.rdb.Pipeline()
	pipe.Expire(ctx, sk, l.ttl)
	pipe.Expire(ctx, zk, l.ttl)
	_, err := pipe.Exec(ctx)
	return err
}

// Top reads the board. ZREVRANGE over a sorted set is O(log n + m): serving the top 100 of
// a 100,000-participant contest is microseconds and constant under load, which is exactly
// what a GROUP BY over the submissions table is not.
func (l *Leaderboard) Top(ctx context.Context, contestID string, n int) ([]ports.RankEntry, error) {
	if n <= 0 || n > 500 {
		n = 100
	}
	zs, err := l.rdb.ZRevRangeWithScores(ctx, zKey(contestID), 0, int64(n-1)).Result()
	if err != nil {
		return nil, err
	}
	out := make([]ports.RankEntry, 0, len(zs))
	for i, z := range zs {
		uid, _ := z.Member.(string)
		out = append(out, ports.RankEntry{
			Rank:   int64(i + 1),
			UserID: uid,
			Score:  z.Score,
			// score = solved*1e9 - penalty with 0 <= penalty < 1e9, so a plain
			// division floors 999_999_979 (one problem solved, 21ms penalty) to 0.
			Solved: int(math.Ceil(z.Score / zsetScale)),
		})
	}
	return out, nil
}

func (l *Leaderboard) RankOf(ctx context.Context, contestID, userID string) (int64, float64, error) {
	rank, err := l.rdb.ZRevRank(ctx, zKey(contestID), userID).Result()
	if err != nil {
		return 0, 0, err
	}
	score, err := l.rdb.ZScore(ctx, zKey(contestID), userID).Result()
	return rank + 1, score, err
}

// Rebuild recomputes the whole board from Postgres.
//
// This is the answer to "what if Redis restarts mid-contest": the leaderboard is a cache
// with a deterministic rebuild from durable data, not a primary store. Run it on API boot
// and expose it as an admin endpoint.
func (l *Leaderboard) Rebuild(ctx context.Context, contestID string,
	stats map[string]map[string]core.UserProblemStat, mode core.ScoreMode, start time.Time) error {

	pipe := l.rdb.Pipeline()
	pipe.Del(ctx, zKey(contestID))
	for uid := range stats {
		pipe.Del(ctx, statsKey(contestID, uid))
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return err
	}
	for uid, byProblem := range stats {
		for pid, st := range byProblem {
			if err := l.Apply(ctx, contestID, uid, pid, st, mode, start); err != nil {
				return err
			}
		}
	}
	return nil
}
