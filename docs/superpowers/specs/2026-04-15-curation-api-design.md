# Second Brain — LLM-Curated Private Search Engine

> Refactor second-brain from a monolithic server into a clean separation:
> **API Server** (curation + REST) and **Collector Daemon** (data ingestion).
> AI agents consume curated knowledge via REST API.

Date: 2026-04-15

---

## 1. Architecture Overview

```
┌─────────────┐     ┌─────────────────┐     ┌──────────┐
│  Collector   │────▶│   PostgreSQL    │◀────│  Server  │
│  (daemon)    │     │  + pgvector     │     │  (API)   │
└─────────────┘     │  + pg_bigm      │     └────┬─────┘
  Slack, GitHub,    └─────────────────┘          │
  GDrive, FS                              REST API
                                                 │
                                          ┌──────▼──────┐
                                          │  AI Agent   │
                                          │ (consumer)  │
                                          └─────────────┘
```

### Binary Separation

| Binary | Role | Port | Docker Service |
|--------|------|------|----------------|
| `cmd/server` | REST API + curation layer | 8080 | `server` |
| `cmd/collector` | Persistent daemon, scheduler-based collection | None | `collector` |

Both binaries share the same Go module and `internal/` packages. Same PostgreSQL database.

### Docker Compose

```yaml
services:
  server:
    build:
      context: .
      target: server
    ports: ["8080:8080"]
    depends_on: [postgres]

  collector:
    build:
      context: .
      target: collector
    # No port exposure — DB access only
    depends_on: [postgres]

  postgres:
    image: pgvector/pgvector:pg16
    # pg_bigm extension installed via migration
```

### Dockerfile (multi-stage, multi-target)

```dockerfile
FROM golang:1.25-alpine AS builder
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .

FROM builder AS build-server
RUN go build -o /app/server ./cmd/server/

FROM builder AS build-collector
RUN go build -o /app/collector ./cmd/collector/

FROM alpine:3.21 AS server
COPY --from=build-server /app/server /app/server
COPY migrations/ /app/migrations/
ENTRYPOINT ["/app/server"]

FROM alpine:3.21 AS collector
COPY --from=build-collector /app/collector /app/collector
ENTRYPOINT ["/app/collector"]
```

---

## 2. Collector Design

### Interface

```go
// internal/collector/collector.go
type Collector interface {
    Name() string
    Collect(ctx context.Context) error
}
```

All source-specific collectors implement this interface. The collector daemon runs a scheduler that invokes each registered collector on its configured schedule.

### Scope

| Source | This Release | Status |
|--------|-------------|--------|
| Filesystem | ✅ | Migrate existing code |
| Google Drive | ✅ | Migrate existing code |
| Slack | ✅ | Migrate existing code |
| GitHub | ✅ | Migrate existing code |
| Discord | ❌ | Interface stub only |
| Telegram | ❌ | Interface stub only |
| Notion | ❌ | Interface stub only |

### Entry Point

`cmd/collector/main.go`:
- Load config (DB, collector credentials)
- Register enabled collectors
- Start scheduler daemon (cron-based, existing `robfig/cron` pattern)
- No HTTP server — no port exposure

### What Moves Out of cmd/server

- All collector registration and scheduling logic
- Collector-specific config parsing (SLACK_BOT_TOKEN, GITHUB_TOKEN, etc.)
- The scheduler package stays shared in `internal/scheduler/`

---

## 3. Curation Layer

### Search Modes

```
GET /api/v1/search?q=...&curated=false  →  Raw hybrid search results (current behavior)
GET /api/v1/search?q=...&curated=true   →  LLM re-ranked + lightly refined results
```

Default: `curated=false` (backward compatible).

### Curated Response Format

```json
{
  "results": [
    {
      "summary": "Light contextual summary (not aggressive compression)",
      "original": {
        "id": "uuid",
        "title": "Document title",
        "content": "Full original content preserved",
        "source": "slack",
        "source_url": "https://...",
        "collected_at": "2026-04-10T09:00:00Z"
      },
      "relevance": 0.95,
      "relevance_reason": "Directly addresses the query topic"
    }
  ],
  "count": 5,
  "query": "온보딩 가이드",
  "took_ms": 120
}
```

### Curation Rules

- Original data ALWAYS included — never lossy
- Summary is lightweight: preserves meaning, no aggressive compression
- LLM re-ranks by relevance to query and filters noise
- `relevance_reason` explains why the result is relevant (optional, for debugging)

### Implementation

New package: `internal/curation/`

```go
type Curator interface {
    Curate(ctx context.Context, query string, results []model.SearchResult) ([]CuratedResult, error)
}
```

Uses the existing LLM client (`internal/llm/`) to call the configured LLM endpoint. The curation prompt instructs the LLM to:
1. Re-rank results by relevance to the query
2. Generate a brief summary for each result (1-2 sentences)
3. Filter out clearly irrelevant results
4. Preserve all original data untouched

---

## 4. Korean Language Search

Triple-hybrid approach:

### 4.1 pg_bigm (2-gram partial matching)

New migration `006_bigm.sql`:

