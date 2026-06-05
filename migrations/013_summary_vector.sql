-- migrations/013_summary_vector.sql
--
-- Adds a summary_embedding vector column whose dimension matches the
-- app.embedding_dim GUC (set by the application before running migrations,
-- same pattern as 011_configurable_embedding_dim.sql).
--
-- Design notes:
--   - summary_embedding shares the same embedder as documents.embedding,
--     so the dimension MUST match (configurable via EMBEDDING_DIM env var).
--   - The HNSW index is created WITHOUT CONCURRENTLY because this migration
--     runs at service startup before traffic begins; CONCURRENTLY would fail
--     inside a transaction context and is unnecessary here.
--   - Both the ALTER and CREATE INDEX are idempotent (IF NOT EXISTS).
--
-- Fully idempotent on repeated runs.

DO $$
DECLARE
    v_dim INT;
BEGIN
    -- Read desired dimension from the app-scoped GUC.
    -- true = missing_ok: returns NULL instead of raising when GUC is not set.
    BEGIN
        v_dim := current_setting('app.embedding_dim', true)::INT;
    EXCEPTION WHEN OTHERS THEN
        v_dim := NULL;
    END;

    -- Fall back to 1536 (text-embedding-3-small default) when GUC is absent.
    IF v_dim IS NULL OR v_dim <= 0 THEN
        v_dim := 1536;
    END IF;

    -- Add the summary_embedding column with the resolved dimension.
    -- IF NOT EXISTS makes this safe to re-run even if the column already exists.
    EXECUTE format(
        'ALTER TABLE documents ADD COLUMN IF NOT EXISTS summary_embedding vector(%s)',
        v_dim
    );

    -- Create HNSW index for cosine-distance nearest-neighbour search.
    -- CONCURRENTLY intentionally omitted: this runs at boot before any traffic.
    EXECUTE format(
        'CREATE INDEX IF NOT EXISTS idx_documents_summary_embedding '
        'ON documents USING hnsw (summary_embedding vector_cosine_ops)'
    );

    RAISE NOTICE '013: summary_embedding vector(%) column and HNSW index ensured.', v_dim;
END;
$$;
