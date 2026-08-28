// file: internal/ports/ports.go
package ports

import (
	"context"
	"io"
	"time"

	"github.com/AnishPrakash/arena/internal/core"
)

// ---------------------------------------------------------------------------
// Execution
// ---------------------------------------------------------------------------

// RunSpec is a single sandboxed execution request.
type RunSpec struct {
	Image       string
	Cmd         []string
	BoxDir      string // host dir bind-mounted to /box
	StdinPath   string // path INSIDE the box, or "" for /dev/null
	StdoutPath  string
	StderrPath  string
	Limits      core.Limits
	Env         []string
	CPUSet      string // pinned core, e.g. "3" — determinism
	Network     bool
	DisableASLR bool
	Name        string // container name, for kill/inspect
}

// Sandbox is the only interface untrusted code execution goes through.
//
// Implementations shipped: dockerSandbox (production), processSandbox (dev only, no
// isolation). Documented upgrade path: gvisorSandbox (runsc runtime — a one-line change to
// the docker adapter) and firecrackerSandbox (microVM per submission). Because every
// caller depends on this interface rather than on Docker, that upgrade is a config value,
// not a rewrite.
type Sandbox interface {
	Run(ctx context.Context, spec RunSpec) (core.ExecOutcome, error)
	// Warm optionally pre-creates containers so a run does not pay creation cost.
	Warm(ctx context.Context, image string, n int) error
	Close() error
}

// ---------------------------------------------------------------------------
// Queue
// ---------------------------------------------------------------------------

// Delivery is one claimed job plus the handle needed to settle it.
type Delivery struct {
	MessageID string
	Job       core.JobSpec
}

// Queue is at-least-once with leases. Handlers MUST be idempotent; the store's unique
// constraint on submission_id turns at-least-once into effectively-once.
type Queue interface {
	Publish(ctx context.Context, job core.JobSpec) error

	// Consume claims up to n messages, blocking up to `block`.
	Consume(ctx context.Context, consumer string, n int, block time.Duration) ([]Delivery, error)

	// Heartbeat resets a message's idle timer so a long-but-healthy job is not stolen.
	Heartbeat(ctx context.Context, consumer string, ids ...string) error

	// Ack removes a completed message permanently.
	Ack(ctx context.Context, ids ...string) error

	// Nack releases a message for immediate redelivery. Called on SIGTERM so spot-instance
	// preemption costs milliseconds instead of a full lease expiry.
	Nack(ctx context.Context, ids ...string) error

	// Reclaim takes over messages idle longer than minIdle (dead runners) and returns
	// those that have exceeded maxAttempts separately so they can be dead-lettered.
	Reclaim(ctx context.Context, consumer string, minIdle time.Duration, maxAttempts int) (live []Delivery, dead []Delivery, err error)

	Depth(ctx context.Context) (pending int64, backlog int64, err error)
	Close() error
}

// ---------------------------------------------------------------------------
// Persistence
// ---------------------------------------------------------------------------

type Store interface {
	// Submissions
	CreateSubmission(ctx context.Context, s Submission) (Submission, error)
	GetSubmission(ctx context.Context, id string) (Submission, error)
	FindByIdempotencyKey(ctx context.Context, userID, key string) (Submission, bool, error)
	MarkJudging(ctx context.Context, id, runnerID string, attempt int) error
	SaveResult(ctx context.Context, r core.SubmissionResult) error
	ListSubmissions(ctx context.Context, f SubmissionFilter) ([]Submission, error)
	StuckSubmissions(ctx context.Context, olderThan time.Duration, limit int) ([]Submission, error)

	// Catalogue
	GetProblem(ctx context.Context, id string) (Problem, error)
	GetProblemBySlug(ctx context.Context, contestSlug, slug string) (Problem, error)
	ListProblems(ctx context.Context, contestID string) ([]Problem, error)
	GetContestBySlug(ctx context.Context, slug string) (Contest, error)
	ListTestCases(ctx context.Context, problemID string) ([]core.TestRef, error)

	// Identity
	CreateUser(ctx context.Context, u User) (User, error)
	GetUserByHandle(ctx context.Context, handle string) (User, error)

	// Verdict cache — see the cost table in 01-ARCHITECTURE §11.
	CachedVerdict(ctx context.Context, key string) (core.SubmissionResult, bool, error)
	PutCachedVerdict(ctx context.Context, key string, r core.SubmissionResult) error

	// Standings rebuild (durable source of truth behind the Redis ZSET)
	ContestStats(ctx context.Context, contestID string) (map[string]map[string]core.UserProblemStat, error)

	Migrate(ctx context.Context) error
	Ping(ctx context.Context) error
	Close()
}

