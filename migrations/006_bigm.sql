-- 006_bigm.sql
-- Enable pg_bigm extension for Korean 2-gram partial matching.
-- pg_bigm handles Korean 조사/어미 variations without morphological analysis.
-- Requires pg_bigm to be installed in the PostgreSQL instance.

CREATE EXTENSION IF NOT EXISTS pg_bigm;

-- Documents table: 2-gram indexes for Korean partial matching
CREATE INDEX IF NOT EXISTS idx_documents_content_bigm ON documents USING gin (content gin_bigm_ops);
CREATE INDEX IF NOT EXISTS idx_documents_title_bigm ON documents USING gin (title gin_bigm_ops);

-- Chunks table: 2-gram index for chunk-level Korean matching
CREATE INDEX IF NOT EXISTS idx_chunks_content_bigm ON chunks USING gin (content gin_bigm_ops);
