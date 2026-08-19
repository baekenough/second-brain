---
name: project-postgres-migration-macmini-ubuntu1
description: PostgreSQL migration macmini (arm64) -> ubuntu1 (x86_64) via Tailscale, 2026-08-19 — moving-tag drift, DATABASE_URL parsing gap, /dev/shm sizing
metadata:
  type: project
---

second-brain's primary Postgres moved from a local macmini container to ubuntu1, bound exclusively on the Tailscale interface (100.68.237.99:5432). Cutover completed 2026-08-19 19:00:12 KST, downtime 17:50:19–19:00:12 (70 min, in line with the 70–170 min plan estimate). Row/index counts verified equal before declaring done: documents 46,603 / chunks 462,340 / entities 7,836 / relations 3,181 / 16 tables / 64 indexes.

Rollback path: the old macmini local postgres was deliberately left running at its dump-time state (not torn down) as the rollback target.

## Three defects found during/after cutover — all worth re-checking on any future DB relocation

1. **pgvector moving-tag drift.** `deploy/postgres/Dockerfile` based on `pgvector/pgvector:pg16` (no patch version) pulled current-day 0.8.6 on rebuild. `pg_dump` does not pin `VERSION` in its `CREATE EXTENSION vector` statement, so an unpinned base image silently installs whatever pgvector build is current at deploy time — HNSW behavior can differ across pgvector patch versions, and the container comes up healthy, the extension installs, indexes build: **every naive smoke check passes.** Fix: pin `0.8.2-pg16` explicitly. The only thing that caught it was a verification step that asked "is this the *same* version" rather than "did the extension install".

2. **DATABASE_URL parsing gap in verification.** A base64-generated password containing `/` broke the URL authority when substituted into `DATABASE_URL`, causing a server restart-crash loop. Neither pre-migration check exercised URL parsing: Task-7-style internal container checks use the local socket, and the pre-cutover connectivity test used `psql -h/-U` with discrete flags. **Only the application itself parses `DATABASE_URL` as a URL** — any credential containing URL-reserved characters (`/`, `@`, `:`, `%`) must be tested through the actual connection-string code path, not just via discrete psql flags. Also found: the parser's own error message bypassed the app's credential-masking layer and printed 28 of 32 password characters to plaintext logs — see [[feedback_credential_masking_bypass_in_error_paths]] in db-postgres-expert memory. Resolution used a 40-char alphanumeric-only password (no URL-reserved chars) rather than patching the masking layer.

3. **`/dev/shm` too small for `maintenance_work_mem=4GB` restore.** Default container `/dev/shm` (64MB) was insufficient for the pg_restore step building 3 HNSW indexes with a large `maintenance_work_mem`. Data load had already succeeded by the time index creation failed — the named volume survived `--force-recreate`, so recovery was just adding `shm_size: "6gb"` to compose and re-running the 3 index builds, not a full restore.

## How to apply

- Any Dockerfile pulling from a rolling/major-only tag (e.g. `pgvector/pgvector:pg16`) that gets restored via `pg_dump`/`pg_restore` should be version-pinned to avoid silent extension-version drift across environments.
- Pre-cutover connectivity checks must include one pass through the exact `DATABASE_URL` string the application will parse, not just discrete host/user/pass flags — reserved URL characters in generated credentials are the failure mode.
- When restoring pgvector/HNSW-heavy dumps with elevated `maintenance_work_mem`, size `/dev/shm` (compose `shm_size`) well above the default 64MB before starting the restore, not after it fails partway through.
- macmini requires a login shell (`zsh -ls`) for `docker` to be on PATH; non-login shells report `command not found`. The `server` container image is distroless (no shell) — use the `web` container for any in-container debugging that needs shell tools.
- compose invocations on macmini require `--env-file .env.local` explicitly or volumes fall back to wrong default paths.

Related: [[project-postgres-migration-macmini-ubuntu1]] (db-postgres-expert copy, restore-mechanics focus), [[feedback_macmini_compose_envfile_flag]] (sys-memory-keeper project memory).
