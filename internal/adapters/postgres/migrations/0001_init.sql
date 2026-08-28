-- file: internal/adapters/postgres/migrations/0001_init.sql

CREATE EXTENSION IF NOT EXISTS pgcrypto;   -- gen_random_uuid()
CREATE EXTENSION IF NOT EXISTS citext;     -- case-insensitive handles

-- ---------------------------------------------------------------- identity
CREATE TABLE users (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    handle        citext NOT NULL UNIQUE,
    email         citext,
    password_hash text   NOT NULL,
    role          text   NOT NULL DEFAULT 'participant'
                         CHECK (role IN ('participant','setter','admin')),
    created_at    timestamptz NOT NULL DEFAULT now()
);

-- ---------------------------------------------------------------- catalogue
CREATE TABLE contests (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    slug         text NOT NULL UNIQUE,
    title        text NOT NULL,
    starts_at    timestamptz NOT NULL,
    ends_at      timestamptz NOT NULL,
    scoring_mode text NOT NULL DEFAULT 'speed' CHECK (scoring_mode IN ('speed','icpc')),
    created_at   timestamptz NOT NULL DEFAULT now(),
    CHECK (ends_at > starts_at)
);

CREATE TABLE problems (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    contest_id   uuid NOT NULL REFERENCES contests(id) ON DELETE CASCADE,
    slug         text NOT NULL,
    title        text NOT NULL,
    statement_md text NOT NULL DEFAULT '',
    points       int  NOT NULL DEFAULT 100,

    -- Limits are stored as columns rather than JSON: they are queried, validated by CHECK
    -- constraints, and small in number. JSON here would push validation into application
    -- code and lose the database's own guarantees.
    cpu_ms     int NOT NULL DEFAULT 2000  CHECK (cpu_ms    BETWEEN 100 AND 30000),
    wall_ms    int NOT NULL DEFAULT 6000  CHECK (wall_ms   BETWEEN 100 AND 60000),
    mem_mb     int NOT NULL DEFAULT 256   CHECK (mem_mb    BETWEEN 16  AND 2048),
    stdout_kb  int NOT NULL DEFAULT 1024  CHECK (stdout_kb BETWEEN 1   AND 65536),
    pids       int NOT NULL DEFAULT 64    CHECK (pids      BETWEEN 1   AND 512),

    checker_type   text  NOT NULL DEFAULT 'token'
                         CHECK (checker_type IN ('exact','token','float')),
    checker_config jsonb NOT NULL DEFAULT '{}'::jsonb,

    -- Content hash of the whole test set. Changing a test case changes this, which
    -- (a) keeps old verdicts explainable and (b) invalidates the verdict cache for free.
    testdata_version text NOT NULL DEFAULT 'v0',

    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (contest_id, slug),
    CHECK (wall_ms >= cpu_ms)
);

CREATE TABLE test_cases (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    problem_id uuid NOT NULL REFERENCES problems(id) ON DELETE CASCADE,
    idx        int  NOT NULL,
    input_ref  text NOT NULL,   -- blob key; never the payload itself
    output_ref text NOT NULL,
    is_sample  bool NOT NULL DEFAULT false,
    points     int  NOT NULL DEFAULT 0,
    UNIQUE (problem_id, idx)
);

-- ---------------------------------------------------------------- submissions
CREATE TABLE submissions (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    uuid NOT NULL REFERENCES users(id)    ON DELETE CASCADE,
    problem_id uuid NOT NULL REFERENCES problems(id) ON DELETE CASCADE,
    contest_id uuid NOT NULL REFERENCES contests(id) ON DELETE CASCADE,

    language    text NOT NULL,
    -- Source lives in the blob store. Tens of thousands of 2-20 KB blobs in a hot table
    -- bloat the heap, hurt VACUUM, and stop the working set fitting in cache.
    source_ref  text NOT NULL,
    source_hash text NOT NULL,

    status  text NOT NULL DEFAULT 'QUEUED'
                 CHECK (status IN ('QUEUED','JUDGING','DONE','FAILED')),
    verdict text CHECK (verdict IN ('AC','WA','TLE','MLE','RE','OLE','CE','IE')),

    cpu_ms      bigint NOT NULL DEFAULT 0,
    wall_ms     bigint NOT NULL DEFAULT 0,
    mem_kb      bigint NOT NULL DEFAULT 0,
    instructions bigint NOT NULL DEFAULT 0,
    score       int    NOT NULL DEFAULT 0,
    failed_test int    NOT NULL DEFAULT -1,
    compile_log text   NOT NULL DEFAULT '',

    attempt   int  NOT NULL DEFAULT 0,
    runner_id text NOT NULL DEFAULT '',

    -- Reproducibility triple: with source_hash these four pin a run exactly.
    image_digest     text NOT NULL DEFAULT '',
    testdata_version text NOT NULL DEFAULT '',
    cpu_model        text NOT NULL DEFAULT '',

    idempotency_key text,
    created_at timestamptz NOT NULL DEFAULT now(),
    judged_at  timestamptz
);

CREATE TABLE submission_tests (
    submission_id uuid NOT NULL REFERENCES submissions(id) ON DELETE CASCADE,
    idx           int  NOT NULL,
    verdict       text NOT NULL,
    cpu_ms        bigint NOT NULL DEFAULT 0,
    wall_ms       bigint NOT NULL DEFAULT 0,
    mem_kb        bigint NOT NULL DEFAULT 0,
    exit_code     int  NOT NULL DEFAULT 0,
    signal        int  NOT NULL DEFAULT 0,
    message       text NOT NULL DEFAULT '',
    skipped       bool NOT NULL DEFAULT false,
    PRIMARY KEY (submission_id, idx)
);
