CREATE EXTENSION IF NOT EXISTS vector;
CREATE EXTENSION IF NOT EXISTS "uuid-ossp";

CREATE TABLE IF NOT EXISTS documents (
    id           UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    source_type  TEXT        NOT NULL,
    source_id    TEXT        NOT NULL,
    title        TEXT        NOT NULL,
    content      TEXT        NOT NULL,
    metadata     JSONB       DEFAULT '{}',
    embedding    vector(1536),
    tsv          tsvector    GENERATED ALWAYS AS (
                     setweight(to_tsvector('english', coalesce(title, '')), 'A') ||
                     setweight(to_tsvector('simple',  coalesce(title, '')), 'A') ||
                     setweight(to_tsvector('english', coalesce(content, '')), 'B') ||
                     setweight(to_tsvector('simple',  coalesce(content, '')), 'B')
                 ) STORED,
    collected_at TIMESTAMPTZ NOT NULL,
    created_at   TIMESTAMPTZ DEFAULT now(),
    updated_at   TIMESTAMPTZ DEFAULT now(),
    UNIQUE (source_type, source_id)
);

CREATE INDEX IF NOT EXISTS idx_documents_tsv        ON documents USING GIN(tsv);
CREATE INDEX IF NOT EXISTS idx_documents_embedding  ON documents USING hnsw(embedding vector_cosine_ops);
CREATE INDEX IF NOT EXISTS idx_documents_source     ON documents(source_type);
CREATE INDEX IF NOT EXISTS idx_documents_collected  ON documents(collected_at DESC);

CREATE TABLE IF NOT EXISTS collection_log (
    id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    source_type     TEXT        NOT NULL,
    started_at      TIMESTAMPTZ NOT NULL,
    finished_at     TIMESTAMPTZ,
    documents_count INT         DEFAULT 0,
    error           TEXT,
    created_at      TIMESTAMPTZ DEFAULT now()
);
