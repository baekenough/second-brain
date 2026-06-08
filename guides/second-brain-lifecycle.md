# Second-Brain Lifecycle Framework

Reference guide mapping the five-stage second-brain lifecycle to current
implementation status. Internalized from scout issue #84 (awesome-second-brain,
https://github.com/aristoapp/awesome-second-brain — a curated comparison guide
and decision framework, not vendored software).

---

## 1. The Five Stages

| Stage | Definition | Status |
|-------|-----------|--------|
| **Collect** | Ingest raw content from external sources | Implemented |
| **Organize** | Structure, chunk, embed, and link content | Partially implemented |
| **Evolve** | Reindex, re-embed, and measure quality over time | Partially implemented |
| **Use** | Surface content via search, API, and MCP | Implemented |
| **Govern** | Manage data quality, retention, and access control | Largely absent |

---

## 2. Stage-by-Stage Codebase Mapping

### Collect — Implemented

`internal/collector/` provides source-specific adapters for Slack, Discord,
Notion, Google Drive, GitHub, Telegram, filesystem, and secretary/LLM-memory
sources. Each adapter satisfies the common `Collector` interface
(`Collect(ctx) ([]model.Document, error)`). The scheduler daemon (`cmd/collector`)
runs these on a `robfig/cron` schedule with Postgres-checkpointed watermarks
(`collector_state`, migration 009) so restarts resume rather than re-scan.

Extraction failures are queued with exponential backoff in the
`extraction_failures` table (migration 003) and retried by
`internal/worker/extraction_retry.go` — dead-lettered after 10 attempts.

### Organize — Partially implemented

**Done:**
- Adaptive chunking per source type: `internal/chunker/adaptive.go` selects
  heading-aware vs. flat splitting, target size, and overlap based on
  `SourceType` (see `guides/rag-chunking-strategy.md`).
- Hybrid embeddings: `internal/search/embed.go` calls an OpenAI-compatible
  endpoint; chunks receive their own vector alongside the parent document
  (`chunk_embeddings` column, migration 015).
- Entity extraction schema: `entities` + `document_entities` tables
  (migration 017, issue #77) are in place for named-entity linking
  (PERSON / ORG / CONCEPT / OTHER).

**Gap — entity extraction pipeline not yet wired:**
Migration 017 defines the schema but there is no collector-time extraction
job populating it. The tables exist; the extraction step is absent. This is
the longest-horizon roadmap item in `EXPANSION.md` → "Knowledge graph
generation — scout #77".

### Evolve — Partially implemented

**Done:**
- Reindex pipeline: `internal/search/reindex.go` re-embeds documents on demand
  or on schedule, with progress checkpointed in `reindex_state` (migration 008).
- Eval pipeline: `cmd/eval` computes NDCG@5, NDCG@10, and MRR@10 against
  positive-feedback pairs, persists results to `eval_metrics` (migration 007),
  and enforces a 5% relative regression gate against the previous baseline.
  Latency per query is also recorded (migration 016).
- Tuning: `internal/search/tune.go` exposes parameter search over the
  hybrid-retrieval weights.

**Gap — no cost/efficiency axis in eval:**
Quality regression is tracked; cost (latency budget, embedding API spend per
ingestion run) is not. See "activation evidence" in section 4 and
`EXPANSION.md` → "Eval cost profiling — scout #75" for the planned extension.

### Use — Implemented

- REST search endpoint (`cmd/server`) exposes hybrid BM25 + vector search with
  RRF fusion (`k=60`), optional LLM curation (`internal/curation/`), and
  HyDE query expansion for Korean (`internal/search/hyde.go`).
- MCP surface: the server doubles as an MCP tool endpoint consumed by AI
  agents in the second-brain Claude Code workspace.
- pg_bigm 2-gram index (migration 006) provides morphology-independent Korean
  full-text recall as a fallback when `EMBEDDING_API_URL` is unset.

### Govern — Largely absent

No structured governance layer exists today.

| Concern | Current state |
|---------|--------------|
| Data retention / TTL | No expiry policy on documents or chunks |
| Access control | Single shared `API_KEY`; no user identity or per-source permissions |
| Audit log | None |
| Data-quality monitoring | Eval pipeline catches retrieval regressions; no coverage-gap or duplication detection |
| PII / secret redaction | Planned for local daemon (see `EXPANSION.md` → Daemon); not applied to remote collectors |

Governance is the largest structural gap relative to the lifecycle model. The
nearest in-progress item is authentication (EXPANSION.md → "Authentication and
Authorization"), which would provide a user-identity foundation.

---

## 3. Activation Evidence

*Activation evidence* is the concept that retrieval quality is not
sufficient on its own — a useful second-brain must confirm that retrieved
memory **actually influenced the AI's decision or output**, not merely that
it was fetched.

### What the eval pipeline currently measures

`cmd/eval` measures **retrieval quality**: given a query derived from
user-approved positive feedback, did the top-K results include the document
that the user previously marked relevant? This answers "did we fetch the
right thing?" (NDCG, MRR).

### What activation evidence would measure

Activation evidence answers a different question: "did the fetched context
change what the model produced?" Concretely:

- Run the same query with and without injected search results.
- Compare model outputs: did the retrieved context appear verbatim, paraphrased,
  or cited in the answer? If the answer is identical with or without context,
  retrieval had zero activation — the model ignored it.
- Track activation rate as a secondary eval metric alongside NDCG.

### Connection to the existing eval axis

The natural extension point is `cmd/eval` and the `eval_metrics` table. A
new `activation_rate` column (or sibling table) could record per-query
activation scores alongside the existing quality and latency columns.

This is a **future eval axis**, not a current implementation gap that blocks
other work. It pairs naturally with the cost-profiling work planned under
`EXPANSION.md` → "Eval cost profiling — scout #75": once read-path and
write-path costs are profiled, activation evidence provides the output-side
signal that completes the evaluation loop.

---

## 4. External Reference

This guide is based on the **awesome-second-brain** reference framework
(https://github.com/aristoapp/awesome-second-brain), a curated comparison
guide and decision framework for second-brain systems. It is kept external —
no code is vendored. Scout issue #84, verdict INTEGRATE (P3).

---

**Related**: `EXPANSION.md` → scout #75 (eval cost profiling), scout #77
(knowledge graph / entity extraction), scout #84 (this guide);
`guides/rag-chunking-strategy.md` (Organize stage detail).