```sql
CREATE EXTENSION IF NOT EXISTS pg_bigm;

-- Documents table
CREATE INDEX idx_documents_content_bigm ON documents USING gin (content gin_bigm_ops);
CREATE INDEX idx_documents_title_bigm ON documents USING gin (title gin_bigm_ops);

-- Chunks table
CREATE INDEX idx_chunks_content_bigm ON chunks USING gin (content gin_bigm_ops);
```

pg_bigm handles Korean 조사/어미 variations without morphological analysis. Query "배포" matches "배포를 완료했습니다", "배포가 되었다", etc.

### 4.2 HyDE (LLM query expansion)

Existing implementation. LLM generates a hypothetical document matching the query, then embeds and searches for similar real documents.

### 4.3 pgvector (embedding similarity)

Existing implementation. 1536-dim OpenAI text-embedding-3-small vectors with HNSW index.

### Search Pipeline

```
Query → [pg_bigm partial match] ─┐
      → [HyDE → pgvector ANN]  ─┤→ RRF merge → (optional) LLM curation → Response
      → [tsvector FTS]          ─┘
```

The existing RRF (Reciprocal Rank Fusion) merge gains a third signal from pg_bigm scores.

---

## 5. Bot Removal

### Remove from cmd/server

- Discord bot response handler (mention-response RAG)
- Discord reaction feedback handler
- Discord bot token requirement from server config
- Discord-specific imports and goroutines

### Keep

- Discord as a collector interface stub (in collector binary, not server)
- Feedback table and endpoint (useful for non-Discord feedback)

### Config Changes

Remove from server:
- `DISCORD_BOT_TOKEN`
- `DISCORD_GUILD_ID`
- Discord-related LLM config (`LLM_API_URL`, `LLM_API_KEY`, `LLM_MODEL`)

LLM config moves to curation layer with new naming:
- `CURATION_LLM_URL`
- `CURATION_LLM_KEY`
- `CURATION_LLM_MODEL`

---

## 6. API Changes

### Modified Endpoints

| Endpoint | Change |
|----------|--------|
| `POST /api/v1/search` | Add `curated` query param (default: false) |
| `POST /api/v1/search` | Curated response includes `summary` + `original` fields |

### Removed Endpoints

| Endpoint | Reason |
|----------|--------|
| `POST /api/v1/collect/trigger` | Moved to collector daemon (or removed — collectors run on schedule) |
| `POST /api/v1/collect/slack/channel` | Moved to collector daemon |

### New Endpoints

| Endpoint | Description |
|----------|-------------|
| `GET /api/v1/search` | GET alias for search (AI agent convenience) |

### Unchanged Endpoints

`GET /health`, `GET /api/v1/documents`, `GET /api/v1/documents/{id}`, `GET /api/v1/documents/{id}/raw`, `GET /api/v1/sources`, `GET /api/v1/stats`, `GET /api/v1/stats/baseline`, `POST /api/v1/feedback`, `GET /api/v1/eval/export`

### TODO: GraphQL

Secondary API interface for complex queries. Not implemented in this release. Service layer is shared; only routing differs.

---

## 7. Document Updates

### Files to Update

| File | Changes |
|------|---------|
| `README.md` | Architecture diagram, project structure, API reference, env vars, quick start (docker-compose) |
| `README.en.md` | English mirror of README.md changes |
| `ARCHITECTURE.md` | System diagram, service layer map, collection pipeline, deployment architecture |
| `ARCHITECTURE.en.md` | English mirror |
| `EXPANSION.md` | Mark completed items, add GraphQL TODO |
| `docs/runbook-deploy.md` | Dual-binary docker deployment, new env vars |
| GitHub repo description | Update to reflect "LLM-curated private search engine" |

---

## 8. Migration Plan

### Phase 1: Infrastructure

1. Add migration `006_bigm.sql` (pg_bigm extension + indexes)
2. Create `cmd/collector/main.go` entry point
3. Update Dockerfile for multi-target build
4. Update `docker-compose.yml` for dual services

### Phase 2: Collector Separation

5. Define `Collector` interface in `internal/collector/collector.go`
6. Migrate existing collectors (filesystem, gdrive, slack, github) to interface
7. Add stubs for discord, telegram, notion
8. Move scheduler + collector registration to `cmd/collector/`
9. Remove collector logic from `cmd/server/`

### Phase 3: Curation Layer

10. Create `internal/curation/` package
11. Add `curated` parameter to search handler
12. Integrate pg_bigm into search query builder
13. Wire LLM curation with existing `internal/llm/` client

### Phase 4: Bot Removal

14. Remove Discord bot response code from server
15. Clean up Discord-specific config from server
16. Rename LLM config vars for curation

### Phase 5: Documentation

17. Update README.md, README.en.md
18. Update ARCHITECTURE.md, ARCHITECTURE.en.md
19. Update EXPANSION.md
20. Update docs/runbook-deploy.md
21. Update GitHub repo description

---

## 9. Out of Scope

- Discord/Telegram/Notion collector implementation (stubs only)
- GraphQL endpoint (TODO)
- Web frontend changes
- Kubernetes manifest updates (docker-compose first)
- Authentication/authorization changes
- New collector sources beyond existing 4