// ---------------------------------------------------------------------------
// Blobs
// ---------------------------------------------------------------------------

type BlobStore interface {
	Put(ctx context.Context, key string, r io.Reader) error
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	// GetToFile writes straight to disk. Test inputs can be tens of MB; never buffer them
	// in the runner's heap — that is exactly the leak the brief warns about.
	GetToFile(ctx context.Context, key, path string) error
	Exists(ctx context.Context, key string) (bool, error)
}

// ---------------------------------------------------------------------------
// Leaderboard + events
// ---------------------------------------------------------------------------

type Leaderboard interface {
	Apply(ctx context.Context, contestID, userID, problemID string, st core.UserProblemStat, mode core.ScoreMode, contestStart time.Time) error
	Top(ctx context.Context, contestID string, n int) ([]RankEntry, error)
	RankOf(ctx context.Context, contestID, userID string) (int64, float64, error)
	Rebuild(ctx context.Context, contestID string, stats map[string]map[string]core.UserProblemStat, mode core.ScoreMode, contestStart time.Time) error
}

type RankEntry struct {
	Rank   int64   `json:"rank"`
	UserID string  `json:"user_id"`
	Handle string  `json:"handle"`
	Score  float64 `json:"score"`
	Solved int     `json:"solved"`
}

// EventBus fans submission events out to every API replica so SSE works behind a load
// balancer. New side effects (webhooks, Discord, plagiarism scan, analytics) subscribe
// here instead of editing the judging loop.
type EventBus interface {
	Publish(ctx context.Context, topic string, payload []byte) error
	Subscribe(ctx context.Context, topic string) (<-chan []byte, func(), error)
}

// RateLimiter is a Redis token bucket.
type RateLimiter interface {
	Allow(ctx context.Context, key string, ratePerMin, burst int) (allowed bool, retryAfter time.Duration, err error)
	// AcquireInFlight enforces "one running job per user" so a spammer cannot occupy the
	// whole fleet during the final minutes of a contest.
	AcquireInFlight(ctx context.Context, userID string, ttl time.Duration) (bool, error)
	ReleaseInFlight(ctx context.Context, userID string) error
}

// ---------------------------------------------------------------------------
// Records
// ---------------------------------------------------------------------------

type User struct {
	ID           string    `json:"id"`
	Handle       string    `json:"handle"`
	Email        string    `json:"email,omitempty"`
	PasswordHash string    `json:"-"`
	Role         string    `json:"role"` // participant | setter | admin
	CreatedAt    time.Time `json:"created_at"`
}

type Contest struct {
	ID          string         `json:"id"`
	Slug        string         `json:"slug"`
	Title       string         `json:"title"`
	StartsAt    time.Time      `json:"starts_at"`
	EndsAt      time.Time      `json:"ends_at"`
	ScoringMode core.ScoreMode `json:"scoring_mode"`
}

type Problem struct {
	ID              string             `json:"id"`
	ContestID       string             `json:"contest_id"`
	Slug            string             `json:"slug"`
	Title           string             `json:"title"`
	StatementMD     string             `json:"statement_md"`
	Limits          core.Limits        `json:"limits"`
	Checker         core.CheckerConfig `json:"checker"`
	TestdataVersion string             `json:"testdata_version"`
	Points          int                `json:"points"`
}

type Submission struct {
	ID              string       `json:"id"`
	UserID          string       `json:"user_id"`
	Handle          string       `json:"handle,omitempty"`
	ProblemID       string       `json:"problem_id"`
	ContestID       string       `json:"contest_id"`
	Language        string       `json:"language"`
	SourceRef       string       `json:"-"`
	SourceHash      string       `json:"source_hash"`
	Status          core.Status  `json:"status"`
	Verdict         core.Verdict `json:"verdict,omitempty"`
	CPUms           int64        `json:"cpu_ms"`
	WallMs          int64        `json:"wall_ms"`
	MemKB           int64        `json:"mem_kb"`
	Score           int          `json:"score"`
	FailedTest      int          `json:"failed_test"`
	CompileLog      string       `json:"compile_log,omitempty"`
	Attempt         int          `json:"attempt"`
	RunnerID        string       `json:"runner_id,omitempty"`
	ImageDigest     string       `json:"image_digest,omitempty"`
	TestdataVersion string       `json:"testdata_version,omitempty"`
	IdempotencyKey  string       `json:"-"`
	CreatedAt       time.Time    `json:"created_at"`
	JudgedAt        *time.Time   `json:"judged_at,omitempty"`
	Tests           []core.TestResult `json:"tests,omitempty"`
}

type SubmissionFilter struct {
	UserID    string
	ProblemID string
	ContestID string
	Limit     int
	Offset    int
}
