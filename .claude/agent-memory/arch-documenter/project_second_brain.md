---
name: project_second_brain
description: second-brain stack, API surface, collector status, and deployment topology as of 2026-04-13
type: project
---

In-house AI knowledge search engine for team documents.

**Why:** Centralizes Google Drive, Slack, GitHub, and filesystem docs into a single hybrid search interface.
**How to apply:** Use this as the authoritative source for stack facts when writing architecture or API docs.

## Architecture Documents

- `ARCHITECTURE.md` (Korean) — written 2026-04-13, ~870 lines, 3 Mermaid diagrams, 10 ADRs
- `ARCHITECTURE.en.md` (English) — written 2026-04-13, ~870 lines, 3 Mermaid diagrams, 10 ADRs

## Stack

- Backend: Go 1.25, `cmd/server/main.go`, port 9200
- Frontend: Next.js 16 standalone, port 3000, Tailwind v4 + `@tailwindcss/typography` + highlight.js
- DB: `pgvector/pgvector:pg16` StatefulSet
- Embedding: OpenAI `text-embedding-3-small` (1536 dim). Accepts API Key OR CliProxy OAuth JWT as Bearer

## Images

- `second-brain:dev` — golang:1.24-alpine → alpine:3.21, uid 10001, ~34.5 MB
- `vibers-web:dev` — node:22-alpine standalone, uid 10001, ~195 MB

## Kubernetes (namespace: second-brain)

- Namespace, ConfigMap `second-brain-config`, Secret `second-brain-secret`
- Out-of-band Secret `cliproxy-auth-secret` (git-excluded, manual creation required each deployment)
- hostPath PV 100 Gi → Pod `/data/drive` (minikube mount uid 10001 required)
- postgres StatefulSet + Service 5432
- second-brain Deployment + Service NodePort 30920 + initContainer wait-for-postgres
- vibers-web Deployment + Service NodePort 30300

## API (7 endpoints, all under /api/v1 except /health)

- `GET /health`
- `POST /api/v1/search` — body: query, source_type?, limit?, sort?, include_deleted?
- `GET /api/v1/documents?limit=&offset=&source=`
- `GET /api/v1/documents/{id}`
- `GET /api/v1/documents/{id}/raw` (filesystem only, 50 MiB limit)
- `POST /api/v1/collect/trigger` (scheduler mutex prevents duplicate runs)
- `GET /api/v1/sources`

## Search

- sort: `"relevance"` (default, RRF) | `"recent"` (collected_at DESC)
- hybrid = BM25 ts_rank_cd + pgvector cosine (`<=>`) → RRF

## Collectors

- filesystem: fully operational, 4,150+ docs verified
- slack: public_channel only, DMs excluded; ERROR then skip if unconfigured
- github: ERROR then skip if unconfigured
- gdrive export: scaffold only, disabled by default (requires ADC)
- notion: removed (deregistered from main.go)

## Known Issues

- BUG-007: 9p mount Korean long filenames `lstat: file name too long` → skip
- `cliproxy-auth-secret` requires manual kubectl create per deployment
- 8 KB embedding truncation: adjust via MAX_EMBED_CHARS (Phase 1 chunks table pending)
- macOS docker driver: NodePort external access limited → use port-forward
