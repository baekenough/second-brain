-- migrations/015_chunk_embeddings.sql
--
-- Adds an embedding vector column to the chunks table using the SAME
-- GUC-driven configurable-dimension approach as migration 011:
--
--   1. app.embedding_dim GUC is set by the application before running migrations.
--   2. Current column dimension != desired dimension.
--   3. Table has zero rows (no existing embeddings to lose).
--
-- All other cases: clean no-op exit. Fully idempotent.
--
-- HNSW index is dropped before ALTER and recreated after (pgvector requires this).
-- This mirrors the 011 pattern exactly, targeting the chunks table instead.

DO $$
DECLARE
    v_target_dim  INT;
    v_current_dim INT;
    v_row_count   BIGINT;
BEGIN
    -- Read desired dimension from app-scoped GUC set before migration run.
    BEGIN
        v_target_dim := current_setting('app.embedding_dim')::INT;
    EXCEPTION WHEN OTHERS THEN
        v_target_dim := NULL;
    END;

    -- No GUC → skip (legacy / plain psql runs).
    IF v_target_dim IS NULL OR v_target_dim <= 0 THEN
        RAISE NOTICE '015: app.embedding_dim not set, skipping chunk embedding setup.';
        RETURN;
    END IF;

    -- Check whether the embedding column already exists on chunks.
    SELECT atttypmod - 4
      INTO v_current_dim
      FROM pg_attribute
      JOIN pg_class     ON pg_class.oid     = pg_attribute.attrelid
      JOIN pg_namespace ON pg_namespace.oid = pg_class.relnamespace
     WHERE pg_namespace.nspname  = current_schema()
       AND pg_class.relname      = 'chunks'
       AND pg_attribute.attname  = 'embedding'
       AND NOT pg_attribute.attisdropped;

    IF v_current_dim IS NULL THEN
        -- Column does not exist yet: add it.
        RAISE NOTICE '015: adding chunks.embedding vector(%) column.', v_target_dim;

        EXECUTE format(
            'ALTER TABLE chunks ADD COLUMN embedding vector(%s)',
            v_target_dim
        );

        EXECUTE format(
            'CREATE INDEX idx_chunks_embedding ON chunks USING hnsw(embedding vector_cosine_ops)'
        );

        RAISE NOTICE '015: chunks.embedding vector(%) added with HNSW index.', v_target_dim;
        RETURN;
    END IF;

    -- Column exists — check if dimension matches.
    IF v_current_dim = v_target_dim THEN
        RAISE NOTICE '015: chunks.embedding already vector(%), nothing to change.', v_target_dim;
        RETURN;
    END IF;

    -- Dimension mismatch: only reshape when no rows with existing embeddings exist.
    -- Counting only non-NULL embeddings avoids a false block when the column was
    -- just added (all NULLs) but the table already has text-only chunk rows.
    SELECT COUNT(*) INTO v_row_count FROM chunks WHERE embedding IS NOT NULL;
    IF v_row_count > 0 THEN
        RAISE NOTICE '015: chunks table has % chunk embedding row(s) — skipping dimension change to prevent data loss. '
                     'Current dim=%, desired dim=%. Drain chunk embeddings first.',
                     v_row_count, v_current_dim, v_target_dim;
        RETURN;
    END IF;

    RAISE NOTICE '015: reshaping chunks.embedding vector(%) → vector(%) (table is empty).',
                 v_current_dim, v_target_dim;

    -- Drop dependent HNSW index (ALTER TYPE fails while it exists).
    DROP INDEX IF EXISTS idx_chunks_embedding;

    -- Reshape column.
    EXECUTE format(
        'ALTER TABLE chunks ALTER COLUMN embedding TYPE vector(%s)',
        v_target_dim
    );

    -- Recreate HNSW index.
    EXECUTE format(
        'CREATE INDEX idx_chunks_embedding ON chunks USING hnsw(embedding vector_cosine_ops)'
    );

    RAISE NOTICE '015: chunks.embedding reshaped to vector(%) and HNSW index recreated.', v_target_dim;
END;
$$;
