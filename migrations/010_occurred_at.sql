-- Add occurred_at: the timestamp of the original event (email sent date, calendar
-- event start time, SMS/call time, etc.), distinct from collected_at which records
-- when second-brain ingested the document.
--
-- Nullable: legacy rows and collectors that have no event-time concept keep NULL.
-- "Latest" queries should use: ORDER BY COALESCE(occurred_at, collected_at) DESC
-- so that untagged documents degrade gracefully to ingest order.
--
-- Idempotent: IF NOT EXISTS + column existence guard via DO block.

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE  table_name = 'documents'
          AND  column_name = 'occurred_at'
    ) THEN
        ALTER TABLE documents ADD COLUMN occurred_at TIMESTAMPTZ;
    END IF;
END;
$$;

-- Index supports ORDER BY COALESCE(occurred_at, collected_at) DESC queries
-- that appear in ListRecent / ListBySource. A partial index on non-NULL rows
-- is lighter and covers the secretary / future collectors that set the column.
CREATE INDEX IF NOT EXISTS idx_documents_occurred
    ON documents (occurred_at DESC)
    WHERE occurred_at IS NOT NULL;

-- Backfill secretary documents: parse the "Timestamp: ..." line that
-- buildSecretaryContent() embeds at the start of the content field.
-- secretary stores ISO-8601 / RFC3339 strings (e.g. "2026-01-15T09:30:00+09:00").
-- AT TIME ZONE 'UTC' normalises any offset to UTC before storing as TIMESTAMPTZ.
-- regexp_match returns NULL when the pattern does not match, so the CASE
-- expression leaves occurred_at = NULL rather than erroring on malformed data.
UPDATE documents
SET    occurred_at = (
           CASE
               WHEN (regexp_match(content, 'Timestamp:\s*([^\n]+)'))[1] IS NOT NULL
               THEN (
                   (regexp_match(content, 'Timestamp:\s*([^\n]+)'))[1]
               )::timestamptz
               ELSE NULL
           END
       )
WHERE  source_type = 'secretary'
  AND  occurred_at IS NULL;
