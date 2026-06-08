-- Migration 018: add entities_processed_at to documents (issue #86).
--
-- Without this column, documents for which entity extraction returned zero
-- entities have no entry in document_entities, so ListWithoutEntities returns
-- them on every tick and triggers a redundant LLM call.
--
-- entities_processed_at is set to now() by the EntityWorker once it has
-- attempted extraction for a document (regardless of whether any entities
-- were found). ListWithoutEntities is updated to filter on this column
-- instead of the NOT EXISTS sub-query against document_entities.
--
-- Additive only — no DROP, no destructive changes.

ALTER TABLE documents
    ADD COLUMN IF NOT EXISTS entities_processed_at TIMESTAMPTZ;

-- Backfill: documents that already have at least one entity row are
-- considered processed. Documents with no entity rows keep NULL and will
-- be re-processed once before the column is set.
--
-- Guard: AND entities_processed_at IS NULL makes this idempotent on re-run
-- (already-set values are not overwritten with a new now()).
UPDATE documents d
   SET entities_processed_at = now()
 WHERE entities_processed_at IS NULL
   AND EXISTS (
       SELECT 1 FROM document_entities de WHERE de.document_id = d.id
   );

-- Partial index: accelerates DocumentStore.ListWithoutEntities.
--   Query: WHERE status = 'active' AND entities_processed_at IS NULL
--          ORDER BY collected_at ASC
--
-- Same pattern as idx_documents_unsummarized / idx_documents_unembedded
-- (migration 014).  The index shrinks as the EntityWorker marks documents
-- processed, so it stays small regardless of total table size.
--
-- CONCURRENTLY intentionally omitted — same convention as 014/013:
-- migrations run at startup before traffic begins.
CREATE INDEX IF NOT EXISTS idx_documents_unentitied
    ON documents (collected_at ASC)
    WHERE status = 'active'
      AND entities_processed_at IS NULL;
