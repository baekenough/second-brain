# PostgreSQL Migration (macmini → ubuntu1) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.
>
> **This is an infrastructure/ops migration, not a code-change plan.** "Test" steps below are replaced with verification commands (SQL aggregates, `pg_restore -l`, container health checks). The bite-sized/exact-command/no-placeholder discipline from `writing-plans` still applies.
>
> **This document is planning-only.** No task in this plan has been executed. Do not run any command in this file against `macmini` or `ubuntu1` without a fresh go/no-go review at the time of execution — the timing/throughput numbers below are estimates pending Task 4's dry-run measurement.

**Goal:** Move the second-brain PostgreSQL database (pgvector + pg_bigm, 12 GB, ~46K active documents) from the macmini Docker Desktop host (aarch64) to ubuntu1 (x86_64), reachable only over Tailscale, with a single bounded downtime window, explicit verification gates, and a documented rollback path.

**Architecture:** Because the source is aarch64 and the target is x86_64, the PGDATA directory cannot be copied — only a logical `pg_dump`/`pg_restore` migration is possible (Task 0 below documents why). All writers on macmini (`collector`, `server`, `mcp`, `eval-runner`, `web`) are stopped before the dump starts, so the dump is a byte-for-byte-consistent snapshot of the whole database at cutover time — no delta/incremental catch-up step is needed. A custom `second-brain-postgres` image (pgvector 0.8.2 + pg_bigm, version-pinned) is built **natively on ubuntu1** (no cross-arch QEMU build) and bound only to ubuntu1's Tailscale interface (`100.68.237.99:5432`), mirroring the existing `deploy/whisper-lb/ubuntu1` Tailscale-only port-binding pattern. Cutover is a single-line change: `DATABASE_URL` in macmini's `.env.local` is repointed at `100.68.237.99`, and macmini's local `postgres` container is left running (idle, frozen at dump time) so the existing `depends_on: postgres: condition: service_healthy` blocks in `docker-compose.local.yml` keep working without any compose-file edits — this doubles as the rollback target.

**Tech Stack:** PostgreSQL 16.14 (`pgvector/pgvector:pg16` base image), pgvector 0.8.2, pg_bigm (version to be pinned in Task 2), `pg_dump`/`pg_restore` directory format (`-Fd`), Docker Compose, Tailscale.

---

## Global Constraints

