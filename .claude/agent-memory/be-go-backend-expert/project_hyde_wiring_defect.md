---
name: project-hyde-wiring-defect
description: HyDE was permanently no-op in prod because llmClient was constructed after searchSvc assembly in cmd/server/main.go — fixed by reordering + buildSearchService extraction
metadata:
  type: project
---

`cmd/server/main.go`'s `.WithLLM(llmClient)` call was missing from the
`search.NewService(...)` chain, AND `llmClient` was constructed (`llm.New(...)`)
*after* that chain ran — so no code point could even reach in and wire it.
Result: `Service.llmClient` stayed nil forever, `internal/search/hyde.go`'s
`Expand()` was a silent no-op on every `UseHyDE: true` request, no error, no
log line. Confirmed in prod via a 0.142s HyDE response (too fast for the
LLM round-trip) and byte-identical ON/OFF results on 40 low-lexical-overlap
golden queries.

Fixed 2026-08-25/26 (session date drifted mid-task): moved `llmClient :=
llm.New(...)` above the search-service assembly block, added `.WithLLM(llmClient)`
to the chain (always injected — `llm.New` never returns nil, `Expand`/`WithLLM`
gate on `Enabled()` internally), and added a dedicated
`"search: HyDE query expansion available/unavailable"` startup log.

**Why:** `search.Service`'s `.With*()` builder chain degrades silently when a
call is missing — no compile error, no startup failure, just a feature that
quietly does nothing. This is the second time this project has shipped a
silent wiring gap this way (see [[project_active_weights_serving]] for the
`WithActiveWeights`/`SEARCH_ACTIVE_WEIGHTS_ENABLED` precedent, which got it
right — flag-gated, logged, tested).

**How to apply:** When touching `cmd/server/main.go`'s search-service assembly,
check (1) construction-order dependencies before adding a new `.With*()` call,
(2) whether the new optional dependency needs a startup log stating
available/unavailable, (3) whether it needs a `buildSearchService`-style pure
function extraction so the wiring itself is unit-testable without live
Postgres/LLM/embedding backends — the pattern already used for
`wireActionsAndBriefing`/`wireEvidenceFeedback`.

**Confirmed NOT bugs during this investigation** (verified by reading code,
not assumed):
- `WithOccurredRangeChecker` is intentionally never called in main.go —
  `Service.occurredRangeChecker()` falls back to a type-assertion on `s.store`,
  and `*store.DocumentStore` (which `docStore` always is in prod) already
  implements `OccurredRangeChecker.FilterIDsByOccurredRange`. The comment on
  `WithOccurredRangeChecker` in `internal/search/search.go` says this
  explicitly. Chunk-lane window filtering (`verifyWindow`) is NOT broken.
- `WithWeights` is intentionally never called in main.go — the compiled
  zero-value `SearchWeights{}` (k=60, equal weights) IS the intended default
  in production; `WithActiveWeights`/`defaultWeights` (see
  [[project_active_weights_serving]]) is the actual per-deployment override
  mechanism, and its own doc comment states "No production caller sets
  WithWeights today" as a deliberate, documented fact rather than an oversight.

Regression tests added: `cmd/server/main_test.go` —
`TestBuildSearchService_WiresLLMClientForHyDE` (positive: query gets HyDE-expanded
through the wired chain) and `TestBuildSearchService_UseHyDEWithoutLLMClientIsANoOp`
(negative: nil llmClient stays a documented no-op, not a panic).
