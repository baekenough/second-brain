# second-brain

An in-house AI search engine that brings all team knowledge into one place. Collects documents scattered across Google Drive, Slack, GitHub, and the local filesystem, then provides hybrid vector + full-text search powered by pgvector and PostgreSQL.

> Korean: [README.md](README.md)

---

## Table of Contents

1. [Features](#features)
2. [Architecture Overview](#architecture-overview)
3. [Quick Start (minikube)](#quick-start-minikube)
4. [Project Structure](#project-structure)
5. [API Reference](#api-reference)
6. [Environment Variables](#environment-variables)
7. [Collector Status](#collector-status)
8. [Web UI Guide](#web-ui-guide)
9. [Operations](#operations)
10. [Development](#development)
11. [Known Issues](#known-issues)
12. [Related Documentation](#related-documentation)
13. [License](#license)

---

## Features

- **Hybrid Search** — Combines BM25 full-text search (`ts_rank_cd`) and pgvector cosine similarity via Reciprocal Rank Fusion (RRF) for high recall and precision
- **Multi-source Collection** — Filesystem, Slack, GitHub, and Google Drive (Export) collectors with automatic scheduling and periodic refresh
- **Rich Format Extraction** — Automatic text extraction from HTML, PDF, DOCX, XLSX, PPTX, and plain text
- **OpenAI-compatible Embeddings** — Accepts both a static API Key and ChatGPT Codex OAuth JWT (CliProxy) as Bearer tokens
- **Soft Delete** — Removed documents are flagged rather than hard-deleted, preserving history
- **Next.js Web UI** — Search, filter, sort, and format-aware rendering (Markdown prose, XLSX tables, syntax-highlighted code)
- **Lightweight Images** — Backend ~34.5 MB (alpine multi-stage), Frontend ~195 MB (Next.js standalone)
- **Kubernetes Deployment** — Declarative deployment to a local minikube cluster via Kustomize

---

## Architecture Overview

```mermaid
flowchart LR
    Browser["Browser\n(localhost:3000)"]
    Web["vibers-web\nNext.js 16\n:3000"]
    Backend["second-brain\nGo 1.25\n:9200"]
    PG["PostgreSQL 16\n+ pgvector"]
    OpenAI["OpenAI Embeddings\ntext-embedding-3-small\n1536 dim"]
    FS["Filesystem\n(Google Drive mount)"]
    Slack["Slack API"]
    GH["GitHub API"]
    GD["Google Drive\nExport"]

    Browser -->|HTTP| Web
    Web -->|REST /api/v1| Backend
    Backend -->|SQL + pgvector| PG
    Backend -->|POST /embeddings| OpenAI
    Backend -->|collect| FS
    Backend -->|collect| Slack
    Backend -->|collect| GH
    Backend -->|collect| GD
```

The scheduler runs all active collectors on a configurable interval (default: 1 hour). Collected text is truncated to a maximum of 8,000 characters before embedding, then stored in a `pgvector` column. The trigger API allows immediate on-demand collection.

### Components

| Component | Image | Base | Size | UID |
|-----------|-------|------|------|-----|
| second-brain (backend) | `second-brain:dev` | golang:1.24-alpine → alpine:3.21 | ~34.5 MB | 10001 |
| vibers-web (frontend) | `vibers-web:dev` | node:22-alpine (standalone) | ~195 MB | 10001 |
| postgres | `pgvector/pgvector:pg16` | PostgreSQL 16 + pgvector | — | — |

### Kubernetes Resources (namespace: `second-brain`)

| Resource | Name | Notes |
|----------|------|-------|
| Namespace | `second-brain` | — |
| ConfigMap | `second-brain-config` | Non-secret environment settings |
| Secret | `second-brain-secret` | Database credentials |
| Secret (out-of-band) | `cliproxy-auth-secret` | OAuth JWT, excluded from git — must be created manually |
| PersistentVolume | — | hostPath 100 Gi, Pod path `/data/drive`, minikube mount uid 10001 required |
| StatefulSet | `postgres` | PostgreSQL 16 + pgvector |
| Service | `postgres` | ClusterIP 5432 |
| Deployment | `second-brain` | initContainer: wait-for-postgres |
| Service | `second-brain` | NodePort 30920 |
| Deployment | `vibers-web` | Next.js standalone |
| Service | `vibers-web` | NodePort 30300 |

---

## Quick Start (minikube)

```bash
# 1. minikube 기동
minikube start --driver=docker --cpus=4 --memory=8g --disk-size=20g

# 2. Google Drive sync 폴더 마운트 (uid=10001 필수)
nohup minikube mount --uid=10001 --gid=10001 \
  "$HOME/Google Drive/공유 드라이브/Vibers.AI:/mnt/drive" \
  > /tmp/minikube-mount.log 2>&1 &

# 3. 이미지 빌드 (minikube 내부 docker)
eval $(minikube docker-env)
docker build -t second-brain:dev .
docker build -t vibers-web:dev -f web/Dockerfile web/

# 4. CliProxy OAuth auth Secret 생성 (민감 데이터, git 제외)
kubectl create namespace second-brain 2>/dev/null || true
kubectl -n second-brain create secret generic cliproxy-auth-secret \
  --from-file=auth.json=$HOME/.cli-proxy-api/codex-*.json

# 5. 매니페스트 적용
kubectl apply -k deploy/k8s/

# 6. Pod Ready 대기
kubectl -n second-brain wait --for=condition=ready pod -l app=postgres --timeout=180s
kubectl -n second-brain wait --for=condition=ready pod -l app=second-brain --timeout=300s
kubectl -n second-brain wait --for=condition=ready pod -l app=vibers-web --timeout=180s

# 7. 로컬 접근 (port-forward)
kubectl -n second-brain port-forward svc/vibers-web 3000:3000 &
kubectl -n second-brain port-forward svc/second-brain 9200:9200 &

# 8. LAN에서 접근하려면
# kubectl -n second-brain port-forward --address 0.0.0.0 svc/vibers-web 3000:3000 &

# 9. 브라우저
open http://localhost:3000
```

> **macOS Docker driver note**: NodePort ports 30920 and 30300 are not directly accessible from the host on macOS with the docker driver. Use `port-forward` as shown in step 7.

---

## Project Structure

```
second-brain/
├── cmd/
│   └── server/
│       └── main.go              # Entry point, router, collector registration (port 9200)
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
│   ├── db/                      # pgvector init, migrations
│   ├── embedding/               # OpenAI-compatible embedding client
│   ├── handler/                 # HTTP handlers (search, documents, sources)
│   ├── model/                   # Document, SearchResult structs
│   └── scheduler/               # Periodic collection scheduler (mutex dedup)
├── migrations/                  # SQL migration files (auto-applied on startup)
├── deploy/
│   └── k8s/                     # Kustomize manifests
│       ├── kustomization.yaml
│       ├── namespace.yaml
│       ├── configmap.yaml
│       ├── postgres/            # StatefulSet, Service
│       └── app/                 # Deployment, Service (NodePort)
├── web/                         # Next.js 16 frontend
│   ├── app/                     # App Router pages
│   ├── components/              # Search UI, document cards, detail viewer
│   └── Dockerfile               # Standalone build
├── Dockerfile                   # Backend multi-stage build
└── go.mod                       # Go 1.25 module definition
```

---

## API Reference

All endpoints use the `/api/v1` prefix. The single exception is `/health`.
The 7-endpoint reference is also available in the web UI at `/api-docs`.

### Endpoint Summary

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/health` | Health check |
| `POST` | `/api/v1/search` | Hybrid search (BM25 + pgvector, RRF) |
| `GET` | `/api/v1/documents` | Paginated document list |
| `GET` | `/api/v1/documents/{id}` | Single document detail |
| `GET` | `/api/v1/documents/{id}/raw` | Raw file streaming (filesystem only, 50 MiB limit) |
| `POST` | `/api/v1/collect/trigger` | Immediate collection trigger (scheduler mutex prevents duplicates) |
| `GET` | `/api/v1/sources` | Registered collector list |

---

### GET /health

Returns 200 when the server is running.

```bash
curl http://localhost:9200/health
```

```json
{"status":"ok"}
```

---

### POST /api/v1/search

Executes a hybrid search combining BM25 full-text (`ts_rank_cd`) and pgvector cosine (`<=>`) scores via RRF.

**Request Body**

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `query` | string | required | Search query |
| `source_type` | string | (all) | Filter by source: `filesystem` \| `slack` \| `github` |
| `limit` | int | 10 | Maximum results to return |
| `sort` | string | `"relevance"` | `"relevance"` (RRF score desc) \| `"recent"` (collected_at desc) |
| `include_deleted` | bool | `false` | Include soft-deleted documents |

```bash
curl -X POST http://localhost:9200/api/v1/search \
  -H "Content-Type: application/json" \
  -d '{"query": "onboarding guide", "limit": 5, "sort": "relevance"}'
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
curl "http://localhost:9200/api/v1/documents?limit=5&offset=0&source=filesystem"
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
curl http://localhost:9200/api/v1/documents/a1b2c3d4-e5f6-7890-abcd-ef1234567890
```

---

### GET /api/v1/documents/{id}/raw

Streams the raw file bytes. Available for filesystem sources only. `Content-Type` is set automatically based on the file extension. Files exceeding 50 MiB return 413.

```bash
# Download file
curl -O -J http://localhost:9200/api/v1/documents/a1b2c3d4-e5f6-7890-abcd-ef1234567890/raw

# Open inline in browser (images, PDFs, etc.)
open "http://localhost:9200/api/v1/documents/a1b2c3d4-e5f6-7890-abcd-ef1234567890/raw"
```

---

### POST /api/v1/collect/trigger

Runs all active collectors immediately. A scheduler mutex prevents concurrent runs — if collection is already in progress, the server returns `409 Conflict`.

```bash
curl -X POST http://localhost:9200/api/v1/collect/trigger
```

---

### GET /api/v1/sources

Returns the list of registered collectors and their status.

```bash
curl http://localhost:9200/api/v1/sources
```

---

## Environment Variables

Full list based on `internal/config/config.go`.

| Key | Default | Description |
|-----|---------|-------------|
| `DATABASE_URL` | `postgres://brain:brain@localhost:5432/second_brain?sslmode=disable` | PostgreSQL connection string |
| `PORT` | `9200` | HTTP server port |
| `COLLECT_INTERVAL` | `1h` | Collector schedule interval (Go duration format) |
| `MAX_EMBED_CHARS` | `8000` | Maximum characters sent to the embedding API. Excess is truncated with a WARN log |
| `EMBEDDING_API_URL` | `https://api.openai.com/v1` | OpenAI-compatible embeddings endpoint |
| `EMBEDDING_MODEL` | `text-embedding-3-small` | Embedding model (1536 dimensions) |
| `EMBEDDING_API_KEY` | — | Static Bearer token. Mutually exclusive with `CLIPROXY_AUTH_FILE` |
| `CLIPROXY_AUTH_FILE` | — | Path to CliProxy OAuth JSON. Reads `access_token` with 5-minute TTL auto-refresh |
| `MIGRATIONS_DIR` | runtime.Caller fallback | In Docker images: `/app/migrations` |
| `FILESYSTEM_PATH` | — | Root directory to collect from. In-cluster path: `/data/drive` |
| `FILESYSTEM_ENABLED` | `false` | Set to `true` to register the filesystem collector |
| `SLACK_BOT_TOKEN` | — | Slack Bot OAuth Token (`xoxb-...`) |
| `SLACK_TEAM_ID` | — | Slack Workspace ID |
| `GITHUB_TOKEN` | — | GitHub Personal Access Token |
| `GITHUB_ORG` | — | GitHub organization name |
| `GDRIVE_CREDENTIALS_JSON` | — | Google ADC JSON path. If unset, the gdrive collector is disabled |
| `API_KEY` | — | Bearer key the web proxy sends when calling the backend |

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

## Web UI Guide

Open `http://localhost:3000` in your browser.

### Search and Filter

- On initial load, the 10 most recently collected documents are displayed automatically
- **Source filter**: All / Drive / Slack / GitHub (results update reactively on filter change)
- **Sort**: Relevance (RRF score descending) / Recent (collected_at descending)
- Document cards: title, source badge, description (up to 180 characters), image thumbnail, timestamps (created / modified)

### Document Detail Rendering

| Format | Renderer |
|--------|----------|
| Markdown (`.md`) | react-markdown + remark-gfm + Tailwind Typography (`prose`) |
| XLSX | TSV parsed into per-sheet HTML tables |
| TXT / LOG | Monospace `<pre>` block |
| Code (json, yaml, go, ts, and 30+ extensions) | Fenced block + highlight.js syntax highlighting |
| Images | Inline display via the `/raw` endpoint |

Three timestamps are shown per document: file modification time, first collected time, and last updated time.

### API Documentation

The 7-endpoint reference is accessible from the header navigation under `/api-docs`.

---

## Operations

### Pod Status

```bash
kubectl -n second-brain get pods
kubectl -n second-brain get pods -w   # live watch
```

### Logs

```bash
# Backend (collection logs, embedding, search queries)
kubectl -n second-brain logs -l app=second-brain --tail=100 -f

# Web frontend
kubectl -n second-brain logs -l app=vibers-web --tail=50 -f

# PostgreSQL
kubectl -n second-brain logs -l app=postgres --tail=50 -f
```

### Port Forwarding

```bash
# Localhost only
kubectl -n second-brain port-forward svc/vibers-web 3000:3000 &
kubectl -n second-brain port-forward svc/second-brain 9200:9200 &

# LAN access
kubectl -n second-brain port-forward --address 0.0.0.0 svc/vibers-web 3000:3000 &
kubectl -n second-brain port-forward --address 0.0.0.0 svc/second-brain 9200:9200 &
```

NodePort access is limited on macOS with the docker driver. Use `port-forward` instead.

### On-demand Collection Trigger

```bash
curl -X POST http://localhost:9200/api/v1/collect/trigger
```

Returns `409 Conflict` if collection is already running. Retry after it completes.

### Direct Database Access

```bash
kubectl -n second-brain exec -it statefulset/postgres -- \
  psql -U brain -d second_brain
```

```sql
-- Document count by source
SELECT source, COUNT(*) FROM documents GROUP BY source;

-- 10 most recently collected documents
SELECT title, source, collected_at FROM documents ORDER BY collected_at DESC LIMIT 10;

-- Documents missing embeddings (excluded from vector search)
SELECT COUNT(*) FROM documents WHERE embedding IS NULL;
```

### Rebuild Images and Rolling Restart

```bash
eval $(minikube docker-env)

# Backend
docker build -t second-brain:dev .
kubectl -n second-brain rollout restart deployment/second-brain
kubectl -n second-brain rollout status deployment/second-brain

# Frontend
docker build -t vibers-web:dev -f web/Dockerfile web/
kubectl -n second-brain rollout restart deployment/vibers-web
kubectl -n second-brain rollout status deployment/vibers-web
```

### Rotate CliProxy OAuth Secret

When the token expires or `auth.json` is replaced:

```bash
kubectl -n second-brain delete secret cliproxy-auth-secret
kubectl -n second-brain create secret generic cliproxy-auth-secret \
  --from-file=auth.json=$HOME/.cli-proxy-api/codex-*.json
kubectl -n second-brain rollout restart deployment/second-brain
```

---

## Development

### Prerequisites

- Go 1.25+
- Node.js 22+ / pnpm
- PostgreSQL 16 + pgvector extension
- Docker / minikube (for Kubernetes deployment)

### Run Backend Locally

```bash
export DATABASE_URL="postgres://brain:brain@localhost:5432/second_brain?sslmode=disable"
export EMBEDDING_API_KEY="sk-..."
export FILESYSTEM_PATH="/path/to/docs"
export FILESYSTEM_ENABLED=true

# Starts the server; migrations are applied automatically
go run ./cmd/server/
```

### Backend Tests and Linting

```bash
go test ./...
go test -race ./...
go vet ./...
gofmt -w .
```

### Frontend Dev Server

```bash
cd web
pnpm install
pnpm dev
```

### Frontend Build and Linting

```bash
cd web
pnpm build
pnpm lint
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
| — | NodePort not reachable from the host on macOS Docker driver | macOS docker driver network limitation | Use `kubectl port-forward` |
| — | gdrive collector not active | `GDRIVE_CREDENTIALS_JSON` unset disables it by default | Provide ADC credentials to enable (currently scaffold-stage implementation) |

---

## Related Documentation

- [`deploy/k8s/README.md`](deploy/k8s/README.md) — Detailed Kubernetes manifest reference
- [`guides/`](guides/) — Operations and development guides
- `wiki/` — Project wiki (agents, skills, rules reference)

---

## License

No license file is present in this repository. Contact the project maintainer before using or distributing this code.

---

Last updated: 2026-04-13
