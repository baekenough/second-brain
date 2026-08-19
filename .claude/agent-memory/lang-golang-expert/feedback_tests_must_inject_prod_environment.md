---
name: tests-must-inject-prod-environment
description: Tests for environment-sensitive code (timezone, locale, TZ) must inject the PRODUCTION environment value, not the dev machine's — KST-only tests passed while UTC containers were 9h off
metadata:
  type: feedback
---

When code reads an ambient environment value (process timezone, locale, hostname), the test must inject the **production** value, not the value the dev machine happens to have. A test that only exercises the dev-machine value asserts a proposition production never violates.

**Why:** PR #197's date-range filter shipped broken. `internal/intent` resolved "오늘"/"이번 주"/"지난달" in `now.Location()`. Dev machines are KST; the `server`/`collector` containers run `Etc/UTC` (only `eval-runner` gets `TZ: Asia/Seoul`). So "today" was the UTC day — 9 hours behind — and an 18:00 KST calendar document fell outside the window, invisible to `/ask`. `TestClassify_DayBoundaryFollowsCallerTimezone` existed and passed: it injected a `FixedZone("KST", +9h)` clock and verified "a KST caller gets KST days". Production callers pass UTC. The test's proposition and the violated proposition were different.

**How to apply:**
- For clock/zone-dependent Go code, inject a **UTC** clock in at least one test, and pick an instant whose UTC calendar *date* differs from its KST date (e.g. `2026-08-17 23:00 UTC` == `2026-08-18 08:00 KST`). Month-edge instants (`2026-08-31 20:00 UTC` == `2026-09-01 05:00 KST`) catch the harsher "지난달 is off by a whole month" variant.
- Add a zone-invariance assertion: the same instant expressed in several zones must yield identical output. That kills the whole class rather than one instance.
- Run the suite under both `TZ=Etc/UTC` and `TZ=Asia/Seoul`; agreement across the two is the real verification, not a single local pass.
- Never resolve calendar boundaries or render user-facing dates in the process-local zone. Use `internal/timeutil.KST()` (`LoadLocation("Asia/Seoul")` with a `FixedZone` fallback) — one definition, shared by `internal/api` and `internal/intent`, so the prompt's "today" and the retrieval window cannot drift apart.
- Fixing this in the app code is preferred over fixing it in `docker-compose.local.yml` via `TZ`: the correctness must not depend on deploy config.

Related: [[test-doubles-must-fail]] (same failure shape — a test double/environment too friendly to reproduce production).
