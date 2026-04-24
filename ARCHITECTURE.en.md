# second-brain Architecture

> Vision: LLM-curated private search engine. A RAG infrastructure that collects and embeds team knowledge from Google Drive, Slack, GitHub, and more, delivering curated search results to AI agents.

---

## Table of Contents

1. [Overview](#1-overview)
2. [System Architecture Diagram](#2-system-architecture-diagram)
3. [Service Layer Map](#3-service-layer-map)
4. [Data Model](#4-data-model)
5. [Collection Pipeline](#5-collection-pipeline)
6. [Extraction Pipeline](#6-extraction-pipeline)
7. [Embedding Pipeline](#7-embedding-pipeline)
8. [Search Pipeline](#8-search-pipeline)
9. [Deployment Architecture](#9-deployment-architecture)
10. [Web UI Architecture](#10-web-ui-architecture)
11. [Configuration and Environment Variables](#11-configuration-and-environment-variables)
12. [Architecture Decision Records](#12-architecture-decision-records)
13. [Known Issues](#13-known-issues)
14. [Roadmap](#14-roadmap)

---

## 1. Overview

second-brain is a team knowledge search platform composed of a Go-based backend service and a Next.js-based frontend UI.

**Target users**: Team members who need to quickly find documents, Slack conversations, and GitHub issues and PRs. The goal is to provide natural-language access to company-wide knowledge without requiring dedicated data engineering.

**Core design philosophy**: Dual binary (API server + collector daemon) → document ingestion → OpenAI embedding vectorization → pgvector + pg_bigm + PostgreSQL hybrid search (BM25 + cosine RRF) → LLM curation (re-ranking + summary). pg_bigm 2-gram indexing enables morphology-independent Korean search, and HyDE query expansion improves search quality.

**Non-functional requirements**:

| Item | Current Target |
|---|---|
| Search latency | p99 < 500ms |
| Collection idempotency | `ON CONFLICT(source_type, source_id) DO UPDATE` |
| Privacy | Slack DMs are excluded by design (`users.conversations` with DM channels filtered out) |
| Security | Bearer token authentication, timing-safe comparison (`subtle.ConstantTimeCompare`) |
| Availability | Single instance, dependency wait via initContainer |

**Runtime environment**: Deployed to a local Kubernetes cluster using Docker Desktop or minikube (docker driver) on macOS. Because the macOS docker driver does not expose NodePort directly on the host, `kubectl port-forward` is the recommended access method.

**Current scope**: Phase 0 complete. Document-level ingestion (no chunking), single Postgres instance, OpenAI text-embedding-3-small, `atomic.Bool` CAS-based scheduler, soft deletes, collection log table.

---

## 2. System Architecture Diagram

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
flowchart TD
    classDef focal fill:#b5523a,stroke:#b5523a,color:#faf7f2,stroke-width:1px;
    classDef muted fill:#f0ebe3,stroke:#57534e,color:#57534e,stroke-width:1px;
    classDef data fill:#faf7f2,stroke:#1c1917,color:#1c1917,stroke-width:1.2px;
    classDef ext fill:#faf7f2,stroke:#57534e,color:#1c1917,stroke-width:1px,stroke-dasharray:4 3;

    subgraph HOST["macOS Host"]
        GD["~/Google Drive\nShared Drive/Vibers.AI"]
        BROWSER["Browser\nlocalhost:30300 or port-forward"]
    end

    subgraph DOCKER["minikube (docker driver)"]
        subgraph NS["Namespace: second-brain"]
            SERVER["second-brain\nAPI Server\nGo HTTP :8080\nNodePort 30080"]
            COLLECTOR["second-brain\nCollector Daemon\nGo"]
            PG["postgres StatefulSet\npgvector/pgvector:pg16\n+ pg_bigm\nClusterIP :5432\nPVC 10Gi"]
            INIT["initContainer\nwait-for-postgres\nbusybox nc -z postgres 5432"]
            WATCHER["SlackChannelWatcher\ngoroutine 60s poll\ndetects new channels → full history collect"]
            PV["PV hostPath 100Gi\nReadOnlyMany\n/mnt/drive → /data/drive\nuid=10001"]
        end
    end

    subgraph EXTERNAL["External APIs"]
        OPENAI["OpenAI API\n/v1/embeddings\ntext-embedding-3-small"]
        SLACK["Slack Web API\nusers.conversations\nconversations.history\nconversations.replies"]
        GITHUB["GitHub REST API\n/orgs/{org}/repos\n/repos/{repo}/issues"]
        GDRIVE_API["Google Drive API\nfiles.export (ADC)\noptional activation"]
    end

    BROWSER -->|HTTP| SERVER
    GD -->|"minikube mount\nuid=10001 gid=10001"| PV
    PV -->|ReadOnly mount\n/data/drive| COLLECTOR
    SERVER --> PG
    SERVER -->|"LLM curation"| LLM_API["LLM API\n(curation)"]
    COLLECTOR --> PG
    COLLECTOR -->|"Bearer JWT\n(API Key or CliProxy OAuth)"| OPENAI
    COLLECTOR -->|"Bearer SLACK_BOT_TOKEN"| SLACK
    COLLECTOR -->|"Bearer GITHUB_TOKEN"| GITHUB
    COLLECTOR -.->|when ADC is configured| GDRIVE_API
    INIT -->|nc polling| PG
    INIT -->|ready| SERVER
    WATCHER -->|goroutine| SLACK
    WATCHER -->|upsert| PG

    class SERVER,COLLECTOR focal;
    class PG data;
    class OPENAI,SLACK,GITHUB,GDRIVE_API,LLM_API ext;
    class INIT,WATCHER,PV,GD,BROWSER muted;
```

### Mount Path Details

```
Host path:       ~/Google Drive/Shared Drive/Vibers.AI
minikube inside: /mnt/drive    (created via minikube mount --uid=10001 --gid=10001 ...)
PV hostPath:     /mnt/drive    (deploy/k8s/second-brain-pv.yaml, 100Gi ReadOnlyMany)
Pod mount:       /data/drive   (deploy/k8s/second-brain-deployment.yaml volumeMounts)
```

CliProxy secret mount:
```
Secret key:   auth.json (cliproxy-auth-secret)
Pod path:     /etc/cliproxy/auth.json
Config var:   CLIPROXY_AUTH_FILE=/etc/cliproxy/auth.json (ConfigMap)
```

---

## 3. Service Layer Map

### Backend Package Dependency Graph

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

    MAIN["cmd/server/main.go\nDI wiring, signal handling"]
    CFG["internal/config\nConfig.Load()"]
    API["internal/api\nchi Router\n9 endpoints"]
    SCHED["internal/scheduler\nScheduler, SlackChannelWatcher\nrobfig/cron"]
    COL["internal/collector\nFilesystem/Slack/GitHub\nGDriveCollector/Extractor"]
    SEARCH["internal/search\nService, EmbedClient"]
    STORE["internal/store\nDocumentStore, Postgres"]
    MODEL["internal/model\nDocument, SearchQuery\nSearchResult, SourceType"]

    MAIN --> CFG
    MAIN --> STORE
    MAIN --> SEARCH
    MAIN --> COL
    MAIN --> SCHED
    MAIN --> API
    API --> STORE
    API --> SEARCH
    API --> SCHED
    SCHED --> COL
    SCHED --> STORE
    SCHED --> SEARCH
    SEARCH --> STORE
    STORE --> MODEL
    COL --> MODEL
    API --> MODEL

    class MAIN focal;
    class STORE data;
    class MODEL muted;
```

### Package Responsibilities and Key Symbols

| Package | Key Types / Functions | Files |
|---|---|---|
| `cmd/server` | `run()` — DI wiring, `migrationsPath()` — MIGRATIONS_DIR resolution | `main.go` |
| `internal/config` | `Config` struct, `Load()` — environment variable parsing with defaults | `config.go` |
| `internal/api` | `Server`, `Handler()` — chi router, `requireAPIKey()` — Bearer middleware | `router.go`, `middleware.go`, `search.go`, `document.go`, `source.go`, `stats.go`, `collect_channel.go` |
| `internal/scheduler` | `Scheduler`, `run()`, `runCollector()`, `embedDocuments()`, `TriggerAll()`, `ForceCollectSlackChannel()`, `LookupSlackChannel()` | `scheduler.go` |
| `internal/collector` | `FilesystemCollector`, `SlackCollector`, `GitHubCollector`, `GDriveCollector`, `SlackChannelWatcher`, `DriveExporter` | `filesystem.go`, `slack.go`, `slack_watcher.go`, `github.go`, `gdrive.go`, `gdrive_export.go` |
| `internal/collector/extractor` | `Registry`, `SanitizeText()`, `TruncateUTF8()`, `HTMLExtractor`, `PDFExtractor`, `DocxExtractor`, `XlsxExtractor`, `PptxExtractor` | `extractor.go`, `html.go`, `pdf.go`, `docx.go`, `xlsx.go`, `pptx.go` |
| `internal/search` | `Service.Search()` — graceful fallback to full-text on embedding failure, `EmbedClient.Embed()`, `EmbedClient.EmbedBatch()`, `cliProxyToken` (5-minute TTL) | `search.go`, `embed.go` |
| `internal/store` | `DocumentStore`, `Upsert()`, `hybridSearch()`, `fulltextSearch()`, `LastCollectedAt()`, `MarkDeleted()`, `CountBySource()` | `document.go`, `postgres.go` |
| `internal/model` | `Document`, `SearchQuery`, `SearchResult`, 5 `SourceType` constants | `document.go` |

### HTTP Server Configuration (`cmd/server/main.go:113`)

```
ReadTimeout:  15s
WriteTimeout: 30s
IdleTimeout:  60s
Port:         cfg.Port (default 8080, injected from ConfigMap)
```

---

## 4. Data Model

### ERD

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
erDiagram
    documents {
        uuid id PK "gen_random_uuid()"
        text source_type "NOT NULL — filesystem|slack|github|gdrive|notion"
        text source_id "NOT NULL — unique identifier within the source"
        text title "NOT NULL"
        text content "NOT NULL"
        jsonb metadata "default '{}'"
        vector_1536 embedding "nullable — NULL when embedding is not yet computed"
        tsvector tsv "GENERATED STORED — title(A) + content(B)"
        timestamptz collected_at "NOT NULL — file ModTime or collection timestamp"
        timestamptz created_at "default now()"
        timestamptz updated_at "default now() — refreshed on Upsert"
        text status "default 'active' — active|deleted|moved"
        timestamptz deleted_at "nullable — soft delete timestamp"
    }

    collection_log {
        uuid id PK
        text source_type "NOT NULL"
        timestamptz started_at "NOT NULL"
        timestamptz finished_at
        int documents_count "default 0"
        text error "nullable"
        timestamptz created_at "default now()"
    }

    documents ||--o{ collection_log : "by source_type"
```

### `documents` Table (`migrations/001_init.sql`, `migrations/002_soft_delete.sql`)

| Column | Type | Constraint | Notes |
|---|---|---|---|
| `id` | uuid | PK, `gen_random_uuid()` | |
| `source_type` | text | NOT NULL | `filesystem`, `slack`, `github`, `gdrive`, `notion` |
| `source_id` | text | NOT NULL | filesystem: relative path, slack: `{channel_id}:{ts}`, github: `{org/repo}#{number}` |
| `title` | text | NOT NULL | |
| `content` | text | NOT NULL | |
| `metadata` | jsonb | default `'{}'` | Source-specific supplementary data |
| `embedding` | vector(1536) | nullable | text-embedding-3-small dimensions |
| `tsv` | tsvector | GENERATED ALWAYS STORED | english(A+B) + simple(A+B) combined |
| `collected_at` | timestamptz | NOT NULL | File ModTime in UTC |
| `created_at` | timestamptz | default now() | |
| `updated_at` | timestamptz | default now() | Refreshed on Upsert |
| `status` | text | default `'active'` | `active`, `deleted`, `moved` |
| `deleted_at` | timestamptz | nullable | Soft delete timestamp |

### Indexes (`migrations/001_init.sql`, `migrations/002_soft_delete.sql`)

| Index Name | Type | Target | Purpose |
|---|---|---|---|
| `(UNIQUE)` | UNIQUE | `(source_type, source_id)` | Upsert conflict key |
| `idx_documents_tsv` | GIN | `tsv` | Full-text search |
| `idx_documents_embedding` | HNSW | `embedding vector_cosine_ops` | Cosine similarity ANN search |
| `idx_documents_source` | B-tree | `source_type` | Source filter |
| `idx_documents_collected` | B-tree | `collected_at DESC` | Recency sort |
| `idx_documents_status` | B-tree | `status` | Soft delete filter |

**pgvector configuration**: `HNSW (vector_cosine_ops)` — `migrations/001_init.sql:26`. HNSW is chosen over IVFFlat because it does not require a `lists` parameter at index build time and is better suited for online inserts. Default HNSW parameters are used (`m=16`, `ef_construction=64`).

### `tsvector` Generation Rule (`migrations/001_init.sql:12-17`)

```sql
setweight(to_tsvector('english', coalesce(title,   '')), 'A') ||
setweight(to_tsvector('simple',  coalesce(title,   '')), 'A') ||
setweight(to_tsvector('english', coalesce(content, '')), 'B') ||
setweight(to_tsvector('simple',  coalesce(content, '')), 'B')
```

Applying both English stemming (`english`) and simple tokenization (`simple`) enables simultaneous root-form search for English words and keyword search for Korean text.

### `collection_log` Table

One row is inserted per collection run. The `started_at`, `finished_at`, `documents_count`, and `error` columns track collection history. Written by `store.RecordCollectionLog()` (`internal/store/document.go:329`).

---

## 5. Collection Pipeline

### End-to-End Sequence

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
sequenceDiagram
    participant CRON as cron.Cron (robfig/cron)<br/>or POST /collect/trigger
    participant SCHED as Scheduler.run()
    participant CAS as atomic.Bool
    participant RC as runCollector()
    participant COL as Collector.Collect(ctx, since)
    participant EMBED as embedDocuments()
    participant STORE as DocumentStore.Upsert()
    participant DD as DeletionDetector (fs only)

    CRON->>SCHED: tick (COLLECT_INTERVAL, default 10m)
    SCHED->>CAS: CompareAndSwap(false→true)
    alt already running
        CAS-->>SCHED: false → warn + return
    else
        CAS-->>SCHED: true
        SCHED->>RC: runCollector(col)
        RC->>STORE: LastCollectedAt(source_type)
        STORE-->>RC: since timestamp (zero value on first run)
        RC->>COL: Collect(ctx, since)
        COL-->>RC: []model.Document
        RC->>EMBED: embedDocuments(batch=20)
        loop each batch of 20
            EMBED->>OpenAI: POST /v1/embeddings
            OpenAI-->>EMBED: []float32 vectors
        end
        loop each Document
            RC->>STORE: Upsert(doc)
            STORE-->>RC: id, created_at, updated_at
        end
        RC->>STORE: RecordCollectionLog(src, started, count, err)
        opt FilesystemCollector (DeletionDetector)
            RC->>DD: ListActiveSourceIDs(ctx)
            DD-->>RC: []string (current file list)
            RC->>STORE: MarkDeleted(source_type, activeIDs)
        end
        SCHED->>CAS: Store(false)
    end
```

### Scheduler Structure (`internal/scheduler/scheduler.go`)

```go
type Scheduler struct {
    cron       *cron.Cron          // robfig/cron v3, seconds resolution
    collectors []collector.Collector
    store      DocumentUpserter
    embed      *search.EmbedClient
    running    atomic.Bool         // CAS mutex (scheduler.go:57)
}
```

- `Register(interval)` — registers cron spec `@every {interval}` for each Collector (scheduler.go:73)
- `TriggerAll()` — manual trigger via `POST /api/v1/collect/trigger`, executes in a goroutine (scheduler.go:101)
- `run()` — cron tick entry point, acquires CAS lock then runs a single Collector (scheduler.go:119)
- `runCollector()` — core collection logic: `LastCollectedAt` → `Collect` → `embedDocuments` → `Upsert` → `RecordCollectionLog` → `MarkDeleted` (scheduler.go:131)

**defaultMaxEmbedChars = 8000** (scheduler.go:25): default truncation value to avoid OpenAI token limits. Can be overridden via the `MAX_EMBED_CHARS` environment variable.

### SlackChannelWatcher (`internal/collector/slack_watcher.go`)

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
sequenceDiagram
    participant MAIN as main.go (goroutine)
    participant W as SlackChannelWatcher.Run()
    participant SLACK as Slack users.conversations
    participant STORE as DocumentStore

    MAIN->>W: go watcher.Run(ctx)
    Note over W: ticker 60s
    W->>SLACK: first tick: ListMemberChannels()
    SLACK-->>W: existing channel list → seed into seen map (no collection)
    loop every 60 seconds
        W->>SLACK: ListMemberChannels()
        SLACK-->>W: current channel list
        W->>W: diff with seen = new channels
        loop each new channel
            W->>SLACK: CollectChannel(ch.ID, ch.Name, time.Time{})
            W->>W: embedDocuments(batch=20)
            W->>STORE: Upsert (each message)
            W->>STORE: RecordCollectionLog
        end
    end
```

The `seen` map is protected by `sync.Mutex` (slack_watcher.go:29). On the first tick, existing channels are added to `seen` without being collected — this prevents a full re-collection on restart.

### Collector Details

#### FilesystemCollector (`internal/collector/filesystem.go`)

| Item | Value |
|---|---|
| Root path | `FILESYSTEM_PATH` env (ConfigMap: `/data/drive`) |
| Incremental basis | `info.ModTime().After(since)` (filesystem.go:145) |
| Skipped directories | `.git`, `node_modules`, `dist`, `.next`, `.omc`, `.sisyphus`, `.claude` |
| Skipped extensions | `.bak`, `.gitkeep`, `.plist`, `.lock`, `.DS_Store` |
| Full-text extensions | `.md`, `.txt`, `.csv`, `.json`, `.js`, `.ts`, `.tsx`, `.py`, `.sh` |
| Text file size limit | 512 KB (`maxTextFileBytes`) |
| Extractor extensions | `.html`, `.htm`, `.pdf`, `.docx`, `.xlsx`, `.pptx` (Registry) |
| Google Workspace | `.gsheet`, `.gdoc`, `.gscript`, `.gslides`, `.gform` — URL extraction + Drive API export attempt |
| Image handling | `.png`, `.jpg`, `.jpeg`, `.gif`, `.svg` — metadata only |
| Archive handling | `.zip`, `.apk`, `.tar`, `.gz` — metadata only |
| Read timeouts | Text files: 3s (`fileReadTimeout`), GWorkspace: 2s, binary extraction: 10s (`extractTimeout`) |
| Soft delete support | Implements `ListActiveSourceIDs()` → satisfies `DeletionDetector` interface |

`SourceID` = `filepath.Rel(rootPath, absPath)` (filesystem.go:225) — relative path used as the deduplication key.

**Known issue**: minikube's 9P filesystem driver returns `lstat: file name too long` for Korean long filenames. The `filepath.WalkDir` callback logs a warning and skips such files (filesystem.go:126-130).

#### SlackCollector (`internal/collector/slack.go`)

| Item | Value |
|---|---|
| Channel scope | `users.conversations` — only channels where the bot is a member (public + private) |
| DM exclusion | `types=public_channel,private_channel` parameter — IM/MPIM explicitly excluded |
| Incremental basis | `oldest` parameter is sent only when `since.Unix() > 0` (slack.go:225) |
| Thread handling | Calls `conversations.replies` for each message with `reply_count > 0` → stored as an independent Document |
| Pagination | Cursor-based, limit=200 (slack.go:183, 218, 266) |
| HTTP timeout | 30s (`http.Client{Timeout: 30 * time.Second}`) |
| SourceID | `{channel_id}:{ts}` (slack.go:119) |
| Title format | `#{channel_name} — {ts}` |

**Known limitation**: If a parent message was posted before `since` but received a thread reply after `since`, the reply is missed during incremental collection (see comment at slack.go:45).

#### GitHubCollector (`internal/collector/github.go`)

| Item | Value |
|---|---|
| Collection unit | All non-archived repositories in `GITHUB_ORG` |
| Collection targets | Issues + Pull Requests (PRs identified by non-nil `pull_request` field) |
| Incremental basis | `/repos/{repo}/issues?state=all&since={RFC3339}` |
| Pagination | Page-based, per_page=100 |
| API version | `X-GitHub-Api-Version: 2022-11-28` |
| HTTP timeout | 30s |
| SourceID | `{org/repo}#{number}` (github.go:122) |
| Title format | `[{org/repo}] {issue_title}` |

#### GDriveCollector (`internal/collector/gdrive.go`, `gdrive_export.go`)

Scaffold state. `Enabled()` returns `false` when credentials are absent. The Drive API is activated only through `DriveExporter`, which is invoked by `FilesystemCollector` to export the content of Google Workspace stub files (`.gdoc`, `.gsheet`, etc.).

`DriveExporter.Export()` MIME mapping:
- `.gsheet` → `text/csv`
- `.gdoc`, `.gscript`, `.gform`, `.gslides` → `text/plain`

#### NotionCollector (`internal/collector/notion.go`)

Disabled. See comment at `cmd/server/main.go:75` — `NewNotionCollector()` must be registered to re-enable.

---

## 6. Extraction Pipeline

The `Registry` in the `internal/collector/extractor/` package selects an Extractor based on file extension. All extractors implement the same interface.

```go
type Extractor interface {
    Supports(ext string) bool
    Extract(ctx context.Context, absPath string) (string, error)
}
```

Registry registration order (extractor.go:42-49): `HTMLExtractor` → `PDFExtractor` → `DocxExtractor` → `XlsxExtractor` → `PptxExtractor`

### SanitizeText (`internal/collector/extractor/extractor.go:92`)

Applied universally to all extractor output:

1. `\x00` → `" "` (space) — prevents Postgres TEXT storage errors
2. `strings.ToValidUTF8(s, "\uFFFD")` — replaces invalid UTF-8 sequences
3. Consecutive `\n\n\n` → `\n\n` compression (preserves paragraph structure)

### TruncateUTF8 (`extractor.go:64`)

After extraction, a `MaxExtractedBytes = 512 KB` ceiling is applied. Truncation respects UTF-8 boundaries and appends a `\n[content truncated]` suffix.

### Extractor Details

| Extension | Extractor | Library | Processing | Notes |
|---|---|---|---|---|
| `.html`, `.htm` | `HTMLExtractor` | `x/net/html` | Parses tags and concatenates text nodes | Inline scripts/styles excluded |
| `.pdf` | `PDFExtractor` | `github.com/ledongthuc/pdf` | Page-by-page `GetPlainText()` concatenation | 10s ctx timeout, goroutine abandon pattern (pdf.go:25-44) |
| `.docx` | `DocxExtractor` | stdlib `archive/zip` + `encoding/xml` | Unzip OOXML → `word/document.xml` → `<w:t>` nodes | Newline inserted at each `</w:p>` (docx.go:93) |
| `.xlsx` | `XlsxExtractor` | `github.com/xuri/excelize/v2` | `RawCellValue:true` → per-sheet TSV blocks | 200 KiB limit (`xlsxMaxBytes`), empty rows/sheets skipped, `\t`/`\n` in cells → space |
| `.pptx` | `PptxExtractor` | stdlib `archive/zip` + `encoding/xml` | Unzip OOXML → `ppt/slides/*.xml` → `<a:t>` nodes | |

### XLSX TSV Output Format

```
##SHEET Sheet1
col_a\tcol_b\tcol_c
val1\tval2\tval3

##SHEET Summary
name\ttotal
Alice\t100
```

- The frontend `XlsxTable.tsx` parses `##SHEET` delimiters and renders up to 200 rows per sheet
- If the output exceeds 200 KiB, a `\n...(truncated)` suffix is appended and processing terminates early (xlsx.go:116)

### Content Size Hierarchy

| Layer | Limit | Location |
|---|---|---|
| Text file inline read | 512 KB | `filesystem.go:43 (maxContentBytes = 1MB` → `maxTextFileBytes = 512KB)` |
| Extractor output | 512 KB | `extractor.go:16 (MaxExtractedBytes)` |
| XLSX intermediate buffer | 200 KB | `xlsx.go:16 (xlsxMaxBytes)` |
| Embedding input | 8000 chars (default) | `scheduler.go:25 (defaultMaxEmbedChars)` |
| Raw file API | 50 MiB | `internal/api/document.go` |

---

## 7. Embedding Pipeline

### Token Source Priority (`internal/search/embed.go:92`)

```go
func NewEmbedClient(apiURL, apiKey, authFilePath, model string) *EmbedClient {
    var ts tokenSource
    switch {
    case apiKey != "":        // Priority 1: EMBEDDING_API_KEY environment variable
        ts = &staticToken{t: apiKey}
    case authFilePath != "": // Priority 2: CliProxy JSON file (auto-refreshed every 5 minutes)
        ts = newCliProxyToken(authFilePath)
    }
    // Priority 3: no Authorization header (compatible with self-hosted endpoints)
}
```

`cliProxyToken` (embed.go:29): 5-minute TTL cache, re-reads the file on expiry. Parses the `access_token` field. Concurrency-safe via `sync.Mutex`.

### EmbedBatch Flow (`internal/scheduler/scheduler.go:198`)

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
sequenceDiagram
    participant SCHED as embedDocuments()
    participant EMBED as EmbedClient.EmbedBatch()
    participant OAI as OpenAI /v1/embeddings

    loop docs in batches of batchSize=20
        SCHED->>SCHED: text = title + "\n\n" + content
        alt len(text) > maxLen (default 8000)
            SCHED->>SCHED: text = text[:maxLen]
            SCHED->>SCHED: slog.Warn("content truncated")
        end
        SCHED->>EMBED: EmbedBatch(ctx, texts[20])
        EMBED->>OAI: POST /v1/embeddings\n{input: [...], model: "text-embedding-3-small"}
        OAI-->>EMBED: [{index, embedding: float32[1536]}]
        alt API error
            EMBED-->>SCHED: error
            SCHED->>SCHED: slog.Warn("batch failed, skipping chunk")
            Note over SCHED: batch failure does not abort the entire collection
        else success
            EMBED-->>SCHED: [][]float32
            SCHED->>SCHED: docs[i].Embedding = vecs[i]
        end
    end
```

### Query Embedding at Search Time (`internal/search/search.go:34`)

```go
func (s *Service) Search(ctx context.Context, q model.SearchQuery) ([]*model.SearchResult, error) {
    if s.embed.Enabled() {
        vec, err := s.embed.Embed(ctx, q.Query)  // single query embedding
        if err != nil {
            slog.Warn("search: embedding failed, falling back to full-text", "error", err)
            // graceful fallback to FTS-only on embedding failure
        } else {
            q.Embedding = vec
        }
    }
    return s.store.Search(ctx, q)
}
```

**Behavior when embedding is disabled**: If `EMBEDDING_API_URL` is an empty string, `EmbedClient.Enabled()` returns false. Embedding is skipped during collection, and search operates in FTS-only mode.

---

## 8. Search Pipeline

### API Endpoint

`POST /api/v1/search` — Bearer authentication required (when `API_KEY` is set)

**Request schema**:
```json
{
  "query": "BBQ meeting",
  "source_type": "filesystem",
  "exclude_source_types": ["slack"],
  "limit": 10,
  "sort": "relevance",
  "include_deleted": false
}
```

**Response schema**:
```json
{
  "results": [
    {
      "id": "uuid",
      "source_type": "filesystem",
      "source_id": "docs/meeting.md",
      "title": "meeting.md",
      "content": "...",
      "match_type": "hybrid",
      "score": 0.0165,
      "collected_at": "2026-04-01T09:00:00Z",
      "created_at": "2026-04-01T09:01:00Z",
      "updated_at": "2026-04-13T12:00:00Z",
      "metadata": {"path": "docs/meeting.md", "ext": ".md"}
    }
  ],
  "count": 10,
  "total": 10,
  "query": "BBQ meeting",
  "took_ms": 42
}
```

### hybridSearch CTE (`internal/store/document.go:242`)

```sql
WITH fts AS (
    SELECT id,
           row_number() OVER (ORDER BY GREATEST(
               ts_rank(tsv, plainto_tsquery('simple',  $1)),
               ts_rank(tsv, plainto_tsquery('english', $1))
           ) DESC) AS rank
    FROM documents
    WHERE (tsv @@ plainto_tsquery('simple',  $1)
        OR tsv @@ plainto_tsquery('english', $1))
    AND status = 'active'  -- when include_deleted=false
    AND source_type = $4   -- when source_type filter is applied
    LIMIT $3               -- limit * 2 (expands RRF candidates)
),
vec AS (
    SELECT id,
           row_number() OVER (ORDER BY embedding <=> $2 ASC) AS rank
    FROM documents
    WHERE embedding IS NOT NULL
    AND status = 'active'
    LIMIT $3
),
rrf AS (
    SELECT
        COALESCE(fts.id, vec.id) AS id,
        COALESCE(1.0/(60.0 + fts.rank), 0)
      + COALESCE(1.0/(60.0 + vec.rank), 0) AS score
    FROM fts
    FULL OUTER JOIN vec ON fts.id = vec.id
)
SELECT d.*, rrf.score
FROM rrf
JOIN documents d ON d.id = rrf.id
ORDER BY {sortOrder}   -- "recent": collected_at DESC | else: score DESC
LIMIT $3
```

**RRF constant k=60**: The standard value adopted by the search community that provides stable rank fusion without over-amplifying the contribution of top-ranked documents.

### Search Mode Selection

| Condition | Mode | Function |
|---|---|---|
| `query.Embedding` is non-empty | hybrid (RRF) | `hybridSearch()` |
| `query.Embedding` is nil or empty slice | fulltext only | `fulltextSearch()` |

Falls back to fulltext mode automatically on embedding failure (search.go:40-46).

### Filter Parameters

| Parameter | Type | Default | Description |
|---|---|---|---|
| `query` | string | (required) | Search query string |
| `source_type` | string | — | Single source filter (`filesystem`, `slack`, `github`) |
| `exclude_source_types` | []string | — | List of sources to exclude |
| `limit` | int | 20 | Number of results; values <= 0 default to 20 |
| `sort` | string | `"relevance"` | `"relevance"` (score DESC) or `"recent"` (collected_at DESC) |
| `include_deleted` | bool | false | When true, includes documents with `status='deleted'` |

`sortOrder()` whitelist (`store/document.go:188`): all values other than `"recent"` map to `"score DESC"` — prevents SQL injection.

### sortOrder Behavior

| sort value | SQL | Meaning |
|---|---|---|
| `"recent"` | `collected_at DESC` | Most recently collected first (based on file ModTime) |
| `"relevance"` or any other | `rrf.score DESC` | Descending RRF score |

---

## 9. Deployment Architecture

### Kubernetes Resource Map

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
flowchart TB
    classDef focal fill:#b5523a,stroke:#b5523a,color:#faf7f2,stroke-width:1px;
    classDef muted fill:#f0ebe3,stroke:#57534e,color:#57534e,stroke-width:1px;
    classDef data fill:#faf7f2,stroke:#1c1917,color:#1c1917,stroke-width:1.2px;
    classDef ext fill:#faf7f2,stroke:#57534e,color:#1c1917,stroke-width:1px,stroke-dasharray:4 3;

    subgraph Cluster["minikube Cluster"]
        NS["Namespace: second-brain"]

        subgraph NS
            CM["ConfigMap\nsecond-brain-config\nnon-sensitive env vars"]
            SEC1["Secret\nsecond-brain-secret\nDB PASSWORD, API_KEY, etc."]
            SEC2["Secret\ncliproxy-auth-secret\nauth.json (out-of-band)"]

            PV["PV\nsecond-brain-drive-pv\nhostPath /mnt/drive\n100Gi ReadOnlyMany"]
            PVC_PG["PVC (StatefulSet)\npgdata 10Gi\nReadWriteOnce"]

            PGSVC["Service\npostgres\nClusterIP :5432"]
            PGSF["StatefulSet\npostgres\npgvector/pgvector:pg16\nreadiness: pg_isready"]

            BRSVC["Service\nsecond-brain\nNodePort 30080→:8080"]
            BRDEP["Deployment\nsecond-brain\nsecond-brain:dev ~34.5MB\nrequests: 128Mi/100m\nlimits: 512Mi/500m"]
            COLDEP["Deployment\nsecond-brain-collector\nsecond-brain-collector:dev ~34.5MB"]
            INIT["initContainer\nwait-for-postgres\nbusybox nc -z postgres 5432"]

            WSVC["Service\nvibers-web\nNodePort 30300→:3000"]
            WDEP["Deployment\nvibers-web\nvibers-web:dev ~195MB"]
        end
    end

    CM --> BRDEP
    SEC1 --> BRDEP
    SEC2 -->|/etc/cliproxy/auth.json| BRDEP
    PV -->|ReadOnly /data/drive| BRDEP
    INIT -->|ready| BRDEP
    PGSF --> PGSVC
    PVC_PG --> PGSF
    BRDEP --> BRSVC
    WDEP --> WSVC
    BRSVC -->|BRAIN_API_URL| WDEP

    class BRDEP,COLDEP focal;
    class PGSF,PV,PVC_PG data;
    class CM,SEC1,SEC2,INIT muted;
```

### K8s Resource List (`deploy/k8s/`)

| File | Resource Type | Role |
|---|---|---|
| `namespace.yaml` | Namespace | `second-brain` namespace |
| `second-brain-configmap.yaml` | ConfigMap | Non-sensitive settings (`COLLECT_INTERVAL=5m`, `MAX_EMBED_CHARS=8000`, etc.) |
| `second-brain-secret.yaml` | Secret | Placeholder values (`POSTGRES_PASSWORD`, `API_KEY`, `SLACK_BOT_TOKEN`, `GITHUB_TOKEN`) |
| `second-brain-pv.yaml` | PersistentVolume | hostPath `/mnt/drive` 100Gi ReadOnlyMany (cluster-scoped) |
| `second-brain-deployment.yaml` | Deployment | second-brain app, initContainer, volume mounts, probes |
| `second-brain-service.yaml` | Service | NodePort 30080 → container 8080 |
| `second-brain-web-deployment.yaml` | Deployment | vibers-web, references BRAIN_API_URL |
| `second-brain-web-service.yaml` | Service | NodePort 30300 → container 3000 |
| `postgres-statefulset.yaml` | StatefulSet | pgvector:pg16, PVC 10Gi, `pg_isready` probe |
| `postgres-service.yaml` | Service | ClusterIP :5432 |
| `kustomization.yaml` | Kustomize | Resource list |

### Image Build

On macOS with the docker driver, `eval $(minikube docker-env)` is required to use minikube's internal Docker daemon:

```bash
eval $(minikube docker-env)
docker build -t second-brain:dev .
docker build -t vibers-web:dev ./web
```

| Image | Build Base | Runtime Base | Size | UID |
|---|---|---|---|---|
| `second-brain:dev` | `golang:1.24-alpine` | `alpine:3.21` | ~34.5 MB | 10001 |
| `vibers-web:dev` | `node:22-alpine` | `node:22-alpine` (standalone) | ~195 MB | 10001 |

### Initial Deployment Order

```bash
# 1. minikube mount (keep running in background)
minikube mount "/Users/sangyi/Google Drive/Shared Drive/Vibers.AI:/mnt/drive" --uid=10001 --gid=10001 &

# 2. Create PV first (cluster-scoped)
kubectl apply -f deploy/k8s/second-brain-pv.yaml

# 3. Apply remaining resources
kubectl apply -k deploy/k8s/

# 4. Create out-of-band Secret manually (not in git)
kubectl create secret generic cliproxy-auth-secret \
  --from-file=auth.json=/path/to/auth.json \
  -n second-brain
```

### Access Method (port-forward)

```bash
# Web UI
kubectl port-forward svc/vibers-web 30300:80 -n second-brain

# Backend API
kubectl port-forward svc/second-brain 30920:8080 -n second-brain
```

### Probe Configuration

| Container | Probe Type | Endpoint | initialDelay | period |
|---|---|---|---|---|
| second-brain | readiness | `GET /health :8080` | 10s | 10s |
| second-brain | liveness | `GET /health :8080` | 30s | 15s |
| postgres | readiness | `pg_isready -U brain -d second_brain` | 10s | 5s |
| postgres | liveness | `pg_isready -U brain -d second_brain` | 30s | 10s |

---

## 10. Web UI Architecture

### Next.js App Router Structure

```
web/src/app/
├── page.tsx                    # Search main — search bar, filters, result card list
├── layout.tsx                  # Header navigation, dark mode support
├── api-docs/page.tsx           # API reference — 9 endpoint cards
├── documents/[id]/page.tsx     # Document detail — render branching by format
├── documents/[id]/MarkdownContent.tsx  # react-markdown + remark-gfm + rehype-highlight
├── documents/[id]/XlsxTable.tsx        # ##SHEET TSV parsing, up to 200 rows per sheet
└── api/                        # Next.js API routes (backend proxy)
    ├── search/route.ts         # GET+POST → BRAIN /api/v1/search
    ├── documents/route.ts      # GET → BRAIN /api/v1/documents
    ├── documents/[id]/route.ts # GET → BRAIN /api/v1/documents/{id}
    ├── documents/[id]/raw/route.ts # GET → BRAIN /api/v1/documents/{id}/raw
    └── stats/route.ts          # GET → BRAIN /api/v1/stats
```

### API Proxy Pattern (`web/src/app/api/search/route.ts`)

```typescript
const BACKEND_URL =
  process.env.BRAIN_API_URL          // Priority 1: K8s service URL
  ?? process.env.NEXT_PUBLIC_API_URL // Priority 2: public URL
  ?? "http://localhost:8080";        // Priority 3: local dev default

const API_KEY = process.env.API_KEY ?? "";  // Backend Bearer token injected server-side
```

Server-side proxy pattern that does not expose the backend address or API key to the client.

### Filter Options (`web/src/app/page.tsx:18`)

```typescript
const FILTER_OPTIONS: FilterOption[] = [
  { value: "all",        label: "All"    },
  { value: "filesystem", label: "Drive"  },
  { value: "slack",      label: "Slack"  },
  { value: "github",     label: "GitHub" },
];
```

Counter display: When the `All` filter is selected, the count shown excludes Slack (`page.tsx:43` — `total - by_source.slack`).

### Reactivity Pattern

Search result auto-refresh triggers:
- `[submittedQuery, activeFilter, sort]` changes — search mode
- `[activeFilter, isSearchMode]` changes — initial document list mode

A `cancelled` flag prevents response race conditions. Initial state (no query): calls `listRecentDocuments(10, source)`.

### Document Detail Render Branching

The `getRenderKind(ext)` function determines the rendering method by file extension:

| Kind | Extensions | Renderer |
|---|---|---|
| `image` | `.png`, `.jpg`, `.gif`, etc. | `<img>` tag |
| `markdown` | `.md`, `.mdx` | `MarkdownContent` — react-markdown + remark-gfm + rehype-highlight |
| `xlsx` | `.xlsx` | `XlsxTable` — `##SHEET` TSV parsing, up to 200 rows per sheet |
| `code` | `.ts`, `.go`, `.py`, etc. | Code block (highlight.js github-dark) |
| `text` | all others | `<pre>` plain text |

### match_type Badges (`page.tsx:37`)

| match_type | Badge Label |
|---|---|
| `fulltext` | Full-text |
| `vector` | Semantic |
| `hybrid` | Hybrid |

---

## 11. Configuration and Environment Variables

### Backend (`internal/config/config.go`)

| Environment Variable | Required | Default | Usage Location | Description |
|---|---|---|---|---|
| `DATABASE_URL` | Optional | `postgres://brain:brain@localhost:5432/second_brain?sslmode=disable` | `store/postgres.go` | Postgres connection string |
| `PORT` | Optional | `8080` | `cmd/server/main.go:113` | HTTP server port |
| `EMBEDDING_API_URL` | Optional | `https://api.openai.com/v1` | `search/embed.go:92` | OpenAI-compatible endpoint |
| `EMBEDDING_API_KEY` | Optional | — | `search/embed.go:95` | Static API key (priority 1 token) |
| `EMBEDDING_MODEL` | Optional | `text-embedding-3-small` | `search/embed.go:101` | Embedding model |
| `CLIPROXY_AUTH_FILE` | Optional | — | `search/embed.go:98` | auth.json path (priority 2 token) |
| `COLLECT_INTERVAL` | Optional | `10m` | `scheduler/scheduler.go:73` | Collection interval (Go duration) |
| `MAX_EMBED_CHARS` | Optional | `8000` | `scheduler/scheduler.go:29` | Maximum characters for embedding input |
| `MIGRATIONS_DIR` | Optional | auto-detected | `cmd/server/main.go:153` | SQL migrations directory |
| `FILESYSTEM_PATH` | Optional | — | `collector/filesystem.go:101` | Filesystem collector root path |
| `FILESYSTEM_ENABLED` | Optional | `false` | `cmd/server/main.go:77` | Enable filesystem collector |
| `SLACK_BOT_TOKEN` | Optional | — | `collector/slack.go:28` | Slack Bot User OAuth token |
| `SLACK_TEAM_ID` | Optional | — | `collector/slack.go:28` | Slack team ID |
| `GITHUB_TOKEN` | Optional | — | `collector/github.go:26` | GitHub Personal Access Token |
| `GITHUB_ORG` | Optional | — | `collector/github.go:26` | GitHub organization name |
| `GDRIVE_CREDENTIALS_JSON` | Optional | — | `collector/gdrive.go` | Google Drive service account JSON |
| `NOTION_TOKEN` | Optional | — | `config/config.go:37` | Notion token (currently unused) |
| `API_KEY` | Optional | — | `api/middleware.go:15` | Bearer token authentication (disabled when empty) |

### Values Injected from ConfigMap (`deploy/k8s/second-brain-configmap.yaml`)

| Key | Value |
|---|---|
| `COLLECT_INTERVAL` | `5m` |
| `MAX_EMBED_CHARS` | `8000` |
| `EMBEDDING_API_URL` | `https://api.openai.com/v1` |
| `EMBEDDING_MODEL` | `text-embedding-3-small` |
| `CLIPROXY_AUTH_FILE` | `/etc/cliproxy/auth.json` |
| `FILESYSTEM_PATH` | `/data/drive` |
| `FILESYSTEM_ENABLED` | `true` |
| `PORT` | `8080` |
| `MIGRATIONS_DIR` | `/app/migrations` |

### Secret Management Principles

| Type | Management Method |
|---|---|
| `second-brain-secret` | `deploy/k8s/second-brain-secret.yaml` contains placeholders → replaced with real values at deploy time |
| `cliproxy-auth-secret` | Out-of-band manual `kubectl create secret` — not included in git |

### Frontend (`web/.env.example`)

| Environment Variable | Required | Description |
|---|---|---|
| `BRAIN_API_URL` | Optional | Backend service URL (K8s internal: `http://second-brain:8080`) |
| `NEXT_PUBLIC_API_URL` | Optional | Client-accessible URL (not recommended for use) |
| `API_KEY` | Optional | Backend Bearer token (server-side only) |

### 11.1 cliproxy Integration

**Purpose**: Proxy OpenAI-compatible API calls (embeddings, chat completions) through ChatGPT Pro / Claude Pro / Gemini OAuth credentials. second-brain treats cliproxy as a **local HTTP proxy**; cliproxy itself handles OAuth token refresh, upstream routing, and rate limiting.

#### Deployment Model

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

    pod[second-brain Pod<br/>:8080] -- "HTTP<br/>Bearer inbound key" --> cliproxy[cliproxy<br/>127.0.0.1:8317]
    cliproxy -- "OAuth access_token<br/>auto-refresh" --> upstream[(OpenAI API<br/>chat.openai.com)]

    subgraph host["host (baekenough-ubuntu24)"]
      cliproxy
      auth["~/.cli-proxy-api/<br/>codex-*.json<br/>claude-*.json<br/>gemini-*.json"]
      cliproxy -.-> auth
    end

    subgraph minikube["minikube docker driver"]
      pod
    end

    pod -->|host.minikube.internal:8317| cliproxy

    class pod focal;
    class upstream ext;
    class auth muted;
```

#### Authentication Layers (2-Stage)

1. **Inbound (second-brain → cliproxy)**: Static API key registered in cliproxy's `config.yaml` under `api-keys`. Stored in environment variables `LLM_API_KEY` and `EMBEDDING_API_KEY`.
2. **Upstream (cliproxy → OpenAI/Claude/Gemini)**: OAuth access_token from `~/.cli-proxy-api/*.json`. cliproxy auto-refreshes these; second-brain requires no involvement.

#### Environment Variable Mapping

| Variable | Purpose | Example Server Value |
|---|---|---|
| `LLM_API_URL` | Chat completions endpoint | `http://host.minikube.internal:8317/v1` |
| `LLM_API_KEY` | Inbound API key | Key from cliproxy `config.yaml` |
| `LLM_MODEL` | Model identifier | `gpt-codex-5.3` |
| `EMBEDDING_API_URL` | Embeddings endpoint (returns 404 if unsupported → falls back to FTS) | Same as above |
| `EMBEDDING_API_KEY` | Inbound API key (may be same as LLM) | Same as above |
| `CLIPROXY_AUTH_FILE` | Deprecated — static key path takes priority | unused |

#### Supported Endpoints

| Path | cliproxy | OpenAI Direct | Current second-brain |
|---|---|---|---|
| `/v1/chat/completions` | ✅ | ✅ | Uses cliproxy |
| `/v1/embeddings` | ❌ (404) | ✅ | Falls back to FTS, routing decision pending (#34) |
| `/v1/models` | ✅ | ✅ | Not used |

#### Failure Modes

| Scenario | Symptom | Remediation |
|---|---|---|
| cliproxy down | All LLM calls fail → Discord mention responses error | `pm2 restart cli-proxy-api` |
| Inbound key mismatch | `401 Invalid API key` | Verify `LLM_API_KEY` / `EMBEDDING_API_KEY` match cliproxy `config.yaml` |
| Embeddings call | `404 page not found` | Expected — handled by FTS fallback |
| OAuth token expiry | cliproxy auto-refreshes | No impact on second-brain |

#### Operational Commands

```bash
# Check cliproxy status
pm2 list | grep cli-proxy-api
pm2 logs cli-proxy-api

# Restart cliproxy
pm2 restart cli-proxy-api

# systemd user service (currently disabled, switchable)
systemctl --user status cliproxy.service
```

**Related Issues**: #33 (this documentation) · #34 (embedding routing decision) · #4 (JWT deprecation — replaced by cliproxy)

---

## 12. Architecture Decision Records

### ADR-001: pgvector Hybrid Search (RRF Fusion)

**Context**: Semantic search (vector) alone produces poor keyword precision for natural-language queries, while full-text search (FTS) alone misses semantically similar documents.

**Decision**: Combine FTS and vector search results using the Reciprocal Rank Fusion (RRF) algorithm. k=60 constant, FULL OUTER JOIN to include results that appear in only one of the two sets (`store/document.go:296`).

**Consequences**: Search quality improves over either approach alone. Each query incurs an OpenAI embedding API call, increasing latency. Embedding failures fall back to FTS, maintaining availability. Reranking and HyDE are planned for Phase 3.

---

### ADR-002: pgvector on Single Postgres 16 Instance

**Context**: Separating the vector store and relational metadata (e.g., Pinecone + RDS) significantly increases operational complexity.

**Decision**: Single `pgvector/pgvector:pg16` StatefulSet instance. Data persistence via PVC (10Gi ReadWriteOnce).

**Consequences**: Simplified operations, single backup target. Type-safe vector operations via the Go + pgx/v5 + pgvector-go stack. Migration to a dedicated vector database may be warranted at hundreds of millions of vectors.

---

### ADR-003: Go + Next.js Separate Services, App Router Proxy

**Context**: Integrating the backend and frontend into a single server reduces development velocity and deployment independence.

**Decision**: Go HTTP server (backend) and Next.js App Router (frontend) run as independent services. The `web/src/app/api/` routes server-side proxy to the BRAIN backend, avoiding CORS issues and hiding the backend address from clients.

**Consequences**: Services can be deployed independently. An additional Next.js API route layer is introduced, but client code is simplified.

---

### ADR-004: minikube docker driver + port-forward

**Context**: Local development must use the same K8s manifests as production and requires multi-container orchestration.

**Decision**: minikube with the docker driver is the standard local environment. Because the macOS docker driver does not expose NodePort directly on the host, `kubectl port-forward` is the recommended access method.

**Consequences**: Local environment is identical to production manifests. `eval $(minikube docker-env)` is required when building images to use the internal Docker daemon.

---

### ADR-005: CliProxy OAuth Token Mounted as K8s Secret Volume

**Context**: ChatGPT Plus Codex OAuth tokens rotate regularly; injecting them as environment variables requires a Pod restart on every renewal.

**Decision**: `cliproxy-auth-secret` is managed out-of-band and mounted as a volume at `/etc/cliproxy/auth.json` inside the Pod. `cliProxyToken` re-reads the file with a 5-minute TTL (`embed.go:46`).

**Consequences**: Token renewal does not require a Pod restart. The Secret is not in git; manual `kubectl create secret` is required on each deployment.

---

### ADR-006: `-trimpath` Build + `MIGRATIONS_DIR` env

**Context**: Embedding absolute paths in the Go binary reduces build reproducibility and creates a security exposure.

**Decision**: Build with `go build -trimpath`. The migrations directory is injected via the `MIGRATIONS_DIR` environment variable (`cmd/server/main.go:153`). Set to `/app/migrations` in the ConfigMap.

**Consequences**: Improved build reproducibility. `runtime.Caller(0)` returns a module-relative path, requiring a `filepath.IsAbs()` check with fallback handling (main.go:163).

---

### ADR-007: `atomic.Bool` CAS Scheduler Mutex

**Context**: A cron tick or manual trigger may attempt to start a duplicate collection run while one is already in progress.

**Decision**: Non-blocking skip implemented with `sync/atomic.Bool.CompareAndSwap(false, true)` (`scheduler.go:57`). CAS is chosen over `sync.Mutex` to eliminate the risk of deadlocks.

**Consequences**: Simple code, no deadlocks. Distributed environments (multi-Pod) would require an external lock such as Redis. Sufficient for the current single-Pod setup.

---

### ADR-008: Shared `SanitizeText` Function (Postgres 0x00 Avoidance)

**Context**: Postgres `text` type cannot store NULL bytes (`\x00`). PDF and Office files may contain invalid UTF-8 sequences.

**Decision**: `SanitizeText` is applied universally to all extractor output (`extractor.go:92`). Order: remove NULL bytes → replace invalid UTF-8 → compress excessive newlines.

**Consequences**: Eliminates DB storage errors. Minimal information loss (NULL bytes, invalid bytes) with no meaningful impact on search quality.

---

### ADR-009: `MAX_EMBED_CHARS` env (Temporary Mitigation Before Chunking)

**Context**: OpenAI embedding API token limit (8191 tokens). Large files can exceed this limit when embedded as a whole.

**Decision**: Character-count truncation via the `MAX_EMBED_CHARS` environment variable (default 8000) (`scheduler.go:25`). Temporary mitigation until Phase 1 chunking is implemented.

**Consequences**: The tail of long documents is absent from embeddings, reducing recall. This environment variable will be removed once chunking is implemented (Phase 1 TODO comment: scheduler.go:213).

---

### ADR-010: Direct Bearer with OpenAI ChatGPT Codex OAuth JWT

**Context**: Using ChatGPT Plus Codex OAuth tokens without an OpenAI API key. Previously believed to require a middleware daemon (`cli-proxy-api`).

**Decision**: A JWT with `iss: auth.openai.com`, `aud: api.openai.com/v1` is used directly as a Bearer header on `/v1/embeddings`. Direct operation has been verified without a daemon.

**Consequences**: Reduced infrastructure complexity (no daemon required). This is an unofficial behavior and may break if OpenAI policy changes. A proper API key is recommended for production.

---

### ADR-011: Slack DMs Excluded by Design (Privacy)

**Context**: Slack DMs are private conversations; collecting them raises privacy concerns.

**Decision**: The `users.conversations` API is called with explicit `types=public_channel,private_channel` parameters, excluding IM (DM) and MPIM (group DM) channels from enumeration (`slack.go:183`). Only channels where the bot is a member are collected, so uninvited channels are automatically excluded.

**Consequences**: DMs between team members are not searchable. The team knowledge base includes only public and private channel conversations. The explicit exclusion is documented in code.

---

### ADR-012: Google Drive FS Scan (vs. Full Drive API)

**Context**: Scanning all of Google Drive via the Drive API consumes API quota and increases authentication complexity.

**Decision**: The Drive folder is exposed as a filesystem via minikube mount, and `FilesystemCollector` scans it like local files. Google Workspace stub files (`.gdoc`, etc.) have their content exported through `DriveExporter` only when ADC is configured (`filesystem.go:389`). Without ADC, only URL metadata is stored.

**Consequences**: No API quota issues. The minikube mount must remain running (background process). Workspace file content is indexed selectively depending on whether ADC is configured.

---

## 13. Known Issues

Tracked as GitHub issues. Full list: https://github.com/baekenough/second-brain/issues

### P0 Bugs (Immediate Fix Required)

| # | Title | Related File |
|---|---|---|
| [#1](https://github.com/baekenough/second-brain/issues/1) | Prevent concurrent execution in Scheduler run() | `internal/scheduler/scheduler.go` |
| [#2](https://github.com/baekenough/second-brain/issues/2) | Implement multi-stage PDF fallback chain | `internal/collector/*/pdf.go` |
| [#3](https://github.com/baekenough/second-brain/issues/3) | Remove 8KB text truncation and switch to chunk-based embedding | `internal/scheduler/scheduler.go` |
| [#4](https://github.com/baekenough/second-brain/issues/4) | Migrate to permanent OpenAI embedding API key | Operations |
| [#5](https://github.com/baekenough/second-brain/issues/5) | Slack rate limit: exponential backoff + respect Retry-After | `internal/collector/slack.go` |
| [#6](https://github.com/baekenough/second-brain/issues/6) | Remove host dependency of minikube hostPath Google Drive mount | `deploy/k8s/*pv*.yaml` |
| [#7](https://github.com/baekenough/second-brain/issues/7) | Handle 9p mount lstat failure for Korean long filenames | `internal/collector/filesystem.go` |

### P1 Bugs

| # | Title |
|---|---|
| [#8](https://github.com/baekenough/second-brain/issues/8) | extraction_failures tracking table + retry worker |

### Pending Decisions

| # | Title |
|---|---|
| [#25](https://github.com/baekenough/second-brain/issues/25) | Decide PDF OCR bundling strategy |
| [#26](https://github.com/baekenough/second-brain/issues/26) | Decide LLM model/API/cost budget for summarization |

### Chore / Blockers

| # | Title |
|---|---|
| [#21](https://github.com/baekenough/second-brain/issues/21) | Set up CI/CD (.github/workflows/) |
| [#22](https://github.com/baekenough/second-brain/issues/22) | Activate GitHub collection integration |
| [#23](https://github.com/baekenough/second-brain/issues/23) | Activate Notion collection integration |
| [#24](https://github.com/baekenough/second-brain/issues/24) | Activate Google Workspace export |

---

## 14. Roadmap

Tracked as GitHub issues. Each phase epic is composed of the issues listed below.

### Phase 0: Baseline (Current Measurement)

| # | Title |
|---|---|
| [#10](https://github.com/baekenough/second-brain/issues/10) | Measure baseline metrics |

### Phase 1: RAG Foundation + Bug Fixes

| # | Title |
|---|---|
| [#9](https://github.com/baekenough/second-brain/issues/9) | chunks table migration and chunk-level embedding |
| [#1](https://github.com/baekenough/second-brain/issues/1)~[#8](https://github.com/baekenough/second-brain/issues/8) | P0/P1 bug fixes (see section 13 above) |

### Phase 2: Semantic Enhancement

| # | Title |
|---|---|
| [#11](https://github.com/baekenough/second-brain/issues/11) | Section/header-based semantic chunking |
| [#12](https://github.com/baekenough/second-brain/issues/12) | Add LLM summary columns (title_summary, bullet_summary) |
| [#13](https://github.com/baekenough/second-brain/issues/13) | Summary-dedicated embedding pipeline |

### Phase 3: Search Quality

| # | Title |
|---|---|
| [#14](https://github.com/baekenough/second-brain/issues/14) | BGE-reranker cross-encoder integration |
| [#15](https://github.com/baekenough/second-brain/issues/15) | HyDE query expansion |
| [#16](https://github.com/baekenough/second-brain/issues/16) | Hybrid search weight auto-tuning |

### Phase 4: Self-Improving Loop

| # | Title |
|---|---|
| [#17](https://github.com/baekenough/second-brain/issues/17) | User feedback table + collection API |
| [#18](https://github.com/baekenough/second-brain/issues/18) | Automated eval set construction |
| [#19](https://github.com/baekenough/second-brain/issues/19) | Nightly eval + regression detection pipeline |
| [#20](https://github.com/baekenough/second-brain/issues/20) | Threshold-based automatic re-indexing |

---

*Last updated: 2026-04-15*
