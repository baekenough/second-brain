# Expansion Plans

This document describes future capabilities that are intentionally **out of scope** for the current PoC. Items here require additional infrastructure, operational maturity, or use cases that have not yet been validated.

---

## Orchestration: Apache Airflow

**Current state**: Collection jobs are scheduled with `robfig/cron` inside the Go collector daemon process. All collectors share the same process lifecycle and have no dependency graph.

**Future state**: Migrate to Apache Airflow once either of these thresholds is crossed:

- Data source count exceeds 10, or
- Cross-source dependencies emerge (e.g., enrich a GitHub PR with its linked Notion spec before embedding).

**Benefits Airflow provides:**

| Concern | Current | With Airflow |
|---|---|---|
| Dependency ordering | Not supported | DAG-native |
| Retry with backoff | Manual | Built-in per-task |
| Backfill | Not supported | `airflow dags backfill` |
| Monitoring | Log files | Web UI + alerts |
| Parameterized runs | Not supported | Jinja-templated vars |

**Migration path:**

1. Extract each collector's `Collect(ctx)` method into a standalone Python or Go operator callable via Airflow.
2. The Go HTTP server remains API-only (already separated as `cmd/server`). Remove `robfig/cron` from the collector daemon.
3. Airflow DAG triggers collector tasks; completed documents are written to PostgreSQL as today.
4. Embed scheduling configuration in DAG files version-controlled alongside this repo.

### Durable Functions in Postgres (pg_durable) — scout #82

