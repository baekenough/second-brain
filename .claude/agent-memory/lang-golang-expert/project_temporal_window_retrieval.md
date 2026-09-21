---
name: temporal-window-retrieval
description: Why occurred_at is a per-lane WHERE predicate (not a sort hint), the chunk-lane include-filter leak next to it, and the eval-harness window mismatch that made golden NDCG look like 0
metadata:
  type: project
---

2026-08-18: `/api/v1/ask` "오늘 일정 알려줘" returned zero calendar documents.
`intent.Classify` had computed an exact window all along; `assembleRetrieval`
threw it away and set `Sort="recent"` instead, with a comment calling recency a
proxy for a date range.

**It is not a proxy, and the distinction generalises.** `sortOrder()` appends an
outer `ORDER BY`; every retrieval lane is separately capped by `LIMIT $3`.
Sorting can only permute candidates a lane already selected, so when one source
outnumbers another by four orders of magnitude (calendar ~14 docs vs secretary
~18.3k vs llm-memory ~19.9k) the small source never enters the candidate set to
be sorted. Any constraint that must *change which documents are retrieved* has
to be a WHERE predicate inside each lane, like `SourceType`/`ExcludeSourceTypes`
already were. Sort hints are only ever a tiebreaker.

Fix: `model.SearchQuery.OccurredFrom/OccurredTo` → predicate in all five RRF
lanes (fts/vec/bigm/summvec/entity) + fulltext, parameter-bound. Verified
against a throwaway local Postgres (temp table, ROLLBACK, synthetic rows):
`[from, to)` includes a row exactly at `from` and excludes one exactly at `to`;
NULL `occurred_at` is excluded under every bound, while
`COALESCE(occurred_at, collected_at)` leaks it — that contrast is the reason the
predicate must not coalesce.

**Testability precondition:** `hybridSearch`/`fulltextSearch` were split into
`buildHybridSearchQuery`/`buildFulltextSearchQuery` (pure) + `resolveWeights`
(the part needing a DB call). Without that split no test can prove a filter
reached all five lanes, which is the only failure mode that matters — four of
five is indistinguishable from zero of five at the API boundary.

**Latent bug the fix activated (closed in the same PR):** `intent`'s
`dayRange`/`weekRange`/`monthRange` returned an INCLUSIVE upper bound
(`23:59:59`). Harmless while `OccurredTo` was dead, wrong the moment it reached
a `< to` predicate — it dropped the period's final second and left a gap between
consecutive windows. All three now return the next period's start
(`time.Date(y, m, d+1, ...)`, `d+7`, `month+1` — which also gives December's
year rollover for free). General rule: when a previously-unused value becomes
load-bearing, re-audit its semantics in the same change; "it was already like
that" does not apply if nothing was reading it.

**Adjacent leak, since fixed as a POST-filter (#196, 2026-08-19):** the chunk
lanes now run `applySourceTypeFilters` (include + exclude), but only after
fetching `limit` rows — the include set still cannot reach the chunk SQL,
because `ChunkSearcher.SearchFTS/SearchVector` take only (query, limit). A
narrow source plan can therefore still starve chunk recall to zero. Original
description of the leak follows.

**Adjacent leak, as originally found:** `search.Service`'s two chunk
lanes apply only `dropExcludedSourceTypes` — the EXCLUDE list. The INCLUDE
filter `q.SourceType` is never applied to them, and chunk results carry no
`occurred_at`/source metadata beyond source type. Reproduced in a scratch test:
`source_type=calendar, limit=20` with 14 active calendar docs → 20 results, 6 of
them SMS, via `mergeRRF`'s fill path. This also explains the "20 results from 14
documents" observation — it is not duplicate rows. The same-shaped hole for the
date window WAS closed here (chunk lanes are skipped when a window is set, so an
empty window answers empty instead of silently widening).

**The same distinction bit the EVALUATOR, not just retrieval (2026-09-21).**
`cmd/eval --golden` scored ndcg10 ≈ 0 on labels the user had just judged
relevant at rank 1–2 on the golden screen. Neither number was wrong: the golden
candidate screen (`internal/api/golden.go`) resolves the question's period
phrase through `intent.DeterministicWindow` anchored at REVIEW TIME and searches
inside that window, while `cmd/eval` searched the whole corpus. Different
candidate pools, so the scores were never comparable. **Before calling a
retrieval metric a regression, check that the harness retrieves from the same
pool the labels were produced in.** Added `--window=plan|none` (default `none`,
anchored by `--as-of`) to reproduce the screen's window; the two-stream
relevance/recency merge and `IncludeRetention` are deliberately NOT reproduced
(screen ergonomics, and a wider corpus than `/ask` uses).

Two conventions established there, both worth keeping:

- **config_hash extension**: a new run-config key is added ONLY for the
  non-default value (`applyWindowProfile` in cmd/eval/provenance.go). Adding it
  unconditionally would change every existing hash and orphan every stored
  baseline for a behaviour change that did not happen. The as-of anchor enters
  the hash as a KST *calendar date*, not an instant — every branch of the
  deterministic parser lands on KST day boundaries, so day granularity is exact
  while second granularity would make every run its own unhashable island.
- **Frozen hash tokens vs live claims**: `run_config.rerank_outcome` still reads
  `"not_instrumented"` even though `Service.RerankStats()` now measures it. That
  string is a hash INPUT, not an assertion; the real counts live in
  `current.rerank_attempts/failures/succeeded`. If a future change "corrects"
  the token, every baseline splits.

Diagnostics (`--dump`) deliberately carry no question text, title or body —
queries are named by `golden_queries.id` or a `sha256:` prefix, timestamps are
truncated to a date, file mode 0600. See [[feedback_personal_data_endpoint_verification]].

Related: [[project_search_rrf_relevance]], [[project_second_brain]]
