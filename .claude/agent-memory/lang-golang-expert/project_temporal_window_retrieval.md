---
name: temporal-window-retrieval
description: Why occurred_at is a per-lane WHERE predicate (not a sort hint), and the chunk-lane include-filter leak found next to it
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

Related: [[project_search_rrf_relevance]], [[project_second_brain]]
