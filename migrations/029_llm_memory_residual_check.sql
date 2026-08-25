-- Migration 029: report (never delete) any remaining source_type='llm-memory'
-- documents.
--
-- Background: source_type='llm-memory' was decommissioned on 2026-08-25 after
-- 19,933 session-transcript documents (353,843 chunks, 76.5% of the corpus)
-- were found to be crowding out real search results; those documents were
-- purged and the memory-collector daemons that produced them were stopped on
-- every machine that ran one (see model.SourceLLMMemory's doc comment in
-- internal/model/document.go for the full account).
--
-- Separately, migration 027 (secretary source normalization) intentionally
-- leaves the one metadata->>'kind'='llm-memory' secretary-routed document
-- untouched rather than folding it into this source_type or deleting it.
--
-- This migration performs NO mutation. It only counts how many
-- source_type='llm-memory' rows exist right now (across all statuses, so a
-- soft-deleted remnant is not silently excluded from the count) and reports
-- the number via RAISE NOTICE. Whether any such rows should be purged,
-- reclassified, or left alone is a decision for a human reviewing the count
-- and the individual rows — not something a migration should decide
-- automatically. Deliberately idempotent: re-running only re-reports the
-- current count and changes nothing.
DO $$
DECLARE
    v_remaining bigint := 0;
BEGIN
    SELECT count(*) INTO v_remaining
    FROM documents
    WHERE source_type = 'llm-memory';

    IF v_remaining > 0 THEN
        RAISE NOTICE 'llm-memory residual check: % document(s) still carry source_type=''llm-memory'' — this source_type is deprecated (see model.SourceLLMMemory doc comment); review manually, this migration does not delete or reclassify them',
            v_remaining;
    ELSE
        RAISE NOTICE 'llm-memory residual check: 0 documents remain under source_type=''llm-memory''';
    END IF;
END $$;