- **No physical volume copy.** `pgdata` is a Docker named volume built for `linux/arm64`; PostgreSQL's on-disk page format is architecture-portable *only* across the same major version and endianness within the same build toolchain assumptions Postgres itself doesn't guarantee across CPU architectures for a raw `PGDATA` copy — the officially supported cross-arch path is dump/restore. `pg_dump -Fc`/`-Fd` output is architecture-neutral (it's SQL/COPY data + a TOC, not page images), so this is the only valid path (spec item 1).
- **Single-writer assumption.** Only macmini's `server` and `collector` containers write to this database (confirmed: `DATABASE_URL` is the only DB entry point, no other host touches this Postgres). Stopping those two containers is sufficient to freeze all writes; `mcp`, `eval-runner`, and `web` are downstream/read-mostly but are stopped too during the window for a clean, unambiguous downtime boundary and because `eval-runner` can trigger a reindex webhook mid-migration if left running.
- **No `--serializable-deferrable`.** This flag exists to let `pg_dump` wait for a safe point when *concurrent* serializable write transactions are in flight. Because Task 8 stops every writer *before* the dump starts, there are no concurrent write transactions to wait for — the flag would be a no-op here. Documented per spec item 5 as a considered-and-rejected option, not an oversight.
- **No live-dump + incremental-catchup considered and rejected.** An alternative design (dump while writers stay up, then a short final delta-sync before cutover) was considered to shrink the downtime window. Rejected because Postgres has no built-in delta `pg_dump`; a correct delta would require logical replication (`pg_create_logical_replication_slot` + `pg_recvlogical`/`pglogical`) or a hand-rolled `WHERE updated_at > snapshot_ts` sync, both of which add real engineering and edge-case risk (duplicate keys, soft-delete races, partial chunk sets) for a single-user personal system where a bounded 1–3 hour maintenance window is acceptable. Simpler and safer: stop-then-dump-once.
- **Personal data never printed.** Every verification query in this plan returns only counts, UUIDs, byte sizes, or extension/index metadata — never `title`, `content`, `metadata`, or embedding vectors. This mirrors the diagnostic method already validated in `.claude/agent-memory/lang-golang-expert/project_search_rrf_relevance.md` (aggregated `COUNT(*)` over SSH, content never read).
- **No production writes during planning.** This plan document was written without running any command against macmini's or ubuntu1's live Postgres. All commands below are for the *execution* phase, later, with explicit human go/no-go at each downtime gate.
- **ubuntu1 existing containers are off-limits.** `morganb-server-{file-api,postgres,nginx,cloudflared}` are never stopped, restarted, or reconfigured by any task in this plan. The new `second-brain-postgres` stack is fully independent (new compose project, new named volume, new port `5432` bound only to the Tailscale interface — host port 5432 is confirmed unused).

---

## File Structure

| File | Action | Responsibility |
|------|--------|-----------------|
| `deploy/postgres-ubuntu1/docker-compose.yml` | Create | ubuntu1-side Postgres stack: prebuilt image reference, Tailscale-only port bind, steady-state memory tuning, named volume |
| `deploy/postgres-ubuntu1/README.md` | Create | Deploy/rollback runbook for this stack, mirrors `deploy/whisper-lb/README.md` structure |
| `deploy/postgres/Dockerfile` | Modify | Pin `ARG PG_BIGM_REF` to the exact tag found in Task 2 (currently `master`) |
| `.env.local` (macmini, not committed) | Modify | `DATABASE_URL` repointed to `100.68.237.99` at cutover (Task 14); reverted at rollback |
| `/root/.env` or `~/second-brain-postgres/.env` (ubuntu1, not committed) | Create | `NODE1_TAILSCALE_IP`, `PG_SUPERUSER_PASSWORD` (freshly generated, not `brain`/`brain`) |

No file in this list has been created or modified by this planning session — all are deferred to execution.

---

## Downtime Estimate (to be refined by Task 4's dry-run)

| Phase | Estimate | Basis |
|-------|----------|-------|
| Dump (Task 9) | 8–20 min | 12 GB, `pg_dump -Fd -j 4`, Docker Desktop VM disk I/O on macmini (virtualized, slower than bare metal) |
| Transfer (Task 11) | 10–40 min | Directory-format dump compresses text/tsvector well but embedding `vector` columns are binary float arrays (low compressibility) → assume ~7–9 GB on the wire; Tailscale WireGuard throughput assumed 5–15 MB/s (varies with direct-peer vs DERP-relay path — checked in Task 2) |
| Restore (Task 12) | 30–90 min | Dominated by rebuilding 3 HNSW indexes (`idx_documents_embedding`, `idx_documents_summary_embedding`, `idx_chunks_embedding`) + 3 `pg_bigm` GIN indexes over an estimated ~1.0–1.5M `chunks` rows (10 GB / ~7–8 KB avg row incl. 1536-dim vector) and a smaller `documents` row count; mitigated with `pg_restore -j 8`, `maintenance_work_mem=4GB`, `max_parallel_maintenance_workers=6` (ubuntu1 has 12 cores / 28 GB free) |
| Verify (Task 13) | 10 min | Aggregate SQL only, no full-table scans of content |
| Cutover + restart + smoke test (Tasks 14–15) | 10 min | `.env.local` edit + `docker compose up -d` for 5 containers + health checks |
| **Total** | **~70–170 min (midpoint ≈ 2 hours)** | Re-derive precisely from Task 4 before scheduling the real window |

**Why a 1–3 hour downtime window is acceptable here:** this is a single-user personal system, not a multi-tenant SaaS. Every pull-based collector (Gmail, Calendar, SMS via the OneDrive bridge, `llm-memory` sqlite) re-scans source state incrementally on its next cycle — a paused collector loses zero data, it just catches up late. The one push-based client, the Android `second-brain-push` app, already has proven retry-on-failure behavior: the 2026-06-21 session (`project_phone_reinstall_ingest_fix.md`) diagnosed the app retrying indefinitely against a slow/erroring server without losing any of 224 records. The same retry path means a 1–3 hour server outage during this migration is safe for the phone client — it will simply retry until `server` comes back up post-cutover.

**Recommended execution window:** late night KST, to minimize the number of missed OneDrive-bridge cycles (600s interval) that need to catch up afterward, and to keep the phone app's retry backlog small.

---

## Task 0: Confirm the architecture-mismatch constraint (no downtime, read-only)

**Objective:** Verify, with actual command output, that the source PGDATA cannot be volume-copied — establishing why dump/restore is the only valid path (spec item 1).

- [ ] **Step 1: Confirm macmini Postgres architecture**

```bash
ssh macmini "docker exec second-brain-local-postgres-1 uname -m"
```

Expected: `aarch64`

- [ ] **Step 2: Confirm ubuntu1 host architecture**

```bash
ssh ubuntu1 "uname -m"
```

Expected: `x86_64`

- [ ] **Step 3: Confirm the base image is arch-specific, not multi-arch-transparent at the data level**

```bash
ssh macmini "docker exec second-brain-local-postgres-1 pg_controldata /var/lib/postgresql/data | grep -E 'Catalog version|Database cluster state'"
```

Expected: a `Catalog version number` line — this number is tied to the compiled server binary's struct layout, which differs across architectures in ways `pg_upgrade --link`/raw copy do not support cross-arch. This is documentary evidence for the plan header, not a blocking check — if this step fails (container name differs), just fix the container name found via `docker ps` and re-run; it does not change the conclusion.

**Verification:** Both architecture strings differ (`aarch64` vs `x86_64`) — confirms dump/restore is required.

**Failure/rollback:** N/A — read-only confirmation step, nothing to roll back.

---

## Task 1: Capture baseline metrics on macmini (no downtime, read-only, aggregates only)

**Objective:** Record the exact "source of truth" counts that Task 13 will diff against. Every query below returns only counts/metadata, never content.

**Files:**
- Create (on macmini, scratch, not committed): `~/pg-migration-baseline.txt`

- [ ] **Step 1: Row counts per table**

```bash
ssh macmini "docker exec second-brain-local-postgres-1 psql -U brain -d second_brain -c \"
SELECT 'documents' AS tbl, count(*) FROM documents
UNION ALL SELECT 'chunks', count(*) FROM chunks
UNION ALL SELECT 'entities', count(*) FROM entities
UNION ALL SELECT 'entity_relations', count(*) FROM entity_relations
UNION ALL SELECT 'ask_sessions', count(*) FROM ask_sessions;
\"" | tee -a ~/pg-migration-baseline.txt
```

Expected: 5 rows, `documents` in the tens of thousands, `chunks` well above that (chunked content).

- [ ] **Step 2: Document counts by `source_type` and `status`**

```bash
ssh macmini "docker exec second-brain-local-postgres-1 psql -U brain -d second_brain -c \"
SELECT source_type, status, count(*) FROM documents GROUP BY source_type, status ORDER BY 1, 2;
\"" | tee -a ~/pg-migration-baseline.txt
```

Expected: breakdown matching the known ~46,126 active documents (`status='active'`) across known source types.

- [ ] **Step 3: Embedding NULL counts (data completeness fingerprint)**

```bash
ssh macmini "docker exec second-brain-local-postgres-1 psql -U brain -d second_brain -c \"
SELECT
  (SELECT count(*) FROM documents WHERE embedding IS NULL) AS doc_embed_null,
  (SELECT count(*) FROM documents WHERE summary_embedding IS NULL) AS doc_summ_embed_null,
  (SELECT count(*) FROM chunks WHERE embedding IS NULL) AS chunk_embed_null;
\"" | tee -a ~/pg-migration-baseline.txt
```

Expected: low/zero counts (matches the earlier ground-truth check: "임베딩누락0" for documents; some `chunks`/`summary_embedding` NULLs may be legitimate for not-yet-processed rows — record whatever the real number is, don't assume zero).

- [ ] **Step 4: Extension versions**

```bash
ssh macmini "docker exec second-brain-local-postgres-1 psql -U brain -d second_brain -c \"
SELECT extname, extversion FROM pg_extension ORDER BY extname;
\"" | tee -a ~/pg-migration-baseline.txt
```

Expected: `plpgsql`, `pg_bigm 1.2`, `uuid-ossp`, `vector 0.8.2` (4 rows) — confirms the facts given in the task brief.

- [ ] **Step 5: Index inventory**

```bash
ssh macmini "docker exec second-brain-local-postgres-1 psql -U brain -d second_brain -c \"
SELECT tablename, indexname FROM pg_indexes WHERE schemaname='public' ORDER BY tablename, indexname;
\"" | tee -a ~/pg-migration-baseline.txt
```

Expected: includes `idx_documents_embedding`, `idx_documents_summary_embedding`, `idx_chunks_embedding` (HNSW), `idx_documents_content_bigm`, `idx_documents_title_bigm`, `idx_chunks_content_bigm` (GIN bigm), plus standard btree PKs/FKs.

- [ ] **Step 6: Migration version**

```bash
ssh macmini "docker exec second-brain-local-postgres-1 psql -U brain -d second_brain -c \"
SELECT * FROM schema_migrations ORDER BY version DESC LIMIT 1;
\" 2>/dev/null || echo 'no schema_migrations table — check actual migration tracking mechanism used by cmd/server'" | tee -a ~/pg-migration-baseline.txt
```

Expected: version `24` or equivalent (matches "마이그레이션 024까지 적용"). If the table name differs, find the real one first with `\dt` — do not guess.

- [ ] **Step 7: Save a copy locally for later diffing**

```bash
scp macmini:~/pg-migration-baseline.txt /Users/sangyi/workspace/projects/second-brain/docs/superpowers/plans/.baseline-2026-08-18.txt
```

(This scratch file is not committed — add it to `.gitignore` locally if it ever risks being staged, or just delete it after Task 13 confirms parity.)

**Verification:** `~/pg-migration-baseline.txt` has 6 non-empty result blocks, no `ERROR` lines.

**Failure/rollback:** N/A — read-only. If any query errors (e.g., table/column renamed since these facts were gathered), stop and re-derive the correct query from `migrations/*.sql` before proceeding — do not guess at schema.

---

## Task 2: Verify container→Tailscale-IP network path (no downtime)

**Objective:** Confirm that a container running inside macmini's Docker Desktop VM can actually reach `100.68.237.99:5432` before any migration work depends on it. This is the single riskiest unknown in this plan (see Report §5).

- [ ] **Step 1: Confirm macmini host itself is tailnet-connected (already known true — whisper-lb's node3 role confirms this)**

```bash
ssh macmini "tailscale status | grep -i ubuntu1"
```

Expected: a line showing `ubuntu1` / `100.68.237.99` as `active` or `idle` (not `offline`).

- [ ] **Step 2: Probe raw TCP reachability from *inside* a throwaway container on macmini's Docker Desktop VM**

```bash
ssh macmini "docker run --rm alpine sh -c 'apk add --no-cache netcat-openbsd >/dev/null 2>&1; nc -zv -w5 100.68.237.99 22'"
```

(Using port 22, which is already open on ubuntu1 for SSH, as a reachability probe — port 5432 doesn't exist yet until Task 6.)

Expected: `100.68.237.99 (100.68.237.99:22) open`.

- [ ] **Step 3: Measure raw throughput over Tailscale (informs the Task 11 transfer estimate)**

```bash
ssh macmini "dd if=/dev/urandom of=/tmp/throughput-test.bin bs=1M count=200"
ssh macmini "time scp /tmp/throughput-test.bin ubuntu1:/tmp/throughput-test.bin"
ssh macmini "rm /tmp/throughput-test.bin"
ssh ubuntu1 "rm -f /tmp/throughput-test.bin"
```

Expected: 200 MB transfer completes; note the `real` time to compute MB/s, and update the Downtime Estimate table's Transfer row with the real number before scheduling the actual window.

**Verification:** Step 2 shows `open`; Step 3 gives a measured MB/s figure.

**Failure/rollback:** If Step 2 fails (`nc` cannot reach `100.68.237.99:22` from inside the container), **stop the entire plan here** — this is a hard blocker, not a task-level failure. Fallback options to investigate before continuing (not designed in this plan; would need a follow-up plan): (a) bind Postgres to macmini's own Tailscale IP too and use a `socat`/`stunnel` relay, (b) run Postgres on macmini's host network namespace instead of Docker Desktop's NAT'd network, (c) reconsider whether ubuntu1 is the right target at all. Do not proceed to Task 3+ until this is resolved.

---

## Task 3: Confirm and pin the `pg_bigm` version (no downtime)

**Objective:** `deploy/postgres/Dockerfile` currently clones `PG_BIGM_REF=master` — rebuilding today could silently pull a different `pg_bigm` version than what's installed on macmini (spec item 2). Pin it.

- [ ] **Step 1: Get the exact installed version on macmini**

```bash
ssh macmini "docker exec second-brain-local-postgres-1 psql -U brain -d second_brain -c \"SELECT extversion FROM pg_extension WHERE extname='pg_bigm';\""
```

Expected: `1.2` (matches the given fact; if it differs, use the real output, not the assumption).

- [ ] **Step 2: Find the matching upstream git tag**

```bash
git ls-remote --tags https://github.com/pgbigm/pg_bigm.git | grep -E "1\.2($|-)"
```

Expected: a tag like `refs/tags/1.2` or `refs/tags/v1.2`. `pg_bigm` upstream tag naming has varied across releases — use the exact ref string found, don't assume the format.

- [ ] **Step 3: Update the Dockerfile default (still a plan-time note, not an execution — but documented here so Task 5 has the exact command)**

```bash
# In deploy/postgres/Dockerfile, change:
#   ARG PG_BIGM_REF=master
# to:
#   ARG PG_BIGM_REF=<exact tag from Step 2>
```

At build time (Task 5), pass it explicitly regardless, so the Dockerfile default is defense-in-depth, not the only source of truth:

```bash
docker build --build-arg PG_BIGM_REF=<exact-tag> -t second-brain-postgres:ubuntu1 deploy/postgres/
```

- [ ] **Step 4: Sanity-check the tag actually builds pg_bigm 1.2 (dry build, can run on any Linux host, does not touch macmini/ubuntu1 data)**

```bash
docker build --build-arg PG_BIGM_REF=<exact-tag> -t second-brain-postgres:pin-check deploy/postgres/ \
  && docker run --rm second-brain-postgres:pin-check sh -c "cat /usr/share/postgresql/16/extension/pg_bigm.control | grep default_version"
```

Expected: `default_version = '1.2'`.

**Verification:** Step 4's `default_version` matches Step 1's `extversion` exactly.

**Failure/rollback:** If no tag matches `1.2` exactly (e.g., upstream only tagged `1.2-1` or similar packaging suffix), use the closest tag and re-run Step 4 to confirm the `.control` file's `default_version` — that's the value that actually matters for `CREATE EXTENSION pg_bigm VERSION '1.2'` compatibility during restore, not the git tag name itself. If restoring later fails on a `pg_bigm` version mismatch, the dump's `CREATE EXTENSION` statement pins the version explicitly — restore will error clearly rather than silently installing a different behavior.

---

## Task 4: Dry-run timing dump on macmini (no downtime — read-only, safe to run against live prod)

**Objective:** `pg_dump` takes an MVCC snapshot and does not block or get blocked by concurrent writers — running it now, before any downtime, is safe and gives real numbers to replace the estimates in the Downtime table.

- [ ] **Step 1: Check available disk space on macmini before dumping (12 GB DB → dump could be 5-12GB depending on compression)**

```bash
ssh macmini "df -h / 2>/dev/null; docker system df"
```

Expected: comfortably more than 15 GB free.

- [ ] **Step 2: Run the dry-run dump with the same flags Task 9 will use, timed**

```bash
ssh macmini "time docker exec second-brain-local-postgres-1 pg_dump -U brain -d second_brain -Fd -j 4 -f /tmp/pg-dryrun-dump"
```

Expected: completes without error; note the `real` time — this becomes the refined Dump estimate.

- [ ] **Step 3: Measure the dump's on-disk size (informs the Transfer estimate)**

```bash
ssh macmini "docker exec second-brain-local-postgres-1 du -sh /tmp/pg-dryrun-dump"
```

- [ ] **Step 4: Sanity-check the dump's table of contents**

```bash
ssh macmini "docker exec second-brain-local-postgres-1 pg_restore -l /tmp/pg-dryrun-dump | grep -c 'TABLE DATA'"
```

Expected: matches the number of user tables (documents, chunks, entities, entity_relations, ask_sessions, and any migration-tracking table — cross-check against Task 1 Step 6's schema inspection).

- [ ] **Step 5: Clean up the dry-run dump (it's already stale the moment writers resume — Task 9 takes the real one later)**

```bash
ssh macmini "docker exec second-brain-local-postgres-1 rm -rf /tmp/pg-dryrun-dump"
```

- [ ] **Step 6: Update the Downtime Estimate table in this document** with the real dump time from Step 2 and the real size from Step 3, and recompute the Transfer row using Task 2 Step 3's measured throughput.

**Verification:** Step 4's `TABLE DATA` count matches the known table count; Step 2 completed with exit code 0.

**Failure/rollback:** If `pg_dump` errors (e.g., disk full, permission denied), fix the underlying cause (free disk / grant `brain` the needed role) and re-run — this is a dry run, no risk to production data either way.

---

## Task 5: Build the postgres image natively on ubuntu1 (no downtime)

**Objective:** Build for `linux/amd64` on real amd64 hardware — avoids slow/flaky QEMU cross-arch emulation.

**Files:**
- Create (on ubuntu1, scratch): `~/second-brain-postgres/deploy-context/Dockerfile`

- [ ] **Step 1: Copy the build context to ubuntu1**

```bash
scp /Users/sangyi/workspace/projects/second-brain/deploy/postgres/Dockerfile ubuntu1:~/second-brain-postgres/deploy-context/Dockerfile
```

- [ ] **Step 2: Build with the pinned `pg_bigm` ref from Task 3**

```bash
ssh ubuntu1 "docker build --build-arg PG_BIGM_REF=<exact-tag-from-task-3> -t second-brain-postgres:ubuntu1 ~/second-brain-postgres/deploy-context/"
```

Expected: build completes; final layer is `pgvector/pgvector:pg16` + compiled `pg_bigm.so`.

- [ ] **Step 3: Confirm image architecture**

```bash
ssh ubuntu1 "docker image inspect second-brain-postgres:ubuntu1 --format '{{.Architecture}}'"
```

Expected: `amd64`.

- [ ] **Step 4: Confirm extension files are present and versions match Task 3**

```bash
ssh ubuntu1 "docker run --rm second-brain-postgres:ubuntu1 sh -c \"psql --version; cat /usr/share/postgresql/16/extension/pg_bigm.control | grep default_version; cat /usr/share/postgresql/16/extension/vector.control | grep default_version\""
```

Expected: `PostgreSQL 16.x`, `pg_bigm default_version = '1.2'`, `vector default_version = '0.8.2'`.

**Verification:** Step 3 and Step 4 both pass.

**Failure/rollback:** If the build fails on `apt-get install` (network flake) or `make` (pg_bigm build error against a newer/older postgres-server-dev-16 package on ubuntu1's Debian mirror than what macmini's build used), retry once; if it persists, compare macmini's actual `postgresql-server-dev-16` package version (`ssh macmini "docker run --rm second-brain-postgres:local dpkg -l | grep postgresql-server-dev"`) against ubuntu1's resolved version and pin explicitly if they differ. No production impact — this is a local image build on ubuntu1 only.

---

## Task 6: Provision the ubuntu1 compose stack — empty DB, Tailscale-only bind (no downtime)

**Objective:** Stand up a fresh, empty Postgres on ubuntu1, bound only to the Tailscale interface, with steady-state memory tuning, before any real data moves.

**Files:**
- Create: `deploy/postgres-ubuntu1/docker-compose.yml`
- Create (ubuntu1, not committed): `~/second-brain-postgres/.env`

- [ ] **Step 1: Generate a strong password (not the weak `brain`/`brain` default — the DB now listens on a network interface, even if Tailscale-private, so this is a reasonable hardening step given the exposure surface changed)**

```bash
ssh ubuntu1 "openssl rand -base64 24"
```

Save the output directly into `~/second-brain-postgres/.env` on ubuntu1 (never echo it into a shared terminal log or this plan document).

- [ ] **Step 2: Write `~/second-brain-postgres/.env` on ubuntu1**

```bash
ssh ubuntu1 "cat > ~/second-brain-postgres/.env <<'EOF'
NODE1_TAILSCALE_IP=100.68.237.99
PG_SUPERUSER_PASSWORD=<value-from-step-1>
EOF
chmod 600 ~/second-brain-postgres/.env"
```

(Variable name `NODE1_TAILSCALE_IP` deliberately reused from `deploy/whisper-lb/ubuntu1/docker-compose.yml` for naming consistency across ubuntu1 deployments.)

- [ ] **Step 3: Create `deploy/postgres-ubuntu1/docker-compose.yml`**

```yaml
# ubuntu1 — second-brain PostgreSQL (pgvector + pg_bigm)
# Tailscale IP: ${NODE1_TAILSCALE_IP} (see deploy/whisper-lb/README.md for the
# same pattern used by the whisper LB stack on this node)
#
# Deploy:
#   scp deploy/postgres-ubuntu1/docker-compose.yml ubuntu1:~/second-brain-postgres/docker-compose.yml
#   ssh ubuntu1 'cd ~/second-brain-postgres && docker compose --env-file .env up -d'
#
# Port binding is scoped to the Tailscale IP only — same principle as this
# project's web service (127.0.0.1-only) and the whisper-lb stack
# (Tailscale-IP-only): personal data must never be reachable on LAN/public.
#
# Image is pre-built natively on ubuntu1 (see docs/superpowers/plans/
# 2026-08-18-postgres-migration-to-ubuntu1.md Task 5) — no `build:` section
# here, this compose file only references the tag.

services:
  postgres:
    image: second-brain-postgres:ubuntu1
    container_name: second-brain-postgres
    environment:
      POSTGRES_DB: second_brain
      POSTGRES_USER: brain
      POSTGRES_PASSWORD: ${PG_SUPERUSER_PASSWORD}
    command:
      - "postgres"
      - "-c"
      - "shared_buffers=6GB"
      - "-c"
      - "effective_cache_size=18GB"
      - "-c"
      - "work_mem=64MB"
      - "-c"
      - "maintenance_work_mem=1GB"
      - "-c"
      - "max_worker_processes=16"
      - "-c"
      - "max_parallel_workers=12"
      - "-c"
      - "max_parallel_maintenance_workers=6"
      - "-c"
      - "random_page_cost=1.1"
    volumes:
      - pgdata_ubuntu1:/var/lib/postgresql/data
    ports:
      # Tailscale IP only — never LAN/public. Mirrors deploy/whisper-lb/ubuntu1.
      - "${NODE1_TAILSCALE_IP}:5432:5432"
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U brain -d second_brain"]
      interval: 5s
      timeout: 5s
      retries: 10
      start_period: 10s
    restart: unless-stopped

volumes:
  pgdata_ubuntu1:
```

Notes on the tuning values: ubuntu1 has 31 GB total / 28 GB free and hosts three unrelated `morganb-server-*` containers already — `shared_buffers=6GB` (~20% of total RAM, conservative vs. the usual 25% guideline to leave headroom for the existing containers) and `effective_cache_size=18GB` follow this project's `postgres-best-practices` skill guidance (`shared_buffers: 25% of RAM`, `effective_cache_size: 50-75% of RAM`) scaled down for a shared host. `maintenance_work_mem=1GB` is the steady-state value; Task 12 additionally overrides it to `4GB` for the one-time restore only, via `PGOPTIONS`, without touching this file.

- [ ] **Step 4: Deploy the empty stack**

```bash
scp /Users/sangyi/workspace/projects/second-brain/deploy/postgres-ubuntu1/docker-compose.yml ubuntu1:~/second-brain-postgres/docker-compose.yml
ssh ubuntu1 "cd ~/second-brain-postgres && docker compose --env-file .env up -d"
```

- [ ] **Step 5: Confirm health and port binding**

```bash
ssh ubuntu1 "docker compose -f ~/second-brain-postgres/docker-compose.yml ps"
ssh ubuntu1 "ss -tlnp | grep 5432"
```

Expected: `second-brain-postgres` shows `healthy`; `ss` shows the listener bound to `100.68.237.99:5432` only, **not** `0.0.0.0:5432`.

- [ ] **Step 6: Confirm existing `morganb-server-*` containers are untouched**

```bash
ssh ubuntu1 "docker ps --filter name=morganb-server --format '{{.Names}}: {{.Status}}'"
```

Expected: same `Up` durations as before this task started (i.e., they were never restarted).

**Verification:** Steps 5 and 6 both pass.

**Failure/rollback:** If port `5432` is somehow already bound (contradicts the given fact that it's free — re-check with `ss -tlnp` before this task, not just after), pick a different host port (e.g. `55432`) and update `DATABASE_URL` accordingly in Task 14. If the container fails to become healthy, check `docker compose logs postgres` — most likely cause is a bad `PG_SUPERUSER_PASSWORD` value in `.env` (unescaped special characters); regenerate with Step 1's command (which produces base64, always shell-safe) and retry `docker compose up -d`.

---

## Task 7: Pre-flight extension check on the empty ubuntu1 DB (no downtime)

**Objective:** Confirm `CREATE EXTENSION` works for all three extensions *before* the real restore depends on it, using the throwaway `postgres` maintenance database — never touching `second_brain`.

- [ ] **Step 1: Test extension creation in the `postgres` DB (not `second_brain`)**

```bash
ssh ubuntu1 "docker exec second-brain-postgres psql -U brain -d postgres -c \"
CREATE EXTENSION IF NOT EXISTS vector;
CREATE EXTENSION IF NOT EXISTS pg_bigm;
CREATE EXTENSION IF NOT EXISTS \\\"uuid-ossp\\\";
SELECT extname, extversion FROM pg_extension ORDER BY extname;
\""
```

Expected: 4 rows (`plpgsql`, `pg_bigm 1.2`, `uuid-ossp <version>`, `vector 0.8.2`) — versions must match Task 1 Step 4's baseline exactly.

- [ ] **Step 2: Clean up (drop from the throwaway DB — `second_brain` itself gets its extensions from the real dump's `CREATE EXTENSION` statements during restore, not from this test)**

```bash
ssh ubuntu1 "docker exec second-brain-postgres psql -U brain -d postgres -c \"
DROP EXTENSION IF EXISTS pg_bigm;
DROP EXTENSION IF EXISTS vector;
\""
```

**Verification:** Step 1's version numbers match Task 1's baseline exactly, for all three extensions.

**Failure/rollback:** If a version mismatches (e.g. `uuid-ossp` differs because ubuntu1's Debian/postgres apt mirror shipped a newer point release), this is informational, not fatal — `uuid-ossp` and `plpgsql` are maintained by the Postgres project itself and track the major version (16), not this project's Dockerfile; a minor version drift here is expected and safe. Only `vector` and `pg_bigm` versions are hard requirements (spec item 2) because this project's data/index format depends on them.

---

# ═══════════════ DOWNTIME WINDOW STARTS HERE (Tasks 8–15) ═══════════════

> Everything from Task 8 through Task 15 happens inside a single bounded maintenance window. Do not start Task 8 until Tasks 0–7 are all verified green and a human has explicitly approved the go/no-go based on the real numbers from Task 4.

## Task 8: Freeze writers on macmini

**Objective:** Stop every container that writes to Postgres, establishing the exact snapshot boundary.

- [ ] **Step 1: Record downtime start timestamp**

```bash
ssh macmini "date -u +%Y-%m-%dT%H:%M:%SZ" | tee -a ~/pg-migration-baseline.txt
```

- [ ] **Step 2: Stop writers and downstream consumers (leave `postgres` itself running — the dump needs it up)**

```bash
ssh macmini "cd ~/second-brain && docker compose -f docker-compose.local.yml --env-file .env.local stop collector server mcp eval-runner web"
```

- [ ] **Step 3: Confirm they're stopped**

```bash
ssh macmini "docker compose -f ~/second-brain/docker-compose.local.yml --env-file ~/second-brain/.env.local ps"
```

Expected: `collector`, `server`, `mcp`, `eval-runner`, `web` show `Exited`; `postgres` still shows `Up (healthy)`.

**Verification:** Step 3's output matches expectation exactly.

**Failure/rollback:** If a container refuses to stop cleanly (hung request), `docker compose stop -t 30 <service>` then `docker compose kill <service>` as a last resort — a mid-write kill is still safe here because Postgres's own transaction atomicity guarantees the in-flight write either fully committed or fully rolled back before the dump's snapshot is taken in Task 9.

---

## Task 9: Take the real consistent dump on macmini

**Objective:** Snapshot the full database with all writers stopped — this dump is authoritative.

- [ ] **Step 1: Run the dump**

```bash
ssh macmini "time docker exec second-brain-local-postgres-1 pg_dump -U brain -d second_brain -Fd -j 4 -f /tmp/pg-migration-dump"
```

Expected: completes with exit code 0; compare the real elapsed time against Task 4's dry-run estimate (should be similar — no concurrent load now, if anything slightly faster).

- [ ] **Step 2: Copy the dump out of the container to the macmini host filesystem**

```bash
ssh macmini "docker cp second-brain-local-postgres-1:/tmp/pg-migration-dump /tmp/pg-migration-dump"
```

**Verification:** Step 1 exit code 0; `du -sh /tmp/pg-migration-dump` on the macmini host is non-trivially sized (within ~20% of Task 4's dry-run size — if wildly different, something changed unexpectedly between the dry run and now, investigate before continuing).

**Failure/rollback:** If `pg_dump` fails partway (disk full, connection drop), **writers are still stopped and no data has been touched** — simply restart the local `postgres` container is not needed (it never stopped), fix the underlying issue, delete the partial `/tmp/pg-migration-dump` directory, and re-run Step 1. This failure mode does not require restarting writers — stay in the downtime window and retry.

---

## Task 10: Verify dump integrity before transferring

**Objective:** Catch a corrupt/incomplete dump *before* spending 10–40 minutes transferring it (spec item 7's "verify" principle applied early).

- [ ] **Step 1: List the dump's table of contents**

```bash
ssh macmini "pg_restore -l /tmp/pg-migration-dump | grep -c 'TABLE DATA'"
```

Expected: matches Task 4 Step 4's count exactly (same schema, same table set).

- [ ] **Step 2: Confirm no `pg_dump` warnings were logged**

```bash
ssh macmini "docker logs second-brain-local-postgres-1 --since 15m 2>&1 | grep -i -E 'error|warning' | grep -v 'FATAL:  role' || echo 'no relevant warnings'"
```

(The `role` filter excludes unrelated stale-connection noise from already-stopped containers reconnecting during shutdown — inspect any other match manually before proceeding.)

**Verification:** Step 1 matches Task 4's baseline table count; Step 2 shows no `pg_dump`-related errors.

**Failure/rollback:** If Step 1's count is lower than Task 4's, the dump is missing tables — do not transfer. Re-run Task 9 Step 1. Writers remain stopped; no data has moved yet, so this costs only the dump's re-run time, not a full rollback.

---

## Task 11: Transfer the dump to ubuntu1

**Objective:** Move the dump over Tailscale using a resumable transfer method.

- [ ] **Step 1: Transfer with `rsync` (resumable if the link drops, unlike a plain `scp` of a whole directory)**

```bash
ssh macmini "rsync -avz --progress /tmp/pg-migration-dump/ ubuntu1:~/pg-migration-dump/"
```

Expected: completes; compare elapsed time against the Downtime Estimate table's Transfer row (refined by Task 2 Step 3).

- [ ] **Step 2: Verify file count and total size match on both ends**

```bash
ssh macmini "find /tmp/pg-migration-dump -type f | wc -l; du -sh /tmp/pg-migration-dump"
ssh ubuntu1 "find ~/pg-migration-dump -type f | wc -l; du -sh ~/pg-migration-dump"
```

Expected: identical file counts on both sides; sizes within a few KB (filesystem block-size rounding only).

**Verification:** Step 2's file counts match exactly.

**Failure/rollback:** If `rsync` drops mid-transfer, re-run the same command — `rsync` resumes/skips already-transferred files by default, no need to restart from zero. If file counts still don't match after a second full pass, do not proceed to restore — investigate (likely a `.gitignore`-style exclusion or a `rsync` flag issue) before Task 12.

---

## Task 12: Restore on ubuntu1

**Objective:** Load the dump into the empty `second_brain` DB provisioned in Task 6, with restore-time-only performance tuning.

- [ ] **Step 1: Confirm `second_brain` DB is still empty (sanity check before a destructive-adjacent operation)**

```bash
ssh ubuntu1 "docker exec second-brain-postgres psql -U brain -d second_brain -c \"SELECT count(*) FROM pg_tables WHERE schemaname='public';\""
```

Expected: `0`.

- [ ] **Step 2: Restore with elevated restore-time-only `maintenance_work_mem` via `PGOPTIONS` (does not touch the steady-state `docker-compose.yml` config from Task 6)**

The dump directory must be visible *inside* the container — `docker cp` it in first, then reference the in-container path:

```bash
ssh ubuntu1 "docker cp ~/pg-migration-dump second-brain-postgres:/tmp/pg-migration-dump"
ssh ubuntu1 "time docker exec -e PGOPTIONS='-c maintenance_work_mem=4GB -c max_parallel_maintenance_workers=6' second-brain-postgres pg_restore -U brain -d second_brain -j 8 --no-owner --no-acl /tmp/pg-migration-dump 2>&1 | tee ~/pg-restore.log"
```

Expected: completes; `pg_restore` may print benign warnings about role ownership (suppressed by `--no-owner --no-acl`, which is correct here since ubuntu1's `brain` role is freshly created and doesn't need to match macmini's role OIDs).

- [ ] **Step 3: Check the restore log for real errors**

```bash
ssh ubuntu1 "grep -i error ~/pg-restore.log || echo 'no errors'"
```

Expected: `no errors` (ignore `NOTICE`-level lines from the migration files' own `RAISE NOTICE` statements, e.g. `011: embedding column already vector(1536)`).

- [ ] **Step 4: `ANALYZE` the restored database (query planner statistics are not part of a `pg_dump`/`pg_restore` cycle)**

```bash
ssh ubuntu1 "time docker exec second-brain-postgres psql -U brain -d second_brain -c 'ANALYZE;'"
```

**Verification:** Step 3 shows no errors; Step 4 completes.

**Failure/rollback:** **Nothing has been written to macmini's local `postgres` volume this entire task** — it's still sitting untouched, frozen at the Task 9 snapshot. If restore fails (e.g., a `pg_bigm` version mismatch error on `CREATE EXTENSION pg_bigm VERSION '1.2'` because Task 3's pin was wrong), fix the image (rebuild per Task 5 with a corrected `PG_BIGM_REF`), `docker exec second-brain-postgres psql -U brain -d postgres -c "DROP DATABASE second_brain; CREATE DATABASE second_brain OWNER brain;"` to reset to empty, and re-run Task 12 from Step 1. Writers on macmini remain stopped throughout retries — this does not extend risk, only extends the downtime window, which is acceptable per the Downtime Estimate section's reasoning.

---

## Task 13: Post-restore verification (aggregates only, no personal data)

**Objective:** Diff ubuntu1's restored state against Task 1's macmini baseline. This is the actual go/no-go gate for cutover.

- [ ] **Step 1: Row counts — must match Task 1 Step 1 exactly**

```bash
ssh ubuntu1 "docker exec second-brain-postgres psql -U brain -d second_brain -c \"
SELECT 'documents' AS tbl, count(*) FROM documents
UNION ALL SELECT 'chunks', count(*) FROM chunks
UNION ALL SELECT 'entities', count(*) FROM entities
UNION ALL SELECT 'entity_relations', count(*) FROM entity_relations
UNION ALL SELECT 'ask_sessions', count(*) FROM ask_sessions;
\""
```

**Pass condition:** every row's count is byte-identical to Task 1 Step 1 — writers were stopped before the dump, so there is zero tolerance for drift here (unlike a live-dump design, where some drift would be expected and acceptable).

- [ ] **Step 2: Embedding NULL counts — must match Task 1 Step 3 exactly**

```bash
ssh ubuntu1 "docker exec second-brain-postgres psql -U brain -d second_brain -c \"
SELECT
  (SELECT count(*) FROM documents WHERE embedding IS NULL) AS doc_embed_null,
  (SELECT count(*) FROM documents WHERE summary_embedding IS NULL) AS doc_summ_embed_null,
  (SELECT count(*) FROM chunks WHERE embedding IS NULL) AS chunk_embed_null;
\""
```

**Pass condition:** exact match with Task 1 Step 3.

- [ ] **Step 3: Extension versions — must match Task 1 Step 4 exactly**

```bash
ssh ubuntu1 "docker exec second-brain-postgres psql -U brain -d second_brain -c \"SELECT extname, extversion FROM pg_extension ORDER BY extname;\""
```

**Pass condition:** exact match.

- [ ] **Step 4: Index inventory — must match Task 1 Step 5 exactly**

```bash
ssh ubuntu1 "docker exec second-brain-postgres psql -U brain -d second_brain -c \"SELECT tablename, indexname FROM pg_indexes WHERE schemaname='public' ORDER BY tablename, indexname;\""
```

**Pass condition:** identical index name set (order may differ trivially, compare as sets).

- [ ] **Step 5: Privacy-safe ANN sanity check — self-referential nearest-neighbor query, IDs only, no content**

```bash
ssh macmini "docker exec second-brain-local-postgres-1 psql -U brain -d second_brain -t -c \"
SELECT id FROM chunks
ORDER BY embedding <=> (SELECT embedding FROM chunks WHERE embedding IS NOT NULL ORDER BY id LIMIT 1)
LIMIT 10;
\"" > /tmp/ann-macmini-ids.txt

ssh ubuntu1 "docker exec second-brain-postgres psql -U brain -d second_brain -t -c \"
SELECT id FROM chunks
ORDER BY embedding <=> (SELECT embedding FROM chunks WHERE embedding IS NOT NULL ORDER BY id LIMIT 1)
LIMIT 10;
\"" > /tmp/ann-ubuntu1-ids.txt

diff <(sort /tmp/ann-macmini-ids.txt) <(sort /tmp/ann-ubuntu1-ids.txt)
rm /tmp/ann-macmini-ids.txt /tmp/ann-ubuntu1-ids.txt
```

**Pass condition:** the two ID sets overlap heavily (≥8/10). They are not required to be byte-identical — HNSW is an approximate index, and a from-scratch index build on ubuntu1 can legitimately traverse the graph slightly differently than macmini's index even over identical data. A large ID-set divergence (e.g., <5/10 overlap) would indicate a real data problem (wrong embedding column restored, dimension mismatch) and should block cutover; a small divergence (8-10/10) is expected ANN behavior, not a defect — this is the same `ef_search`-sensitivity documented in `project_search_rrf_relevance.md` (spec item 8), not new to this migration.

**Verification:** Steps 1–4 are exact-match pass/fail gates. Step 5 is a heuristic gate (≥8/10 overlap).

**Failure/rollback:** If Steps 1–4 fail (any exact mismatch), **do not proceed to Task 14.** Macmini's local `postgres` is still the live source of truth (writers are stopped but pointed nowhere new yet) — go back to Task 9 and re-dump, or Task 12 and re-restore, depending on where the discrepancy traces to. This is still within the downtime window; extending it is safe per the reasoning in the Downtime Estimate section. Do not cut traffic over on a failed verification gate under time pressure.

---

## Task 14: Cutover

**Objective:** Repoint macmini's writers at ubuntu1 and bring the stack back up. This is the moment the downtime window ends.

**Files:**
- Modify (macmini, not committed): `.env.local`

- [ ] **Step 1: Update `DATABASE_URL` in macmini's `.env.local`**

```bash
ssh macmini "cd ~/second-brain && sed -i.bak 's|^DATABASE_URL=.*|DATABASE_URL=postgres://brain:<PG_SUPERUSER_PASSWORD-from-task-6>@100.68.237.99:5432/second_brain?sslmode=disable|' .env.local"
```

(`.bak` suffix preserves the pre-cutover value for Task 16's rollback path — do not delete `.env.local.bak` until the soak period, Task 16, concludes.)

- [ ] **Step 2: Restart the writer/consumer stack — `depends_on: postgres: condition: service_healthy` still refers to macmini's *local* `postgres` container, which is untouched and still healthy, so this works without any `docker-compose.local.yml` edit**

```bash
ssh macmini "cd ~/second-brain && docker compose -f docker-compose.local.yml --env-file .env.local up -d collector server mcp eval-runner web"
```

- [ ] **Step 3: Confirm all 5 containers are healthy and connected to ubuntu1, not the local postgres**

```bash
ssh macmini "docker compose -f ~/second-brain/docker-compose.local.yml --env-file ~/second-brain/.env.local ps"
ssh macmini "docker logs second-brain-local-server-1 --since 2m 2>&1 | grep -i -E 'connect|migrat' | head -20"
```

Expected: all 5 `Up`; server logs show a successful connection/migration-check line referencing the new host (or at minimum, no connection-refused errors).

- [ ] **Step 4: Record downtime end timestamp and compute actual elapsed time**

```bash
ssh macmini "date -u +%Y-%m-%dT%H:%M:%SZ"
```

**Verification:** Step 3 passes; actual downtime (Task 8 Step 1 timestamp → this step's timestamp) is within the estimated range, or at least explainable if it isn't.

**Failure/rollback:** If `server`/`collector` fail to connect (network path broken despite Task 2's earlier check, or password typo in Step 1's `sed`), immediately revert:

```bash
ssh macmini "cd ~/second-brain && mv .env.local.bak .env.local && docker compose -f docker-compose.local.yml --env-file .env.local up -d collector server mcp eval-runner web"
```

This is the "immediate rollback window" — see Report §4 and the Rollback Procedure section below. Because no writes have landed on ubuntu1 yet at this point (cutover just happened), this revert is lossless.

---

## Task 15: Post-cutover smoke test

**Objective:** Confirm the system is actually functional against ubuntu1, not just "containers are up."

- [ ] **Step 1: Health endpoint check**

```bash
ssh macmini "curl -sf http://localhost:8081/healthz || curl -sf http://localhost:8081/health"
```

Expected: `200 OK` (check `internal/api/router.go` for the exact health path if this guess is wrong).

- [ ] **Step 2: A real read-path query, aggregate-only**

```bash
ssh macmini "docker exec second-brain-local-postgres-1 psql -U brain -d second_brain -c \"SELECT count(*) FROM documents;\" 2>&1 | head -5 || echo 'expected: local postgres still queryable but frozen — this just confirms it did NOT receive new writes'"
ssh ubuntu1 "docker exec second-brain-postgres psql -U brain -d second_brain -c \"SELECT count(*) FROM documents;\""
```

Run this again ~10 minutes later — ubuntu1's count should have started ticking up (from collector catch-up / retried phone-app writes) while macmini's local count stays frozen at the Task 9 snapshot value. That divergence is the actual proof cutover succeeded.

- [ ] **Step 3: Container log scan for the first 10 minutes post-cutover**

```bash
ssh macmini "docker compose -f ~/second-brain/docker-compose.local.yml --env-file ~/second-brain/.env.local logs --since 10m collector server mcp eval-runner web 2>&1 | grep -i -E 'error|panic|fatal' || echo 'clean'"
```

Expected: `clean`, or only benign/expected errors (e.g., a single retried phone-app request from during the downtime window, now succeeding).

**Verification:** Steps 1–3 all pass; Step 2's re-check 10 minutes later shows ubuntu1's count increasing.

**Failure/rollback:** Any hard failure here (500s, panics, connection storms) is grounds for the same immediate rollback as Task 14 — see the Rollback Procedure below. This is still inside the "point of no easy return" window (see Report §4) as long as it's caught within roughly the first hour.

# ═══════════════ DOWNTIME WINDOW ENDS (end of Task 15) ═══════════════

---

## Rollback Procedure

**Trigger conditions (spec item 6):**

| When | Condition | Action |
|------|-----------|--------|
| During Task 13 (pre-cutover) | Any exact-match verification fails (row counts, extension versions, index inventory, embedding NULL counts) | Do not cut over. Re-dump/re-restore. Downtime extends but no data risk — this is the cheapest failure mode. |
| During Task 14–15 (cutover / smoke test, "immediate rollback window", ≲ 1 hour post-cutover) | Connection failures, 500s, panics, or the Step 2 divergence check in Task 15 never shows ubuntu1's count increasing | **Lossless rollback**: flip `DATABASE_URL` back via the `.env.local.bak` restore shown in Task 14's failure section, restart the stack. No data was written to ubuntu1 yet, or so little that re-running the migration from scratch later is cheaper than reconciling it. |
| Soak period (Task 16, T+1h to T+72h post-cutover) | Sustained latency regression (API p95 roughly doubles — Tailscale WireGuard round-trip adds real per-query latency vs. the previous same-host Docker network hop), Tailscale link instability causing repeated collector ingest failures, or a data-integrity bug discovered in ubuntu1's copy | **Lossy rollback**: flipping `DATABASE_URL` back to macmini now loses every write that landed on ubuntu1 since cutover (new SMS/notes/insights/entities). This requires an explicit decision to accept that loss, or to instead do a **forward-fix**: dump ubuntu1's current state and restore *it* back onto macmini (a mirror-image of this entire plan, run in reverse) rather than reverting to the stale macmini snapshot. |
| Post T+72h | Soak period passed cleanly | Rollback is no longer "revert" — any further fix is forward-only (patch ubuntu1 in place, or a full reverse-migration). Proceed to Task 18 (decommission planning) instead. |

**The critical judgment call (spec item 6):** the "point of no easy return" is roughly **T+1 hour post-cutover**. Before that, ubuntu1 has accumulated at most a handful of retried writes (mostly the phone app's backlog from the downtime window itself) — reverting loses little and is the safe default on any ambiguous signal. After that, real new data accumulates on ubuntu1 that doesn't exist on macmini's frozen copy, and reverting starts trading one data-loss risk for another. This plan recommends treating Task 15 (smoke test) as the last cheap checkpoint to catch problems, and escalating anything found in the T+1h–T+72h soak window to a forward-fix rather than a revert, unless the discovered problem is severe enough that losing the interim ubuntu1-only writes is clearly the lesser harm (a judgment call for whoever is running the soak, not something this plan can pre-decide).

**macmini's local postgres is the rollback target and must not be touched until the soak period concludes:** do not `docker compose down -v` it, do not run `VACUUM FULL` or any write against it, and do not reuse its volume for anything else, for the full duration of Task 16's soak window.

---

## Task 16: Soak period monitoring (T+1h to T+72h post-cutover)

**Objective:** Decide, with evidence, whether to keep ubuntu1 as the source of truth or invoke the Rollback Procedure.

- [ ] **Step 1: At T+1h, re-run Task 15 Step 2's divergence check and Task 15 Step 3's log scan.** This is the primary "immediate rollback window" decision point — see Rollback Procedure table.

- [ ] **Step 2: Monitor collector cycle success over the next 24h**

```bash
ssh macmini "docker compose -f ~/second-brain/docker-compose.local.yml --env-file ~/second-brain/.env.local logs --since 24h collector 2>&1 | grep -i -E 'error|fail' | wc -l"
```

Compare against a pre-migration 24h baseline (run the same query against historical logs, or accept that "0 or a small number matching known-flaky sources" is the bar).

- [ ] **Step 3: Confirm macmini's local postgres remains untouched (frozen row count) as the rollback safety net**

```bash
ssh macmini "docker exec second-brain-local-postgres-1 psql -U brain -d second_brain -c \"SELECT count(*) FROM documents;\""
```

Expected: identical to the Task 9 snapshot value for the entire soak period — if this number ever changes, something is still writing to the local DB (a config that wasn't fully cut over), investigate immediately.

- [ ] **Step 4: At T+72h with no red flags, declare the migration complete** and proceed to Task 17 (optional) / Task 18 (deferred).

**Verification:** All of Steps 1–3 show no red flags across the full 72h window.

**Failure/rollback:** See the Rollback Procedure table above — the specific action depends on which day of the soak window the problem surfaces.

---

## Task 17 (Optional): Re-tune `hnsw.ef_search` now that indexes are freshly rebuilt

**Objective:** Not caused by this migration, but the HNSW indexes are being rebuilt from scratch as a side effect of the restore — a natural, low-cost opportunity to measure whether the known `ef_search` relevance bug (`project_search_rrf_relevance.md`: default `ef_search=40` misses 37/40 true top-40 rows under a `status='active'` filter) behaves any differently on a fresh index, and to gather real numbers for eventually setting `SET LOCAL hnsw.ef_search=N` in `internal/store/document.go`'s `hybridSearch` and `internal/store/chunks.go`'s `SearchVector`.

- [ ] **Step 1: Re-run the exact brute-force-vs-ANN probe described in `project_search_rrf_relevance.md`, against ubuntu1's fresh index**

```bash
ssh ubuntu1 "docker exec second-brain-postgres psql -U brain -d second_brain -c \"
SET enable_indexscan = off;
CREATE TEMP TABLE brute_force_top40 AS
SELECT id FROM documents WHERE status='active'
ORDER BY embedding <=> (SELECT embedding FROM documents WHERE status='active' AND embedding IS NOT NULL ORDER BY id LIMIT 1)
LIMIT 40;
RESET enable_indexscan;
SET hnsw.ef_search = 40;
CREATE TEMP TABLE ann_ef40_top40 AS
SELECT id FROM documents WHERE status='active'
ORDER BY embedding <=> (SELECT embedding FROM documents WHERE status='active' AND embedding IS NOT NULL ORDER BY id LIMIT 1)
LIMIT 40;
SELECT count(*) AS overlap_ef40 FROM brute_force_top40 b JOIN ann_ef40_top40 a USING (id);
\""
```

(Uses a self-referential probe vector, same privacy-safe pattern as Task 13 Step 5 — IDs and counts only.)

- [ ] **Step 2: Repeat with higher `ef_search` values (100, 200, 400) to find the recall/latency knee**

```bash
ssh ubuntu1 "docker exec second-brain-postgres psql -U brain -d second_brain -c \"
SET hnsw.ef_search = 200;
EXPLAIN (ANALYZE, BUFFERS) SELECT id FROM documents WHERE status='active'
ORDER BY embedding <=> (SELECT embedding FROM documents WHERE status='active' AND embedding IS NOT NULL ORDER BY id LIMIT 1)
LIMIT 40;
\""
```

Record the overlap-with-brute-force count (repeat Step 1's join at each `ef_search` value) alongside the `EXPLAIN ANALYZE` timing, to find where recall plateaus vs. where latency starts hurting.

- [ ] **Step 3: If a good `ef_search` value is found, file a follow-up code-change plan** (not part of this migration plan) to add `SET LOCAL hnsw.ef_search=N` to the two call sites named in `project_search_rrf_relevance.md`.

**Verification:** N/A — this task produces measurements, not a pass/fail gate.

**Failure/rollback:** N/A — read-only probes against the already-live ubuntu1 database, no risk.

---

## Task 18 (Deferred — separate follow-up plan, not part of this migration)

**Objective:** Decommission macmini's local `postgres` container and volume once the soak period (Task 16) concludes cleanly.

This is intentionally **not detailed in this plan** — it requires editing `docker-compose.local.yml` to remove the `postgres` service and every `depends_on: postgres: condition: service_healthy` block from `collector`, `server`, `mcp`, and `eval-runner` (Task 14 deliberately avoided this edit so cutover wouldn't require a compose-file change under time pressure). That's a code change to a tracked file and belongs in its own plan, reviewed on its own, once there's confidence ubuntu1 is stable — not bundled into the migration's downtime window.

---

## Self-Review

**Spec coverage check** (against the 8 numbered requirements in the task brief):
1. Architecture-mismatch rationale — Task 0 + plan header. ✓
2. `pg_bigm` version pinning — Task 3, enforced again at build time in Task 5. ✓
3. HNSW rebuild time — Downtime Estimate table + Task 12's `--jobs`/`maintenance_work_mem` tuning. ✓
4. Tailscale-only port binding — Task 6 compose file (`${NODE1_TAILSCALE_IP}:5432:5432`), verified in Task 6 Step 5. ✓
5. Downtime ordering decision (stop-then-dump, `--serializable-deferrable` rejected) — Global Constraints + Task 8/9. ✓
6. Rollback path + judgment criteria — Rollback Procedure section, Task 16. ✓
7. Post-migration verification, aggregates only — Task 13, Task 15 Step 2. ✓
8. `ef_search` re-tuning opportunity — Task 17 (optional). ✓

**Placeholder scan:** no `TBD`/`TODO`/"add appropriate handling" found — the two spots with `<value-from-step-1>`/`<exact-tag-from-task-3>` are intentional secret/discovered-value placeholders (the actual password and the actual pg_bigm tag are only known at execution time, not plan-writing time), not scope gaps.

**Type/name consistency check:** container names (`second-brain-local-postgres-1`, `second-brain-postgres`), compose file paths, and the `NODE1_TAILSCALE_IP` variable name are used consistently across all tasks that reference them.
