-- migrations/014_unsummarized_index.sql
--
-- Adds partial indexes to accelerate background backfill workers.
--
-- index 1: idx_documents_unsummarized
--   Accelerates DocumentStore.ListUnsummarized (internal/store/document.go).
--   Query: WHERE title_summary IS NULL AND status = 'active' ORDER BY collected_at ASC
--
--   A partial index on collected_at filtered to only qualifying rows
--   (active + not yet summarized) lets PostgreSQL resolve the query as an
--   index scan and skip the sort step.  The index shrinks automatically as
--   documents are summarized (title_summary becomes non-NULL), so it stays
--   small regardless of total table size.
--
-- index 2: idx_documents_unembedded
--   Companion index for DocumentStore.ListUnembedded.
--   Query: WHERE embedding IS NULL AND status = 'active' ORDER BY collected_at ASC
--   Same rationale as above: partial index stays small as backfill progresses.
--
-- Idempotent: CREATE INDEX IF NOT EXISTS is safe on repeated runs.
--
-- CONCURRENTLY is intentionally omitted: these migrations run at service
-- startup before traffic begins (same convention as 013_summary_vector.sql).
-- If you need to add these indexes on a live database without locking the
-- table, run the CREATE INDEX CONCURRENTLY statements manually outside of a
-- migration transaction.

CREATE INDEX IF NOT EXISTS idx_documents_unsummarized
    ON documents (collected_at ASC)
    WHERE status = 'active'
      AND title_summary IS NULL;

CREATE INDEX IF NOT EXISTS idx_documents_unembedded
    ON documents (collected_at ASC)
    WHERE status = 'active'
      AND embedding IS NULL;
