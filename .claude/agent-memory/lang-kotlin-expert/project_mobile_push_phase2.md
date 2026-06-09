---
name: project-mobile-push-phase2
description: Phase 2 Android app (second-brain-push) built for Galaxy Flip6 — pushes SMS, call-log, call recordings to second-brain server endpoints
metadata:
  type: project
---

Android sideloadable app created at `mobile/second-brain-push/` per spec `docs/superpowers/specs/2026-06-09-mobile-push-ingest-design.md`.

**Why:** Replace the OneDrive → onedrive-bridge → SMSCollector pipeline which had 3 structural failures (placeholder files, full XML re-parse, no recording support).

**Key decisions:**
- WorkManager PeriodicWorkRequest (3h default, battery-not-low constrained) — no ContentObserver/foreground service/wake lock
- PathDetector scans 5 candidate dirs, caches in DataStore — re-detects when cache is empty
- Cutover epoch = `2026-05-30T00:00:00Z` = 1780099200000 ms (first-run cursor default)
- Recording timestamps parsed as KST (UTC+9) from filenames; epoch values reflect 2026 dates
- Two filename patterns: One UI (`+821012345678_20260601143022.m4a`), Mediweil (`메디웨일_260601_143022.m4a`)
- `ClassifiedSms` retains `rawType` field so server receives the original Android type int (not an ordinal)
- Bearer token stored in `EncryptedSharedPreferences` via `androidx.security.crypto`
- Server endpoints (Phase 2): `POST /api/v1/ingest/messages`, `POST /api/v1/ingest/recording` — built in parallel by Go team

**How to apply:** When resuming Android work, check `mobile/second-brain-push/` for the existing module structure before creating new files.
