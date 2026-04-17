CREATE TABLE IF NOT EXISTS reindex_state (
    id                   SERIAL PRIMARY KEY,
    last_reindex_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    doc_count_at_reindex INTEGER     NOT NULL,
    trigger_reason       TEXT        NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_reindex_state_last ON reindex_state (last_reindex_at DESC);
