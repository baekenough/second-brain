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
- **Korean Search** — pg_bigm 2-gram indexing + bigm_similarity ordering + HyDE query expansion for morphology-independent Korean search
- **Dual Binary** — API server and collector daemon run independently
- **5-lane Hybrid Search** — FTS (tsvector BM25) + pgvector cosine + pg_bigm + summary embedding + entity RRF for high recall and precision. Optional BGE cross-encoder rerank and HyDE query expansion
- **Rune-based Chunking** — Equal information density for Korean and English text (issue #145). Heading-aware, paragraph/sentence boundary splitting with overlap
- **Multi-source Collection** — Filesystem, Slack, GitHub, SMS/calls/recordings (Android), secretary SQLite, Gmail, Calendar, llm-memory, and more
- **Rich Format Extraction** — Automatic text extraction from HTML, PDF, DOCX, XLSX, PPTX, HWPX
- **Entity Extraction** — LLM-based knowledge graph entity (PERSON/ORG/CONCEPT/OTHER) extraction with integrated RRF search lane
- **Data Guard** — 3-layer soft-delete mass-deletion protection: filesystem root stat / 50% deletion-ratio guard / document.go empty no-op
- **Freshness Monitor** — collection_log staleness monitor + `GET /api/v1/collect/status` endpoint
- **Eval Self-improvement Loop** — Nightly eval-runner (03:00 KST), NDCG/MRR regression detection, webhook alerts, reindex recommendations
- **OpenAI-compatible Embeddings** — Accepts both a static API Key and ChatGPT Codex OAuth JWT (CliProxy) as Bearer tokens
- **SMS Stable source_id** — `sms:{ms}:{addrHash}:{direction}` format guarantees idempotent upsert (issue #144)
- **Soft Delete** — Removed documents are flagged rather than hard-deleted, preserving history
- **Migration Advisory Lock** — Prevents duplicate migration execution when multiple instances start simultaneously
- **Lightweight Images** — Server ~34.5 MB, Collector ~34.5 MB (alpine multi-stage)

---

## Architecture Overview

```mermaid
%%{init: {
  "theme": "base",
  "themeVariables": {
    "background": "#faf7f2",
    "primaryColor": "#faf7f2",
    "primaryTextColor": "#1c1917",
    "primaryBorderColor": "#1c1917",
    "lineColor": "#57534e",
    "secondaryColor": "#f0ebe3",
    "tertiaryColor": "#faf7f2",
    "fontFamily": "-apple-system, BlinkMacSystemFont, Geist, Inter, sans-serif",
    "fontSize": "13px"
  }
}}%%
flowchart LR
    classDef focal fill:#b5523a,stroke:#b5523a,color:#faf7f2,stroke-width:1px;
    classDef muted fill:#f0ebe3,stroke:#57534e,color:#57534e,stroke-width:1px;
    classDef data fill:#faf7f2,stroke:#1c1917,color:#1c1917,stroke-width:1.2px;
    classDef ext fill:#faf7f2,stroke:#57534e,color:#1c1917,stroke-width:1px,stroke-dasharray:4 3;

    Agent["AI Agent\n(MCP / HTTP)"]
    Server["second-brain\nAPI Server\nGo\n:8080"]
    MCP["MCP Server\n:8090"]
    Collector["second-brain\nCollector Daemon\nGo"]
    EvalRunner["eval-runner\nnightly 03:00 KST"]
    PG["PostgreSQL 16\n+ pgvector\n+ pg_bigm"]
    LLM["LLM / Ollama\n(curation + HyDE\n+ entity extract)"]
    OpenAI["OpenAI Embeddings\ntext-embedding-3-small\n1536 dim"]
    Whisper["Whisper\nASR :8000"]
    Diarization["diarization\nspeaker sep :8001"]
    Sources["Collection Sources\nFilesystem / Slack / GitHub\nSMS / Calls / Recordings\nGmail / Calendar / llm-memory"]

    Agent -->|REST /api/v1| Server
    Agent -->|MCP streamable HTTP| MCP
    Server -->|SQL + pgvector| PG
    Server -->|curation| LLM
    MCP -->|SQL + pgvector| PG
    Collector -->|SQL + pgvector| PG
    Collector -->|POST /embeddings| OpenAI
    Collector -->|collect| Sources
    Collector -->|audio files| Whisper
    Collector -->|diarize| Diarization
    EvalRunner -->|eval query| PG
    EvalRunner -->|regression webhook| LLM

    class Server,Collector focal;
    class PG data;
    class LLM,OpenAI,Whisper,Diarization,Sources ext;
    class Agent,MCP,EvalRunner muted;
```

![System Runtime Topology](docs/diagrams/01-system-runtime-topology.png)

The server (API) and collector are separate binaries. Each collector runs on a configurable per-source `COLLECT_INTERVAL`. Collected text is split by a rune-based chunker, then embedded and stored in a `pgvector` column. Production runs on **Mac mini docker-compose** (`docker-compose.local.yml`); `deploy/k8s/` is the future Kubernetes target.

### Components

| Component | Image | Base | UID |
|-----------|-------|------|-----|
| second-brain server (API) | `second-brain-server:local` | golang:alpine → alpine | 10001 |
| second-brain collector (daemon) | `second-brain-collector:local` | golang:alpine → alpine | 10001 |
| second-brain mcp (MCP server) | `second-brain-mcp:local` | golang:alpine → alpine | 10001 |
| second-brain eval (nightly eval) | `second-brain-eval:local` | golang:alpine → alpine | — |
| postgres | `second-brain-postgres:local` | pgvector/pgvector:pg16 + pg_bigm | — |
| ollama | `ollama/ollama:latest` | — | — |
| whisper | `fedirz/faster-whisper-server:latest-cpu` | faster-whisper (int8 CPU) | — |
| diarization | `second-brain-diarization:local` | pyannote.audio | — |
| web | `second-brain-web:local` | node:alpine (Next.js standalone) | 10001 |

---

## Quick Start

```bash
# 1. Generate .env with the setup wizard (recommended)
go run -tags setup ./cmd/collector/ setup

# Or configure manually
cp .env.local.example .env.local
# Edit .env.local with required values

# 2. Start services
docker compose -f docker-compose.local.yml up -d --build

# 3. Health check
curl http://localhost:8081/health

# 4. Test search
curl -X POST http://localhost:8081/api/v1/search \
  -H "Content-Type: application/json" \
  -d '{"query": "onboarding guide", "limit": 5}'

# 5. Check collection freshness
curl http://localhost:8081/api/v1/collect/status
```

> **Note**: The setup wizard requires the `-tags setup` build tag. Production Docker images do not include the wizard.

---

## Project Structure

```
second-brain/
├── cmd/
│   ├── server/
│   │   └── main.go              # API server entry point (port 8080)
│   ├── collector/
│   │   └── main.go              # Collector daemon entry point
│   ├── mcp/
│   │   └── main.go              # MCP streamable HTTP server (port 8090)
│   └── eval/
│       └── main.go              # Nightly eval binary
├── internal/
│   ├── api/                     # HTTP handlers + router
│   │   ├── collect_status.go    # GET /api/v1/collect/status + FreshnessChecker
│   │   ├── reindex_check.go     # GET /api/v1/reindex/check
│   │   └── search.go            # POST|GET /api/v1/search (HyDE, rerank options)
│   ├── chunker/                 # Rune-based text chunking (#145)
│   │   ├── chunker.go           # Options{TargetSize, MaxSize, Overlap, HeadingAware}
│   │   └── adaptive.go          # SourceType-aware Options selection
│   ├── collector/
│   │   ├── extractor/           # File format extractors
│   │   ├── filesystem.go        # Local filesystem collector
│   │   ├── slack.go             # Slack collector (public channels only)
│   │   ├── github.go            # GitHub collector
│   │   ├── sms.go               # SMS + call-log collector (Android push)
│   │   ├── recording.go         # Call recording collector (Whisper ASR)
│   │   ├── secretary.go         # secretary SQLite collector
│   │   ├── gmail.go             # Gmail collector
│   │   ├── calendar.go          # Calendar collector
│   │   └── llm_memory.go        # llm-memory SQLite collector
│   ├── config/
│   │   └── config.go            # Environment variable parsing
│   ├── curation/                # LLM curation layer
│   ├── llm/                     # LLM client (HyDE, entity extraction)
│   ├── search/
│   │   ├── search.go            # Service.Search() — HyDE, rerank, entity integration
│   │   ├── embed.go             # EmbedClient (staticToken / cliProxyToken)
│   │   ├── rerank.go            # HTTPReranker (BGE cross-encoder)
│   │   ├── hyde.go              # HyDE query expansion
│   │   └── tune.go              # Hybrid search weight tuning
│   ├── store/
│   │   ├── document.go          # DocumentStore — 5-lane hybridSearch CTE
│   │   ├── collection_status.go # CollectionStatus + FreshnessChecker
│   │   ├── entities.go          # EntityStore — upsert, link, fetch
│   │   └── postgres.go          # pgx/v5 connection + advisory lock migrations
│   ├── model/                   # Document, SearchQuery, Entity structs
│   ├── scheduler/               # Periodic collection scheduler + deletion guard
│   └── worker/                  # Async embedding backfill worker
├── migrations/                  # SQL migrations 001–019 (auto-applied on startup)
├── deploy/
│   ├── k8s/                     # Kustomize manifests (future Kubernetes target)
│   ├── postgres/                # Custom PostgreSQL image with pg_bigm
│   └── diarization/             # pyannote.audio diarization server
├── web/                         # Next.js frontend
├── mobile/second-brain-push/    # Android collector app (SMS/calls/recordings)
├── docker-compose.local.yml     # Production Compose (Mac mini)
├── Dockerfile                   # Multi-target build (server/collector/mcp/eval)
└── go.mod                       # Go module definition
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
| `GET` | `/api/v1/stats/baseline` | Document count, chunk count, p50/p95 statistics |
| `GET` | `/api/v1/collect/status` | Per-source collection freshness (last_success_at, stale_seconds) |
| `POST` | `/api/v1/collect/trigger` | Manual collection trigger |
| `POST` | `/api/v1/ingest/messages` | Receive SMS + call-log JSON batch (Android app) |
| `POST` | `/api/v1/ingest/recording` | Receive call recording `.m4a` multipart (Android app) |

---

### GET /health

```bash
curl http://localhost:8081/health
```
```json
{"status":"ok"}
```

---

### POST /api/v1/search

JSON body-based 5-lane hybrid search. Combines FTS (tsvector BM25) + pgvector cosine + pg_bigm + summary embedding + entity via RRF.

![Hybrid Search RRF](docs/diagrams/05-hybrid-search-rrf.png)

**Request Body**

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `query` | string | required | Search query |
| `source_type` | string | (all) | Filter by source: `filesystem` \| `slack` \| `github` \| `sms` |
| `exclude_source_types` | []string | — | Sources to exclude |
| `limit` | int | 20 | Maximum results to return |
| `sort` | string | `"relevance"` | `"relevance"` (RRF score desc) \| `"recent"` (collected_at desc) |
| `include_deleted` | bool | `false` | Include soft-deleted documents |
| `curated` | bool | `false` | Enable LLM curation (re-ranking + summary) |
| `use_hyde` | bool | `false` | Enable HyDE query expansion (~1-3s additional latency) |
| `use_rerank` | bool | `false` | Enable BGE cross-encoder reranking |

```bash
curl -X POST http://localhost:8081/api/v1/search \
  -H "Content-Type: application/json" \
  -d '{"query": "onboarding guide", "limit": 5, "use_rerank": true}'
```

```json
{
  "results": [
    {
      "id": "a1b2c3d4-e5f6-7890-abcd-ef1234567890",
      "title": "New Employee Onboarding Guide.docx",
      "content": "During your first week ...",
      "source_type": "filesystem",
      "source_id": "HR/New Employee Onboarding Guide.docx",
      "match_type": "hybrid",
      "score": 0.0312,
      "collected_at": "2026-04-10T09:00:00Z"
    }
  ],
  "count": 1,
  "total": 1,
  "query": "onboarding guide",
  "took_ms": 42
}
```

---

### GET /api/v1/collect/status

Returns per-source collection freshness derived from the collection_log table.

```bash
curl http://localhost:8081/api/v1/collect/status
```

```json
{
  "sources": [
    {
      "source_type": "filesystem",
      "last_success_at": "2026-06-13T03:00:00Z",
      "last_attempt_at": "2026-06-13T03:00:01Z",
      "consecutive_failures": 0,
      "total_runs": 1240,
      "stale_seconds": 3600.5
    }
  ]
}
```

---

### GET /api/v1/documents

| Query Parameter | Type | Default | Description |
|-----------------|------|---------|-------------|
| `limit` | int | 20 | Max 100 |
| `offset` | int | 0 | Pagination offset |
| `source` | string | (all) | `filesystem` \| `slack` \| `github` etc. |

---

### POST /api/v1/ingest/messages

The Android second-brain-push app sends SMS and call logs as a JSON batch. Source ID format: `sms:{dateMs}:{addrHash}:{direction}`.

---

## Environment Variables

Key environment variables based on `internal/config/config.go`.

### Server

| Key | Default | Description |
|-----|---------|-------------|
| `DATABASE_URL` | `postgres://brain:brain@localhost:5432/second_brain?sslmode=disable` | PostgreSQL connection string |
| `PORT` | `8080` | HTTP server port |
| `EMBEDDING_API_URL` | `https://api.openai.com/v1` | OpenAI-compatible embeddings endpoint |
| `EMBEDDING_MODEL` | `text-embedding-3-small` | Embedding model (1536 dimensions) |
| `EMBEDDING_API_KEY` | — | Static Bearer token. Mutually exclusive with `CLIPROXY_AUTH_FILE` |
| `CLIPROXY_AUTH_FILE` | — | CliProxy OAuth JSON path. Auto-refreshes with 5-minute TTL |
| `LLM_API_URL` | — | LLM endpoint for curation, HyDE, and entity extraction |
| `LLM_API_KEY` | — | LLM API key |
| `LLM_MODEL` | — | LLM model identifier |
| `API_KEY` | — | API authentication Bearer token |
| `ALERT_WEBHOOK_URL` | — | Slack webhook URL for collection freshness alerts |
| `RERANK_API_URL` | — | BGE cross-encoder reranking endpoint |
| `ENTITY_EXTRACTION_ENABLED` | `false` | Enable entity extraction + search lane |

### Collector

| Key | Default | Description |
|-----|---------|-------------|
| `DATABASE_URL` | (same as above) | PostgreSQL connection string |
| `COLLECT_INTERVAL` | `1h` | Default collection interval (overridable per source) |
| `MAX_EMBED_CHARS` | `8000` | Maximum characters for embedding input |
| `FILESYSTEM_PATH` | — | Root directory to collect from |
| `FILESYSTEM_ENABLED` | `false` | Set to `true` to activate the filesystem collector |
| `SLACK_BOT_TOKEN` | — | Slack Bot OAuth Token (`xoxb-...`) |
| `SLACK_TEAM_ID` | — | Slack Workspace ID |
| `GITHUB_TOKEN` | — | GitHub Personal Access Token |
| `GITHUB_ORG` | — | GitHub organization name |
| `GDRIVE_CREDENTIALS_JSON` | — | Google ADC JSON path |
| `DIARIZATION_API_URL` | — | pyannote.audio speaker diarization server URL |
| `DIARIZATION_ENABLED` | `false` | Enable speaker diarization |
| `ENTITY_EXTRACTION_ENABLED` | `false` | Enable entity extraction |

> When both `EMBEDDING_API_KEY` and `CLIPROXY_AUTH_FILE` are set, `CLIPROXY_AUTH_FILE` takes precedence.

---

## Collector Status

| Source | Active When | Implementation | Notes |
|--------|-------------|----------------|-------|
| filesystem | `FILESYSTEM_PATH` + `FILESYSTEM_ENABLED=true` | Fully operational | 4,150+ documents verified |
| slack | `SLACK_BOT_TOKEN` set | Complete | Public channels only; DMs automatically excluded |
| github | `GITHUB_TOKEN` + `GITHUB_ORG` set | Complete | Issues + PRs |
| sms / call | Android app + server URL/token configured | Fully operational | Galaxy Z Flip6 live-tested. App: [`mobile/second-brain-push/`](mobile/second-brain-push/README.md) |
| recording | Same as above + Whisper server | Fully operational | Whisper ASR → text transcription |
| secretary | `SECRETARY_DB_HOST_PATH` mounted | Fully operational | secretary SQLite read-only mount |
| gmail | `/data/gmail` mounted | Fully operational | Gmail export via secretary |
| calendar | `/data/calendar` mounted | Fully operational | Calendar export via secretary |
| llm-memory | `LLM_MEMORY_DB_HOST_PATH` mounted | Fully operational | llm-memory SQLite read-only mount |
| gdrive (export) | `GDRIVE_CREDENTIALS_JSON` set | Scaffold only | Requires ADC; disabled by default |
| notion | — | Removed | Deregistered from main.go |

### File Extractors (`internal/collector/extractor/`)

| Format | Library | Notes |
|--------|---------|-------|
| HTML | `golang.org/x/net/html` | Tag stripping |
| PDF | `ledongthuc/pdf` | 10-second timeout; NUL byte sanitization |
| DOCX | OOXML unzip | Parses `word/document.xml` |
| XLSX | `github.com/xuri/excelize/v2` | TSV output; `##SHEET {name}` headers; 200 KiB cap |
| PPTX | OOXML unzip | Parses `ppt/slides/*.xml` |
| HWPX | OWPML unzip | Parses `Contents/section*.xml` `<hp:t>` nodes |
| All | `SanitizeText` | 0x00 removal + UTF-8 validation + whitespace compression |

---

## Operations

### Service Status

```bash
docker compose -f docker-compose.local.yml ps
docker compose -f docker-compose.local.yml logs server --tail=100 -f
docker compose -f docker-compose.local.yml logs collector --tail=100 -f
```

### Check Collection Freshness

```bash
curl http://localhost:8081/api/v1/collect/status | jq '.sources[] | {source_type, consecutive_failures, stale_seconds}'
```

### Direct Database Access

```bash
docker compose -f docker-compose.local.yml exec postgres psql -U brain -d second_brain
```

```sql
-- Document count by source
SELECT source_type, COUNT(*) FROM documents GROUP BY source_type;

-- 10 most recently collected documents
SELECT title, source_type, collected_at FROM documents ORDER BY collected_at DESC LIMIT 10;

-- Active documents missing embeddings
SELECT COUNT(*) FROM documents WHERE embedding IS NULL AND status = 'active';

-- Recent collection log entries
SELECT source_type, started_at, documents_count, error FROM collection_log ORDER BY started_at DESC LIMIT 20;
```

### Rebuild Images

```bash
docker compose -f docker-compose.local.yml build --no-cache
docker compose -f docker-compose.local.yml up -d
```

### Initial Ollama Model Download

```bash
docker exec second-brain-ollama ollama pull gemma3:12b-it-qat
```

---

## Contributor Setup

Run once per clone if you have push access:

```bash
make install-hooks
```

This tells git to use the pre-push hook in `.githooks/`. On every push, the following run automatically:

- `scripts/ci-checks.sh` (secret guard, discord placeholder, .env tracking detection, etc.)
- `go vet` / `go build` / `go test`

To run the same checks locally without pushing:

```bash
make check
```

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

Migration files live in `migrations/` (001–019) and are applied in order under an advisory lock when the server starts. Already-applied migrations are not re-run.

---

## Known Issues

| Symptom | Cause | Workaround |
|---------|-------|------------|
| Some files skipped during minikube mount collection | 9p mount raises `lstat: file name too long` for Korean filenames exceeding 255 bytes | Shorten filenames to under 255 bytes |
| Slack/GitHub collectors log ERROR then skip | Credential environment variables not set | Set the relevant `*_TOKEN` / `*_ORG` variables |
| gdrive collector not active | `GDRIVE_CREDENTIALS_JSON` unset disables it | Provide ADC credentials to enable |
| Ollama slow on first start | gemma3:12b-it-qat CPU inference on macOS arm64 | Pull model once; subsequent runs reuse cached weights |

---

## Related Documentation

- [`ARCHITECTURE.md`](ARCHITECTURE.md) — Detailed architecture reference (Korean)
- [`ARCHITECTURE.en.md`](ARCHITECTURE.en.md) — Detailed architecture reference (English)
- [`docs/runbook-deploy.md`](docs/runbook-deploy.md) — Deployment runbook
- [`guides/`](guides/) — Operations and development guides

---

## License

This project is distributed under the [MIT License](LICENSE). See [CONTRIBUTE.md](CONTRIBUTE.md) for contribution guidelines.

---

Last updated: 2026-06-13 (v0.20.4)
