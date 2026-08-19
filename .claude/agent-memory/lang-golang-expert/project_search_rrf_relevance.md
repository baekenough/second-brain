---
name: search-rrf-relevance-investigation
description: mergeRRF equal-weight bug (fixed) + a bigger unfixed HNSW ef_search relevance bug found while diagnosing it
metadata:
  type: project
---

2026-08-17: diagnosed "half the search results for a proper-noun query are
irrelevant" (e.g. `한영석` → secretary×10 + call-log×10, though the name
appears in zero secretary documents). Root cause confirmed against prod DB
(aggregated counts only, no content read) via a from-scratch SQL replica of
both `internal/store/document.go`'s `hybridSearch` and
`internal/search/search.go`'s `mergeRRF`.

## Fixed: mergeRRF equal-weight list fusion (`internal/search/search.go`)

`mergeRRF(primary, secondary, limit)` fused the document-store's hybrid
result (`primary`) and the chunk-vector search result (`secondary`) with
equal per-list, rank-only RRF weight. `secondary` is a single
un-corroborated semantic signal (raw embedding proximity, no term match);
`primary` already fuses up to 5 weighted signals. Measured: for `한영석`,
primary alone = 20/20 correct (name present); secondary alone = 20/20
unrelated docs, **zero ID overlap** with primary. Equal-weight merge → 10
correct + 10 unrelated.

Fix: secondary now only (a) boosts documents primary already returned, or
(b) fills slots primary left empty (`remaining := limit - len(primary)`).
Once primary fills the page, secondary cannot displace it. This preserves
all 5 pre-existing `mergeRRF` tests and both `EmptyPrimary`/chunk-FTS-fallback
paths untouched — verified by re-running the exact simulation with the fix
applied (`한영석` → 20/20 correct).

## NOT fixed, flagged as a bigger follow-up: HNSW `ef_search` under filtering

While probing `primary`'s own `vec` CTE (`embedding <=> query LIMIT 40` with
`WHERE status='active'`), found the **HNSW ANN index itself is badly
inaccurate under this filter**: `hnsw.ef_search=40` (default, == the LIMIT)
returns a candidate set that differs from the true top-40 (brute-force,
`enable_indexscan=off`) in 37/40 rows, and **completely misses all 15
genuinely-matching call-log documents** that the exact computation finds at
true rank 11-30. Raising `ef_search` to 1000 only improves this to 15/40
still differing — doesn't fully converge. This affects every `vec`/`summvec`
CTE call in `internal/store/document.go`'s `hybridSearch` and every
`ChunkStore.SearchVector` call in `internal/store/chunks.go`, i.e. it's
active on essentially every hybrid search query, not just proper nouns.

Separately (and true even under exact/brute-force computation, so NOT an
HNSW artifact): a bare Korean personal-name query's embedding is closest to
several `secretary` documents (rank 4-5) purely because `secretary` is ~39%
of the corpus by document count — pure vector similarity is inherently weak
signal for short proper-noun queries, independent of ANN approximation.

Not fixed because: `internal/store/document.go`'s RRF SQL had just been
patched (PR #186, float8 cast) when this session started and the task
explicitly said not to touch it further without strong necessity; fixing
`ef_search` properly needs either a per-query `SET LOCAL hnsw.ef_search=N`
(cheap, but N needs real tuning/eval, not a guessed constant) or an index
rebuild — infra-level work, not a Go-code-only fix. Left as an open
follow-up; whoever picks it up should read this note first rather than
re-deriving the same probe from scratch.

## Diagnostic method note (worth repeating)

Diagnosed entirely with aggregated `COUNT(*)`/`source_type` SQL over SSH to
the macmini prod Postgres container — never read document title/content.
Computed the query embedding via `docker exec` into the *server* container
(so the API key never had to be typed into a shell command directly) and
copied the resulting vector file to build `WHERE embedding <=> '[...]'::vector`
probes. This let me replicate `hybridSearch` and `SearchVector` outside Go,
compare "current SQL" vs "corrected SQL" variants side by side, and get an
exact before/after row-count comparison for the code fix without deploying
anything or running a backfill.

Related: [[project_second_brain]]
