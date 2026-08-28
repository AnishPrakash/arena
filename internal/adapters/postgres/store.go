// file: internal/adapters/postgres/store.go
package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AnishPrakash/arena/internal/core"
	"github.com/AnishPrakash/arena/internal/ports"
)

var ErrNotFound = errors.New("not found")

type Store struct{ pool *pgxpool.Pool }

func New(ctx context.Context, dsn string, maxConns int32) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	// Connection-pool sizing is a scalability decision, not a default to accept.
	// Postgres handles a few dozen active connections well and degrades past that; a pool
	// per replica of 20 with 4 replicas is 80 sockets, which is fine. If you ever run more
	// replicas than that, put PgBouncer in transaction mode in front rather than raising
	// this number.
	if maxConns > 0 {
		cfg.MaxConns = maxConns
	}
	cfg.MinConns = 2
	cfg.MaxConnLifetime = 30 * time.Minute
	cfg.MaxConnIdleTime = 5 * time.Minute
	cfg.HealthCheckPeriod = 30 * time.Second

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		return nil, fmt.Errorf("postgres ping: %w", err)
	}
	return &Store{pool: pool}, nil
}

func (s *Store) Migrate(ctx context.Context) error { return Migrate(ctx, s.pool) }
func (s *Store) Ping(ctx context.Context) error    { return s.pool.Ping(ctx) }
func (s *Store) Close()                            { s.pool.Close() }

// ------------------------------------------------------------------ users

func (s *Store) CreateUser(ctx context.Context, u ports.User) (ports.User, error) {
	err := s.pool.QueryRow(ctx, `
		INSERT INTO users (handle, email, password_hash, role)
		VALUES ($1,$2,$3,$4)
		RETURNING id, created_at`,
		u.Handle, nullStr(u.Email), u.PasswordHash, u.Role,
	).Scan(&u.ID, &u.CreatedAt)
	return u, err
}

func (s *Store) GetUserByHandle(ctx context.Context, handle string) (ports.User, error) {
	var u ports.User
	var email *string
	err := s.pool.QueryRow(ctx, `
		SELECT id, handle, email, password_hash, role, created_at
		FROM users WHERE handle = $1`, handle,
	).Scan(&u.ID, &u.Handle, &email, &u.PasswordHash, &u.Role, &u.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return u, ErrNotFound
	}
	if email != nil {
		u.Email = *email
	}
	return u, err
}

// ------------------------------------------------------------------ catalogue

const problemCols = `
	id, contest_id, slug, title, statement_md, points,
	cpu_ms, wall_ms, mem_mb, stdout_kb, pids,
	checker_type, checker_config, testdata_version`

func scanProblem(row pgx.Row) (ports.Problem, error) {
	var p ports.Problem
	var cfg []byte
	err := row.Scan(&p.ID, &p.ContestID, &p.Slug, &p.Title, &p.StatementMD, &p.Points,
		&p.Limits.CPUms, &p.Limits.WallMs, &p.Limits.MemMB, &p.Limits.StdoutKB, &p.Limits.Pids,
		&p.Checker.Type, &cfg, &p.TestdataVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return p, ErrNotFound
	}
	if err != nil {
		return p, err
	}
	if len(cfg) > 0 {
		_ = json.Unmarshal(cfg, &p.Checker)
	}
	p.Limits = p.Limits.Normalize()
	return p, nil
}

func (s *Store) GetProblem(ctx context.Context, id string) (ports.Problem, error) {
	return scanProblem(s.pool.QueryRow(ctx,
		`SELECT `+problemCols+` FROM problems WHERE id = $1`, id))
}

func (s *Store) GetProblemBySlug(ctx context.Context, contestSlug, slug string) (ports.Problem, error) {
	return scanProblem(s.pool.QueryRow(ctx,
		`SELECT p.id, p.contest_id, p.slug, p.title, p.statement_md, p.points,
		        p.cpu_ms, p.wall_ms, p.mem_mb, p.stdout_kb, p.pids,
		        p.checker_type, p.checker_config, p.testdata_version
		 FROM problems p JOIN contests c ON c.id = p.contest_id
		 WHERE c.slug = $1 AND p.slug = $2`, contestSlug, slug))
}

