-- migrations/012_summary_columns.sql
--
-- Adds LLM-generated summary columns to the documents table.
-- title_summary: a single-sentence summary of the document (≤ 20 words).
-- bullet_summary: 3-5 bullet points covering key facts, decisions, or actions.
--
-- Fully idempotent: ADD COLUMN IF NOT EXISTS is safe on repeated runs.
-- The summarizer worker (internal/worker/summarizer.go) backfills these columns
-- asynchronously; NULL indicates the document is pending summarization.

ALTER TABLE documents
    ADD COLUMN IF NOT EXISTS title_summary  text,
    ADD COLUMN IF NOT EXISTS bullet_summary text;
