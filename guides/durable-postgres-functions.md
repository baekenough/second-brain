# Durable Functions in Postgres

Reference guide for the durable-step pattern that second-brain uses to make
background work (retry, scheduling, fan-out) survive process restarts using
PostgreSQL as the single source of truth. Internalized from scout issue #82
(`pg_durable`, https://news.hada.io/topic?id=30225).

---

## 1. The Pattern

A *durable function* checkpoints every step of a long-running or retryable
operation into a database so that a crash or restart resumes from the last
committed step instead of starting over. `pg_durable` packages this as a SQL
DSL — retry, scheduling, parallel fan-out, and conditional branching — with all
state living inside PostgreSQL. No external queue (Kafka, SQS) or worker
container is required.

This fits second-brain's architecture directly: the system already depends on
a single PostgreSQL instance (`pgvector` for embeddings, `pg_bigm` for Korean
2-gram search). Keeping durable orchestration in the same database avoids
adding an operational dependency.

## 2. What the Codebase Already Implements

second-brain already embodies the durable-retry half of this pattern in Go,
checkpointed in Postgres:

| Concern | Where | Mechanism |
|---------|-------|-----------|
| Retry queue | `extraction_failures` table (`migrations/003_extraction_failures.sql`) | `attempts`, `next_retry_at` (default `now() + 5 min`), `dead_letter` |
| Due-row polling | same table | partial index `idx_extraction_failures_next_retry ... WHERE NOT dead_letter` |
| Backoff + dead-letter | `internal/worker/extraction_retry.go` (`ExtractionRetryWorker`) | polls `DueForRetry`, re-runs extraction, exponential backoff, dead-letters at 10 attempts |
| Per-instance watermark | `collector_state` (`migrations/009`) | resumable collection checkpoint per `(instance_id, source_type)` |
| Reindex checkpoint | `reindex_state` (`migrations/008`) | resumable reindex progress |
| Scheduling | `internal/scheduler/scheduler.go` | `robfig/cron` (in-process, not yet durable) |

The key takeaway: durable retry is **not a missing capability**. It is
implemented ad hoc, one bespoke table per worker.

## 3. The Consolidation Opportunity

`pg_durable` is therefore best framed as a *consolidation* candidate, not a new
feature. A generic durable-step DSL could replace the per-worker retry tables
with one mechanism. Candidate migration targets, in rough order of payoff:

1. **Collector retry** — move the `extraction_failures` bespoke logic onto the
   generic DSL.
2. **Embedding-batch fan-out** — parallel embed of unembedded documents with
   per-item checkpointing.
3. **Scheduling** — replace part of the in-process `robfig/cron` schedules,
   which today are lost on restart, with durable scheduled steps.

## 4. Risk and Recommended Approach

**Risk**: `pg_durable` is a PostgreSQL **extension**. Installing extensions may
be impossible on managed PG offerings (RDS/CloudSQL allow only an approved
extension list), and the project is early-stage.

**Recommended next step — a verification spike, not wholesale adoption:**

1. Install `pg_durable` in the local `docker-compose` stack only.
2. Migrate a **single** collector retry path (e.g., the `extraction_failures`
   loop) onto the DSL as a proof of concept.
3. Compare operability, observability, and managed-PG portability against the
   current bespoke approach before committing further.

Do not migrate scheduling or fan-out until the retry-path spike validates the
extension against the target deployment environment.

---

**Related**: `EXPANSION.md` → "Orchestration: Apache Airflow" → "Durable
Functions in Postgres (pg_durable)"; scout issue #82.
