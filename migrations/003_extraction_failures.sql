-- Extraction failures tracking (issue #8)
-- Tracks files that failed extraction so they can be retried with exponential back-off.
-- Dead-letter threshold: attempts >= 10 (set automatically on conflict update).
CREATE TABLE IF NOT EXISTS extraction_failures (
    id              BIGSERIAL   PRIMARY KEY,
    source_type     TEXT        NOT NULL,
    source_id       TEXT        NOT NULL,
    file_path       TEXT        NOT NULL,
    error_message   TEXT        NOT NULL,
    attempts        INT         NOT NULL DEFAULT 1,
    next_retry_at   TIMESTAMPTZ NOT NULL DEFAULT now() + INTERVAL '5 minutes',
    dead_letter     BOOLEAN     NOT NULL DEFAULT false,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (source_type, source_id)
);

-- Fast lookup of retryable rows ordered by next_retry_at.
-- Partial index excludes dead-letter rows to keep the index small.
CREATE INDEX IF NOT EXISTS idx_extraction_failures_next_retry
    ON extraction_failures (next_retry_at)
    WHERE NOT dead_letter;

-- Source-type filter used by monitoring / admin queries.
CREATE INDEX IF NOT EXISTS idx_extraction_failures_source
    ON extraction_failures (source_type);
