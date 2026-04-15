# second-brain

LLM-curated private search engine. Collects and embeds knowledge from diverse sources, delivering curated search results to AI agents.

> Korean: [README.md](README.md)

---

## Table of Contents

1. [Features](#features)
2. [Architecture Overview](#architecture-overview)
3. [Quick Start](#quick-start)
4. [Project Structure](#project-structure)
5. [API Reference](#api-reference)
6. [Environment Variables](#environment-variables)
7. [Collector Status](#collector-status)
8. [Operations](#operations)
9. [Development](#development)
10. [Known Issues](#known-issues)
11. [Related Documentation](#related-documentation)
12. [License](#license)

---

## Features

- **LLM Curation** — LLM re-ranks search results and generates lightweight summaries. Raw data is always included
- **Korean Search** — pg_bigm 2-gram indexing + HyDE query expansion for morphology-independent Korean search
- **Dual Binary** — API server and collector daemon run independently
- **Hybrid Search** — Combines BM25 full-text search (`ts_rank_cd`) and pgvector cosine similarity via Reciprocal Rank Fusion (RRF) for high recall and precision
- **Multi-source Collection** — Filesystem, Slack, GitHub, and Google Drive (Export) collectors with automatic scheduling and periodic refresh
- **Rich Format Extraction** — Automatic text extraction from HTML, PDF, DOCX, XLSX, PPTX, and plain text
- **OpenAI-compatible Embeddings** — Accepts both a static API Key and ChatGPT Codex OAuth JWT (CliProxy) as Bearer tokens
- **Soft Delete** — Removed documents are flagged rather than hard-deleted, preserving history
- **Lightweight Images** — Server ~34.5 MB, Collector ~34.5 MB (alpine multi-stage)

---

## Architecture Overview

```mermaid
flowchart LR
    Agent["AI Agent\n(MCP / HTTP)"]
    Server["second-brain\nAPI Server\nGo 1.25\n:8080"]
    Collector["second-brain\nCollector Daemon\nGo 1.25"]
    PG["PostgreSQL 16\n+ pgvector\n+ pg_bigm"]
    LLM["LLM API\n(curation)"]
    OpenAI["OpenAI Embeddings\ntext-embedding-3-small\n1536 dim"]
    FS["Filesystem\n(Google Drive mount)"]
    Slack["Slack API"]
    GH["GitHub API"]
    GD["Google Drive\nExport"]

    Agent -->|REST /api/v1| Server
    Server -->|SQL + pgvector| PG
    Server -->|curated search| LLM
    Collector -->|SQL + pgvector| PG
    Collector -->|POST /embeddings| OpenAI
    Collector -->|collect| FS
    Collector -->|collect| Slack
    Collector -->|collect| GH
    Collector -->|collect| GD
```

The server (API) and collector are separate binaries. The collector runs on a configurable schedule (default: 1 hour). Collected text is truncated to a maximum of 8,000 characters before embedding, then stored in a `pgvector` column.

### Components

| Component | Image | Base | Size | UID |
|-----------|-------|------|------|-----|
| second-brain server (API) | `second-brain:dev` | golang:1.24-alpine → alpine:3.21 | ~34.5 MB | 10001 |
| second-brain collector (daemon) | `second-brain-collector:dev` | golang:1.24-alpine → alpine:3.21 | ~34.5 MB | 10001 |
| postgres | `pgvector/pgvector:pg16` | PostgreSQL 16 + pgvector + pg_bigm | — | — |

---

## Quick Start

```bash
# 1. Set up environment variables
cp .env.example .env
# Edit .env with required values (DATABASE_URL, EMBEDDING_API_KEY, etc.)

# 2. Start services
docker compose up -d

# 3. Health check
curl http://localhost:8080/health

# 4. Test search
curl -X POST http://localhost:8080/api/v1/search \
  -H "Content-Type: application/json" \
  -d '{"query": "onboarding guide", "limit": 5}'
```

---

## Project Structure

```
second-brain/
├── cmd/
│   ├── server/
│   │   └── main.go              # API server entry point (port 8080)
│   └── collector/
│       └── main.go              # Collector daemon entry point
├── internal/
│   ├── collector/
│   │   ├── extractor/           # File format extractors
│   │   │   ├── extractor.go     # Interface + SanitizeText
│   │   │   ├── html.go          # x/net/html tag stripping
│   │   │   ├── pdf.go           # ledongthuc/pdf, 10s timeout
│   │   │   ├── docx.go          # OOXML word/document.xml
│   │   │   ├── xlsx.go          # excelize/v2 TSV, 200 KiB cap
│   │   │   └── pptx.go          # OOXML ppt/slides
│   │   ├── filesystem.go        # Local filesystem collector
│   │   ├── slack.go             # Slack collector (public channels only)
│   │   ├── github.go            # GitHub collector
│   │   └── gdrive_export.go     # Google Drive Export collector
│   ├── config/
│   │   └── config.go            # Environment variable parsing
│   ├── curation/                # LLM curation layer
│   ├── db/                      # pgvector init, migrations
│   ├── embedding/               # OpenAI-compatible embedding client
│   ├── handler/                 # HTTP handlers (search, documents, sources)
│   ├── model/                   # Document, SearchResult structs
│   └── scheduler/               # Periodic collection scheduler (mutex dedup)
├── migrations/                  # SQL migration files (auto-applied on startup)
├── deploy/
│   └── k8s/                     # Kustomize manifests
├── Dockerfile                   # Multi-target build (server + collector)
├── docker-compose.yml           # Local development Compose
└── go.mod                       # Go 1.25 module definition
```

---

## API Reference

All endpoints use the `/api/v1` prefix. The single exception is `/health`.

### Endpoint Summary

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/health` | Health check |
| `GET` | `/api/v1/search` | Hybrid search (GET, query parameters) |
| `POST` | `/api/v1/search` | Hybrid search (POST, JSON body) |
| `GET` | `/api/v1/documents` | Paginated document list |
| `GET` | `/api/v1/documents/{id}` | Single document detail |
| `GET` | `/api/v1/documents/{id}/raw` | Raw file streaming (filesystem only, 50 MiB limit) |
| `GET` | `/api/v1/sources` | Registered collector list |

---

### GET /health

Returns 200 when the server is running.

```bash
curl http://localhost:8080/health
```

```json
{"status":"ok"}
```

---

### GET /api/v1/search

Query parameter-based hybrid search.

| Query Parameter | Type | Default | Description |
|-----------------|------|---------|-------------|
| `q` | string | required | Search query |
| `source_type` | string | (all) | Filter by source: `filesystem` \| `slack` \| `github` |
| `limit` | int | 10 | Maximum results to return |
| `curated` | bool | `false` | Enable LLM curation (re-ranking + summary) |

```bash
curl "http://localhost:8080/api/v1/search?q=onboarding+guide&limit=5&curated=true"
```

---

### POST /api/v1/search

JSON body-based hybrid search combining BM25 full-text (`ts_rank_cd`) and pgvector cosine (`<=>`) scores via RRF.

**Request Body**

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `query` | string | required | Search query |
| `source_type` | string | (all) | Filter by source: `filesystem` \| `slack` \| `github` |
| `limit` | int | 10 | Maximum results to return |
| `sort` | string | `"relevance"` | `"relevance"` (RRF score desc) \| `"recent"` (collected_at desc) |
| `include_deleted` | bool | `false` | Include soft-deleted documents |
| `curated` | bool | `false` | Enable LLM curation (re-ranking + summary) |

```bash
curl -X POST http://localhost:8080/api/v1/search \
  -H "Content-Type: application/json" \
  -d '{"query": "onboarding guide", "limit": 5, "sort": "relevance", "curated": true}'
```

```json
{
  "results": [
    {
      "id": "a1b2c3d4-e5f6-7890-abcd-ef1234567890",
      "title": "New Employee Onboarding Guide.docx",
      "content": "During your first week ...",
      "source": "filesystem",
      "source_url": "/data/drive/HR/New Employee Onboarding Guide.docx",
      "collected_at": "2026-04-10T09:00:00Z",
      "score": 0.0312
    }
  ],
  "count": 1,
  "total": 1,
  "query": "onboarding guide",
  "took_ms": 42
}
```

---

### GET /api/v1/documents

| Query Parameter | Type | Default | Description |
|-----------------|------|---------|-------------|
| `limit` | int | 20 | Max 100 |
| `offset` | int | 0 | Pagination offset |
| `source` | string | (all) | `filesystem` \| `slack` \| `github` |

```bash
curl "http://localhost:8080/api/v1/documents?limit=5&offset=0&source=filesystem"
```

```json
{
  "documents": [
    {
      "id": "a1b2c3d4-e5f6-7890-abcd-ef1234567890",
      "title": "README.md",
      "source": "filesystem",
      "source_url": "/data/drive/README.md",
      "collected_at": "2026-04-10T09:00:00Z",
      "updated_at": "2026-04-12T15:30:00Z"
    }
  ]
}
```

---

### GET /api/v1/documents/{id}

Returns the full metadata and content of a single document as JSON.

```bash
curl http://localhost:8080/api/v1/documents/a1b2c3d4-e5f6-7890-abcd-ef1234567890
```

---

### GET /api/v1/documents/{id}/raw

Streams the raw file bytes. Available for filesystem sources only. `Content-Type` is set automatically based on the file extension. Files exceeding 50 MiB return 413.

```bash
# Download file
curl -O -J http://localhost:8080/api/v1/documents/a1b2c3d4-e5f6-7890-abcd-ef1234567890/raw

# Open inline in browser (images, PDFs, etc.)
open "http://localhost:8080/api/v1/documents/a1b2c3d4-e5f6-7890-abcd-ef1234567890/raw"
```

---

### GET /api/v1/sources

Returns the list of registered collectors and their status.

```bash
curl http://localhost:8080/api/v1/sources
```

---

## Environment Variables

Full list based on `internal/config/config.go`.

### Server Environment Variables

| Key | Default | Description |
|-----|---------|-------------|
| `DATABASE_URL` | `postgres://brain:brain@localhost:5432/second_brain?sslmode=disable` | PostgreSQL connection string |
| `PORT` | `8080` | HTTP server port |
| `EMBEDDING_API_URL` | `https://api.openai.com/v1` | OpenAI-compatible embeddings endpoint |
| `EMBEDDING_MODEL` | `text-embedding-3-small` | Embedding model (1536 dimensions) |
| `EMBEDDING_API_KEY` | — | Static Bearer token. Mutually exclusive with `CLIPROXY_AUTH_FILE` |
| `CLIPROXY_AUTH_FILE` | — | Path to CliProxy OAuth JSON. Reads `access_token` with 5-minute TTL auto-refresh |
| `LLM_API_URL` | — | LLM curation chat completions endpoint |
| `LLM_API_KEY` | — | LLM API key |
| `LLM_MODEL` | — | LLM model identifier |
| `MIGRATIONS_DIR` | runtime.Caller fallback | In Docker images: `/app/migrations` |
| `API_KEY` | — | API authentication Bearer token |

### Collector Environment Variables

| Key | Default | Description |
|-----|---------|-------------|
| `DATABASE_URL` | (same as above) | PostgreSQL connection string |
| `COLLECT_INTERVAL` | `1h` | Collector schedule interval (Go duration format) |
| `MAX_EMBED_CHARS` | `8000` | Maximum characters sent to the embedding API. Excess is truncated with a WARN log |
| `EMBEDDING_API_URL` | `https://api.openai.com/v1` | OpenAI-compatible embeddings endpoint |
| `EMBEDDING_MODEL` | `text-embedding-3-small` | Embedding model |
| `EMBEDDING_API_KEY` | — | Static Bearer token |
| `CLIPROXY_AUTH_FILE` | — | Path to CliProxy OAuth JSON |
| `FILESYSTEM_PATH` | — | Root directory to collect from. In-cluster path: `/data/drive` |
| `FILESYSTEM_ENABLED` | `false` | Set to `true` to register the filesystem collector |
| `SLACK_BOT_TOKEN` | — | Slack Bot OAuth Token (`xoxb-...`) |
| `SLACK_TEAM_ID` | — | Slack Workspace ID |
| `GITHUB_TOKEN` | — | GitHub Personal Access Token |
| `GITHUB_ORG` | — | GitHub organization name |
| `GDRIVE_CREDENTIALS_JSON` | — | Google ADC JSON path. If unset, the gdrive collector is disabled |

> When both `EMBEDDING_API_KEY` and `CLIPROXY_AUTH_FILE` are set, `CLIPROXY_AUTH_FILE` takes precedence. Set only one.

---

## Collector Status

| Source | Active When | Implementation | Notes |
|--------|-------------|----------------|-------|
| filesystem | `FILESYSTEM_PATH` set + `FILESYSTEM_ENABLED=true` | Fully operational | 4,150+ documents verified |
| slack | `SLACK_BOT_TOKEN` set | Complete | Public channels only; DMs automatically excluded. ERROR then skip if unset |
| github | `GITHUB_TOKEN` + `GITHUB_ORG` set | Complete | ERROR then skip if unset |
| gdrive (export) | `GDRIVE_CREDENTIALS_JSON` set | Scaffold only | Requires ADC; disabled by default |
| notion | — | Removed | Deregistered from main.go |

### File Extractors (`internal/collector/extractor/`)

| Format | Library | Notes |
|--------|---------|-------|
| HTML | `golang.org/x/net/html` | HTML tag stripping |
| PDF | `ledongthuc/pdf` | 10-second timeout; NUL byte sanitization |
| DOCX | OOXML unzip | Parses `word/document.xml` |
| XLSX | `github.com/xuri/excelize/v2` | TSV output; `##SHEET {name}` header + tab-separated rows; 200 KiB cap |
| PPTX | OOXML unzip | Parses `ppt/slides/*.xml` |
| All | `SanitizeText` | 0x00 removal + UTF-8 validation + whitespace compression |

---

## Operations

### Service Status

```bash
docker compose ps
docker compose logs server --tail=100 -f
docker compose logs collector --tail=100 -f
```

### Direct Database Access

```bash
docker compose exec postgres psql -U brain -d second_brain
```

```sql
-- Document count by source
SELECT source, COUNT(*) FROM documents GROUP BY source;

-- 10 most recently collected documents
SELECT title, source, collected_at FROM documents ORDER BY collected_at DESC LIMIT 10;

-- Documents missing embeddings (excluded from vector search)
SELECT COUNT(*) FROM documents WHERE embedding IS NULL;
```

### Rebuild Images

```bash
docker compose build --no-cache
docker compose up -d
```

### Rotate CliProxy OAuth Secret

When the token expires or `auth.json` is replaced, restart the Collector.

---

## Development

### Prerequisites

- Go 1.25+
- PostgreSQL 16 + pgvector + pg_bigm extensions
- Docker / docker compose

### Run Backend Locally

```bash
export DATABASE_URL="postgres://brain:brain@localhost:5432/second_brain?sslmode=disable"
export EMBEDDING_API_KEY="sk-..."

# Start API server; migrations are applied automatically
go run ./cmd/server/

# Start collector daemon (separate terminal)
export FILESYSTEM_PATH="/path/to/docs"
export FILESYSTEM_ENABLED=true
go run ./cmd/collector/
```

### Backend Tests and Linting

```bash
go test ./...
go test -race ./...
go vet ./...
gofmt -w .
```

### Migrations

Migration files live in `migrations/` and are applied automatically in order when the server starts. Already-applied migrations are not re-run.

---

## Known Issues

| ID | Symptom | Cause | Workaround |
|----|---------|-------|------------|
| BUG-007 | Some files skipped during minikube mount collection | 9p mount raises `lstat: file name too long` for Korean filenames exceeding 255 bytes | Shorten filenames to under 255 bytes |
| — | Embedding failures when `cliproxy-auth-secret` is missing | Out-of-band Secret excluded from git | Run `kubectl create secret --from-file=auth.json=~/.cli-proxy-api/codex-*.json` manually on each deployment |
| — | Slack/GitHub collectors log ERROR then skip | Credential environment variables not set | Set the relevant `*_TOKEN` / `*_ORG` variables. Other collectors continue to run normally |
| — | Documents longer than 8,000 characters are embedding-truncated | `MAX_EMBED_CHARS` default of 8,000 | Increase `MAX_EMBED_CHARS`. Intended as a temporary measure until the Phase 1 chunks table is introduced |
| — | gdrive collector not active | `GDRIVE_CREDENTIALS_JSON` unset disables it by default | Provide ADC credentials to enable (currently scaffold-stage implementation) |

---

## Related Documentation

- [`ARCHITECTURE.md`](ARCHITECTURE.md) — Detailed architecture reference
- [`EXPANSION.md`](EXPANSION.md) — Expansion plans
- [`docs/runbook-deploy.md`](docs/runbook-deploy.md) — Deployment runbook
- [`guides/`](guides/) — Operations and development guides

---

## License

No license file is present in this repository. Contact the project maintainer before using or distributing this code.

---

Last updated: 2026-04-15
