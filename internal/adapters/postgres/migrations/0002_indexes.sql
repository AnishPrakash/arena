-- file: internal/adapters/postgres/migrations/0002_indexes.sql

-- "My submissions for this problem", the single most frequent authenticated read.
CREATE INDEX idx_sub_user_problem
    ON submissions (user_id, problem_id, created_at DESC);

-- Contest-wide feed / admin view.
CREATE INDEX idx_sub_contest_created
    ON submissions (contest_id, created_at DESC);

-- PARTIAL index for the reconciler that finds stuck work.
-- After a contest, >99% of rows are DONE. A full index on status would be almost entirely
-- dead weight in cache; this one stays tiny forever because it only holds live rows, so
-- the reconciler scans thousands rather than millions.
CREATE INDEX idx_sub_inflight
    ON submissions (created_at)
    WHERE status IN ('QUEUED','JUDGING');

-- Idempotency. Partial-unique so the (common) NULL case is not indexed at all.
CREATE UNIQUE INDEX idx_sub_idem
    ON submissions (user_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL;

-- Duplicate-source detection: cheap plagiarism signal, and the key for the verdict cache.
CREATE INDEX idx_sub_source_hash
    ON submissions (problem_id, source_hash);

-- Standings rebuild reads only accepted rows.
CREATE INDEX idx_sub_accepted
    ON submissions (contest_id, user_id, problem_id, cpu_ms)
    WHERE verdict = 'AC';

CREATE INDEX idx_testcases_problem ON test_cases (problem_id, idx);
