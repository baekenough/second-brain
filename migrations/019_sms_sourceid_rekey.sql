-- Migration 019: Rekey SMS documents from bodyHash-based to direction-based SourceIDs.
--
-- Background (#144):
--   Old format: sms:{dateMs}:{sha256(addr)[:16]}:{sha256(body)[:8]}
--   New format: sms:{dateMs}:{sha256(addr)[:16]}:{direction}
--
-- The old bodyHash discriminator caused body-only edits to create duplicate
-- documents instead of updating in-place. The new direction-based SourceID is
-- stable: the same message always maps to the same ID regardless of body edits,
-- enabling ON CONFLICT UPDATE (upsert) semantics.
--
-- Scope:
--   Only active sms documents whose 4th segment is an 8-hex-char string
--   (old bodyHash format) AND whose metadata contains a "direction" key.
--   Documents already in the new format (direction string is non-hex) are
--   excluded by the WHERE clause and are safe to re-run (idempotent).
--
-- Collision handling:
--   When multiple old documents map to the same new_id (same ms + addr +
--   direction, different body), only the most-recently-updated document survives.
--   All others are soft-deleted (status='deleted') — never hard-deleted, so
--   they can be recovered by setting status='active' if needed.
--   The soft-delete step runs BEFORE the winner's source_id update to avoid
--   a unique-constraint violation on (source_type, source_id).
--
-- Recovery:
--   Soft-deleted losers retain their old bodyHash source_id and all content.
--   To inspect: SELECT id, source_id, updated_at FROM documents
--               WHERE source_type='sms' AND status='deleted'
--               AND split_part(source_id,':',4) ~ '^[0-9a-f]{8}$';
--   To restore a specific doc: UPDATE documents SET status='active',
--               deleted_at=NULL WHERE id = '<uuid>';

DO $$
DECLARE
    v_rekeyed   bigint := 0;
    v_conflicts bigint := 0;
BEGIN

    -- Step 1: Identify old-format active sms documents that need rekeying.
    -- Compute the new_id for each and rank duplicates (same new_id) by recency.
    -- "Old format" = 4th colon-separated segment is exactly 8 hex characters
    -- (the sha256[:4] bodyHash).  Direction values (received, sent, draft, etc.)
    -- are never pure hex, so the regex is a reliable discriminator.

    -- Step 2: Soft-delete duplicate losers FIRST to avoid unique constraint
    -- violations when we UPDATE the winner's source_id in Step 3.
    -- Losers keep their original (bodyHash) source_id — different from the
    -- winner's new direction-based id — so there is no conflict on status update.
    WITH ranked AS (
        SELECT
            id,
            source_id,
            'sms:' || split_part(source_id, ':', 2)
                || ':' || split_part(source_id, ':', 3)
                || ':' || (metadata ->> 'direction')                 AS new_id,
            row_number() OVER (
                PARTITION BY
                    split_part(source_id, ':', 2),   -- dateMs segment
                    split_part(source_id, ':', 3),   -- addrHash segment
                    (metadata ->> 'direction')        -- direction
                ORDER BY updated_at DESC, id DESC
            )                                                        AS rn
        FROM documents
        WHERE source_type    = 'sms'
          AND status         = 'active'
          AND split_part(source_id, ':', 4) ~ '^[0-9a-f]{8}$'  -- old bodyHash format
          AND metadata ? 'direction'
          AND (metadata ->> 'direction') IS NOT NULL
          AND (metadata ->> 'direction') <> ''
    ),
    losers AS (
        SELECT id FROM ranked WHERE rn > 1
    )
    UPDATE documents
    SET    status     = 'deleted',
           deleted_at = now()
    WHERE  id IN (SELECT id FROM losers);

    GET DIAGNOSTICS v_conflicts = ROW_COUNT;

    -- Safety guard: an unexpectedly high collision count indicates a bug or
    -- anomalous data condition — abort (full rollback, since this entire DO block
    -- runs inside a single implicit transaction) for human review rather than
    -- mass soft-deleting legitimate documents.
    --
    -- Normal expectation: same-millisecond + same-address + same-direction with
    -- a *different* body is extremely rare in practice (single-digit to low-tens
    -- at most on a 18 000-document corpus).  500 is chosen as a conservative
    -- "something is structurally wrong" signal — well above any realistic
    -- collision volume while still low enough to catch a runaway match.
    --
    -- On RAISE EXCEPTION the entire transaction rolls back; no losers are
    -- soft-deleted and no source_ids are updated.  Re-run after investigation.
    IF v_conflicts > 500 THEN
        RAISE EXCEPTION 'sms rekey aborted: collision soft-delete count % exceeds safety threshold 500 — review data before re-running', v_conflicts;
    END IF;

    -- Step 3: Update the winner's source_id to the new stable format.
    -- At this point, no other active document shares the new_id for this
    -- (dateMs, addrHash, direction) tuple, so the unique constraint is safe.
    WITH winners AS (
        SELECT
            id,
            'sms:' || split_part(source_id, ':', 2)
                || ':' || split_part(source_id, ':', 3)
                || ':' || (metadata ->> 'direction')  AS new_id
        FROM documents
        WHERE source_type    = 'sms'
          AND status         = 'active'
          AND split_part(source_id, ':', 4) ~ '^[0-9a-f]{8}$'  -- still old format
          AND metadata ? 'direction'
          AND (metadata ->> 'direction') IS NOT NULL
          AND (metadata ->> 'direction') <> ''
    )
    UPDATE documents d
    SET    source_id  = w.new_id,
           updated_at = now()
    FROM   winners w
    WHERE  d.id = w.id;

    GET DIAGNOSTICS v_rekeyed = ROW_COUNT;

    RAISE NOTICE 'sms rekey: % rekeyed, % collision soft-deleted', v_rekeyed, v_conflicts;

END $$;
