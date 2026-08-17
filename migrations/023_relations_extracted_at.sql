-- Migration 023: documents.relations_extracted_at (Part A progress tracking).
--
-- Deliberately a NEW column, not a reuse of migration 018's
-- entities_processed_at. That column belongs to the pre-existing, currently
-- dormant EntityWorker (#77/#86), which extracts a DIFFERENT artifact via a
-- DIFFERENT LLM call. Spec §10.4 states Part A does not replace that
-- pipeline. Sharing the column would: (a) silently skip documents
-- EntityWorker already marked despite never having relation/action
-- extraction attempted, and (b) race EntityWorker if it is ever reactivated
-- (ENTITY_EXTRACTION_ENABLED=true), starving this pipeline of its own scan.
--
-- Additive only.

ALTER TABLE documents
    ADD COLUMN IF NOT EXISTS relations_extracted_at TIMESTAMPTZ;

-- Partial index matches internal/store/document.go's ListPendingForExtraction
-- query exactly (status + null check + source_type scope). occurred_at ASC
-- ordering (not collected_at) since the 30-day window filter in the query is
-- occurred_at-based (spec §5.1).
CREATE INDEX IF NOT EXISTS idx_documents_relations_pending
    ON documents (occurred_at ASC)
    WHERE status = 'active'
      AND relations_extracted_at IS NULL
      AND source_type IN ('gmail', 'sms', 'call-log', 'call-transcript');
