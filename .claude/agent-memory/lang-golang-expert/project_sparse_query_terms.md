---
name: sparse-query-terms
description: #276 SEARCH_SPARSE_QUERY knob (internal/sparseq) — raw-SQL golden snapshot, append-terms-last placeholder rule, unreferenced-$1 trap, EXPLAIN-on-empty-table trap, repo-wide gofmt baseline
metadata:
  type: project
---

#276 (2026-09-24, branch feature/v0.25.0-b, uncommitted at hand-off): sentence
questions ("이번 주 회의 일정 알려줘") returned 0 sparse-lane rows because
tsvectors are `simple` (eojeol+particle = one lexeme) and the lanes used
`plainto_tsquery` AND + `LIKE '%whole question%'`. Fix = `internal/sparseq`
(stdlib-only keyword extractor) + knob `raw|chunk|chunk_doc`, default raw.

Non-obvious things that will matter again:

- **Raw byte-identity is pinned by a golden file**,
  `internal/store/testdata/sparse_query_raw.golden`, generated from the
  pre-#276 builders (`-update-sparse-snapshot` flag). Chunk SQL builders were
  first split out verbatim (`buildChunkFTSQuery`, `buildSparseContextQuery`)
  so they could be snapshotted before any behaviour change. Any edit to lane
  SQL must keep this green or consciously regenerate it.
- **Term params are appended LAST** (after the entity param). That keeps every
  existing `$n` fixed and the entity CTE byte-identical; builder test asserts
  raw args are an exact prefix of terms-mode args.
- **An unreferenced bound parameter makes PostgreSQL fail** ("could not
  determine data type of parameter $1"). Terms-mode fulltext almost dropped
  `$1`; `assertPlaceholdersDense` checks every `$1..$len(args)` is used.
- **EXPLAIN of a whole lane query on near-empty test tables proves nothing**:
  the planner joins via a documents index and pushes the LIKEs into a Filter.
  Index usability was proven by EXPLAINing the generated predicate alone
  (`chunkSparseExprs`) with `enable_seqscan=off` → BitmapOr over bigm GIN.
- Repo baseline: ~45 files were already not gofmt-clean at HEAD; "gofmt -l
  empty" can only be claimed for changed/new files.

Related: [[project_search_rrf_relevance]], [[project_temporal_window_retrieval]].
Measurement (§6 golden-eval on ubuntu1) and follow-up issues (entity-lane
direction F3) were out of scope and not done.
