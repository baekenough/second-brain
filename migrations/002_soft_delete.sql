-- Add status tracking for document lifecycle (active, deleted, moved)
ALTER TABLE documents ADD COLUMN IF NOT EXISTS status TEXT NOT NULL DEFAULT 'active';
ALTER TABLE documents ADD COLUMN IF NOT EXISTS deleted_at TIMESTAMPTZ;

CREATE INDEX IF NOT EXISTS idx_documents_status ON documents(status);

COMMENT ON COLUMN documents.status IS 'active | deleted | moved';
COMMENT ON COLUMN documents.deleted_at IS 'When the source file was detected as removed';
