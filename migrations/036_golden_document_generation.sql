-- Explicit generation can replenish questions from current source documents.
-- Existing questions, completed statuses and judgments remain unchanged.
ALTER TABLE golden_queries DROP CONSTRAINT IF EXISTS golden_queries_source_check;
ALTER TABLE golden_queries ADD CONSTRAINT golden_queries_source_check
    CHECK (source IN ('ask_history', 'seed', 'manual', 'hermes', 'document'));

ALTER TABLE golden_queries ADD COLUMN IF NOT EXISTS source_document_id UUID
    REFERENCES documents(id) ON DELETE SET NULL;

CREATE UNIQUE INDEX IF NOT EXISTS idx_golden_queries_source_document
    ON golden_queries(source_document_id) WHERE source_document_id IS NOT NULL;
