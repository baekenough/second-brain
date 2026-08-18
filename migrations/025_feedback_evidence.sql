-- Feedback: per-evidence labelling support (Part D — feedback loop, Task 1).
--
-- ADDITIVE ONLY. This migration adds one nullable column, backfills it, and
-- creates two PARTIAL indexes. There is deliberately no DROP, no ALTER ... TYPE
-- and no SET NOT NULL anywhere in this file: the table already holds real user
-- feedback rows collected since issue #17, and losing them would destroy
-- labels that cannot be regenerated.
--
-- Two design points that make this migration unable to fail on existing data
-- (plan §2.1):
--
--  1. `split` is nullable with a NOT VALID CHECK. NOT VALID skips the re-scan of
--     existing rows, so the constraint cannot reject data that predates it. New
--     and updated rows are still checked.
--  2. The uniqueness index is PARTIAL on `source = 'ask_evidence'`. Existing
--     rows use 'search' / 'discord_bot' / 'api', so the index covers zero rows
--     at creation time and therefore cannot fail on a pre-existing duplicate.
--     A global UNIQUE index on (session_id, query, document_id) could fail here.
--
-- The split rule is fixed by plan §2.2 and MUST match internal/dataset.SplitOf:
--   uint32(first 4 bytes of md5('sbfeedback:v1:' || lower(btrim(query)))) % 100
--   < 70  -> 'train', otherwise 'holdout'
-- In PostgreSQL, ('x'||<8 hex>)::bit(32)::bigint yields the UNSIGNED 32-bit
-- value (verified: 'c01cfb8b' -> 3223124875), which is exactly Go's
-- binary.BigEndian.Uint32. abs() is therefore a no-op and is kept only so the
-- expression stays total if that cast ever changes sign semantics.
--
-- The seed string 'sbfeedback:v1:' is frozen. Changing it reshuffles every past
-- split and silently contaminates the holdout set; a future rule needs a new
-- prefix AND a new column.

ALTER TABLE feedback ADD COLUMN IF NOT EXISTS split TEXT;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint WHERE conname = 'feedback_split_chk'
    ) THEN
        ALTER TABLE feedback
            ADD CONSTRAINT feedback_split_chk
            CHECK (split IS NULL OR split IN ('train','holdout')) NOT VALID;
    END IF;
END
$$;

-- Backfill. Rows without a usable query are not splittable and keep split NULL;
-- the dataset loader ignores NULL splits. Re-running this statement is a no-op
-- because of the `split IS NULL` guard, so an already-assigned row can never be
-- moved between train and holdout by a repeated migration run.
UPDATE feedback SET split = CASE
        WHEN abs(('x' || substr(md5('sbfeedback:v1:' || lower(btrim(query))),1,8))::bit(32)::bigint) % 100 < 70
        THEN 'train' ELSE 'holdout' END
WHERE split IS NULL AND query IS NOT NULL AND btrim(query) <> '';

-- Idempotency key for per-evidence votes: one label per (conversation, query,
-- document). Different sessions asking the same question are independent
-- signals and deliberately produce separate rows.
CREATE UNIQUE INDEX IF NOT EXISTS uq_feedback_ask_evidence
  ON feedback (session_id, md5(lower(btrim(query))), document_id)
  WHERE source = 'ask_evidence';

CREATE INDEX IF NOT EXISTS idx_feedback_split
  ON feedback (split, thumbs) WHERE source = 'ask_evidence';
