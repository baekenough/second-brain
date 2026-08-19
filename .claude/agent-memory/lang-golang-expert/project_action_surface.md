---
name: project-action-surface
description: /actions cards were identical + nameless — root causes (constant summary, counterpart never resolved for awaiting_my_reply) and how rows self-heal
metadata:
  type: project
---

`/actions` shipped unusable: 50 identical cards, no counterpart name. Two independent causes, both fixed 2026-08-18.

1. `awaiting_my_reply` summaries were a Go constant, so all 434 rows read the same. Now generated per thread (`internal/action/summary.go`).
2. `counterpart_entity_id` was NULL for 51% of rows because **nothing ever attempted resolution for `awaiting_my_reply`** — the structural worker holds no entity store, and the extraction worker's awaiting branch deliberately skips the LLM-named counterpart. Not a normalization mismatch.

**Why:** the fix had to land without touching `cmd/collector/main.go` (locked by a concurrent agent), so entity resolution lives in `ActionStore.UpsertAction` — writers state a label (`model.Action.CounterpartName/Type`), the already-wired store canonicalizes it. `ON CONFLICT` now COALESCEs the column, which is also what lets pre-existing NULL rows repair themselves on the next worker tick instead of needing a migration.

Both fixes are confirmed working in production (2026-08-19: 420 rows, 373 distinct summaries, 0 NULL `counterpart_entity_id`). A reported "PR #209 killed awaiting_my_reply" regression was investigated and **disproved** — see [[prove-worker-liveness-before-regression-verdict]]. The one real defect found was that `StructuralSignalWorker` was unobservable: all four early returns in `processMessage` are silent, so a tick that wrote 419 actions logged exactly what a tick that skipped 419 logged (nothing). `Tick` now returns `tickStats` and logs a per-tick count line; the buckets must sum to `Listed`, so a future silent early return fails a test instead of hiding.

**How to apply:** before proposing a backfill migration for actions, remember the structural worker re-asserts every open thread every 10 min — only rows whose thread has left the 30-day window stay stale. Also note `listLatestPerThreadQuery` filters the window on `occurred_at`, so a thread whose newest document has NULL `occurred_at` never produces an action at all (verified, left unchanged — widening it would invent new actions). Related: [[project-second-brain]].