func (s *Store) ListProblems(ctx context.Context, contestID string) ([]ports.Problem, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+problemCols+` FROM problems WHERE contest_id = $1 ORDER BY slug`, contestID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ports.Problem
	for rows.Next() {
		p, err := scanProblem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) GetContestBySlug(ctx context.Context, slug string) (ports.Contest, error) {
	var c ports.Contest
	var mode string
	err := s.pool.QueryRow(ctx,
		`SELECT id, slug, title, starts_at, ends_at, scoring_mode FROM contests WHERE slug = $1`,
		slug).Scan(&c.ID, &c.Slug, &c.Title, &c.StartsAt, &c.EndsAt, &mode)
	if errors.Is(err, pgx.ErrNoRows) {
		return c, ErrNotFound
	}
	c.ScoringMode = core.ScoreMode(mode)
	return c, err
}

func (s *Store) ListTestCases(ctx context.Context, problemID string) ([]core.TestRef, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT idx, input_ref, output_ref, is_sample, points
		 FROM test_cases WHERE problem_id = $1 ORDER BY idx`, problemID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []core.TestRef
	for rows.Next() {
		var t core.TestRef
		if err := rows.Scan(&t.Index, &t.InputRef, &t.OutputRef, &t.IsSample, &t.Points); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ------------------------------------------------------------------ submissions

func (s *Store) CreateSubmission(ctx context.Context, in ports.Submission) (ports.Submission, error) {
	err := s.pool.QueryRow(ctx, `
		INSERT INTO submissions
		  (user_id, problem_id, contest_id, language, source_ref, source_hash,
		   status, testdata_version, idempotency_key)
		VALUES ($1,$2,$3,$4,$5,$6,'QUEUED',$7,$8)
		RETURNING id, status, created_at`,
		in.UserID, in.ProblemID, in.ContestID, in.Language, in.SourceRef, in.SourceHash,
		in.TestdataVersion, nullStr(in.IdempotencyKey),
	).Scan(&in.ID, &in.Status, &in.CreatedAt)
	return in, err
}

func (s *Store) FindByIdempotencyKey(ctx context.Context, userID, key string) (ports.Submission, bool, error) {
	if key == "" {
		return ports.Submission{}, false, nil
	}
	sub, err := s.getSubmission(ctx,
		`SELECT `+submissionCols+` FROM submissions WHERE user_id = $1 AND idempotency_key = $2`,
		userID, key)
	if errors.Is(err, ErrNotFound) {
		return sub, false, nil
	}
	return sub, err == nil, err
}

const submissionCols = `
	id, user_id, problem_id, contest_id, language, source_ref, source_hash,
	status, coalesce(verdict,''), cpu_ms, wall_ms, mem_kb, instructions, score,
	failed_test, compile_log, attempt, runner_id, image_digest, testdata_version,
	created_at, judged_at`

func (s *Store) getSubmission(ctx context.Context, q string, args ...any) (ports.Submission, error) {
	var x ports.Submission
	var verdict string
	var judged *time.Time
	var instructions int64
	err := s.pool.QueryRow(ctx, q, args...).Scan(
		&x.ID, &x.UserID, &x.ProblemID, &x.ContestID, &x.Language, &x.SourceRef, &x.SourceHash,
		&x.Status, &verdict, &x.CPUms, &x.WallMs, &x.MemKB, &instructions, &x.Score,
		&x.FailedTest, &x.CompileLog, &x.Attempt, &x.RunnerID, &x.ImageDigest, &x.TestdataVersion,
		&x.CreatedAt, &judged)
	if errors.Is(err, pgx.ErrNoRows) {
		return x, ErrNotFound
	}
	x.Verdict = core.Verdict(verdict)
	x.JudgedAt = judged
	return x, err
}

func (s *Store) GetSubmission(ctx context.Context, id string) (ports.Submission, error) {
	sub, err := s.getSubmission(ctx, `SELECT `+submissionCols+` FROM submissions WHERE id = $1`, id)
	if err != nil {
		return sub, err
	}
	rows, err := s.pool.Query(ctx, `
		SELECT idx, verdict, cpu_ms, wall_ms, mem_kb, exit_code, signal, message, skipped
		FROM submission_tests WHERE submission_id = $1 ORDER BY idx`, id)
	if err != nil {
		return sub, err
	}
	defer rows.Close()
	for rows.Next() {
		var t core.TestResult
		var v string
		if err := rows.Scan(&t.Index, &v, &t.CPUms, &t.WallMs, &t.MemKB,
			&t.ExitCode, &t.Signal, &t.Message, &t.Skipped); err != nil {
			return sub, err
		}
		t.Verdict = core.Verdict(v)
		sub.Tests = append(sub.Tests, t)
	}
	return sub, rows.Err()
}

func (s *Store) MarkJudging(ctx context.Context, id, runnerID string, attempt int) error {
	// The status guard makes this idempotent: a duplicate delivery of an already-finished
	// submission cannot drag it back into JUDGING.
	_, err := s.pool.Exec(ctx, `
		UPDATE submissions SET status='JUDGING', runner_id=$2, attempt=$3
		WHERE id=$1 AND status IN ('QUEUED','JUDGING')`, id, runnerID, attempt)
	return err
}

// SaveResult writes the verdict and per-test rows in ONE transaction.
//
// The `WHERE status <> 'DONE'` guard is what turns the queue's at-least-once delivery into
// effectively-once semantics: if a runner is preempted after reporting but before ACKing,
// the redelivered job's second report is a no-op instead of a double count on the
// leaderboard.
func (s *Store) SaveResult(ctx context.Context, r core.SubmissionResult) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE submissions SET
			  status=$2, verdict=$3, cpu_ms=$4, wall_ms=$5, mem_kb=$6, instructions=$7,
			  score=$8, failed_test=$9, compile_log=$10, runner_id=$11, attempt=$12,
			  image_digest=$13, cpu_model=$14, judged_at=now()
			WHERE id=$1 AND status <> 'DONE'`,
			r.SubmissionID, string(r.Status), string(r.Verdict), r.CPUms, r.WallMs, r.MemKB,
			r.Instructions, r.Score, r.FailedTest, truncate(r.CompileLog, 8000),
			r.RunnerID, r.Attempt, r.ImageDigest, r.CPUModel)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return nil // already finalised by an earlier attempt — not an error
		}
		for _, t := range r.Tests {
			if _, err := tx.Exec(ctx, `
				INSERT INTO submission_tests
				  (submission_id, idx, verdict, cpu_ms, wall_ms, mem_kb, exit_code, signal, message, skipped)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
				ON CONFLICT (submission_id, idx) DO UPDATE SET
				  verdict=EXCLUDED.verdict, cpu_ms=EXCLUDED.cpu_ms, wall_ms=EXCLUDED.wall_ms,
				  mem_kb=EXCLUDED.mem_kb, exit_code=EXCLUDED.exit_code, signal=EXCLUDED.signal,
				  message=EXCLUDED.message, skipped=EXCLUDED.skipped`,
				r.SubmissionID, t.Index, string(t.Verdict), t.CPUms, t.WallMs, t.MemKB,
				t.ExitCode, t.Signal, truncate(t.Message, 500), t.Skipped); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) ListSubmissions(ctx context.Context, f ports.SubmissionFilter) ([]ports.Submission, error) {
	if f.Limit <= 0 || f.Limit > 100 {
		f.Limit = 50
	}
	rows, err := s.pool.Query(ctx, `
		SELECT s.id, s.user_id, u.handle, s.problem_id, s.contest_id, s.language,
		       s.status, coalesce(s.verdict,''), s.cpu_ms, s.mem_kb, s.score,
		       s.failed_test, s.created_at
		FROM submissions s JOIN users u ON u.id = s.user_id
		WHERE ($1 = '' OR s.user_id::text    = $1)
		  AND ($2 = '' OR s.problem_id::text = $2)
		  AND ($3 = '' OR s.contest_id::text = $3)
		ORDER BY s.created_at DESC
		LIMIT $4 OFFSET $5`,
		f.UserID, f.ProblemID, f.ContestID, f.Limit, f.Offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ports.Submission
	for rows.Next() {
		var x ports.Submission
		var v string
		if err := rows.Scan(&x.ID, &x.UserID, &x.Handle, &x.ProblemID, &x.ContestID,
			&x.Language, &x.Status, &v, &x.CPUms, &x.MemKB, &x.Score,
			&x.FailedTest, &x.CreatedAt); err != nil {
			return nil, err
		}
		x.Verdict = core.Verdict(v)
		out = append(out, x)
	}
	return out, rows.Err()
}

// StuckSubmissions backs the reconciler. Queue leases handle a dead runner; this handles
// the rarer case where the message itself was lost (Redis failover, DLQ) and the row would
// otherwise sit in QUEUED forever with a participant staring at a spinner.
func (s *Store) StuckSubmissions(ctx context.Context, olderThan time.Duration, limit int) ([]ports.Submission, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+submissionCols+`
		FROM submissions
		WHERE status IN ('QUEUED','JUDGING') AND created_at < now() - $1::interval
		ORDER BY created_at LIMIT $2`,
		fmt.Sprintf("%d seconds", int(olderThan.Seconds())), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ports.Submission
	for rows.Next() {
		var x ports.Submission
		var v string
		var judged *time.Time
		var instr int64
		if err := rows.Scan(&x.ID, &x.UserID, &x.ProblemID, &x.ContestID, &x.Language,
			&x.SourceRef, &x.SourceHash, &x.Status, &v, &x.CPUms, &x.WallMs, &x.MemKB,
			&instr, &x.Score, &x.FailedTest, &x.CompileLog, &x.Attempt, &x.RunnerID,
			&x.ImageDigest, &x.TestdataVersion, &x.CreatedAt, &judged); err != nil {
			return nil, err
		}
		x.Verdict = core.Verdict(v)
		out = append(out, x)
	}
	return out, rows.Err()
}

// ------------------------------------------------------------------ verdict cache

func (s *Store) CachedVerdict(ctx context.Context, key string) (core.SubmissionResult, bool, error) {
	var payload []byte
	err := s.pool.QueryRow(ctx,
		`UPDATE verdict_cache SET hits = hits + 1 WHERE cache_key = $1 RETURNING payload`,
		key).Scan(&payload)
	if errors.Is(err, pgx.ErrNoRows) {
		return core.SubmissionResult{}, false, nil
	}
	if err != nil {
		return core.SubmissionResult{}, false, err
	}
	var r core.SubmissionResult
	if err := json.Unmarshal(payload, &r); err != nil {
		return r, false, nil // corrupt entry: treat as a miss, never as an error
	}
	return r, true, nil
}

func (s *Store) PutCachedVerdict(ctx context.Context, key string, r core.SubmissionResult) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO verdict_cache
		  (cache_key, verdict, cpu_ms, wall_ms, mem_kb, instructions, score, failed_test, compile_log, payload)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		ON CONFLICT (cache_key) DO NOTHING`,
		key, string(r.Verdict), r.CPUms, r.WallMs, r.MemKB, r.Instructions,
		r.Score, r.FailedTest, truncate(r.CompileLog, 8000), b)
	return err
}

// ------------------------------------------------------------------ standings

// ContestStats returns user -> problem -> best state, computed from accepted submissions.
// This is the DURABLE source of truth used to rebuild the Redis leaderboard after a Redis
// restart or a cache eviction. It is deliberately NOT on the request path.
func (s *Store) ContestStats(ctx context.Context, contestID string) (map[string]map[string]core.UserProblemStat, error) {
	out := map[string]map[string]core.UserProblemStat{}

	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT ON (user_id, problem_id)
		       user_id::text, problem_id::text, cpu_ms, mem_kb, instructions, judged_at
		FROM submissions
		WHERE contest_id = $1 AND verdict = 'AC'
		ORDER BY user_id, problem_id, cpu_ms ASC`, contestID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var uid, pid string
		var st core.UserProblemStat
		var judged *time.Time
		if err := rows.Scan(&uid, &pid, &st.BestCPUms, &st.BestMemKB, &st.Instructions, &judged); err != nil {
			return nil, err
		}
		st.Solved = true
		if judged != nil {
			st.SolvedAt = *judged
		}
		if out[uid] == nil {
			out[uid] = map[string]core.UserProblemStat{}
		}
		out[uid][pid] = st
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Second pass: rejected attempts, needed for the ICPC penalty.
	arows, err := s.pool.Query(ctx, `
		SELECT user_id::text, problem_id::text, count(*)
		FROM submissions
		WHERE contest_id = $1 AND verdict IS NOT NULL AND verdict <> 'AC' AND verdict <> 'CE'
		GROUP BY 1,2`, contestID)
	if err != nil {
		return out, nil // penalties are best-effort; never fail a rebuild over them
	}
	defer arows.Close()
	for arows.Next() {
		var uid, pid string
		var n int
		if err := arows.Scan(&uid, &pid, &n); err != nil {
			return out, nil
		}
		if m, ok := out[uid]; ok {
			if st, ok := m[pid]; ok {
				st.Attempts = n
				m[pid] = st
			}
		}
	}
	return out, nil
}

// ------------------------------------------------------------------ helpers

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n... [truncated]"
}

// Pool exposes the underlying pool. Used only by the seeder and by tests; application code
// goes through the Store methods so that queries stay in one reviewable place.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }
