-- Feedback table (issue #17)
-- Stores user feedback on search results and RAG answers for eval set construction.
CREATE TABLE IF NOT EXISTS feedback (
    id              BIGSERIAL PRIMARY KEY,
    query           TEXT,                           -- original query (nullable for direct doc rating)
    document_id     BIGINT REFERENCES documents(id) ON DELETE SET NULL,
    chunk_id        BIGINT REFERENCES chunks(id)    ON DELETE SET NULL,
    source          TEXT NOT NULL,                  -- "search", "discord_bot", "api", ...
    session_id      TEXT,                           -- optional conversation/session grouping
    user_id         TEXT,                           -- opaque user identifier (bot context: Discord user ID)
    thumbs          SMALLINT NOT NULL,              -- +1 / -1 / 0 (reset)
    comment         TEXT,                           -- optional free-text feedback
    metadata        JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (thumbs BETWEEN -1 AND 1)
);

CREATE INDEX IF NOT EXISTS idx_feedback_user     ON feedback (user_id);
CREATE INDEX IF NOT EXISTS idx_feedback_session  ON feedback (session_id);
CREATE INDEX IF NOT EXISTS idx_feedback_created  ON feedback (created_at DESC);
CREATE INDEX IF NOT EXISTS idx_feedback_document ON feedback (document_id);
