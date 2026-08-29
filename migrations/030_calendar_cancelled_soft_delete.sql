-- Migration 030: soft-delete calendar documents for events Google Calendar
-- reports as cancelled.
--
-- Background:
--   internal/collector/calendar.go recorded metadata.status = ev.Status on
--   every collected event but never branched on that value. A cancelled
--   event (single deletion, or a cancelled instance of a recurring series)
--   therefore stayed status='active' forever, and — because Google Calendar
--   frequently omits summary/description on a cancelled event — several of
--   these rows carry an empty title AND empty content, polluting the search
--   candidate pool with blank documents. The collector-side fix (see
--   CalendarCollector.Collect, CalendarCollector.cancellationStore) now
--   soft-deletes these going forward; this migration is the one-off backfill
--   for rows created before that fix shipped.
--
-- Scope:
--   Only source_type='calendar' rows that are currently status='active' AND
--   whose metadata->>'status' = 'cancelled'. Every other source_type and
--   every non-cancelled calendar row is untouched.
--
-- Safety guard:
--   As of 2026-08-29, production has exactly 13 such rows (occurred_at
--   2026-08-20..2026-08-27). 500 is used as the "something is structurally
--   wrong" threshold (same value and rationale as migration 019's SMS rekey
--   guard) — comfortably above any realistic volume of cancelled-but-still-
--   active calendar rows, while still low enough to catch a runaway match
--   (e.g. a WHERE clause bug that widens the scope far beyond cancelled
--   events). On RAISE EXCEPTION the entire transaction rolls back — no rows
--   are soft-deleted. Re-run after investigation.
--
-- Idempotent: the WHERE clause only ever matches status='active' rows, so a
-- second run always affects 0 rows once the first run has completed.
--
-- Hard delete is never used — rows are soft-deleted (status='deleted',
-- deleted_at=now()) and remain recoverable via
--   UPDATE documents SET status='active', deleted_at=NULL WHERE id = '<uuid>';
DO $$
DECLARE
    v_would_affect bigint := 0;
    v_deleted      bigint := 0;
BEGIN
    SELECT count(*) INTO v_would_affect
    FROM documents
    WHERE source_type = 'calendar'
      AND status = 'active'
      AND metadata ->> 'status' = 'cancelled';

    IF v_would_affect > 500 THEN
        RAISE EXCEPTION 'calendar cancelled soft-delete aborted: % row(s) would be affected, exceeds safety threshold 500 — review data before re-running', v_would_affect;
    END IF;

    UPDATE documents
    SET    status     = 'deleted',
           deleted_at = now()
    WHERE  source_type = 'calendar'
      AND  status = 'active'
      AND  metadata ->> 'status' = 'cancelled';

    GET DIAGNOSTICS v_deleted = ROW_COUNT;

    RAISE NOTICE 'calendar cancelled soft-delete: % row(s) soft-deleted', v_deleted;
END $$;
