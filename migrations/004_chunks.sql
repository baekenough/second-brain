-- Migration 004: chunks table for chunk-based FTS (issue #9, removes 8 KB truncation from #3).
--
-- Each document row in `documents` is split into overlapping text chunks stored
-- here. The generated `content_tsv` column maintains a per-chunk tsvector using
-- the 'simple' dictionary so that searches are language-agnostic and consistent
-- with the existing documents.tsv search path (which also includes 'simple').
--
-- Embedding columns are intentionally omitted here; they will be added in a
-- follow-up migration once cliproxy /v1/embeddings support is confirmed (TODO(issue#34)).

CREATE TABLE IF NOT EXISTS chunks (
    id              BIGSERIAL   PRIMARY KEY,
    document_id     UUID        NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
    chunk_index     INT         NOT NULL,
    content         TEXT        NOT NULL,
    content_tsv     tsvector    GENERATED ALWAYS AS (to_tsvector('simple', content)) STORED,
    byte_size       INT         NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (document_id, chunk_index)
);

-- Lookup chunks by parent document (used in ReplaceDocument DELETE + cascade).
CREATE INDEX IF NOT EXISTS idx_chunks_document ON chunks (document_id);

-- GIN index on the tsvector column for fast FTS (used in SearchFTS).
CREATE INDEX IF NOT EXISTS idx_chunks_tsv ON chunks USING gin (content_tsv);
