# second-brain Architecture

> Vision: LLM-curated private search engine. A RAG infrastructure that collects and embeds personal and team knowledge from Google Drive, Slack, GitHub, SMS, Gmail, and more, delivering curated search results to AI agents.

---

## Table of Contents

1. [Overview](#1-overview)
2. [System Topology](#2-system-topology)
3. [Service Layer Map](#3-service-layer-map)
4. [Data Model](#4-data-model)
5. [Collection Pipeline](#5-collection-pipeline)
6. [Extraction Pipeline](#6-extraction-pipeline)
7. [Embedding Pipeline](#7-embedding-pipeline)
8. [Search Pipeline](#8-search-pipeline)
9. [Eval Self-improvement Loop](#9-eval-self-improvement-loop)
10. [Deployment Architecture](#10-deployment-architecture)
11. [Web UI Architecture](#11-web-ui-architecture)
12. [Configuration and Environment Variables](#12-configuration-and-environment-variables)
13. [Architecture Decision Records](#13-architecture-decision-records)
14. [Known Issues](#14-known-issues)
15. [Implemented Items](#15-implemented-items)

---

## 1. Overview

second-brain is a personal and team knowledge search platform built on a Go backend and a Next.js frontend UI.

**Target users**: Anyone who wants to search team documents, Slack conversations, GitHub issues and PRs, SMS messages, call logs, Gmail, and Calendar entries using natural language.

**Core design philosophy**: Dual binary (API server + collector daemon) → document collection → rune-based chunking → OpenAI embedding → 5-lane hybrid search (FTS + pgvector + pg_bigm + summary embedding + entity RRF) → LLM curation (re-ranking + summary). Optional BGE cross-encoder rerank and HyDE query expansion.

**Non-functional requirements**:

| Item | Target |
|---|---|
| Search latency | p99 < 500ms (without reranking) |
| Collection idempotency | `ON CONFLICT(source_type, source_id) DO UPDATE` |
| Privacy | Slack DM non-collection design (`users.conversations` — IM channels explicitly excluded) |
| Security | Bearer token authentication, timing-safe comparison (`subtle.ConstantTimeCompare`) |
| Deletion safety | 3-layer soft-delete mass-deletion guard (filesystem root stat / 50% ratio / no-op) |
| Migrations | Sequential application under advisory lock (001–019, multi-instance safe) |

**Runtime environment**: Production runs on **Mac mini docker-compose** (`docker-compose.local.yml`). `deploy/k8s/` contains Kustomize manifests for a future Kubernetes migration (currently unused).

---

## 2. System Topology

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

    subgraph HOST["Mac mini Host (macOS arm64)"]
        ANDROID["Android App\nsecond-brain-push\nSMS / calls / recordings"]
        BROWSER["Browser\nlocalhost:3000"]
        STAGE["~/.second-brain/stage/\nsms / call (OneDrive bridge)"]
    end

    subgraph COMPOSE["docker-compose.local.yml"]
        subgraph CORE["Core Services"]
            SERVER["server\nGo API :8080\n(→ host :8081)"]
            COLLECTOR["collector\nGo collector daemon"]
            MCP["mcp\nMCP HTTP :8090"]
            EVALRUNNER["eval-runner\nnightly 03:00 KST"]
            PG["postgres\nPostgreSQL 16\n+ pgvector + pg_bigm\npgdata volume"]
        end
        subgraph AI["AI Services"]
            OLLAMA["ollama\ngemma3:12b-it-qat\nollama_models volume"]
            WHISPER["whisper\nfaster-whisper-medium\nint8 CPU :8000"]
            DIARIZATION["diarization\npyannote.audio\n:8001"]
        end
        WEB["web\nNext.js standalone\n:3000"]
    end

    subgraph EXTERNAL["External APIs"]
        OPENAI["OpenAI API\ntext-embedding-3-small"]
        SLACK["Slack Web API"]
        GITHUB["GitHub REST API"]
        WEBHOOK["Alert Webhook\n(Slack)"]
    end

    subgraph MOUNTS["Host Mounts (read-only)"]
        SECRETARYDB["secretary.db\n(SQLite)"]
        LLMMEMORY["llm-memory.sqlite"]
        GMAIL["/data/gmail"]
        CALENDAR["/data/calendar"]
    end

    BROWSER --> WEB
    WEB --> SERVER
    ANDROID -->|POST /ingest/messages\nPOST /ingest/recording| SERVER
    SERVER --> PG
    SERVER --> OLLAMA
    MCP --> PG
    EVALRUNNER --> PG
    EVALRUNNER --> WEBHOOK
    COLLECTOR --> PG
    COLLECTOR --> OPENAI
    COLLECTOR --> SLACK
    COLLECTOR --> GITHUB
    COLLECTOR --> WHISPER
    COLLECTOR --> DIARIZATION
    COLLECTOR --> OLLAMA
    COLLECTOR -.-> SECRETARYDB
    COLLECTOR -.-> LLMMEMORY
    COLLECTOR -.-> GMAIL
    COLLECTOR -.-> CALENDAR
    STAGE -.->|sms / call staging| COLLECTOR

    class SERVER,COLLECTOR focal;
    class PG data;
    class OPENAI,SLACK,GITHUB,WEBHOOK ext;
    class OLLAMA,WHISPER,DIARIZATION,WEB,MCP,EVALRUNNER,ANDROID muted;
```

> **eraser render** ([edit](https://app.eraser.io/workspace/PyHgjPmM97MYtJNoVD5H)):
>
> ![System Runtime Topology](docs/diagrams/01-system-runtime-topology.png)

### docker-compose.local.yml Services

| Service | Image | Port | Role |
|---------|-------|------|------|
| postgres | `second-brain-postgres:local` | 5432 (internal) | PostgreSQL 16 + pgvector + pg_bigm |
| server | `second-brain-server:local` | 8081→8080 | API server; applies DB migrations |
| collector | `second-brain-collector:local` | — | Collection daemon, per-source schedule |
| mcp | `second-brain-mcp:local` | 8090 | MCP streamable HTTP server |
| eval-runner | `second-brain-eval:local` | — | Nightly eval scheduler (03:00 KST) |
| ollama | `ollama/ollama:latest` | 11434 (internal) | Local LLM (gemma3:12b-it-qat) |
| whisper | `fedirz/faster-whisper-server:latest-cpu` | 8000 (internal) | Whisper ASR (int8 CPU) |
| diarization | `second-brain-diarization:local` | 8001 (internal) | pyannote.audio speaker diarization |
| web | `second-brain-web:local` | 3000 | Next.js frontend |

---

## 3. Service Layer Map

### Backend Package Dependencies

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

    CMD["cmd/server + cmd/collector\n+ cmd/mcp + cmd/eval\nDI wiring, signal handling"]
    CFG["internal/config\nConfig.Load()"]
    API["internal/api\nchi Router\nendpoints + middleware\ncollect_status / reindex_check"]
    SCHED["internal/scheduler\nScheduler\nDeleteGuard (50% ratio)\nFreshnessChecker"]
    COL["internal/collector\nFilesystem / Slack / GitHub\nSMS / Recording / Secretary\nGmail / Calendar / LlmMemory"]
    CHUNKER["internal/chunker\nrune-based chunking\nHeadingAware / adaptive"]
    SEARCH["internal/search\nService\nHyDE / Reranker / EntityFetcher\nEmbedClient (static/cliProxy)"]
    STORE["internal/store\nDocumentStore (5-lane hybridSearch)\nEntityStore\nCollectionStatus\nChunkStore"]
    MODEL["internal/model\nDocument / SearchQuery\nEntity / SearchWeights"]
    LLM["internal/llm\nCompleter (Ollama/OpenAI)\ncuration + HyDE + entity extract"]
    WORKER["internal/worker\nasync embedding backfill"]

    CMD --> CFG
    CMD --> STORE
    CMD --> SEARCH
    CMD --> COL
    CMD --> SCHED
    CMD --> API
    CMD --> LLM
    API --> STORE
    API --> SEARCH
    API --> SCHED
    SCHED --> COL
    SCHED --> STORE
    SCHED --> SEARCH
    SCHED --> CHUNKER
    SCHED --> LLM
    SCHED --> WORKER
    SEARCH --> STORE
    SEARCH --> LLM
    COL --> MODEL
    COL --> CHUNKER
    STORE --> MODEL
    API --> MODEL
    WORKER --> STORE

    class CMD focal;
    class STORE data;
    class MODEL,CFG muted;
```

> **eraser render** ([edit](https://app.eraser.io/workspace/Z8JviN6EySSKzjgtXNp4)):
>
> ![Backend Service Layer Map](docs/diagrams/02-service-layer-map.png)

### Key Symbols by Package

| Package | Key Types / Functions | Files |
|---|---|---|
| `internal/api` | `Server`, `Handler()`, `requireAPIKey()`, `collectStatusHandler()`, `FreshnessChecker` | `router.go`, `collect_status.go`, `search.go` |
| `internal/chunker` | `Options{TargetSize, MaxSize, Overlap, HeadingAware}`, `Chunk()`, `SelectOptions()` | `chunker.go`, `adaptive.go` |
| `internal/search` | `Service.Search()`, `WithHyDE()`, `WithReranker()`, `WithEntityFetcher()`, `EmbedClient`, `HTTPReranker` | `search.go`, `embed.go`, `rerank.go`, `hyde.go`, `tune.go` |
| `internal/store` | `DocumentStore`, `hybridSearch()` (5-lane CTE), `CollectionStatus()`, `EntityStore`, `CountActiveDocuments()` | `document.go`, `collection_status.go`, `entities.go` |
| `internal/scheduler` | `Scheduler`, `deletionRatioThreshold=0.50`, `WithEntityExtraction()` | `scheduler.go`, `scheduler_deletion_guard*.go` |
| `internal/llm` | `Completer` interface (Ollama / OpenAI chat completions), entity extraction, HyDE prompt | `llm.go` |
| `internal/model` | `Document`, `SearchQuery{UseHyDE, UseRerank}`, `Entity`, `SearchWeights` | `document.go` |

---

## 4. Data Model

### ERD (migrations 001–019)

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
        text source_type "NOT NULL — filesystem|slack|github|sms|recording|secretary|gmail|calendar|llm_memory"
        text source_id "NOT NULL — unique per source; sms:{ms}:{addrHash}:{direction}"
        text title "NOT NULL"
        text content "NOT NULL"
        jsonb metadata "default '{}'"
        vector_1536 embedding "nullable — text-embedding-3-small"
        vector_1536 summary_embedding "nullable — summary embedding lane (#013)"
        text title_summary "nullable — LLM summary title (#012)"
        text bullet_summary "nullable — LLM bullet summary (#012)"
        tsvector tsv "GENERATED STORED — english(A+B) + simple(A+B)"
        timestamptz collected_at "NOT NULL — file ModTime or collection time"
        timestamptz created_at "default now()"
        timestamptz updated_at "default now()"
        text status "default 'active' — active|deleted|moved"
        timestamptz deleted_at "nullable"
        timestamptz entity_processed_at "nullable — entity extraction timestamp (#018)"
    }

    chunks {
        bigserial id PK
        uuid document_id FK "→ documents.id ON DELETE CASCADE"
        int chunk_index "NOT NULL"
        text content "NOT NULL"
        tsvector content_tsv "GENERATED — to_tsvector('simple', content)"
        int byte_size "NOT NULL"
        vector_1536 embedding "nullable — per-chunk embedding (#015)"
        timestamptz created_at "default now()"
    }

    entities {
        bigserial id PK
        text name "NOT NULL"
        text type "NOT NULL — PERSON|ORG|CONCEPT|OTHER"
        text normalized_name "NOT NULL — lower(trim(name))"
        timestamptz created_at "default now()"
    }

    document_entities {
        uuid document_id FK "→ documents.id ON DELETE CASCADE"
        bigint entity_id FK "→ entities.id ON DELETE CASCADE"
    }

    collection_log {
        uuid id PK
        text source_type "NOT NULL"
        timestamptz started_at "NOT NULL"
        timestamptz finished_at "nullable"
        int documents_count "default 0"
        text error "nullable"
        timestamptz created_at "default now()"
    }

    feedback {
        bigserial id PK
        text query "nullable"
        uuid document_id FK "→ documents.id ON DELETE SET NULL"
        bigint chunk_id FK "→ chunks.id ON DELETE SET NULL"
        text source "NOT NULL — search|discord_bot|api"
        text user_id "nullable"
        smallint thumbs "NOT NULL — +1|-1|0"
        text comment "nullable"
        jsonb metadata "default '{}'"
        timestamptz created_at "NOT NULL default now()"
    }

    eval_metrics {
        serial id PK
        double ndcg5 "NOT NULL"
        double ndcg10 "NOT NULL"
        double mrr10 "NOT NULL"
        int pairs "NOT NULL — eval set size"
        timestamptz run_at "NOT NULL default now()"
    }

    documents ||--o{ chunks : "document_id"
    documents ||--o{ document_entities : "document_id"
    entities ||--o{ document_entities : "entity_id"
    documents ||--o{ feedback : "document_id"
    chunks ||--o{ feedback : "chunk_id"
```

> **eraser render** ([edit](https://app.eraser.io/workspace/O901Iet3HpcIaldLfQ1e)):
>
> ![Data Model ERD](docs/diagrams/03-data-model-erd.png)

### Migration List (001–019)

| # | File | Key Change |
|---|------|-----------|
| 001 | `001_init.sql` | documents, collection_log, HNSW index, tsvector GENERATED |
| 002 | `002_soft_delete.sql` | status, deleted_at columns + index |
| 003 | `003_extraction_failures.sql` | Extraction failure tracking |
| 004 | `004_chunks.sql` | chunks table + content_tsv GIN |
| 005 | `005_feedback.sql` | feedback table (thumbs, query, session_id) |
| 006 | `006_bigm.sql` | pg_bigm extension + GIN bigm index |
| 007 | `007_eval_metrics.sql` | eval_metrics (ndcg5, ndcg10, mrr10) |
| 008 | `008_reindex_state.sql` | Reindex state tracking |
| 009 | `009_collector_state.sql` | Collector state (incremental cursor) |
| 010 | `010_occurred_at.sql` | SMS/call occurred_at column |
| 011 | `011_configurable_embedding_dim.sql` | Configurable embedding dimension |
| 012 | `012_summary_columns.sql` | title_summary, bullet_summary columns |
| 013 | `013_summary_vector.sql` | summary_embedding vector(1536) + HNSW |
| 014 | `014_unsummarized_index.sql` | Partial index for unsummarized documents |
| 015 | `015_chunk_embeddings.sql` | chunks.embedding vector(1536) + HNSW |
| 016 | `016_eval_latency.sql` | Eval run latency tracking |
| 017 | `017_entities.sql` | entities + document_entities tables |
| 018 | `018_entity_processed_at.sql` | documents.entity_processed_at column |
| 019 | `019_sms_sourceid_rekey.sql` | SMS source_id rekey: bodyHash → direction |

### Key Indexes on `documents`

| Index | Type | Target | Purpose |
|---|---|---|---|
| `(UNIQUE)` | UNIQUE | `(source_type, source_id)` | Upsert conflict key |
| `idx_documents_tsv` | GIN | `tsv` | BM25 full-text search |
| `idx_documents_embedding` | HNSW | `embedding vector_cosine_ops` | Cosine ANN |
| `idx_documents_summary_embedding` | HNSW | `summary_embedding vector_cosine_ops` | Summary ANN |
| `idx_documents_bigm_content` | GIN bigm | `content gin_bigm_ops` | pg_bigm 2-gram |
| `idx_documents_bigm_title` | GIN bigm | `title gin_bigm_ops` | pg_bigm title |
| `idx_documents_entity_processed_at` | B-tree | `entity_processed_at` | Unprocessed document lookup |

---

## 5. Collection Pipeline

### End-to-End Sequence (IndexAware + Deletion Guard)

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
    participant CRON as robfig/cron<br/>or POST /collect/trigger
    participant SCHED as Scheduler.run()
    participant CAS as atomic.Bool (CAS)
    participant RC as runCollector()
    participant COL as Collector.Collect(ctx, since)
    participant CHUNK as chunker.Chunk()
    participant EMBED as embedDocuments()
    participant STORE as DocumentStore.Upsert()
    participant GUARD as DeletionGuard (fs only)
    participant FRESH as FreshnessChecker

    CRON->>SCHED: tick (per-source COLLECT_INTERVAL)
    SCHED->>CAS: CompareAndSwap(false→true)
    alt already running
        CAS-->>SCHED: false → warn + return
    else
        CAS-->>SCHED: true
        SCHED->>RC: runCollector(col)
        RC->>STORE: LastCollectedAt(instanceID, source_type)
        STORE-->>RC: since timestamp
        RC->>COL: Collect(ctx, since)
        COL-->>RC: []model.Document

        loop each Document
            RC->>CHUNK: Chunk(text, adaptive.SelectOptions(sourceType))
            CHUNK-->>RC: []string chunks
            RC->>STORE: Upsert(doc + chunks)
        end

        RC->>EMBED: embedDocuments(batch=20)
        loop batches of 20
            EMBED->>OpenAI: POST /v1/embeddings
            OpenAI-->>EMBED: []float32[1536]
        end

        RC->>STORE: RecordCollectionLog(src, started, count, err)

        opt FilesystemCollector only
            RC->>STORE: CountActiveDocuments(source_type)
            STORE-->>RC: activeInDB
            RC->>COL: ListActiveSourceIDs()
            COL-->>RC: activeOnFS (filesystem walk)

            alt (activeInDB - activeOnFS) / activeInDB > 0.50
                RC->>RC: slog.Warn("deletion ratio exceeded") — SKIP MarkDeleted
            else
                RC->>STORE: MarkDeleted(source_type, activeOnFS)
            end
        end

        SCHED->>CAS: Store(false)
        SCHED->>FRESH: Check(ctx)
        FRESH->>STORE: CollectionStatus(ctx)
        FRESH-->>SCHED: alert if stale or consecutive failures
    end
```

> **eraser render** ([edit](https://app.eraser.io/workspace/2NzpSSDbSx4Oh3gCs6DV)):
>
> ![Collection Pipeline Sequence](docs/diagrams/04-collection-pipeline-sequence.png)

### Scheduler Structure (`internal/scheduler/scheduler.go`)

```go
type Scheduler struct {
    cron       *cron.Cron       // robfig/cron v3, seconds resolution
    collectors []collector.Collector
    store      DocumentUpserter // includes CountActiveDocuments (required, #148)
    embed      *search.EmbedClient
    running    atomic.Bool      // CAS mutex
    freshness  *api.FreshnessChecker // staleness monitor (#137)
}
```

**Deletion guard (`deletionRatioThreshold = 0.50`)**: After a filesystem collection run, if the ratio of documents to be deleted to currently active documents exceeds 50%, `MarkDeleted` is skipped and a warning is logged. `CountActiveDocuments` is promoted to a required method on the `DocumentUpserter` interface to ensure the guard is always enforced at compile time (#148).

### Collector Details

#### FilesystemCollector

| Item | Value |
|---|---|
| Root path | `FILESYSTEM_PATH` env |
| Incremental basis | `info.ModTime().After(since)` |
| Skipped directories | `.git`, `node_modules`, `dist`, `.next`, `.omc`, `.sisyphus`, `.claude` |
| Text file limit | 512 KB |
| DeletionDetector | `ListActiveSourceIDs()` implemented |

#### SlackCollector

| Item | Value |
|---|---|
| Channel scope | `users.conversations` — bot-member channels only |
| DM exclusion | `types=public_channel,private_channel` — IM channels explicitly excluded |
| Threads | `reply_count > 0` → `conversations.replies` → separate Document |
| SourceID | `{channel_id}:{ts}` |

#### SMSCollector / RecordingCollector

SMS and calls: The Android second-brain-push app sends data via `POST /api/v1/ingest/messages`. Source ID format: `sms:{dateMs}:{sha256(addr)[:16]}:{direction}`. Migration 019 re-keyed existing bodyHash-based source IDs.

Recordings: Received as `.m4a` multipart via `POST /api/v1/ingest/recording` → Whisper ASR → text conversion and storage. Corrupted audio is isolated (#152).

#### secretary / gmail / calendar / llm-memory

Host SQLite files and directories are accessed directly via docker volume read-only mounts. The secretary collector was retired in v0.20.x (#151).

---

## 6. Extraction Pipeline

The `Registry` in `internal/collector/extractor/` selects an `Extractor` by file extension.

```go
type Extractor interface {
    Supports(ext string) bool
    Extract(ctx context.Context, absPath string) (string, error)
}
```

Registry registration order: `HTMLExtractor` → `PDFExtractor` → `DocxExtractor` → `XlsxExtractor` → `PptxExtractor` → `HwpxExtractor`

### SanitizeText

Applied to all extractor output:
1. `\x00` → `" "` — prevents Postgres TEXT storage errors
2. `strings.ToValidUTF8(s, "")` — replaces invalid UTF-8 sequences
3. Consecutive `\n\n\n` → `\n\n` compression

### Content Size Hierarchy

| Layer | Limit |
|---|---|
| Inline text file read | 512 KB |
| Extractor output (`MaxExtractedBytes`) | 512 KB |
| XLSX intermediate buffer (`xlsxMaxBytes`) | 200 KB |
| Embedding input (`defaultMaxEmbedChars`) | 8000 chars (default) |
| Raw file API | 50 MiB |

### Rune-based Chunking (`internal/chunker/`)

```go
type Options struct {
    TargetSize  int  // target chunk size in runes (default: 2000)
    MaxSize     int  // hard chunk size limit in runes (default: 4000)
    Overlap     int  // rune overlap between chunks (default: 100)
    HeadingAware bool // section splitting on Markdown/HTML headings
}
```

Using runes (rather than bytes) ensures equal information density for Korean (3 bytes/rune) and English (1 byte/rune) text (#145). `adaptive.SelectOptions(sourceType)` automatically selects optimized options per source type.

---

## 7. Embedding Pipeline

### Token Source Priority (`internal/search/embed.go`)

```go
switch {
case apiKey != "":        // Priority 1: EMBEDDING_API_KEY
    ts = &staticToken{t: apiKey}
case authFilePath != "": // Priority 2: CliProxy JSON (5-minute TTL auto-refresh)
    ts = newCliProxyToken(authFilePath)
}
// Priority 3: No Authorization header (compatible with self-hosted endpoints)
```

### Embedding Flow

```
embedDocuments(batch=20)
  └─ Each Document:
       text = title + "\n\n" + content
       if len(text) > maxLen: text = text[:maxLen] + warn
       EmbedBatch(ctx, texts[20]) → OpenAI /v1/embeddings
         → []float32[1536]
       docs[i].Embedding = vecs[i]

chunk embeddings (#015):
  └─ Each Chunk:
       EmbedBatch(ctx, chunkTexts[20]) → chunk.embedding
```

**On embedding failure**: A batch error does not abort the entire collection run. The failed batch is skipped with a WARN log. The worker package handles async backfill (#141).

### Query Embedding at Search Time

```go
// search.go
if q.UseHyDE && s.llmClient != nil {
    hydeDoc := s.llmClient.Complete(ctx, hydePrompt(q.Query))
    vec, _ = s.embed.Embed(ctx, hydeDoc)  // embed hypothetical document
} else {
    vec, _ = s.embed.Embed(ctx, q.Query)  // embed query directly
}
q.Embedding = vec
```

---

## 8. Search Pipeline

### 5-lane Hybrid Search RRF

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
    classDef lane fill:#f0ebe3,stroke:#57534e,color:#57534e,stroke-width:1px;
    classDef data fill:#faf7f2,stroke:#1c1917,color:#1c1917,stroke-width:1.2px;

    QUERY["User Query"]
    HYDE["HyDE Expansion\n(opt-in)\nLLM → hypothetical doc\n→ embed"]
    EMBED["Query Embedding\ntext-embedding-3-small\nvec: float32[1536]"]

    subgraph LANES["5-lane Retrieval (hybridSearch CTE)"]
        FTS["FTS lane\ntsv @@ plainto_tsquery\n(simple + english)\nts_rank ordering"]
        VEC["Vec lane\nembedding <=> vec\ncosine ANN (HNSW)"]
        BIGM["bigm lane\nLIKE '%' || query || '%'\nbigm_similarity ordering (#138)\nKorean partial match"]
        SUMMVEC["summvec lane\nsummary_embedding <=> vec\n80% coverage gate (#63)"]
        ENTITY["entity lane\nnormalized_name LIKE query\nentity join rank (#139)\nrequires ENTITY_EXTRACTION_ENABLED"]
    end

    RRF["RRF Fusion\nscore = Σ weight / (60 + rank)\nFULL OUTER JOIN\nstandard k=60"]
    RERANK["BGE cross-encoder\nrerank (opt-in)\njina-reranker-v2-base-multilingual"]
    RESULT["Search Results\n+ Entities attached\n(EntityFetcher)"]

    QUERY --> HYDE
    HYDE --> EMBED
    QUERY --> EMBED
    EMBED --> VEC
    EMBED --> SUMMVEC
    QUERY --> FTS
    QUERY --> BIGM
    QUERY --> ENTITY
    FTS --> RRF
    VEC --> RRF
    BIGM --> RRF
    SUMMVEC --> RRF
    ENTITY --> RRF
    RRF --> RERANK
    RERANK --> RESULT

    class QUERY focal;
    class FTS,VEC,BIGM,SUMMVEC,ENTITY lane;
    class RRF,RERANK data;
```

> **eraser render** ([edit](https://app.eraser.io/workspace/K10FmwBysYGTBVp8E9Wb)):
>
> ![5-lane Hybrid Search RRF](docs/diagrams/05-hybrid-search-rrf.png)

### hybridSearch CTE Structure (`internal/store/document.go`)

Five CTEs (fts, vec, bigm, summvec, entity) combined via FULL OUTER JOIN:

```sql
WITH fts AS (
    SELECT id, row_number() OVER (ORDER BY
        GREATEST(ts_rank(tsv, plainto_tsquery('simple', $1)),
                 ts_rank(tsv, plainto_tsquery('english', $1))) DESC) AS rank
    FROM documents WHERE tsv @@ ... LIMIT $3
),
vec AS (
    SELECT id, row_number() OVER (ORDER BY embedding <=> $2 ASC) AS rank
    FROM documents WHERE embedding IS NOT NULL LIMIT $3
),
bigm AS (
    SELECT id, row_number() OVER (ORDER BY
        GREATEST(bigm_similarity(content, $1),
                 bigm_similarity(title, $1)) DESC, id ASC) AS rank
    FROM documents WHERE content LIKE '%' || $1 || '%' ... LIMIT $3
),
summvec AS (
    SELECT id, row_number() OVER (ORDER BY summary_embedding <=> $2 ASC) AS rank
    FROM documents WHERE summary_embedding IS NOT NULL LIMIT $3
),
entity AS (
    SELECT de.document_id AS id,
           row_number() OVER (ORDER BY COUNT(*) DESC) AS rank
    FROM document_entities de JOIN entities e ON e.id = de.entity_id
    WHERE e.normalized_name LIKE '%' || $N || '%'
    GROUP BY de.document_id LIMIT $3
)
SELECT d.*, (
    w.FTS    / (60 + COALESCE(fts.rank,    0)) +
    w.Vec    / (60 + COALESCE(vec.rank,    0)) +
    w.Bigm   / (60 + COALESCE(bigm.rank,   0)) +
    w.SummVec/ (60 + COALESCE(summvec.rank,0)) +
    w.Entity / (60 + COALESCE(entity.rank, 0))
) AS score
FROM ... FULL OUTER JOIN ... ORDER BY score DESC
```

**Coverage gate**: The `summvec` lane is automatically disabled when summary_embedding coverage is below 80% (prevents ranking bias during backfill, #63).

**Entity lane**: Activated when `ENTITY_EXTRACTION_ENABLED=true`. The FULL OUTER JOIN ensures documents without entities are still ranked normally by other lanes (#139).

### sort Behavior

| sort value | SQL | Meaning |
|---|---|---|
| `"recent"` | `collected_at DESC` | Most recently collected first |
| `"relevance"` or other | `rrf.score DESC` | RRF score descending |

SQL injection prevention: `sortOrder()` whitelist — all values other than `"recent"` map to `"score DESC"`.

---

## 9. Eval Self-improvement Loop

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

    FEEDBACK["User Feedback\nfeedback table\n+1 / -1 thumbs\nFP penalty (#140)"]
    EVALSET["Eval Set Construction\nfeedback → query + relevant_doc_ids\nJSONL format\n(GET /api/v1/eval/export)"]
    EVALRUN["eval-runner\nnightly 03:00 KST\n/app/eval --check-reindex"]
    METRICS["NDCG@5, NDCG@10, MRR@10\nrecorded in eval_metrics\nwith latency (#016)"]
    REGCHECK["Regression Detection\nvs previous run\nexit 1 if below threshold"]
    REINDEX["Reindex Recommendation\nExit 2 → webhook alert\n(#142)"]
    WEBHOOK["Alert Webhook\nSlack notification\nALERT_WEBHOOK_URL"]
    REINDEXACT["Reindexing\nmanual or automatic\nembedding backfill worker"]

    FEEDBACK --> EVALSET
    EVALSET --> EVALRUN
    EVALRUN --> METRICS
    METRICS --> REGCHECK
    REGCHECK -->|"exit 1\n(regression)"| WEBHOOK
    REGCHECK -->|"exit 2\n(reindex rec.)"| REINDEX
    REINDEX --> WEBHOOK
    WEBHOOK --> REINDEXACT

    class EVALRUN focal;
    class METRICS,EVALSET data;
    class FEEDBACK,WEBHOOK,REINDEXACT muted;
```

> **eraser render** ([edit](https://app.eraser.io/workspace/pWBbodLRKu5nxflA5yjJ)):
>
> ![Eval Self-improvement Loop](docs/diagrams/06-eval-self-improvement-loop.png)

### eval-runner Behavior

The `eval-runner` service in `docker-compose.local.yml` uses a shell loop to run `/app/eval --check-reindex` every day at 03:00 KST:

| Exit code | Meaning | Action |
|---|---|---|
| 0 | Eval passed (no regression) | Wait for next cycle |
| 1 | Regression detected | Log + webhook alert |
| 2 | Reindex recommended | Send webhook then continue |

**FP penalty** (#140): When false positives are recorded in feedback, their weight is reduced during eval set construction to prevent NDCG overestimation from misclassified queries.

---

## 10. Deployment Architecture

### Current: Mac mini docker-compose (Production)

Production runs on `docker-compose.local.yml`. External access is handled via the `your-domain.example` tunnel.

```bash
# Deploy / update
docker compose -f docker-compose.local.yml pull
docker compose -f docker-compose.local.yml up -d --build

# Check status
docker compose -f docker-compose.local.yml ps
docker compose -f docker-compose.local.yml logs --tail=50
```

### Future: Kubernetes (`deploy/k8s/`)

Kustomize manifests exist in `deploy/k8s/` but are not currently in use. Reference for future migration:

| File | Resource | Role |
|---|---|---|
| `namespace.yaml` | Namespace | `second-brain` |
| `second-brain-configmap.yaml` | ConfigMap | Non-sensitive settings |
| `second-brain-secret.yaml` | Secret | Sensitive settings placeholder |
| `second-brain-pv.yaml` | PersistentVolume | hostPath 100Gi ReadOnlyMany |
| `second-brain-deployment.yaml` | Deployment | API server + initContainer |
| `second-brain-service.yaml` | Service | NodePort 30080 |
| `postgres-statefulset.yaml` | StatefulSet | pgvector:pg16, PVC 10Gi |
| `postgres-service.yaml` | Service | ClusterIP :5432 |
| `kustomization.yaml` | Kustomize | Resource list |

---

## 11. Web UI Architecture

### Next.js App Router Structure

```
web/src/app/
├── page.tsx                    # Search main page
├── layout.tsx                  # Header, dark mode
├── api-docs/page.tsx           # API reference
├── documents/[id]/page.tsx     # Document detail
├── documents/[id]/MarkdownContent.tsx  # react-markdown + rehype-highlight
├── documents/[id]/XlsxTable.tsx        # ##SHEET TSV parsing, 200 rows
└── api/                        # Next.js API routes (backend proxy)
    ├── search/route.ts
    ├── documents/route.ts
    ├── documents/[id]/route.ts
    ├── documents/[id]/raw/route.ts
    └── stats/route.ts
```

### API Proxy Pattern

```typescript
const BACKEND_URL =
  process.env.BRAIN_API_URL          // Priority 1: docker-compose internal URL
  ?? process.env.NEXT_PUBLIC_API_URL // Priority 2: public URL
  ?? "http://localhost:8080";        // Priority 3: local dev default

const API_KEY = process.env.API_KEY ?? "";  // Server-side only (not exposed to client)
```

Server-side proxy pattern: backend address and API key are never exposed to the client.

---

## 12. Configuration and Environment Variables

### Backend (`internal/config/config.go`)

| Environment Variable | Default | Description |
|---|---|---|
| `DATABASE_URL` | `postgres://brain:brain@localhost:5432/second_brain?sslmode=disable` | Postgres connection string |
| `PORT` | `8080` | HTTP server port |
| `EMBEDDING_API_URL` | `https://api.openai.com/v1` | OpenAI-compatible embeddings endpoint |
| `EMBEDDING_API_KEY` | — | Static API key (priority 1) |
| `EMBEDDING_MODEL` | `text-embedding-3-small` | Embedding model |
| `CLIPROXY_AUTH_FILE` | — | auth.json path (priority 2) |
| `COLLECT_INTERVAL` | `1h` | Default collection interval |
| `MAX_EMBED_CHARS` | `8000` | Maximum characters for embedding input |
| `LLM_API_URL` | — | LLM endpoint (curation, HyDE, entity extraction) |
| `LLM_API_KEY` | — | LLM API key |
| `LLM_MODEL` | — | LLM model identifier |
| `RERANK_API_URL` | — | BGE cross-encoder endpoint |
| `API_KEY` | — | Bearer token authentication |
| `ALERT_WEBHOOK_URL` | — | Slack webhook URL (freshness alerts) |
| `ENTITY_EXTRACTION_ENABLED` | `false` | Enable entity extraction + search lane |
| `FILESYSTEM_PATH` | — | Filesystem collection root |
| `FILESYSTEM_ENABLED` | `false` | Activate filesystem collector |
| `DIARIZATION_API_URL` | — | Speaker diarization server URL |
| `DIARIZATION_ENABLED` | `false` | Enable speaker diarization |

### cliproxy Integration

```
second-brain Pod
  → HTTP Bearer inbound key
  → cliproxy 127.0.0.1:8317
  → OAuth access_token (auto-refreshed)
  → OpenAI API / chat.openai.com
```

| Path | cliproxy | Notes |
|---|---|---|
| `/v1/chat/completions` | Supported | LLM curation, HyDE, entity extraction |
| `/v1/embeddings` | Not supported (404) | FTS fallback or direct OpenAI |

---

## 13. Architecture Decision Records

### ADR-001: pgvector 5-lane Hybrid Search (RRF)

**Context**: A single search method (FTS or vector) provides insufficient Korean search quality.

**Decision**: Combine FTS + pgvector cosine + pg_bigm + summary embedding + entity via RRF. Standard k=60 constant, FULL OUTER JOIN.

**Consequences**: Improved search quality. Requires an embedding API call per query. FTS fallback on embedding failure maintains availability.

---

### ADR-002: pgvector on Postgres 16 (Single DB)

**Context**: Separating the vector store from relational metadata (e.g., Pinecone + RDS) would greatly increase operational complexity.

**Decision**: Single `pgvector/pgvector:pg16` instance. PVC-based data persistence.

**Consequences**: Simpler operations. At hundreds-of-millions-of-vectors scale, migration to a dedicated vector DB should be considered.

---

### ADR-003: Mac mini docker-compose (Current Production)

**Context**: minikube k8s has high local development complexity and is over-engineered for a single-machine deployment.

**Decision**: `docker-compose.local.yml` adopted as production. `deploy/k8s/` preserved for future migration.

**Consequences**: Simplified deployment. No cluster-level HA.

---

### ADR-004: Rune-based Chunking (#145)

**Context**: Byte-based chunking over-fragments Korean text (3 bytes/rune) to 1/3 the size of equivalent English text.

**Decision**: `chunker.Options{TargetSize, MaxSize, Overlap}` defined in rune units.

**Consequences**: Equal information density for Korean and English. `adaptive.SelectOptions(sourceType)` provides per-source optimization.

---

### ADR-005: SMS Stable source_id (#144)

**Context**: The old `sms:{ms}:{addrHash}:{bodyHash}` format created duplicate documents when message bodies were edited.

**Decision**: Changed to `sms:{ms}:{addrHash}:{direction}`. Migration 019 re-keyed existing data.

**Consequences**: The same message always maps to the same source_id → ON CONFLICT DO UPDATE guarantees idempotent upsert.

---

### ADR-006: 3-layer Deletion Guard (#135, #147, #148)

**Context**: A failed or dismounted filesystem walk returning an empty list would soft-delete all documents.

**Decision**:
1. Filesystem root stat: skip collection if root directory is absent
2. 50% ratio guard: skip `MarkDeleted` when `(activeInDB - activeOnFS) / activeInDB > 0.50`
3. document.go empty no-op: empty activeIDs array is a no-op in `MarkDeleted`

**Consequences**: Prevents accidental mass deletions. Operators can force deletion with an override flag (#147) when intentional.

---

### ADR-007: collection_log Staleness Monitor (#137)

**Context**: Silent accumulation of collector errors degrades search result freshness.

**Decision**: `GET /api/v1/collect/status` exposes `last_success_at`, `stale_seconds`, and `consecutive_failures`. `FreshnessChecker` sends a Slack webhook alert when thresholds are exceeded.

**Consequences**: Early detection of collection errors. Prevents operators from missing freshness degradation silently.

---

### ADR-008: Advisory Lock for Migrations (#155)

**Context**: Simultaneous startup of multiple instances could cause migration conflicts.

**Decision**: `store/postgres.go` acquires `pg_try_advisory_lock` before running migrations.

**Consequences**: Multi-instance safe. Instances that fail to acquire the lock skip migration (the winning instance applies it).

---

### ADR-009: Eval Self-improvement Loop (#17–#20, #140, #142)

**Context**: Manually detecting search quality regression is impractical.

**Decision**: Nightly eval-runner measures NDCG/MRR and sends webhook alerts on regression or reindex recommendations.

**Consequences**: Automated quality monitoring. FP penalty minimizes eval set miscalibration.

---

### ADR-010: `atomic.Bool` CAS Scheduler Mutex

**Context**: Cron ticks and manual triggers may attempt concurrent collection runs.

**Decision**: `CompareAndSwap(false, true)` for non-blocking skip. CAS over `sync.Mutex` eliminates deadlock risk.

**Consequences**: Simple code, no deadlocks. Distributed environments (multi-pod) would require external locking.

---

## 14. Known Issues

GitHub issue tracker: https://github.com/baekenough/second-brain/issues

| Symptom | Cause | Workaround |
|---|---|---|
| Some files skipped in minikube mount collection | 9p mount raises `lstat: file name too long` for Korean filenames exceeding 255 bytes | Shorten filenames |
| Slack/GitHub collectors log ERROR then skip | Credential environment variables not set | Set `*_TOKEN` / `*_ORG` variables |
| gdrive collector not active | `GDRIVE_CREDENTIALS_JSON` unset | Provide ADC credentials |
| Ollama slow on first start | gemma3:12b-it-qat CPU inference on macOS arm64 | Pull model once; subsequent runs use cached weights |
| summary_embedding coverage < 80% | Backfill in progress | summvec lane auto-disabled until backfill completes |

---

## 15. Implemented Items

The following items were previously listed as "roadmap" but are **already implemented**.

### Search Quality

| Item | File | Issue |
|---|---|---|
| BGE cross-encoder rerank | `internal/search/rerank.go` | #14 |
| HyDE query expansion | `internal/search/hyde.go` | #15 |
| Hybrid search weight tuning | `internal/search/tune.go` | #16 |
| Summary embedding lane | `migrations/013_summary_vector.sql` | #13 |
| Entity RRF lane | `internal/store/document.go:entityCTE` | #139 |
| bigm_similarity ordering (length→relevance) | `internal/store/document.go:bigm CTE` | #138 |
| Chunk bigm FTS | `migrations/004_chunks.sql` | #146 |
| Rune-based chunking | `internal/chunker/` | #145 |

### Embedding

| Item | File | Issue |
|---|---|---|
| Per-chunk embedding | `migrations/015_chunk_embeddings.sql` | #71 |
| Retry/backoff + chunk backfill | `internal/worker/` | #141 |

### Entity Extraction

| Item | File | Issue |
|---|---|---|
| entities table | `migrations/017_entities.sql` | #77 |
| entity_processed_at column | `migrations/018_entity_processed_at.sql` | — |
| EntityStore | `internal/store/entities.go` | — |

### Data Guard & Freshness

| Item | File | Issue |
|---|---|---|
| Soft-delete mass-deletion guard (3 layers) | `internal/scheduler/scheduler_deletion_guard*.go` | #135, #147 |
| CountActiveDocuments interface | `internal/store/document.go`, `internal/scheduler/scheduler.go` | #148 |
| collection_log staleness monitor | `internal/store/collection_status.go` | #137 |
| GET /api/v1/collect/status | `internal/api/collect_status.go` | #137 |
| Per-source COLLECT_INTERVAL | `internal/config/config.go` | #143 |

### Eval Self-improvement

| Item | File | Issue |
|---|---|---|
| Nightly eval-runner Compose service | `docker-compose.local.yml` | — |
| FP penalty | `cmd/eval/` | #140 |
| Reindex recommendation webhook | `cmd/eval/` | #142 |
| eval_metrics latency | `migrations/016_eval_latency.sql` | #16 |

### Data Integrity

| Item | File | Issue |
|---|---|---|
| SMS stable source_id | `migrations/019_sms_sourceid_rekey.sql` | #144 |
| Migration advisory lock | `internal/store/postgres.go` | #155 |

### Operational Cleanup

| Item | Issue |
|---|---|
| Secretary collector retired | #151 |
| Whisper corrupted audio isolation | #152 |
| Whisper local detection fix | #153 |
| llm-memory sqlite driver restore + graceful skip | #156 |

---

*Last updated: 2026-06-13 (v0.20.4)*
