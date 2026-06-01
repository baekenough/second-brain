-- migrations/011_configurable_embedding_dim.sql
--
-- Reshapes documents.embedding to vector(N) ONLY when ALL conditions hold:
--   1. app.embedding_dim GUC is set (by the application before running migrations)
--   2. Current column dimension != desired dimension
--   3. Table has zero rows (no existing embeddings to lose)
-- All other cases: clean no-op exit. Fully idempotent.
--
-- HNSW index is dropped before ALTER and recreated after (pgvector requires this).
-- New installs: set EMBEDDING_DIM=384 in env; app calls SetEmbeddingDimGUC before migrate.Up().
-- Existing installs with data: block exits with NOTICE, column untouched.

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

    -- No GUC → nothing to do. Exit cleanly (legacy / plain psql runs).
    IF v_target_dim IS NULL OR v_target_dim <= 0 THEN
        RAISE NOTICE '011: app.embedding_dim not set, skipping dimension check.';
        RETURN;
    END IF;

    -- Read actual column dimension from catalog.
    -- atttypmod for vector(N) encodes N as (atttypmod - 4) per pgvector convention.
    SELECT atttypmod - 4
      INTO v_current_dim
      FROM pg_attribute
      JOIN pg_class     ON pg_class.oid     = pg_attribute.attrelid
      JOIN pg_namespace ON pg_namespace.oid = pg_class.relnamespace
     WHERE pg_namespace.nspname  = current_schema()
       AND pg_class.relname      = 'documents'
       AND pg_attribute.attname  = 'embedding'
       AND NOT pg_attribute.attisdropped;

    IF v_current_dim IS NULL OR v_current_dim <= 0 THEN
        RAISE NOTICE '011: embedding column not found or already variable-dim, skipping.';
        RETURN;
    END IF;

    -- Idempotent exit when already correct.
    IF v_current_dim = v_target_dim THEN
        RAISE NOTICE '011: embedding column already vector(%), nothing to change.', v_target_dim;
        RETURN;
    END IF;

    -- Guard: only reshape when no rows exist.
    SELECT COUNT(*) INTO v_row_count FROM documents;
    IF v_row_count > 0 THEN
        RAISE NOTICE '011: table has % row(s) — skipping dimension change to prevent data loss. '
                     'Current dim=%, desired dim=%. Drain embeddings first (see docs/embedding-dimension.md).',
                     v_row_count, v_current_dim, v_target_dim;
        RETURN;
    END IF;

    RAISE NOTICE '011: reshaping embedding vector(%) → vector(%) (table is empty).',
                 v_current_dim, v_target_dim;

    -- Drop dependent HNSW index (ALTER TYPE fails while it exists).
    DROP INDEX IF EXISTS idx_documents_embedding;

    -- Reshape column.
    EXECUTE format(
        'ALTER TABLE documents ALTER COLUMN embedding TYPE vector(%s)',
        v_target_dim
    );

    -- Recreate HNSW index with cosine distance.
    EXECUTE format(
        'CREATE INDEX idx_documents_embedding ON documents USING hnsw(embedding vector_cosine_ops)'
    );

    RAISE NOTICE '011: embedding column reshaped to vector(%) and HNSW index recreated.', v_target_dim;
END;
$$;
