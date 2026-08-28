-- file: internal/adapters/postgres/migrations/0003_verdict_cache.sql

-- Contest participants resubmit identical code constantly (to re-check, after a page
-- refresh, or by accident). Typically 15-30% of submissions are byte-identical to one
-- already judged. Serving those from cache is the cheapest capacity you will ever add.
--
-- The key binds source, language, image and test data together, so any change to any of
-- them invalidates the entry automatically:
--   sha256(source_hash | language | image_digest | testdata_version)
CREATE TABLE verdict_cache (
    cache_key   text PRIMARY KEY,
    verdict     text   NOT NULL,
    cpu_ms      bigint NOT NULL,
    wall_ms     bigint NOT NULL,
    mem_kb      bigint NOT NULL,
    instructions bigint NOT NULL DEFAULT 0,
    score       int    NOT NULL,
    failed_test int    NOT NULL,
    compile_log text   NOT NULL DEFAULT '',
    payload     jsonb  NOT NULL,      -- full SubmissionResult for replay
    hits        bigint NOT NULL DEFAULT 0,
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX idx_verdict_cache_created ON verdict_cache (created_at);