> **Reframed to a consolidation spike** (scout issue #82, https://news.hada.io/topic?id=30225).
> `pg_durable` implements retry, scheduling, parallel fan-out, and conditional branching as a SQL DSL with all state checkpointed inside PostgreSQL — no external queue or container. This aligns with second-brain's Postgres-single-dependency architecture (pgvector + pg_bigm).

**Current state**: The "Retry with backoff: Manual" row above understates what already exists. Postgres-checkpointed durable retry is already implemented in Go:

- `extraction_failures` table (`migrations/003_extraction_failures.sql`): `attempts`, `next_retry_at` (default `now() + 5min`), `dead_letter`, plus partial index `idx_extraction_failures_next_retry ... WHERE NOT dead_letter` for cheap due-row polling.
- `internal/worker/extraction_retry.go` (`ExtractionRetryWorker`): polls `DueForRetry`, re-runs extraction, increments `attempts` with exponential backoff, dead-letters at 10 attempts.
- Companion checkpoint tables: `collector_state` (`migrations/009`, per-instance watermark), `reindex_state` (`migrations/008`).

**Opportunity**: `pg_durable` is therefore not a missing capability but a *consolidation* candidate — replace the ad-hoc per-worker retry tables with a generic durable-step DSL. Candidate migration targets: (1) collector retry, (2) embedding-batch fan-out, (3) part of the `robfig/cron` schedules.

**Risk**: PostgreSQL extension install burden — possible incompatibility with managed PG offerings, and the project is early-stage.

**Recommended next step**: A verification spike, not wholesale adoption — install `pg_durable` in the local `docker-compose` stack and migrate a single collector retry path as a proof of concept before committing.

---

## Search: Advanced Vector Embeddings

**Current state**: `pgvector` stores 1536-dimension OpenAI-compatible embeddings. Embedding is optional; the system falls back to PostgreSQL full-text search when `EMBEDDING_API_URL` is unset. pg_bigm 2-gram indexing provides morphology-independent Korean search.

**Planned improvements:**

### Fine-tuned embedding models

Generic embedding models do not understand company-specific vocabulary (product names, internal acronyms, team jargon). Fine-tuning on internal corpora improves recall significantly for these queries.

- Training data: collected documents labeled by relevance via implicit feedback (clicks, follow-up queries).
- Hosting: self-hosted model server (e.g., Infinity, vLLM) to avoid per-token API costs at scale.

### Chunking strategies

> **Partially done**: Basic chunking infrastructure is in place (`internal/chunker/`). Sliding-window and semantic chunking are TODO.

Long documents currently embed as a single vector, which degrades recall for targeted queries. Replace with:

- **Sliding window**: fixed-size chunks with overlap — low complexity, good baseline.
- **Semantic chunking**: split at paragraph/section boundaries detected by a lightweight sentence model.
- Store parent document ID on each chunk to reassemble full context at retrieval time.

### Re-ranking with cross-encoder models

Two-stage retrieval:

1. First pass: ANN search with pgvector (high recall, fast).
2. Second pass: cross-encoder re-ranks top-K candidates against the original query (high precision, slower).

Reduces hallucination risk when the LLM downstream consumes search results.

### Curation ranking with interest-drift adaptation — scout #93

> **Internalized from** scout issue #93 (arXiv "PaperFlow: Profiling, Recommending, and Adapting Across Daily Paper Streams", http://arxiv.org/abs/2606.07454v1).
> PaperFlow maintains per-user interest profiles, performs multi-signal recommendation, and adapts to interest drift over time — validated on a 24-user, 50-day, 20,727-paper longitudinal benchmark.

**Relevance**: second-brain's `cmd/scout` already produces a daily stream of papers and repos. PaperFlow's profiling + drift-adaptation framework could improve scout's curation ranking — surfacing items that match a user's evolving interests rather than static keyword matches. This also informs the LLM curation/re-ranking layer (`internal/curation/`) with a proven multi-signal design. Borrow the profiling + drift-adaptation framework as a design reference when extending scout or the curation layer.

**Constraint**: PaperFlow is designed for academic paper streams; adapting it to heterogeneous second-brain sources (GitHub repos, Notion pages, etc.) requires generalising the signal schema. Treat as a design reference, not a code dependency.

### Multi-modal search

Google Drive and Notion contain images, diagrams, and PDFs. Future work:

- Extract text from PDFs with `pdftotext` or a cloud OCR service.
- Embed diagram descriptions generated by a vision model (e.g., GPT-4o vision, LLaVA).
- Surface image results alongside text results with a thumbnail preview in the web UI.

### Unembedding-matrix embedding filter — scout #91

> **Internalized from** scout issue #91 (arXiv "Your UnEmbedding Matrix is Secretly a Feature Lens for Text Embeddings", http://arxiv.org/abs/2606.07502v1).
> LLM-derived embeddings align to high-frequency, semantically meaningless tokens when projected through the model's unembedding matrix. "EmbedFilter" identifies and removes that noisy subspace, refining the semantic representation and reducing effective dimensionality — improving both retrieval quality and search speed without retraining.

**Relevance**: directly applicable to second-brain's pgvector embedding quality; removing low-signal dimensions would sharpen the hybrid BM25 + vector RRF path (`internal/search/search.go`).

**Constraint**: EmbedFilter targets LLM-derived embeddings and requires access to the model's unembedding weight matrix. The current stack uses a 1536-dim OpenAI-compatible API embedding where the matrix is not exposed, so this technique applies only if/when self-hosted LLM-based embeddings (see "Fine-tuned embedding models" above) are adopted. Borrow the post-processing filter as an embedding-quality step at that point.

### Eval cost profiling — scout #75

> **Internalized from** scout issue #75 (arXiv "Agent Memory: Characterization and System Implications of Stateful Long-Horizon Workloads", http://arxiv.org/abs/2606.06448v1).
> The paper quantifies the **read-path vs write-path cost asymmetry** of agent memory systems and provides a profiling methodology across two benchmarks.

**Current state**: The eval pipeline (`cmd/eval`, `internal/store/eval.go`, `eval_metrics` table in `migrations/007_eval_metrics.sql`) already computes search **quality** metrics — NDCG@5, NDCG@10, MRR@10 — with a 5% relative regression gate against the previous baseline. Quality is covered; **cost is not**.

**Planned**: Add a cost axis to the eval pipeline per the paper's methodology — profile read-path (query → embed → hybrid retrieve → rerank) vs write-path (collect → chunk → embed batch → upsert) latency and `$` cost separately. Use the resulting cost asymmetry as tuning evidence for collector embedding batch size and rerank call frequency. Extend `eval_metrics` (or a sibling table) with cost columns so regressions in cost are tracked alongside quality regressions.

---

## Binary Separation

> **DONE** (2026-04-15): The monolithic server has been split into two independent binaries:
> - `cmd/server/` — API server (port 8080, search + curation + document endpoints)
> - `cmd/collector/` — Collector daemon (scheduling, embedding, source collection)
>
> Docker multi-target build produces separate `second-brain` and `second-brain-collector` images.

---

## LLM Curation Layer

> **DONE** (2026-04-15): The curation layer is implemented in `internal/curation/`.
> - `curated` parameter on search endpoints triggers LLM re-ranking and lightweight summary generation.
> - Raw data is always included alongside curated results.
> - Configurable via `LLM_API_URL`, `LLM_API_KEY`, `LLM_MODEL` environment variables.

---

## Korean Search (pg_bigm + HyDE)

> **DONE** (2026-04-15): Korean search improvements are implemented.
> - pg_bigm 2-gram indexing for morphology-independent Korean search (no dependency on Korean morphological analyzer).
> - HyDE (Hypothetical Document Embeddings) query expansion improves recall for Korean queries.

---

## GraphQL Secondary API

**Status**: TODO

**Concept**: Add a GraphQL endpoint alongside the REST API for clients that need flexible field selection, nested document relationships, or batched queries.

**Motivation**:
- AI agents and MCP clients often need only specific fields (e.g., `id`, `title`, `score`) — GraphQL avoids over-fetching.
- Future knowledge graph features (entity relationships, cross-references) map naturally to GraphQL's nested query model — see "Knowledge graph generation — scout #77" under AI Features.

**Scope**:
- Read-only GraphQL endpoint at `/api/v1/graphql`.
- Expose `search`, `documents`, `sources` queries.
- Schema auto-generated from existing Go model types.

---

## Daemon: Local Data Collection Agent

**Concept**: A lightweight background agent running on each team member's machine. Captures work context that is never published to a shared service (browser activity, local files, IDE sessions) and syncs a privacy-filtered summary to the second-brain server.

**Data collected:**

- Browser history (title + URL, no page content by default)
- Local documents opened (path, last-modified, optional content hash)
- IDE activity (active file, language, session duration)
- Terminal sessions (commands and exit codes; secrets automatically redacted)

**Privacy model:**

- All raw data stays on the local machine in an SQLite buffer.
- The user configures inclusion/exclusion rules (glob patterns, domain blocklists) before any sync occurs.
- A local preview UI shows exactly what will be synced; no data leaves without explicit confirmation or scheduled approval.
- Sync payloads are encrypted with mTLS; the server never stores credentials or raw terminal output.

**Architecture sketch:**

```
[local daemon]
  ├── data source plugins (browser, IDE, terminal, filesystem)
  ├── SQLite buffer  (local, git-ignored)
  ├── privacy filter (user-configurable rules)
  └── sync client (mTLS → second-brain server)
```

- Go binary, target size ~10 MB.
- Plugin system: each data source adapter is a Go interface compiled in or loaded via shared library.
- Sync interval configurable; defaults to 15 minutes.
- Server receives opaque document payloads identical in schema to remote collectors — no special handling required.

---

## Infrastructure

### Kubernetes deployment

- Helm chart with separate deployments for server, collector, and web.
- Horizontal pod autoscaling on CPU for the search endpoint.
- PersistentVolumeClaim for PostgreSQL, or managed RDS/CloudSQL instead.
- Ingress with TLS termination via cert-manager.

### Multi-region replication

- Read replicas in secondary regions for low-latency search.
- Write primary remains single-region; collectors write to primary only.
- pgvector index replicated via standard PostgreSQL logical replication.

### Backup and disaster recovery

- Continuous WAL archiving to object storage (S3 / GCS).
- Daily logical backups with `pg_dump` retained for 30 days.
- Recovery time objective (RTO): < 1 hour; recovery point objective (RPO): < 5 minutes.

### Monitoring

- Prometheus metrics exported from the Go server (`/metrics` endpoint).
- Grafana dashboards: collection throughput, search latency p50/p95/p99, embedding queue depth.
- Alerts: collector failures, database connection pool exhaustion, embedding API errors.

---

## Authentication and Authorization

**Current state**: Single shared `API_KEY` header. No user identity.

**Planned model:**

- OAuth2/OIDC integration with the company identity provider (Google Workspace, Okta, etc.).
- JWT-based session tokens issued at login; verified on every API request.
- Role hierarchy:

| Role | Permissions |
|---|---|
| `viewer` | Search, read documents |
| `editor` | Trigger manual collection, annotate documents |
| `admin` | Manage connectors, view audit log, manage users |

- Per-document access control: propagate visibility from the source system. A private Google Drive file remains private in second-brain.
- SSO session sharing with other baekenough internal tools.

---

## Additional Data Sources

| Source | Notes |
|---|---|
| Jira / Linear | Issues, comments, sprint history |
| Confluence / Wiki | Page tree, version history |
| Gmail / Outlook | Subject + body; attachments as separate documents |
| Google Calendar | Event titles, attendees, descriptions |
| Figma | Frame names, component descriptions via Figma REST API |
| Custom webhooks | Inbound POST to `/api/ingest` for arbitrary sources |

Each source follows the existing `Collector` interface: `Collect(ctx) ([]model.Document, error)`. Adding a new source does not require changes to the search or storage layers.

---

## AI Features

### Conversational search

A chat interface backed by an LLM with retrieval-augmented generation (RAG). The user asks a question in natural language; second-brain retrieves relevant documents and the LLM synthesizes a cited answer.

### Auto-summarization

Summarize long documents at collection time and store the summary alongside the full content. Surface summaries in search results to reduce time-to-answer.

### Second-Brain lifecycle framework — scout #84

> **Internalized from** scout issue #84 (awesome-second-brain,
> https://github.com/aristoapp/awesome-second-brain) — a curated comparison
> guide and decision framework (not a software tool). Verdict: INTEGRATE (P3),
> keep-external.

Maps the five-stage second-brain lifecycle (Collect → Organize → Evolve →
Use → Govern) to current codebase status. Introduces the "activation evidence"
concept as a future eval axis — does retrieved memory actually influence AI
output? — which pairs with the cost-profiling work in scout #75.

See `guides/second-brain-lifecycle.md` for the full mapping and Govern-stage
gap analysis.

### Knowledge graph generation — scout #77

Extract entities (people, projects, products, decisions) and relationships from documents. Expose a graph query API and visualize connections in the web UI. Useful for onboarding and impact analysis.

> **Internalized from** scout issue #77 (`zaydmulani09/mnemo`, https://github.com/zaydmulani09/mnemo) — a Rust local-first memory layer with a persistent knowledge graph, entity extraction, and semantic search.

**Why this is a genuinely new axis**: there is currently **no entity or knowledge-graph table** in the codebase — search is hybrid (BM25 + vector RRF, `k=60`) over documents/chunks only (`internal/search/search.go`). Adding an entity → relationship-edge layer would augment recall with graph-hop traversal on top of the existing hybrid path.

**Sketch** (longest-horizon roadmap item):
- Extract entities at collection time → `entity` table + `entity_edge` (relationship) table in Postgres.
- Add a graph-hop recall source into the existing RRF fusion alongside FTS + vector.
- Surface via the GraphQL API below, whose nested query model fits entity relationships naturally.

**Constraint**: mnemo is an early (193-star) project — borrow the **schema design** only; do not take a code dependency.

### Shared code-graph layer — scout #92

> **Internalized from** scout issue #92 (GitHub "anvia-hq/lexa", https://github.com/anvia-hq/lexa).
> lexa is a Rust code-intelligence tool that converts a codebase into a portable, queryable graph so that humans and AI agents share one stable, consistent project view — all tools query a single common code graph rather than independently re-parsing source.

**Relevance**: GitHub-collected repos that land in second-brain could feed such a shared code graph into the entity/knowledge-graph layer (scout #77 above), improving code-document embedding quality and enabling graph-hop recall over code entities (functions, modules, call edges) alongside prose entities. The "single shared code graph that all tools query" design principle aligns with second-brain's goal of a unified retrieval surface. Borrow the design pattern.

**Constraint**: lexa is an early (83 stars) Rust tool — reference the architectural pattern only; no code dependency. Cross-reference with scout #77 entity extraction roadmap.

### Proactive notifications

A background job detects when newly collected content is relevant to a user's recent activity (based on their local daemon context or recent searches) and sends a digest notification via Slack or email.

### Team knowledge gap detection

Analyze which topics have low document coverage or are queried frequently with poor results. Surface gaps as a periodic report for knowledge managers so they know where to invest documentation effort.
