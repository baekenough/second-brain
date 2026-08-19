---
name: prove-worker-liveness-before-regression-verdict
description: Date the rows (actions_id_seq contiguity + pg_stat_user_tables) before blaming a deploy for a periodic worker producing nothing — and never DELETE prod rows to test a 10-min worker
metadata:
  type: feedback
---

For a periodic worker, "it produced nothing" is never sufficient evidence of a code regression. Date the rows first, then form a verdict.

**Why:** on 2026-08-19 a full regression hunt was opened against PR #209 (`awaiting_my_reply` 447 → 0, "worker ran several ticks and regenerated none"). There was no bug. The rows had been deleted manually and the very next scheduled tick rebuilt all 419 — the observation was made inside the single ≤10-minute gap before that tick. Every named suspect (`is_auth_like` / `isOutbound` / `CounterpartIdentity` / `threadKeyForDocument` / nil entity store / nil `w.now`) was eliminated, and the cost was hours plus a 447-row production DELETE performed on a false premise.

**How to apply:** before touching code, get these four read-only facts.

1. `docker inspect -f '{{.State.StartedAt}} {{.RestartCount}}'` — `RestartCount=0` kills every panic/crash-loop hypothesis outright. Do this first; it is one command.
2. **Sequence forensics.** `INSERT ... ON CONFLICT DO UPDATE` burns a sequence value per attempted row, so a table's `id` column dates its own inserts. A contiguous id block exactly the width of one batch = one tick inserted the whole set. `(last_value - max_id) / batch_size` = how many ticks have elapsed since, which converts to wall-clock via the interval.
3. **`pg_stat_user_tables`** — `n_tup_del` confirms the size of a manual delete (447 matched exactly), and `last_autoanalyze` on a low-churn child table (`action_status`, which gets no UPDATEs) timestamps the repopulation to within the autovacuum naptime. This is how the insert was pinned to a specific tick.
4. **Conversion ratio.** Count eligible source rows and output rows in the same query. `416 threads / 416 actions` proves no early return is firing — far stronger than reading the branches and arguing about them.

Beware two traps this incident turned on: `min(observed_at)` dates a row's **exit** from the working set, not its insert (every tick refreshes `observed_at`), and untouched sibling data (`my_commitment` etc. looking healthy) proves nothing about liveness when only one kind was deleted.

Do not delete production rows to test a worker whose interval is 10 minutes — wait one interval, or drive `Tick` directly in a test. Related: [[project-action-surface]], [[second-brain-project]].
