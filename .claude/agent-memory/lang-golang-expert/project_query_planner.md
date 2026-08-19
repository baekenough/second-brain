---
name: query-planner
description: /ask Stage 1b QueryPlan — why the regex path is only a cache, what LLM_THINKING=disabled silently breaks, and the three retrieval gaps the planner makes worse
metadata:
  type: project
---

2026-08-19: `internal/intent` gained a **planner** (`LLMPlanner.Plan` → one
`QueryPlan`) in front of `assembleRetrieval`, per
`docs/superpowers/specs/2026-08-19-query-planner-design.md`.

**The split that decides every future question in this area:** `QueryPlan`
decides WHICH documents may enter the candidate set (occurred_at window, source
include set, limit → WHERE predicates); `intent.Params` decides HOW candidates
are RANKED (entity boost, bigm nudge, `Sort`). `assembleRetrieval` takes both.
`params.OccurredFrom/To` is now **ignored** there — the planner runs the same
date regexes, so two writers of one field would drift again. `Params` still
carries the fields (Classify sets them, its tests cover them); nothing reads
them in the retrieval path.

**Why the regex path survives at all:** not latency, not cost. Both paths return
the same type, so a vocabulary miss costs one LLM round-trip instead of deleting
the filter. The original bug was shape divergence — "오늘" produced a window and
"내일" produced nothing, and "nothing" is indistinguishable from "no time
expression". The golden set (spec §8, 14 rows, `internal/intent/plan_test.go`)
therefore runs **one table against both paths**; the LLM path is forced via the
test-only `DisableDeterministicPath`.

**Deployment condition, not a preference:** `LLM_THINKING=disabled`. With
reasoning on, the planner's ~750-token budget is eaten by `reasoning_content`,
every call returns `*llm.TruncatedError` with a 0-byte body, and every plan
becomes `Origin="fallback"` — i.e. the feature is off and **no error reaches the
user**. Only the `origin` field distinguishes that from working. See
[[llm-reasoning-budget]].

**Two rules that are judgement calls, not spec text:**
- Deterministic source narrowing happens on exactly two triggers: an explicit
  calendar word (일정/스케줄/캘린더/약속), or a window lying **entirely in the
  future** (nothing unhappened is observable in SMS/calls/mail). Narrowing is
  destructive — over-narrow returns zero — so everything subtler is left to the
  LLM path, whose wider "no filter" answer is always a valid plan.
- The planner never emits `insight`. An insight-only include set empties the
  observed lane, and `ask.go` reports an empty observed lane as
  `finish_reason="no_evidence"` regardless of how much inference came back.
  Dropping it widens to the whole corpus, so the widening is **stated in
  Reason** (spec §6.2 allows disclosed widening, forbids silent widening).

**R3 CLOSED 2026-08-19 (#212), and the reason it closed where it did:**
`Sort="recent"` was an unconditional DESC, i.e. furthest-future-first for a
planner-emitted future window. `store.sortOrder` now derives the direction from
`OccurredFrom`: `>= now` (window provably entirely future) -> `occurred_at ASC`;
everything else keeps `COALESCE(occurred_at, collected_at) DESC`. Straddling
windows deliberately stay DESC — "nearest to now" has no unambiguous direction
there, and the fix had to be invisible to every query that already worked.

The direction was NOT added to `QueryPlan`, and that is the reusable part: it is
a pure function of a field the store already receives, so a plan field would be
a second writer of a derived value — exactly the drift that made the `Sort`
doc-comment ("collected_at DESC") disagree with the code for as long as it did.
It also keeps the change inside `internal/store`, touching neither `intent` nor
`api`. The `plan owns retrieval / Params owns ranking` split above still holds:
ranking DIRECTION is derived, not planned.

**Open, in priority order:**
1. Chunk lanes filter sources **after** fetching `limit` rows
   (`applySourceTypeFilters`). Pushing it into SQL needs `ChunkSearcher`'s
   signature changed in `internal/search/search.go`; narrow plans can starve
   chunk recall to 0.
2. R2: any window at all excludes `occurred_at IS NULL` rows by store contract,
   and the planner sets windows far more often than the old regex path did.

Related: [[temporal-window-retrieval]], [[llm-reasoning-budget]],
[[tests-must-inject-prod-environment]].
