-- Migration 028: index supporting the cross-source duplicate-arrival guard.
--
-- Background: migration 027 found that 4,692 of the 10,096 secretary/gmail
-- documents were already duplicated under source_type='gmail' with a
-- matching title and occurred_at (within a small tolerance). To stop this
-- class of duplication from recurring silently, internal/store now runs an
-- indexed probe on every document upsert (see
-- internal/store/document_source_guard.go checkDuplicateArrival):
--
--   SELECT source_type FROM documents
--   WHERE title = $1 AND occurred_at BETWEEN $2 AND $3
--     AND source_type <> $4 AND status = 'active'
--   LIMIT 1
--
-- This index makes that probe an index range scan (equality on title,
-- range on occurred_at) rather than a sequential scan over the whole table.
-- It is scoped to status='active' (a partial index) because the guard only
-- ever needs to detect an existing ACTIVE duplicate — deleted/moved rows are
-- irrelevant to a "is this about to duplicate something live" check, and
-- excluding them keeps the index smaller.
CREATE INDEX IF NOT EXISTS idx_documents_title_occurred_at
    ON documents (title, occurred_at)
    WHERE status = 'active';
