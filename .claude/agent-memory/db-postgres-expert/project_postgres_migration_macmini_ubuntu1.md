---
name: project-postgres-migration-macmini-ubuntu1
description: Logical dump/restore mechanics for the macmini->ubuntu1 postgres cutover, 2026-08-19 — timing, extension pinning, maintenance_work_mem/shm relationship
metadata:
  type: project
---

second-brain's Postgres relocated from macmini (arm64) to ubuntu1 (x86_64), Tailscale-only bind (100.68.237.99:5432), 2026-08-19. Logical dump/restore (not streaming replication or physical copy) was used given the arch change.

## Measured timing (useful as a future estimation baseline)

- `pg_dump`: 4m58s for 4.1GB
- Transfer over Tailscale: 97s at ~49MB/s
- Total downtime: 70 minutes (plan had estimated 70–170 min — actual landed at the low end)
- Post-restore verification: row/index counts compared exactly — documents 46,603 / chunks 462,340 / entities 7,836 / relations 3,181 / 16 tables / 64 indexes, all matching source

## Extension version pinning — pg_dump does not capture it

`pg_dump`'s `CREATE EXTENSION vector` statement has no `VERSION` clause by default, so a restore onto a container built from an unpinned image tag (e.g. `pgvector/pgvector:pg16`) can silently install a different pgvector patch version than the source had (0.8.2 source vs 0.8.6 pulled at restore time here). HNSW index behavior is not guaranteed identical across pgvector patch versions. This is invisible to "did the extension install / did the index build" checks — those pass regardless. The only catch is an explicit version-equality check against source. See [[project_postgres_migration_macmini_ubuntu1]] (infra-docker-expert copy) for the Dockerfile-pinning fix.

## `/dev/shm` sizing is coupled to `maintenance_work_mem` during index-heavy restores

Restoring a dump that rebuilds HNSW (or any parallel-build) indexes with an elevated `maintenance_work_mem` (4GB here) can exceed the container's default `/dev/shm` (64MB), causing index creation to fail partway through — while the table data itself has already loaded successfully. Because pg_restore's table-copy and index-build are separate phases, a named-volume-backed container survives `--force-recreate` with the data intact, so recovery is: bump `shm_size` (compose) to cover the parallel workers' shared memory needs, then re-run only the failed `CREATE INDEX` statements — no need to re-run the full restore.

## Credential masking bypass in error paths (feedback)

A base64-generated password containing `/` broke `DATABASE_URL` parsing on first boot. The resulting parser error message printed 28 of the 32 password characters in plaintext to logs — the application's normal credential-masking logic only covers the intended output paths, not ad-hoc error formatting inside a URL parser. **How to apply**: when generating credentials for connection strings, either restrict the character set to avoid URL-reserved characters (`/`, `@`, `:`, `%`) — this project resolved it with a 40-char alphanumeric password — or explicitly audit error-handling code paths for credential leakage, since masking applied at the "normal" logging layer does not automatically cover exception/error formatting.

## How to apply

- Before any dump-based cross-arch/cross-host Postgres migration: pin extension versions in the target image, verify with an explicit version-equality check (not just "extension present").
- Size `/dev/shm` generously (multi-GB) whenever restoring with elevated `maintenance_work_mem` and index-heavy schemas (HNSW/pgvector especially).
- Prefer credential character sets without URL-reserved characters for any secret that will be embedded in a connection-string env var; don't rely solely on masking layers to catch leaks in error paths.
- Use `pg_stat_user_tables` (not just row counts at a point in time) to distinguish "no activity yet" from "broken" when verifying a freshly cut-over database — see [[feedback_prove_worker_liveness_before_regression_verdict]] (lang-golang-expert memory) for the general pattern.
