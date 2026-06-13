# second-brain 아키텍처

> 비전: LLM 큐레이션 프라이빗 검색 엔진. Google Drive, Slack, GitHub, SMS, Gmail 등 개인·팀 지식을 수집·임베딩하여 AI 에이전트에게 큐레이션된 검색 결과를 제공하는 RAG 인프라.

---

## 목차

1. [개요](#1-개요)
2. [시스템 토폴로지](#2-시스템-토폴로지)
3. [서비스 레이어 맵](#3-서비스-레이어-맵)
4. [데이터 모델](#4-데이터-모델)
5. [수집 파이프라인](#5-수집-파이프라인)
6. [추출 파이프라인](#6-추출-파이프라인)
7. [임베딩 파이프라인](#7-임베딩-파이프라인)
8. [검색 파이프라인](#8-검색-파이프라인)
9. [eval 자기개선 루프](#9-eval-자기개선-루프)
10. [배포 아키텍처](#10-배포-아키텍처)
11. [웹 UI 아키텍처](#11-웹-ui-아키텍처)
12. [설정 및 환경 변수](#12-설정-및-환경-변수)
13. [설계 결정 ADR](#13-설계-결정-adr)
14. [알려진 이슈](#14-알려진-이슈)
15. [구현 완료 항목](#15-구현-완료-항목)

---

## 1. 개요

second-brain은 Go 기반 백엔드 서비스와 Next.js 기반 프론트엔드 UI로 구성된 개인·팀 지식 검색 플랫폼이다.

**대상 사용자**: 팀 문서, Slack 대화, GitHub 이슈·PR, SMS, 통화 기록, Gmail, Calendar를 자연어로 검색하고 싶은 사용자.

**핵심 설계 철학**: 듀얼 바이너리(API 서버 + 수집 데몬) → 문서 수집 → rune 기반 청킹 → OpenAI 임베딩 → 5-lane 하이브리드 검색(FTS + pgvector + pg_bigm + 요약 임베딩 + 엔티티 RRF) → LLM 큐레이션(재랭킹 + 요약). BGE cross-encoder rerank와 HyDE 쿼리 확장을 옵션으로 제공한다.

**비기능 요구사항**:

| 항목 | 목표 |
|---|---|
| 검색 지연 | p99 < 500ms (rerank 미사용 기준) |
| 수집 멱등성 | `ON CONFLICT(source_type, source_id) DO UPDATE` |
| 프라이버시 | Slack DM 비수집 설계 (`users.conversations` — IM 채널 명시적 제외) |
| 보안 | Bearer 토큰 인증, timing-safe 비교 (`subtle.ConstantTimeCompare`) |
| 삭제 안전성 | 3계층 soft-delete 대량 삭제 가드 (filesystem root stat / 50% 비율 / no-op) |
| 마이그레이션 | advisory lock 하에 순차 적용 (001~019, 다중 인스턴스 안전) |

**실행 환경**: 프로덕션은 **Mac mini docker-compose** (`docker-compose.local.yml`). `deploy/k8s/`는 향후 Kubernetes 전환용 Kustomize 매니페스트(현재 미사용).

---

## 2. 시스템 토폴로지

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
        ANDROID["Android App\nsecond-brain-push\nSMS / 통화 / 녹음"]
        BROWSER["Browser\nlocalhost:3000"]
        STAGE["~/.second-brain/stage/\nsms / call (OneDrive bridge)"]
    end

    subgraph COMPOSE["docker-compose.local.yml"]
        subgraph CORE["Core Services"]
            SERVER["server\nGo API :8080\n(→ host :8081)"]
            COLLECTOR["collector\nGo 수집 데몬"]
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
    STAGE -.->|sms / call 스테이징| COLLECTOR

    class SERVER,COLLECTOR focal;
    class PG data;
    class OPENAI,SLACK,GITHUB,WEBHOOK ext;
    class OLLAMA,WHISPER,DIARIZATION,WEB,MCP,EVALRUNNER,ANDROID muted;
```

### docker-compose.local.yml 서비스 목록

| 서비스 | 이미지 | 포트 | 역할 |
|--------|--------|------|------|
| postgres | `second-brain-postgres:local` | 5432 (내부) | PostgreSQL 16 + pgvector + pg_bigm |
| server | `second-brain-server:local` | 8081→8080 | API 서버, DB 마이그레이션 적용 |
| collector | `second-brain-collector:local` | — | 수집 데몬, 소스별 주기 실행 |
| mcp | `second-brain-mcp:local` | 8090 | MCP streamable HTTP 서버 |
| eval-runner | `second-brain-eval:local` | — | nightly eval 스케줄러 (03:00 KST) |
| ollama | `ollama/ollama:latest` | 11434 (내부) | 로컬 LLM (gemma3:12b-it-qat) |
| whisper | `fedirz/faster-whisper-server:latest-cpu` | 8000 (내부) | Whisper ASR (int8 CPU) |
| diarization | `second-brain-diarization:local` | 8001 (내부) | pyannote.audio 화자 분리 |
| web | `second-brain-web:local` | 3000 | Next.js 프론트엔드 |

---

## 3. 서비스 레이어 맵

### 백엔드 패키지 의존 관계

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

    CMD["cmd/server + cmd/collector\n+ cmd/mcp + cmd/eval\nDI 조립, 시그널 처리"]
    CFG["internal/config\nConfig.Load()"]
    API["internal/api\nchi Router\n엔드포인트 + 미들웨어\ncollect_status / reindex_check"]
    SCHED["internal/scheduler\nScheduler\nDeleteGuard (50% ratio)\nFreshnessChecker"]
    COL["internal/collector\nFilesystem / Slack / GitHub\nSMS / Recording / Secretary\nGmail / Calendar / LlmMemory"]
    CHUNKER["internal/chunker\nrune 기반 청킹\nHeadingAware / adaptive"]
    SEARCH["internal/search\nService\nHyDE / Reranker / EntityFetcher\nEmbedClient (static/cliProxy)"]
    STORE["internal/store\nDocumentStore (5-lane hybridSearch)\nEntityStore\nCollectionStatus\nChunkStore"]
    MODEL["internal/model\nDocument / SearchQuery\nEntity / SearchWeights"]
    LLM["internal/llm\nCompleter (Ollama/OpenAI)\ncuration + HyDE + entity extract"]
    WORKER["internal/worker\n비동기 임베딩 백필"]

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

### 패키지별 주요 심볼

| 패키지 | 주요 타입 / 함수 | 파일 |
|---|---|---|
| `internal/api` | `Server`, `Handler()`, `requireAPIKey()`, `collectStatusHandler()`, `FreshnessChecker` | `router.go`, `collect_status.go`, `search.go` |
| `internal/chunker` | `Options{TargetSize, MaxSize, Overlap, HeadingAware}`, `Chunk()`, `SelectOptions()` | `chunker.go`, `adaptive.go` |
| `internal/search` | `Service.Search()`, `WithHyDE()`, `WithReranker()`, `WithEntityFetcher()`, `EmbedClient`, `HTTPReranker` | `search.go`, `embed.go`, `rerank.go`, `hyde.go`, `tune.go` |
| `internal/store` | `DocumentStore`, `hybridSearch()` (5-lane CTE), `CollectionStatus()`, `EntityStore`, `CountActiveDocuments()` | `document.go`, `collection_status.go`, `entities.go` |
| `internal/scheduler` | `Scheduler`, `deletionRatioThreshold=0.50`, `WithEntityExtraction()` | `scheduler.go`, `scheduler_deletion_guard*.go` |
| `internal/llm` | `Completer` interface (Ollama / OpenAI chat completions), entity extraction, HyDE prompt | `llm.go` |
| `internal/model` | `Document`, `SearchQuery{UseHyDE, UseRerank}`, `Entity`, `SearchWeights` | `document.go` |

---

## 4. 데이터 모델

### ERD (migration 001~019)

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
        text source_id "NOT NULL — 소스 내 고유 식별자; sms:{ms}:{addrHash}:{direction}"
        text title "NOT NULL"
        text content "NOT NULL"
        jsonb metadata "default '{}'"
        vector_1536 embedding "nullable — text-embedding-3-small"
        vector_1536 summary_embedding "nullable — 요약 임베딩 lane (#013)"
        text title_summary "nullable — LLM 요약 제목 (#012)"
        text bullet_summary "nullable — LLM 요약 bullet (#012)"
        tsvector tsv "GENERATED STORED — english(A+B) + simple(A+B)"
        timestamptz collected_at "NOT NULL — 파일 ModTime 또는 수집 시각"
        timestamptz created_at "default now()"
        timestamptz updated_at "default now()"
        text status "default 'active' — active|deleted|moved"
        timestamptz deleted_at "nullable"
        timestamptz entity_processed_at "nullable — 엔티티 추출 완료 시각 (#018)"
    }

    chunks {
        bigserial id PK
        uuid document_id FK "→ documents.id ON DELETE CASCADE"
        int chunk_index "NOT NULL — 청크 순번"
        text content "NOT NULL"
        tsvector content_tsv "GENERATED — to_tsvector('simple', content)"
        int byte_size "NOT NULL"
        vector_1536 embedding "nullable — per-chunk 임베딩 (#015)"
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
        int pairs "NOT NULL — eval set 크기"
        timestamptz run_at "NOT NULL default now()"
    }

    documents ||--o{ chunks : "document_id"
    documents ||--o{ document_entities : "document_id"
    entities ||--o{ document_entities : "entity_id"
    documents ||--o{ feedback : "document_id"
    chunks ||--o{ feedback : "chunk_id"
```

### 마이그레이션 목록 (001~019)

| # | 파일 | 주요 변경 |
|---|------|-----------|
| 001 | `001_init.sql` | documents, collection_log, HNSW 인덱스, tsvector GENERATED |
| 002 | `002_soft_delete.sql` | status, deleted_at 컬럼 + 인덱스 |
| 003 | `003_extraction_failures.sql` | extraction_failures 추적 |
| 004 | `004_chunks.sql` | chunks 테이블 + content_tsv GIN |
| 005 | `005_feedback.sql` | feedback 테이블 (thumbs, query, session_id) |
| 006 | `006_bigm.sql` | pg_bigm 확장 + GIN bigm 인덱스 |
| 007 | `007_eval_metrics.sql` | eval_metrics (ndcg5, ndcg10, mrr10) |
| 008 | `008_reindex_state.sql` | reindex_state 추적 테이블 |
| 009 | `009_collector_state.sql` | collector_state (증분 수집 커서) |
| 010 | `010_occurred_at.sql` | SMS/통화 occurred_at 컬럼 |
| 011 | `011_configurable_embedding_dim.sql` | 임베딩 차원 설정 가능 |
| 012 | `012_summary_columns.sql` | title_summary, bullet_summary 컬럼 |
| 013 | `013_summary_vector.sql` | summary_embedding vector(1536) + HNSW |
| 014 | `014_unsummarized_index.sql` | 미요약 문서 조회용 부분 인덱스 |
| 015 | `015_chunk_embeddings.sql` | chunks.embedding vector(1536) + HNSW |
| 016 | `016_eval_latency.sql` | eval 실행 latency 기록 |
| 017 | `017_entities.sql` | entities + document_entities 테이블 |
| 018 | `018_entity_processed_at.sql` | documents.entity_processed_at 컬럼 |
| 019 | `019_sms_sourceid_rekey.sql` | SMS source_id bodyHash→direction 재키잉 |

### `documents` 주요 인덱스

| 인덱스명 | 종류 | 대상 | 목적 |
|---|---|---|---|
| `(UNIQUE)` | UNIQUE | `(source_type, source_id)` | Upsert 충돌 키 |
| `idx_documents_tsv` | GIN | `tsv` | BM25 전문 검색 |
| `idx_documents_embedding` | HNSW | `embedding vector_cosine_ops` | 코사인 ANN |
| `idx_documents_summary_embedding` | HNSW | `summary_embedding vector_cosine_ops` | 요약 임베딩 ANN |
| `idx_documents_bigm_content` | GIN bigm | `content gin_bigm_ops` | pg_bigm 2-gram |
| `idx_documents_bigm_title` | GIN bigm | `title gin_bigm_ops` | pg_bigm 제목 |
| `idx_documents_entity_processed_at` | B-tree | `entity_processed_at` | 미처리 문서 조회 |

---

## 5. 수집 파이프라인

### 전체 흐름 시퀀스 (IndexAware + 삭제 가드)

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
    participant CRON as robfig/cron<br/>또는 POST /collect/trigger
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
    alt 이미 실행 중
        CAS-->>SCHED: false → warn + return
    else
        CAS-->>SCHED: true
        SCHED->>RC: runCollector(col)
        RC->>STORE: LastCollectedAt(instanceID, source_type)
        STORE-->>RC: since timestamp
        RC->>COL: Collect(ctx, since)
        COL-->>RC: []model.Document

        loop 각 Document
            RC->>CHUNK: Chunk(text, adaptive.SelectOptions(sourceType))
            CHUNK-->>RC: []string chunks
            RC->>STORE: Upsert(doc + chunks)
        end

        RC->>EMBED: embedDocuments(batch=20)
        loop 배치 20개
            EMBED->>OpenAI: POST /v1/embeddings
            OpenAI-->>EMBED: []float32[1536]
        end

        RC->>STORE: RecordCollectionLog(src, started, count, err)

        opt FilesystemCollector only
            RC->>STORE: CountActiveDocuments(source_type)
            STORE-->>RC: activeInDB
            RC->>COL: ListActiveSourceIDs()
            COL-->>RC: activeOnFS (파일시스템 walk)

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

### Scheduler 구조 (`internal/scheduler/scheduler.go`)

```go
type Scheduler struct {
    cron       *cron.Cron       // robfig/cron v3, seconds resolution
    collectors []collector.Collector
    store      DocumentUpserter // includes CountActiveDocuments (required, #148)
    embed      *search.EmbedClient
    running    atomic.Bool      // CAS 뮤텍스
    freshness  *api.FreshnessChecker // staleness 모니터 (#137)
}
```

**삭제 가드 (`deletionRatioThreshold = 0.50`)**: filesystem 수집 후 DB 활성 문서 대비 삭제 예정 비율이 50% 초과 시 `MarkDeleted`를 건너뛰고 경고 로그만 출력. `CountActiveDocuments`는 `DocumentUpserter` 인터페이스에 필수 메서드로 승격되어 컴파일 타임에 보장됨 (#148).

### 수집기별 상세

#### FilesystemCollector

| 항목 | 값 |
|---|---|
| 루트 경로 | `FILESYSTEM_PATH` env |
| 증분 기준 | `info.ModTime().After(since)` |
| 스킵 디렉토리 | `.git`, `node_modules`, `dist`, `.next`, `.omc`, `.sisyphus`, `.claude` |
| 텍스트 파일 상한 | 512 KB |
| DeletionDetector | `ListActiveSourceIDs()` 구현 |

#### SlackCollector

| 항목 | 값 |
|---|---|
| 채널 범위 | `users.conversations` — 봇 member 채널만 |
| DM 비수집 | `types=public_channel,private_channel` — IM 명시적 제외 |
| 쓰레드 | `reply_count > 0` → `conversations.replies` → 독립 Document |
| SourceID | `{channel_id}:{ts}` |

#### SMSCollector / RecordingCollector

SMS·통화: Android second-brain-push 앱이 `POST /api/v1/ingest/messages`로 전송. `source_id = sms:{dateMs}:{sha256(addr)[:16]}:{direction}`. migration 019로 이전 bodyHash 기반 source_id를 재키잉.

녹음: `POST /api/v1/ingest/recording`으로 `.m4a` multipart 수신 → Whisper ASR → 텍스트 변환 후 저장. 손상 오디오는 격리 처리 (#152).

#### secretary / gmail / calendar / llm-memory

호스트 SQLite·파일을 docker volume ro 마운트로 직접 접근. secretary collector는 v0.20.x에서 정리됨 (#151).

---

## 6. 추출 파이프라인

`internal/collector/extractor/` 패키지의 `Registry`가 확장자별 Extractor를 선택한다.

```go
type Extractor interface {
    Supports(ext string) bool
    Extract(ctx context.Context, absPath string) (string, error)
}
```

Registry 등록 순서: `HTMLExtractor` → `PDFExtractor` → `DocxExtractor` → `XlsxExtractor` → `PptxExtractor` → `HwpxExtractor`

### SanitizeText

모든 추출기 출력에 공통 적용:
1. `\x00` → `" "` — Postgres TEXT 저장 오류 방지
2. `strings.ToValidUTF8(s, "�")` — 유효하지 않은 UTF-8 치환
3. 연속 `\n\n\n` → `\n\n` 압축

### 컨텐츠 크기 계층

| 레이어 | 상한 |
|---|---|
| 텍스트 파일 인라인 읽기 | 512 KB |
| 추출기 출력 (`MaxExtractedBytes`) | 512 KB |
| XLSX 중간 버퍼 (`xlsxMaxBytes`) | 200 KB |
| 임베딩 입력 (`defaultMaxEmbedChars`) | 8000자 (기본) |
| raw 파일 API | 50 MiB |

### rune 기반 청킹 (`internal/chunker/`)

```go
type Options struct {
    TargetSize  int  // 청크 목표 크기 (rune 단위, 기본 2000)
    MaxSize     int  // 청크 최대 크기 (rune 단위, 기본 4000)
    Overlap     int  // 청크 간 오버랩 (rune 단위, 기본 100)
    HeadingAware bool // 마크다운/HTML 헤딩 기반 섹션 분리
}
```

rune 단위 사용으로 한국어(3바이트/rune)·영어(1바이트/rune) 동일한 정보 밀도 보장 (#145). `adaptive.SelectOptions(sourceType)`이 소스 타입별 최적 옵션을 자동 선택.

---

## 7. 임베딩 파이프라인

### 토큰 소스 우선순위 (`internal/search/embed.go`)

```go
switch {
case apiKey != "":        // 1순위: EMBEDDING_API_KEY
    ts = &staticToken{t: apiKey}
case authFilePath != "": // 2순위: CliProxy JSON (5분 TTL 자동 갱신)
    ts = newCliProxyToken(authFilePath)
}
// 3순위: Authorization 헤더 없음 (자체 호스팅 호환)
```

### 임베딩 흐름

```
embedDocuments(batch=20)
  └─ 각 Document:
       text = title + "\n\n" + content
       if len(text) > maxLen: text = text[:maxLen] + warn
       EmbedBatch(ctx, texts[20]) → OpenAI /v1/embeddings
         → []float32[1536]
       docs[i].Embedding = vecs[i]

chunks 임베딩 (#015):
  └─ 각 Chunk:
       EmbedBatch(ctx, chunkTexts[20]) → chunk.embedding
```

**임베딩 실패 시**: 배치 오류가 전체 수집을 중단하지 않음. 해당 배치만 WARN 로그 후 스킵. worker 패키지가 비동기 백필 처리 (#141).

### 검색 시 쿼리 임베딩

```go
// search.go
if q.UseHyDE && s.llmClient != nil {
    hydeDoc := s.llmClient.Complete(ctx, hydePrompt(q.Query))
    vec, _ = s.embed.Embed(ctx, hydeDoc)  // HyDE 문서 임베딩
} else {
    vec, _ = s.embed.Embed(ctx, q.Query)  // 쿼리 직접 임베딩
}
q.Embedding = vec
```

---

## 8. 검색 파이프라인

### 5-lane 하이브리드 검색 RRF

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
    HYDE["HyDE 확장\n(opt-in)\nLLM → 가상 문서\n→ embed"]
    EMBED["Query Embedding\ntext-embedding-3-small\nvec: float32[1536]"]

    subgraph LANES["5-lane Retrieval (hybridSearch CTE)"]
        FTS["FTS lane\ntsv @@ plainto_tsquery\n(simple + english)\nts_rank 내림차순"]
        VEC["Vec lane\nembedding <=> vec\n코사인 ANN (HNSW)"]
        BIGM["bigm lane\nLIKE '%' || query || '%'\nbigm_similarity 정렬 (#138)\n한국어 부분 매칭"]
        SUMMVEC["summvec lane\nsummary_embedding <=> vec\n80% coverage gate (#63)"]
        ENTITY["entity lane\nnormalized_name LIKE query\nentity join rank (#139)\nENTITY_EXTRACTION_ENABLED 필요"]
    end

    RRF["RRF Fusion\nscore = Σ weight / (60 + rank)\nFULL OUTER JOIN\nk=60 표준값"]
    RERANK["BGE cross-encoder\nrerank (opt-in)\njina-reranker-v2-base-multilingual"]
    RESULT["Search Results\n+ Entities 부착\n(EntityFetcher)"]

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

### hybridSearch CTE 구조 (`internal/store/document.go`)

5개의 CTE(fts, vec, bigm, summvec, entity)를 FULL OUTER JOIN으로 결합:

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

**커버리지 게이트**: `summvec` 레인은 summary_embedding 커버리지 80% 미만 시 자동 비활성화 (백필 중 순위 편향 방지, #63).

**엔티티 레인**: `ENTITY_EXTRACTION_ENABLED=true` 설정 시 활성화. FULL OUTER JOIN이므로 엔티티 없는 문서도 다른 레인에서 정상 랭킹됨 (#139).

### API 엔드포인트

`POST /api/v1/search` — Bearer 인증 필요 (API_KEY 설정 시)

**요청 스키마**:
```json
{
  "query": "BBQ 미팅",
  "source_type": "filesystem",
  "exclude_source_types": ["slack"],
  "limit": 10,
  "sort": "relevance",
  "include_deleted": false,
  "use_hyde": false,
  "use_rerank": false
}
```

### sortOrder 동작

| sort 값 | SQL | 의미 |
|---|---|---|
| `"recent"` | `collected_at DESC` | 최근 수집 순 |
| `"relevance"` 또는 그 외 | `rrf.score DESC` | RRF 점수 내림차순 |

SQL 인젝션 방지: `sortOrder()` 화이트리스트 — `"recent"` 외 모든 값은 `"score DESC"`.

---

## 9. eval 자기개선 루프

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

    FEEDBACK["사용자 피드백\nfeedback 테이블\n+1 / -1 thumbs\nFP penalty (#140)"]
    EVALSET["eval set 구축\nfeedback → query+relevant_doc_ids\nJSONL 형식\n(GET /api/v1/eval/export)"]
    EVALRUN["eval-runner\nnightly 03:00 KST\n/app/eval --check-reindex"]
    METRICS["NDCG@5, NDCG@10, MRR@10\neval_metrics 테이블에 기록\nlatency 포함 (#016)"]
    REGCHECK["회귀 감지\n이전 실행 대비\n임계치 이하 시 exit 1"]
    REINDEX["reindex 추천\nExit 2 → 웹훅 알림\n(#142)"]
    WEBHOOK["Alert Webhook\nSlack 알림\nALERT_WEBHOOK_URL"]
    REINDEXACT["재인덱싱 실행\n수동 또는 자동\n임베딩 백필 워커"]

    FEEDBACK --> EVALSET
    EVALSET --> EVALRUN
    EVALRUN --> METRICS
    METRICS --> REGCHECK
    REGCHECK -->|"exit 1\n(회귀)"| WEBHOOK
    REGCHECK -->|"exit 2\n(reindex 추천)"| REINDEX
    REINDEX --> WEBHOOK
    WEBHOOK --> REINDEXACT

    class EVALRUN focal;
    class METRICS,EVALSET data;
    class FEEDBACK,WEBHOOK,REINDEXACT muted;
```

### eval-runner 동작

`docker-compose.local.yml`의 `eval-runner` 서비스가 셸 루프로 매일 03:00 KST에 `/app/eval --check-reindex`를 실행:

| Exit code | 의미 | 동작 |
|---|---|---|
| 0 | eval 통과 (회귀 없음) | 다음 사이클 대기 |
| 1 | 회귀 감지 | 로그 기록 + 웹훅 알림 |
| 2 | reindex 권장 | 웹훅 알림 발송 후 계속 |

**FP penalty** (#140): feedback에 false positive가 기록되면 eval set 구성 시 해당 쌍의 가중치를 낮춰 오분류 쿼리에 의한 NDCG 과추정을 방지.

---

## 10. 배포 아키텍처

### 현행: Mac mini docker-compose (프로덕션)

프로덕션 환경은 `docker-compose.local.yml`로 운영한다. 외부 접근은 `your-domain.example` 터널을 통해 처리.

```bash
# 배포 / 업데이트
docker compose -f docker-compose.local.yml pull
docker compose -f docker-compose.local.yml up -d --build

# 상태 확인
docker compose -f docker-compose.local.yml ps
docker compose -f docker-compose.local.yml logs --tail=50
```

### 향후: Kubernetes (`deploy/k8s/`)

`deploy/k8s/`에 Kustomize 매니페스트가 있으나 현재 미사용. 전환 시 참고:

| 파일 | 리소스 | 역할 |
|---|---|---|
| `namespace.yaml` | Namespace | `second-brain` |
| `second-brain-configmap.yaml` | ConfigMap | 비민감 설정 |
| `second-brain-secret.yaml` | Secret | 민감 설정 placeholder |
| `second-brain-pv.yaml` | PersistentVolume | hostPath 100Gi ReadOnlyMany |
| `second-brain-deployment.yaml` | Deployment | API 서버 + initContainer |
| `second-brain-service.yaml` | Service | NodePort 30080 |
| `postgres-statefulset.yaml` | StatefulSet | pgvector:pg16, PVC 10Gi |
| `postgres-service.yaml` | Service | ClusterIP :5432 |
| `kustomization.yaml` | Kustomize | 리소스 목록 |

---

## 11. 웹 UI 아키텍처

### Next.js App Router 구조

```
web/src/app/
├── page.tsx                    # 검색 메인
├── layout.tsx                  # 헤더, dark mode
├── api-docs/page.tsx           # API 레퍼런스
├── documents/[id]/page.tsx     # 문서 상세
├── documents/[id]/MarkdownContent.tsx  # react-markdown + rehype-highlight
├── documents/[id]/XlsxTable.tsx        # ##SHEET TSV 파싱, 200행
└── api/                        # Next.js API 라우트 (백엔드 프록시)
    ├── search/route.ts
    ├── documents/route.ts
    ├── documents/[id]/route.ts
    ├── documents/[id]/raw/route.ts
    └── stats/route.ts
```

### API 프록시 패턴

```typescript
const BACKEND_URL =
  process.env.BRAIN_API_URL          // 1순위: docker-compose 내부 URL
  ?? process.env.NEXT_PUBLIC_API_URL // 2순위: 공개 URL
  ?? "http://localhost:8080";        // 3순위: 로컬 개발

const API_KEY = process.env.API_KEY ?? "";  // 서버사이드만 사용 (클라이언트 미노출)
```

클라이언트에 백엔드 주소·API 키를 노출하지 않는 서버사이드 프록시 패턴.

---

## 12. 설정 및 환경 변수

### 백엔드 (`internal/config/config.go`)

| 환경 변수 | 기본값 | 설명 |
|---|---|---|
| `DATABASE_URL` | `postgres://brain:brain@localhost:5432/second_brain?sslmode=disable` | Postgres 연결 문자열 |
| `PORT` | `8080` | HTTP 서버 포트 |
| `EMBEDDING_API_URL` | `https://api.openai.com/v1` | OpenAI 호환 임베딩 엔드포인트 |
| `EMBEDDING_API_KEY` | — | Static API 키 (1순위) |
| `EMBEDDING_MODEL` | `text-embedding-3-small` | 임베딩 모델 |
| `CLIPROXY_AUTH_FILE` | — | auth.json 경로 (2순위) |
| `COLLECT_INTERVAL` | `1h` | 기본 수집 주기 |
| `MAX_EMBED_CHARS` | `8000` | 임베딩 입력 최대 문자 수 |
| `LLM_API_URL` | — | LLM 엔드포인트 (curation·HyDE·엔티티) |
| `LLM_API_KEY` | — | LLM API 키 |
| `LLM_MODEL` | — | LLM 모델 |
| `RERANK_API_URL` | — | BGE cross-encoder 엔드포인트 |
| `API_KEY` | — | Bearer 토큰 인증 |
| `ALERT_WEBHOOK_URL` | — | Slack 웹훅 URL (신선도 알림) |
| `ENTITY_EXTRACTION_ENABLED` | `false` | 엔티티 추출 + 검색 레인 활성화 |
| `FILESYSTEM_PATH` | — | 파일시스템 수집 루트 |
| `FILESYSTEM_ENABLED` | `false` | 파일시스템 수집기 활성화 |
| `DIARIZATION_API_URL` | — | 화자 분리 서버 URL |
| `DIARIZATION_ENABLED` | `false` | 화자 분리 활성화 |

### cliproxy 통합

```
second-brain Pod
  → HTTP Bearer inbound key
  → cliproxy 127.0.0.1:8317
  → OAuth access_token (자동 갱신)
  → OpenAI API / chat.openai.com
```

| Path | cliproxy | 비고 |
|---|---|---|
| `/v1/chat/completions` | 지원 | LLM curation·HyDE·엔티티 |
| `/v1/embeddings` | 미지원 (404) | FTS 폴백 또는 직접 OpenAI 사용 |

---

## 13. 설계 결정 ADR

### ADR-001: pgvector 5-lane 하이브리드 검색 (RRF)

**Context**: 단일 검색 방식(FTS 또는 벡터)은 한국어 검색 품질이 불충분하다.

**Decision**: FTS + pgvector 코사인 + pg_bigm + 요약 임베딩 + 엔티티 RRF. k=60 상수, FULL OUTER JOIN.

**Consequences**: 검색 품질 향상. 쿼리당 임베딩 API 호출 필요. 임베딩 실패 시 FTS 폴백으로 가용성 유지.

---

### ADR-002: pgvector on Postgres 16 단일 DB

**Context**: 벡터 스토어와 관계형 메타데이터 분리 시 운영 복잡도 급증.

**Decision**: `pgvector/pgvector:pg16` 단일 인스턴스. PVC 기반 데이터 영속성.

**Consequences**: 운영 단순화. 수억 벡터 수준에서는 전용 벡터 DB 마이그레이션 검토 필요.

---

### ADR-003: Mac mini docker-compose (현행 프로덕션)

**Context**: minikube k8s는 로컬 개발 복잡도가 높고, 단일 머신 배포에 오버엔지니어링이다.

**Decision**: `docker-compose.local.yml`을 프로덕션으로 채택. `deploy/k8s/`는 향후 전환용으로 보존.

**Consequences**: 배포 단순화. 클러스터 수준 HA는 제공하지 않음.

---

### ADR-004: rune 기반 청킹 (#145)

**Context**: 바이트 기반 청킹은 한국어(3바이트/rune)를 영어 대비 1/3 크기로 과다 분할한다.

**Decision**: `chunker.Options{TargetSize, MaxSize, Overlap}`을 rune 단위로 정의.

**Consequences**: 한국어·영어 동일한 정보 밀도. `adaptive.SelectOptions(sourceType)`으로 소스별 최적화.

---

### ADR-005: SMS stable source_id (#144)

**Context**: 기존 `sms:{ms}:{addrHash}:{bodyHash}` 형식은 본문 수정 시 중복 문서를 생성한다.

**Decision**: `sms:{ms}:{addrHash}:{direction}`으로 변경. migration 019로 기존 데이터 재키잉.

**Consequences**: 동일 메시지는 항상 동일 source_id → ON CONFLICT DO UPDATE로 멱등 upsert 보장.

---

### ADR-006: 삭제 가드 3계층 (#135, #147, #148)

**Context**: 마운트 해제나 임시 오류로 파일시스템 워크가 빈 목록을 반환할 경우 전체 문서가 soft-delete될 수 있다.

**Decision**:
1. filesystem root stat: 루트 디렉터리가 없으면 수집 스킵
2. 50% 비율 가드: `(activeInDB - activeOnFS) / activeInDB > 0.50` 이면 `MarkDeleted` 건너뜀
3. document.go empty no-op: 빈 activeIDs 배열은 `MarkDeleted`에서 no-op 처리

**Consequences**: 의도치 않은 대량 삭제 방지. 오탐 시 운영자가 override 플래그(#147)로 수동 강제 삭제 가능.

---

### ADR-007: collection_log staleness 모니터 (#137)

**Context**: 수집기 오류가 조용히 누적되면 검색 결과 신선도가 저하된다.

**Decision**: `GET /api/v1/collect/status` 엔드포인트로 `last_success_at`, `stale_seconds`, `consecutive_failures` 노출. `FreshnessChecker`가 임계치 초과 시 Slack 웹훅 알림.

**Consequences**: 수집 오류 조기 감지. 운영자가 알림 없이 신선도 저하를 놓치는 상황 방지.

---

### ADR-008: advisory lock 마이그레이션 (#155)

**Context**: 멀티 인스턴스 동시 기동 시 마이그레이션이 중복 실행되어 충돌할 수 있다.

**Decision**: `store/postgres.go`에서 `pg_try_advisory_lock`을 취득 후 마이그레이션 실행.

**Consequences**: 복수 인스턴스 안전. 락 취득 실패 시 해당 인스턴스는 마이그레이션 건너뜀.

---

### ADR-009: eval 자기개선 루프 (#17-#20, #140, #142)

**Context**: 검색 품질 회귀를 수동으로 감지하기 어렵다.

**Decision**: nightly eval-runner가 NDCG/MRR을 측정하고 회귀·reindex 필요 시 웹훅 알림.

**Consequences**: 자동화된 품질 모니터링. FP penalty로 eval set 오분류 최소화.

---

### ADR-010: `atomic.Bool` CAS scheduler 뮤텍스

**Context**: cron 틱과 수동 트리거의 중복 실행 방지.

**Decision**: `CompareAndSwap(false, true)` 논블로킹 skip. `sync.Mutex` 대신 CAS로 교착 상태 위험 제거.

**Consequences**: 코드 단순, 교착 없음. 분산 환경(멀티 Pod)에서는 외부 잠금 필요.

---

## 14. 알려진 이슈

GitHub 이슈로 추적: https://github.com/baekenough/second-brain/issues

| 증상 | 원인 | 우회 방법 |
|---|---|---|
| minikube mount에서 일부 파일 스킵 | 9p 마운트 한글 장파일명 255바이트 초과 | 파일명 단축 |
| Slack/GitHub 수집기 ERROR 후 skip | 자격증명 미설정 | `*_TOKEN` / `*_ORG` 환경 변수 설정 |
| gdrive 수집기 미동작 | `GDRIVE_CREDENTIALS_JSON` 미설정 | ADC 자격증명 설정 |
| Ollama 첫 기동 느림 | gemma3:12b-it-qat CPU 추론 | 모델 pull 후 재사용 |
| summary_embedding coverage < 80% | 백필 진행 중 | 백필 완료 전까지 summvec 레인 자동 비활성 |

---

## 15. 구현 완료 항목

아래 항목들은 이전 로드맵에서 "예정"으로 기재되었으나 **이미 구현 완료** 상태다.

### 검색 품질

| 항목 | 파일 | 이슈 |
|---|---|---|
| BGE cross-encoder rerank | `internal/search/rerank.go` | #14 |
| HyDE 쿼리 확장 | `internal/search/hyde.go` | #15 |
| 하이브리드 검색 가중치 튜닝 | `internal/search/tune.go` | #16 |
| 요약 임베딩 lane | `migrations/013_summary_vector.sql` | #13 |
| 엔티티 RRF lane | `internal/store/document.go:entityCTE` | #139 |
| bigm_similarity 정렬 (length→relevance) | `internal/store/document.go:bigm CTE` | #138 |
| 청크 bigm FTS | `migrations/004_chunks.sql` | #146 |
| rune 기반 청킹 | `internal/chunker/` | #145 |

### 임베딩

| 항목 | 파일 | 이슈 |
|---|---|---|
| per-chunk 임베딩 | `migrations/015_chunk_embeddings.sql` | #71 |
| retry/backoff + 청크 백필 | `internal/worker/` | #141 |

### 엔티티 추출

| 항목 | 파일 | 이슈 |
|---|---|---|
| entities 테이블 | `migrations/017_entities.sql` | #77 |
| entity_processed_at 컬럼 | `migrations/018_entity_processed_at.sql` | — |
| EntityStore | `internal/store/entities.go` | — |

### 데이터 가드 & 신선도

| 항목 | 파일 | 이슈 |
|---|---|---|
| soft-delete 대량 삭제 가드 (3계층) | `internal/scheduler/scheduler_deletion_guard*.go` | #135, #147 |
| CountActiveDocuments 인터페이스 | `internal/store/document.go`, `internal/scheduler/scheduler.go` | #148 |
| collection_log staleness 모니터 | `internal/store/collection_status.go` | #137 |
| GET /api/v1/collect/status | `internal/api/collect_status.go` | #137 |
| 소스별 COLLECT_INTERVAL | `internal/config/config.go` | #143 |

### eval 자기개선

| 항목 | 파일 | 이슈 |
|---|---|---|
| nightly eval-runner Compose 서비스 | `docker-compose.local.yml` | — |
| FP penalty | `cmd/eval/` | #140 |
| reindex 추천 웹훅 | `cmd/eval/` | #142 |
| eval_metrics latency | `migrations/016_eval_latency.sql` | #16 |

### 데이터 무결성

| 항목 | 파일 | 이슈 |
|---|---|---|
| SMS stable source_id | `migrations/019_sms_sourceid_rekey.sql` | #144 |
| 마이그레이션 advisory lock | `internal/store/postgres.go` | #155 |

### 운영 정리

| 항목 | 이슈 |
|---|---|
| secretary collector 제거 | #151 |
| whisper 손상 오디오 격리 | #152 |
| whisper 로컬 판별 수정 | #153 |
| llm-memory sqlite 드라이버 복원 + graceful skip | #156 |

---

*Last updated: 2026-06-13 (v0.20.4)*
