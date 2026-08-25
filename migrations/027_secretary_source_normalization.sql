-- Migration 027: Normalize source_type='secretary' into its real, per-kind
-- source types (gmail, sms, call-log, call-transcript, calendar).
--
-- Background:
--   'secretary' is not a real source — it is a legacy container populated by
--   the decommissioned secretary collector (torn down, see project history)
--   that mixed six unrelated kinds of content under one source_type:
--
--     metadata->>'kind'   count (measured in production, 2026-08-25)
--     ------------------- -----
--     gmail                10096
--     call-transcript       3381
--     sms                   2807
--     call-log              2002
--     calendar                41
--     llm-memory               1
--     -------------------------
--     total                18328
--
--   Because 'secretary' is a single source_type, it cannot be split by the
--   search source-type filter (internal/search) or routed selectively by the
--   query planner (docs/superpowers/specs/2026-08-19-query-planner-design.md).
--   A query for "최근 받은 문자" (recent texts) or "누구와 통화했는지" (who I
--   called) returns an undifferentiated 'secretary' bucket instead of sms /
--   call-log / call-transcript, and 'secretary' ends up crowding out the
--   genuine post-cutover gmail/sms/call-log/call-transcript documents in
--   ranked results.
--
-- Safety verified before writing this migration:
--   A production audit of UNIQUE (source_type, source_id) collisions between
--   each candidate's (new_source_type, source_id) and any EXISTING document
--   already stored under that (source_type, source_id) found ZERO collisions
--   across every kind. This holds because source_id already carries a
--   kind-specific namespace prefix (e.g. "gmail:1965c1008ab534f3",
--   "sms:<dateMs>:<addrHash>:<direction>") that is unique regardless of which
--   source_type value currently wraps it. This migration re-verifies the
--   collision count at execution time (Step 1) rather than trusting the
--   earlier audit, because new secretary/gmail/etc documents may have been
--   collected in between.
--
-- Scope:
--   Only rows with source_type='secretary' AND metadata->>'kind' IN
--   ('gmail','sms','call-log','call-transcript','calendar') are touched.
--
--   metadata->>'kind' = 'llm-memory' (1 row) is INTENTIONALLY EXCLUDED from
--   the WHERE clause below and left as source_type='secretary'. Reasoning:
--   the llm-memory *source* (internal/collector/llm_memory.go, MCP
--   add_note-authored memories) was already decommissioned separately and is
--   unrelated in shape to this one secretary-collected row — normalizing it
--   to 'llm-memory' would silently resurrect a retired source_type for a
--   single legacy document, which is more confusing than leaving it in the
--   container it has always lived in. It is not deleted: deleting content on
--   a *classification* migration is out of scope and would be an
--   irreversible, unrelated side effect.
--
--   Rows with no metadata->>'kind', or a kind outside the five listed above,
--   are not matched by the WHERE clause and are left completely untouched.
--
-- Idempotency:
--   Re-running this migration is a no-op on the second pass: once a row's
--   source_type is updated away from 'secretary', the WHERE source_type =
--   'secretary' predicate no longer matches it. Unlike migration 019 (which
--   restructured source_id and could produce new collisions on re-run), this
--   migration only ever narrows its own candidate set, so idempotency does
--   not depend on any additional guard.
--
-- Reversibility:
--   The original source_type is preserved before being overwritten, via
--   metadata['legacy_source_type'] = 'secretary'. To revert a specific
--   document:
--     UPDATE documents
--     SET source_type = metadata->>'legacy_source_type'
--     WHERE id = '<uuid>' AND metadata ? 'legacy_source_type';
--   (legacy_source_type is intentionally left in metadata after revert too —
--   it is a historical marker, not a mutable working field.)
--
-- Generated columns / other tables (verified, no action needed):
--   - documents.tsv is `GENERATED ALWAYS AS (... to_tsvector(title/content) ...)
--     STORED` (see migrations/001_init.sql). It depends only on title and
--     content, neither of which this migration touches, so tsv is byte-for-byte
--     unchanged by this migration and needs no explicit refresh.
--   - chunks references documents only via document_id (see migrations/004_chunks.sql);
--     it carries no copy of source_type, so it needs no update here.
--
-- Safety guards (abort = full ROLLBACK, since the DO block runs in one
-- implicit transaction):
--   1. Collision guard: if ANY candidate's (new_source_type, source_id) pair
--      already exists as an active-or-not document, abort. A collision here
--      would violate the UNIQUE (source_type, source_id) constraint and
--      indicates the zero-collision assumption above no longer holds.
--   2. Volume guard: if the candidate row count exceeds 18400 (a small margin
--      over the 18328 measured at audit time — this repo has consistently
--      used "current known volume + small margin" as its runaway-match
--      threshold, see migration 019), abort. A count above this signals the
--      WHERE clause is matching more than the known secretary corpus, which
--      is a sign of an unrelated schema or data change since this migration
--      was written, not a legitimate reason to proceed.

DO $$
DECLARE
    v_conflicts bigint := 0;
    v_candidates bigint := 0;
    v_updated bigint := 0;
BEGIN

    -- Step 1: Collision re-check. For every candidate row, does an existing
    -- document already occupy (kind-as-source_type, source_id)? This is the
    -- exact shape of the UNIQUE (source_type, source_id) constraint the
    -- UPDATE in Step 3 would violate.
    SELECT count(*) INTO v_conflicts
    FROM documents c
    JOIN documents d
      ON d.source_type = (c.metadata ->> 'kind')
     AND d.source_id   = c.source_id
    WHERE c.source_type = 'secretary'
      AND c.metadata ->> 'kind' IN ('gmail', 'sms', 'call-log', 'call-transcript', 'calendar');

    IF v_conflicts > 0 THEN
        RAISE NOTICE 'secretary source normalization aborted: % (source_type, source_id) collision(s) detected between secretary candidates and existing documents — the zero-collision assumption no longer holds, review before re-running',
            v_conflicts;
        RAISE EXCEPTION 'secretary source normalization aborted: % collision(s)', v_conflicts;
    END IF;

    -- Step 2: Volume guard. Count the candidate set before touching anything.
    SELECT count(*) INTO v_candidates
    FROM documents
    WHERE source_type = 'secretary'
      AND metadata ->> 'kind' IN ('gmail', 'sms', 'call-log', 'call-transcript', 'calendar');

    IF v_candidates > 18400 THEN
        RAISE NOTICE 'secretary source normalization aborted: % candidate rows exceeds the 18400 safety threshold (18328 measured at audit time + margin) — review before re-running',
            v_candidates;
        RAISE EXCEPTION 'secretary source normalization aborted: % candidates exceeds threshold', v_candidates;
    END IF;

    -- Step 3: Normalize. Preserve the original source_type in metadata
    -- ('legacy_source_type') before overwriting it with the per-kind value.
    -- llm-memory and any kind-less/unrecognized-kind rows are excluded by the
    -- WHERE clause and are not touched by this statement.
    UPDATE documents
    SET source_type = metadata ->> 'kind',
        metadata    = metadata || jsonb_build_object('legacy_source_type', 'secretary'),
        updated_at  = now()
    WHERE source_type = 'secretary'
      AND metadata ->> 'kind' IN ('gmail', 'sms', 'call-log', 'call-transcript', 'calendar');

    GET DIAGNOSTICS v_updated = ROW_COUNT;

    RAISE NOTICE 'secretary source normalization: % of % candidate rows re-typed (gmail/sms/call-log/call-transcript/calendar); llm-memory (1 row) and kind-less/unrecognized rows left as secretary',
        v_updated, v_candidates;

END $$;
