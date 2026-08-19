---
name: test-doubles-must-fail
description: In this repo, a test double whose write methods cannot fail hides context/retry bugs — add error injection AND record ctx.Err() at call time
metadata:
  type: feedback
---

Every test double for a store/persistence interface must be able to FAIL on
every write method, and must record `ctx.Err()` at call time.

**Why:** two Critical bugs shipped through a green CI on `feature/capture-backend`
(2026-08-17) for exactly this reason. `fakeNoteLister` had an error field only
for `ListPendingNotes`; `MarkNoteEnriched`, `MarkNoteEnrichmentAttemptFailed`
and `MarkNoteEnrichmentTerminal` could not be made to fail in any test. Every
retry-policy test therefore ran in the one world where "bookkeeping runs on an
expired context" and "a failed commit write is swallowed" are both invisible.

The `ctx.Err()` recording is the non-obvious half. A write handed an
already-expired context still *calls* the fake — it just calls it with a dead
context, and pgx would have rejected it before touching the database. A test
asserting only "the call happened" passes against the bug. Only
`if call.ctxErr != nil { t.Errorf(...) }` catches it.

**How to apply:** when writing or reviewing a `fake*`/`mock*` store double here,
check (1) can each write method be made to fail? (2) does each record the
context error it was handed? (3) does the fake model state transitions
(persisted counters written back, terminal rows leaving the pending list) so
multi-tick behaviour is assertable rather than one isolated call? If a
production change is meant to fix a bug, verify the new test actually FAILS
against the reverted change before committing — a test green both before and
after is not a regression test.

Related: [[project_note_capture_enrichment]], [[feedback_no_blocking_remote_calls_in_write_path]]
