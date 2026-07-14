---
name: project-durable-issue82
description: Issue #82 — durable retry consolidation via internal/durable package (v0.13.0)
metadata:
  type: project
---

Extracted SQL backoff logic from `extraction_failures` into a pure Go helper at `internal/durable/backoff.go`.

**Why:** Backoff/dead-letter was duplicated as inline SQL in the ON CONFLICT clause. Go helper is unit-testable without Postgres.

**How to apply:** `durable.ExtractionBackoff.NextRetryAt(currentAttempts, now)` returns `(retryAt time.Time, deadLetter bool)`. `currentAttempts` is the DB value BEFORE increment (mirrors the SQL ON CONFLICT semantics).

Key design decisions:
- `FirstDelay` (5m) covers the INSERT path (attempts==0); `BaseDelay * Factor^n` covers ON CONFLICT
- `ExtractionFailureStore.Record()` now calls `durable.ExtractionBackoff.NextRetryAt(f.Attempts, now)` and passes `nextRetryAt`/`deadLetter` as SQL params — SQL is now a dumb store
- DB schema unchanged, no new external dependency
- `collector_state` and `reindex_state` are documented as future candidates in code comments but NOT touched

Backoff table (matches SQL LEAST(60, 2^n) minutes):
- attempts=0 → 5min (FirstDelay, INSERT hardcode)
- attempts=1 → 2min, 2→4, 3→8, 4→16, 5→32, 6+→60min (capped)
- dead_letter at attempts=9 (9+1>=10=MaxAttempts)
