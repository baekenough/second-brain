---
name: project-embedding-dim-wiring
description: Issue #50 — how EMBEDDING_DIM env var reaches migration 011 on the correct DB connection
metadata:
  type: project
---

Issue #50 fixed the wiring so `EMBEDDING_DIM` actually reshapes `documents.embedding` on fresh installs.

**Approach chosen**: `RunMigrations` accepts `embeddingDim int`; for file `011_configurable_embedding_dim.sql`, it calls `runMigration011` which:
1. `pool.Acquire(ctx)` — dedicated connection from pgxpool
2. `conn.Begin(ctx)` — transaction
3. `SET LOCAL app.embedding_dim = N` — tx-scoped GUC visible to the DO block
4. `tx.Exec(ctx, migrationSQL)` — same conn/tx
5. `tx.Commit(ctx)`

**Why:** `pg.pool.Exec()` dispatches to an arbitrary pool connection. A `SET` on one connection does NOT survive to another. `pgxpool.Acquire` + `Begin` + `SET LOCAL` guarantees the same backend process sees the GUC.

**Key files changed:**
- `internal/store/postgres.go` — `RunMigrations` signature + `runMigration011`
- `internal/config/embedding_check.go` — dead `*sql.DB` code removed; now documentation-only
- `internal/config/config.go` — added `slog.Warn` for invalid `EMBEDDING_DIM` value
- `cmd/server/main.go`, `cmd/collector/main.go`, `cmd/eval/main.go` — pass `cfg.EmbeddingDim` to `RunMigrations`
- `internal/store/postgres_embedding_test.go` — new unit tests for filename detection + branch logic

**Existing-data no-op guard:** Preserved in migration 011 SQL (row count check before ALTER).

**Why:** [[project_second_brain]]
