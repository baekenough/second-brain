# second-brain 아키텍처

> 비전: Google Drive, Slack, GitHub 등 팀 지식을 단일 벡터+전문 검색 엔진으로 통합하여 자연어 질의로 즉시 검색할 수 있는 사내 RAG 인프라.

---

## 목차

1. [개요](#1-개요)
2. [시스템 구성도](#2-시스템-구성도)
3. [서비스 레이어 맵](#3-서비스-레이어-맵)
4. [데이터 모델](#4-데이터-모델)
5. [수집 파이프라인](#5-수집-파이프라인)
6. [추출 파이프라인](#6-추출-파이프라인)
7. [임베딩 파이프라인](#7-임베딩-파이프라인)
8. [검색 파이프라인](#8-검색-파이프라인)
9. [배포 아키텍처](#9-배포-아키텍처)
10. [웹 UI 아키텍처](#10-웹-ui-아키텍처)
11. [설정 및 환경 변수](#11-설정-및-환경-변수)
12. [설계 결정 ADR](#12-설계-결정-adr)
13. [알려진 이슈](#13-알려진-이슈)
14. [로드맵](#14-로드맵)

---

## 1. 개요

second-brain은 Go 기반 백엔드 서비스와 Next.js 기반 프론트엔드 UI로 구성된 팀 지식 검색 플랫폼이다.

**대상 사용자**: 팀 내 문서, Slack 대화, GitHub 이슈·PR을 빠르게 찾고 싶은 팀원. 별도 데이터 엔지니어링 없이 자연어로 전사 지식에 접근하는 것이 목표다.

**핵심 설계 철학**: 문서 단위 수집 → OpenAI 임베딩 벡터화 → pgvector + PostgreSQL 하이브리드 검색(BM25+코사인 RRF). 현재는 Phase 0 완료 상태로, 청킹 없이 전체 문서를 임베딩한다.

**비기능 요구사항**:

| 항목 | 현재 목표 |
|---|---|
| 검색 지연 | p99 < 500ms |
| 수집 멱등성 | `ON CONFLICT(source_type, source_id) DO UPDATE` |
| 프라이버시 | Slack DM 비수집 설계 (`users.conversations` 사용, DM 채널 제외) |
| 보안 | Bearer 토큰 인증, timing-safe 비교 (`subtle.ConstantTimeCompare`) |
| 가용성 | 단일 인스턴스, initContainer 기반 의존성 대기 |

**실행 환경**: macOS에서 Docker Desktop 또는 minikube(docker driver)를 사용하여 로컬 Kubernetes 클러스터에 배포한다. macOS docker driver 특성상 NodePort가 호스트에 직접 노출되지 않으므로 `kubectl port-forward` 방식을 권장한다.

**현재 스코프**: Phase 0 완료. 문서 단위 수집(청킹 없음), 단일 Postgres 인스턴스, OpenAI text-embedding-3-small, `atomic.Bool` CAS 기반 스케줄러, 소프트 삭제, 수집 로그 테이블.

---

## 2. 시스템 구성도

```mermaid
flowchart TD
    subgraph HOST["macOS Host"]
        GD["~/Google Drive\n공유 드라이브/Vibers.AI"]
        BROWSER["Browser\nlocalhost:30300 또는 port-forward"]
    end

    subgraph DOCKER["minikube (docker driver)"]
        subgraph NS["Namespace: second-brain"]
            WEB["vibers-web\nNext.js standalone\nNodePort 30300 → container 3000"]
            BRAIN["second-brain\nGo HTTP :9200\nNodePort 30920"]
            PG["postgres StatefulSet\npgvector/pgvector:pg16\nClusterIP :5432\nPVC 10Gi"]
            INIT["initContainer\nwait-for-postgres\nbusybox nc -z postgres 5432"]
            WATCHER["SlackChannelWatcher\ngoroutine 60s poll\nsees new channels → full history collect"]
            PV["PV hostPath 100Gi\nReadOnlyMany\n/mnt/drive → /data/drive\nuid=10001"]
        end
    end

    subgraph EXTERNAL["External APIs"]
        OPENAI["OpenAI API\n/v1/embeddings\ntext-embedding-3-small"]
        SLACK["Slack Web API\nusers.conversations\nconversations.history\nconversations.replies"]
        GITHUB["GitHub REST API\n/orgs/{org}/repos\n/repos/{repo}/issues"]
        GDRIVE_API["Google Drive API\nfiles.export (ADC)\n선택적 활성화"]
    end

    BROWSER -->|HTTP| WEB
    WEB -->|proxy /api/*\nBRAIN_API_URL env| BRAIN
    GD -->|"minikube mount\nuid=10001 gid=10001"| PV
    PV -->|ReadOnly mount\n/data/drive| BRAIN
    BRAIN --> PG
    BRAIN -->|"Bearer JWT\n(API Key or CliProxy OAuth)"| OPENAI
    BRAIN -->|"Bearer SLACK_BOT_TOKEN"| SLACK
    BRAIN -->|"Bearer GITHUB_TOKEN"| GITHUB
    BRAIN -.->|ADC 설정 시| GDRIVE_API
    INIT -->|nc polling| PG
    INIT -->|ready| BRAIN
    WATCHER -->|goroutine| SLACK
    WATCHER -->|upsert| PG
```

### 마운트 경로 상세

```
호스트 경로:     ~/Google Drive/공유 드라이브/Vibers.AI
minikube 내부:   /mnt/drive    (minikube mount --uid=10001 --gid=10001 ... 으로 생성)
PV hostPath:     /mnt/drive    (deploy/k8s/second-brain-pv.yaml, 100Gi ReadOnlyMany)
Pod 마운트:      /data/drive   (deploy/k8s/second-brain-deployment.yaml volumeMounts)
```

CliProxy 시크릿 마운트:
```
Secret key:   auth.json (cliproxy-auth-secret)
Pod 경로:     /etc/cliproxy/auth.json
설정 변수:    CLIPROXY_AUTH_FILE=/etc/cliproxy/auth.json (ConfigMap)
```

---

## 3. 서비스 레이어 맵

### 백엔드 패키지 의존 관계

```mermaid
flowchart LR
    MAIN["cmd/server/main.go\nDI 조립, 시그널 처리"]
    CFG["internal/config\nConfig.Load()"]
    API["internal/api\nchi Router\n9개 엔드포인트"]
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
```

### 패키지별 책임 및 주요 심볼

| 패키지 | 주요 타입 / 함수 | 파일 |
|---|---|---|
| `cmd/server` | `run()` — DI 조립, `migrationsPath()` — MIGRATIONS_DIR 우선 해석 | `main.go` |
| `internal/config` | `Config` struct, `Load()` — 환경변수 파싱, 기본값 처리 | `config.go` |
| `internal/api` | `Server`, `Handler()` — chi 라우터, `requireAPIKey()` — Bearer 미들웨어 | `router.go`, `middleware.go`, `search.go`, `document.go`, `source.go`, `stats.go`, `collect_channel.go` |
| `internal/scheduler` | `Scheduler`, `run()`, `runCollector()`, `embedDocuments()`, `TriggerAll()`, `ForceCollectSlackChannel()`, `LookupSlackChannel()` | `scheduler.go` |
| `internal/collector` | `FilesystemCollector`, `SlackCollector`, `GitHubCollector`, `GDriveCollector`, `SlackChannelWatcher`, `DriveExporter` | `filesystem.go`, `slack.go`, `slack_watcher.go`, `github.go`, `gdrive.go`, `gdrive_export.go` |
| `internal/collector/extractor` | `Registry`, `SanitizeText()`, `TruncateUTF8()`, `HTMLExtractor`, `PDFExtractor`, `DocxExtractor`, `XlsxExtractor`, `PptxExtractor` | `extractor.go`, `html.go`, `pdf.go`, `docx.go`, `xlsx.go`, `pptx.go` |
| `internal/search` | `Service.Search()` — 임베딩 실패 시 fulltext 폴백, `EmbedClient.Embed()`, `EmbedClient.EmbedBatch()`, `cliProxyToken` (5분 TTL) | `search.go`, `embed.go` |
| `internal/store` | `DocumentStore`, `Upsert()`, `hybridSearch()`, `fulltextSearch()`, `LastCollectedAt()`, `MarkDeleted()`, `CountBySource()` | `document.go`, `postgres.go` |
| `internal/model` | `Document`, `SearchQuery`, `SearchResult`, `SourceType` 상수 5개 | `document.go` |

### HTTP 서버 설정 (`cmd/server/main.go:113`)

```
ReadTimeout:  15s
WriteTimeout: 30s
IdleTimeout:  60s
Port:         cfg.Port (기본 9200, ConfigMap에서 주입)
```

---

## 4. 데이터 모델

### ERD

```mermaid
erDiagram
    documents {
        uuid id PK "gen_random_uuid()"
        text source_type "NOT NULL — filesystem|slack|github|gdrive|notion"
        text source_id "NOT NULL — 소스 내 고유 식별자"
        text title "NOT NULL"
        text content "NOT NULL"
        jsonb metadata "default '{}'"
        vector_1536 embedding "nullable — 임베딩 미완료 시 NULL"
        tsvector tsv "GENERATED STORED — title(A) + content(B)"
        timestamptz collected_at "NOT NULL — 파일 ModTime 또는 수집 시각"
        timestamptz created_at "default now()"
        timestamptz updated_at "default now() — Upsert 시 갱신"
        text status "default 'active' — active|deleted|moved"
        timestamptz deleted_at "nullable — 소프트 삭제 시점"
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

    documents ||--o{ collection_log : "source_type 기준"
```

### `documents` 테이블 (`migrations/001_init.sql`, `migrations/002_soft_delete.sql`)

| 컬럼 | 타입 | 제약 조건 | 비고 |
|---|---|---|---|
| `id` | uuid | PK, `gen_random_uuid()` | |
| `source_type` | text | NOT NULL | `filesystem`, `slack`, `github`, `gdrive`, `notion` |
| `source_id` | text | NOT NULL | filesystem: 상대경로, slack: `{channel_id}:{ts}`, github: `{org/repo}#{number}` |
| `title` | text | NOT NULL | |
| `content` | text | NOT NULL | |
| `metadata` | jsonb | default `'{}'` | 소스별 부가 정보 |
| `embedding` | vector(1536) | nullable | text-embedding-3-small 차원 |
| `tsv` | tsvector | GENERATED ALWAYS STORED | english(A+B) + simple(A+B) 혼용 |
| `collected_at` | timestamptz | NOT NULL | 파일 ModTime UTC 기준 |
| `created_at` | timestamptz | default now() | |
| `updated_at` | timestamptz | default now() | Upsert 시 갱신 |
| `status` | text | default `'active'` | `active`, `deleted`, `moved` |
| `deleted_at` | timestamptz | nullable | 소프트 삭제 시점 |

### 인덱스 (`migrations/001_init.sql`, `migrations/002_soft_delete.sql`)

| 인덱스명 | 종류 | 대상 | 목적 |
|---|---|---|---|
| `(UNIQUE)` | UNIQUE | `(source_type, source_id)` | Upsert 충돌 키 |
| `idx_documents_tsv` | GIN | `tsv` | 전문 검색 |
| `idx_documents_embedding` | HNSW | `embedding vector_cosine_ops` | 코사인 유사도 ANN 검색 |
| `idx_documents_source` | B-tree | `source_type` | 소스별 필터 |
| `idx_documents_collected` | B-tree | `collected_at DESC` | 최신순 정렬 |
| `idx_documents_status` | B-tree | `status` | 소프트 삭제 필터 |

**pgvector 설정**: `HNSW (vector_cosine_ops)` — `migrations/001_init.sql:26`. IVFFlat 대신 HNSW를 선택한 이유는 인덱스 빌드 시점에 lists 파라미터가 필요 없고, 온라인 insert에 더 적합하기 때문이다. 기본 HNSW 파라미터(`m=16`, `ef_construction=64`)를 사용한다.

### `tsvector` 생성 규칙 (`migrations/001_init.sql:12-17`)

```sql
setweight(to_tsvector('english', coalesce(title,   '')), 'A') ||
setweight(to_tsvector('simple',  coalesce(title,   '')), 'A') ||
setweight(to_tsvector('english', coalesce(content, '')), 'B') ||
setweight(to_tsvector('simple',  coalesce(content, '')), 'B')
```

영어 형태소 분석(`english`)과 단순 토큰화(`simple`)를 모두 적용하여 영어 단어의 원형 검색과 한국어 키워드 검색을 동시에 지원한다.

### `collection_log` 테이블

수집 실행마다 한 행 기록. `started_at`, `finished_at`, `documents_count`, `error` 컬럼으로 수집 이력 추적. `store.RecordCollectionLog()` (`internal/store/document.go:329`)에서 기록.

---

## 5. 수집 파이프라인

### 전체 흐름 시퀀스

```mermaid
sequenceDiagram
    participant CRON as cron.Cron (robfig/cron)<br/>또는 POST /collect/trigger
    participant SCHED as Scheduler.run()
    participant CAS as atomic.Bool
    participant RC as runCollector()
    participant COL as Collector.Collect(ctx, since)
    participant EMBED as embedDocuments()
    participant STORE as DocumentStore.Upsert()
    participant DD as DeletionDetector (fs only)

    CRON->>SCHED: tick (COLLECT_INTERVAL, default 10m)
    SCHED->>CAS: CompareAndSwap(false→true)
    alt 이미 실행 중
        CAS-->>SCHED: false → warn + return
    else
        CAS-->>SCHED: true
        SCHED->>RC: runCollector(col)
        RC->>STORE: LastCollectedAt(source_type)
        STORE-->>RC: since timestamp (첫 실행: time.Time{})
        RC->>COL: Collect(ctx, since)
        COL-->>RC: []model.Document
        RC->>EMBED: embedDocuments(batch=20)
        loop 각 배치 20개
            EMBED->>OpenAI: POST /v1/embeddings
            OpenAI-->>EMBED: []float32 vectors
        end
        loop 각 Document
            RC->>STORE: Upsert(doc)
            STORE-->>RC: id, created_at, updated_at
        end
        RC->>STORE: RecordCollectionLog(src, started, count, err)
        opt FilesystemCollector (DeletionDetector)
            RC->>DD: ListActiveSourceIDs(ctx)
            DD-->>RC: []string (현재 파일 목록)
            RC->>STORE: MarkDeleted(source_type, activeIDs)
        end
        SCHED->>CAS: Store(false)
    end
```

### Scheduler 구조 (`internal/scheduler/scheduler.go`)

```go
type Scheduler struct {
    cron       *cron.Cron          // robfig/cron v3, seconds resolution
    collectors []collector.Collector
    store      DocumentUpserter
    embed      *search.EmbedClient
    running    atomic.Bool         // CAS 뮤텍스 (scheduler.go:57)
}
```

- `Register(interval)` — 각 Collector에 cron 스펙 `@every {interval}` 등록 (scheduler.go:73)
- `TriggerAll()` — `POST /api/v1/collect/trigger` 수동 트리거, goroutine 실행 (scheduler.go:101)
- `run()` — cron 틱 진입점, CAS 획득 후 단일 Collector 실행 (scheduler.go:119)
- `runCollector()` — 실제 수집 로직, `LastCollectedAt` → `Collect` → `embedDocuments` → `Upsert` → `RecordCollectionLog` → `MarkDeleted` (scheduler.go:131)

**defaultMaxEmbedChars = 8000** (scheduler.go:25): OpenAI 토큰 한도 회피용 기본 절단값. `MAX_EMBED_CHARS` 환경변수로 오버라이드 가능.

### SlackChannelWatcher (`internal/collector/slack_watcher.go`)

```mermaid
sequenceDiagram
    participant MAIN as main.go (goroutine)
    participant W as SlackChannelWatcher.Run()
    participant SLACK as Slack users.conversations
    participant STORE as DocumentStore

    MAIN->>W: go watcher.Run(ctx)
    Note over W: ticker 60s
    W->>SLACK: 첫 틱: ListMemberChannels()
    SLACK-->>W: 기존 채널 목록 → seen 맵에 seed (수집 안 함)
    loop 60초마다
        W->>SLACK: ListMemberChannels()
        SLACK-->>W: 현재 채널 목록
        W->>W: seen 차집합 = 신규 채널
        loop 각 신규 채널
            W->>SLACK: CollectChannel(ch.ID, ch.Name, time.Time{})
            W->>W: embedDocuments(batch=20)
            W->>STORE: Upsert (각 메시지)
            W->>STORE: RecordCollectionLog
        end
    end
```

`seen` 맵은 `sync.Mutex`로 보호 (slack_watcher.go:29). 첫 틱에서 기존 채널을 seen에만 추가하고 수집은 스킵 — 재시작 시 전체 재수집 방지.

### 수집기별 상세

#### FilesystemCollector (`internal/collector/filesystem.go`)

| 항목 | 값 |
|---|---|
| 루트 경로 | `FILESYSTEM_PATH` env (ConfigMap: `/data/drive`) |
| 증분 기준 | `info.ModTime().After(since)` (filesystem.go:145) |
| 스킵 디렉토리 | `.git`, `node_modules`, `dist`, `.next`, `.omc`, `.sisyphus`, `.claude` |
| 스킵 확장자 | `.bak`, `.gitkeep`, `.plist`, `.lock`, `.DS_Store` |
| 전체 텍스트 확장자 | `.md`, `.txt`, `.csv`, `.json`, `.js`, `.ts`, `.tsx`, `.py`, `.sh` |
| 텍스트 파일 상한 | 512 KB (`maxTextFileBytes`) |
| 추출기 확장자 | `.html`, `.htm`, `.pdf`, `.docx`, `.xlsx`, `.pptx` (Registry) |
| Google Workspace | `.gsheet`, `.gdoc`, `.gscript`, `.gslides`, `.gform` — URL 추출 + Drive API export 시도 |
| 이미지 처리 | `.png`, `.jpg`, `.jpeg`, `.gif`, `.svg` — 메타데이터 전용 |
| 아카이브 처리 | `.zip`, `.apk`, `.tar`, `.gz` — 메타데이터 전용 |
| 읽기 타임아웃 | 텍스트 파일: 3s (`fileReadTimeout`), GWorkspace: 2s, 바이너리 추출: 10s (`extractTimeout`) |
| 소프트 삭제 지원 | `ListActiveSourceIDs()` 구현 → `DeletionDetector` 인터페이스 충족 |

`SourceID` = `filepath.Rel(rootPath, absPath)` (filesystem.go:225) — 상대 경로를 중복 검사 키로 사용.

**알려진 이슈**: minikube의 9P 파일시스템 드라이버는 한국어 장파일명 처리 시 `lstat: file name too long` 오류를 반환. `filepath.WalkDir` 콜백에서 warn 로그 후 스킵 처리 (filesystem.go:126-130).

#### SlackCollector (`internal/collector/slack.go`)

| 항목 | 값 |
|---|---|
| 채널 범위 | `users.conversations` — 봇이 member인 채널만 (public + private) |
| DM 비수집 | `types=public_channel,private_channel` 파라미터 — IM/MPIM 명시적 제외 |
| 증분 기준 | `since.Unix() > 0`일 때만 `oldest` 파라미터 전송 (slack.go:225) |
| 쓰레드 처리 | `reply_count > 0`인 메시지마다 `conversations.replies` 호출 → 독립 Document로 저장 |
| 페이지네이션 | cursor 기반, limit=200 (slack.go:183, 218, 266) |
| HTTP 타임아웃 | 30s (`http.Client{Timeout: 30 * time.Second}`) |
| SourceID | `{channel_id}:{ts}` (slack.go:119) |
| Title 형식 | `#{channel_name} — {ts}` |

**알려진 한계**: 부모 메시지가 `since` 이전에 작성되었지만 그 이후에 thread reply가 달린 경우, 증분 수집에서 해당 reply를 놓침 (slack.go:45 주석).

#### GitHubCollector (`internal/collector/github.go`)

| 항목 | 값 |
|---|---|
| 수집 단위 | `GITHUB_ORG` 내 비아카이브 레포지터리 전체 |
| 수집 대상 | Issues + Pull Requests (PR은 `pull_request` 필드 non-nil로 구분) |
| 증분 기준 | `/repos/{repo}/issues?state=all&since={RFC3339}` |
| 페이지네이션 | page 기반, per_page=100 |
| API 버전 | `X-GitHub-Api-Version: 2022-11-28` |
| HTTP 타임아웃 | 30s |
| SourceID | `{org/repo}#{number}` (github.go:122) |
| Title 형식 | `[{org/repo}] {issue_title}` |

#### GDriveCollector (`internal/collector/gdrive.go`, `gdrive_export.go`)

스캐폴드 상태. `Enabled()` = `false` (credentials 없을 때). Drive API는 `FilesystemCollector`와 연동된 `DriveExporter`를 통해 Google Workspace stub 파일(`.gdoc`, `.gsheet` 등)의 내용을 export하는 형태로만 활성화.

`DriveExporter.Export()` MIME 매핑:
- `.gsheet` → `text/csv`
- `.gdoc`, `.gscript`, `.gform`, `.gslides` → `text/plain`

#### NotionCollector (`internal/collector/notion.go`)

비활성화. `cmd/server/main.go:75` 주석 참조 — 재활성화 시 `NewNotionCollector()` 등록 필요.

---

## 6. 추출 파이프라인

`internal/collector/extractor/` 패키지의 `Registry`가 확장자별 Extractor를 선택한다. 모든 추출기는 같은 인터페이스를 구현한다.

```go
type Extractor interface {
    Supports(ext string) bool
    Extract(ctx context.Context, absPath string) (string, error)
}
```

Registry 등록 순서 (extractor.go:42-49): `HTMLExtractor` → `PDFExtractor` → `DocxExtractor` → `XlsxExtractor` → `PptxExtractor`

### SanitizeText (`internal/collector/extractor/extractor.go:92`)

모든 추출기 출력에 공통 적용:

1. `\x00` → `" "` (space) — Postgres TEXT 저장 오류 방지
2. `strings.ToValidUTF8(s, "\uFFFD")` — 유효하지 않은 UTF-8 치환
3. 연속 `\n\n\n` → `\n\n` 압축 (문단 구조 유지)

### TruncateUTF8 (`extractor.go:64`)

추출 완료 후 `MaxExtractedBytes = 512 KB` 상한 적용. UTF-8 경계를 지켜 절단하고 `\n[content truncated]` 접미사 추가.

### 추출기별 상세

| 확장자 | 추출기 | 라이브러리 | 처리 방식 | 특이사항 |
|---|---|---|---|---|
| `.html`, `.htm` | `HTMLExtractor` | `x/net/html` | 태그 파싱 후 텍스트 노드 연결 | 인라인 스크립트/스타일 제외 |
| `.pdf` | `PDFExtractor` | `github.com/ledongthuc/pdf` | 페이지별 `GetPlainText()` 순차 연결 | 10s ctx 타임아웃, goroutine abandon 패턴 (pdf.go:25-44) |
| `.docx` | `DocxExtractor` | 표준 `archive/zip` + `encoding/xml` | OOXML 압축 해제 → `word/document.xml` → `<w:t>` 노드 | `</w:p>` 마다 개행 삽입 (docx.go:93) |
| `.xlsx` | `XlsxExtractor` | `github.com/xuri/excelize/v2` | `RawCellValue:true` → 시트별 TSV 블록 | 200 KiB 상한 (`xlsxMaxBytes`), 빈 행/시트 스킵, 셀 내 `\t`/`\n` → 공백 |
| `.pptx` | `PptxExtractor` | 표준 `archive/zip` + `encoding/xml` | OOXML 압축 해제 → `ppt/slides/*.xml` → `<a:t>` 노드 | |

### XLSX TSV 출력 형식

```
##SHEET Sheet1
col_a\tcol_b\tcol_c
val1\tval2\tval3

##SHEET Summary
name\ttotal
Alice\t100
```

- 프론트엔드 `XlsxTable.tsx`가 `##SHEET` 구분자를 파싱하여 시트당 최대 200행 렌더링
- 200 KiB 초과 시 `\n...(truncated)` 접미사 추가 후 조기 종료 (xlsx.go:116)

### 컨텐츠 크기 계층

| 레이어 | 상한 | 위치 |
|---|---|---|
| 텍스트 파일 인라인 읽기 | 512 KB | `filesystem.go:43 (maxContentBytes = 1MB` → `maxTextFileBytes = 512KB`) |
| 추출기 출력 | 512 KB | `extractor.go:16 (MaxExtractedBytes)` |
| XLSX 중간 버퍼 | 200 KB | `xlsx.go:16 (xlsxMaxBytes)` |
| 임베딩 입력 | 8000자 (기본) | `scheduler.go:25 (defaultMaxEmbedChars)` |
| raw 파일 API | 50 MiB | `internal/api/document.go` |

---

## 7. 임베딩 파이프라인

### 토큰 소스 우선순위 (`internal/search/embed.go:92`)

```go
func NewEmbedClient(apiURL, apiKey, authFilePath, model string) *EmbedClient {
    var ts tokenSource
    switch {
    case apiKey != "":        // 1순위: EMBEDDING_API_KEY 환경변수
        ts = &staticToken{t: apiKey}
    case authFilePath != "": // 2순위: CliProxy JSON 파일 (5분 TTL 자동 갱신)
        ts = newCliProxyToken(authFilePath)
    }
    // 3순위: Authorization 헤더 없음 (자체 호스팅 호환)
}
```

`cliProxyToken` (embed.go:29): 5분 TTL 캐시, 만료 시 파일 재읽기. `access_token` 필드 파싱. `sync.Mutex`로 동시성 보호.

### EmbedBatch 흐름 (`internal/scheduler/scheduler.go:198`)

```mermaid
sequenceDiagram
    participant SCHED as embedDocuments()
    participant EMBED as EmbedClient.EmbedBatch()
    participant OAI as OpenAI /v1/embeddings

    loop docs를 batchSize=20 단위로
        SCHED->>SCHED: text = title + "\n\n" + content
        alt len(text) > maxLen (default 8000)
            SCHED->>SCHED: text = text[:maxLen]
            SCHED->>SCHED: slog.Warn("content truncated")
        end
        SCHED->>EMBED: EmbedBatch(ctx, texts[20])
        EMBED->>OAI: POST /v1/embeddings\n{input: [...], model: "text-embedding-3-small"}
        OAI-->>EMBED: [{index, embedding: float32[1536]}]
        alt API 오류
            EMBED-->>SCHED: error
            SCHED->>SCHED: slog.Warn("batch failed, skipping chunk")
            Note over SCHED: 배치 실패가 전체 수집을 중단하지 않음
        else 성공
            EMBED-->>SCHED: [][]float32
            SCHED->>SCHED: docs[i].Embedding = vecs[i]
        end
    end
```

### 검색 시 쿼리 임베딩 (`internal/search/search.go:34`)

```go
func (s *Service) Search(ctx context.Context, q model.SearchQuery) ([]*model.SearchResult, error) {
    if s.embed.Enabled() {
        vec, err := s.embed.Embed(ctx, q.Query)  // 단일 쿼리 임베딩
        if err != nil {
            slog.Warn("search: embedding failed, falling back to full-text", "error", err)
            // 임베딩 실패 시 FTS 전용으로 그레이스풀 폴백
        } else {
            q.Embedding = vec
        }
    }
    return s.store.Search(ctx, q)
}
```

**임베딩 비활성화 시 동작**: `EMBEDDING_API_URL`이 빈 문자열이면 `EmbedClient.Enabled()` = false. 수집 시 임베딩 스킵, 검색 시 FTS 전용.

---

## 8. 검색 파이프라인

### API 엔드포인트

`POST /api/v1/search` — Bearer 인증 필요 (API_KEY 설정 시)

`GET /api/v1/stats/baseline` — Bearer 인증 필요. 문서 수·컨텐츠 길이 백분위수(p50/p95), 청크 집계, 추출 실패 수, 소스별 최신 수집 시각을 JSON으로 반환. 스키마: `{documents, chunks, extraction_failures, collection}`.

**요청 스키마**:
```json
{
  "query": "BBQ 미팅",
  "source_type": "filesystem",
  "exclude_source_types": ["slack"],
  "limit": 10,
  "sort": "relevance",
  "include_deleted": false
}
```

**응답 스키마**:
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
  "query": "BBQ 미팅",
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
    AND status = 'active'  -- include_deleted=false 시
    AND source_type = $4   -- source_type 필터 시
    LIMIT $3               -- limit * 2 (RRF 후보 확장)
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

**RRF 상수 k=60**: 고순위 문서의 기여를 과도하게 증폭시키지 않으면서 순위 결합 효과를 안정적으로 제공하는 검색 커뮤니티 표준값.

### 검색 모드 결정

| 조건 | 모드 | 함수 |
|---|---|---|
| `query.Embedding` 비어 있지 않음 | hybrid (RRF) | `hybridSearch()` |
| `query.Embedding` nil 또는 빈 슬라이스 | fulltext only | `fulltextSearch()` |

임베딩 실패 시 자동으로 fulltext 모드로 폴백 (search.go:40-46).

### 필터 파라미터

| 파라미터 | 타입 | 기본값 | 설명 |
|---|---|---|---|
| `query` | string | (필수) | 검색어 |
| `source_type` | string | — | 단일 소스 필터 (`filesystem`, `slack`, `github`) |
| `exclude_source_types` | []string | — | 제외할 소스 목록 |
| `limit` | int | 20 | 결과 수, 0 이하면 20 적용 |
| `sort` | string | `"relevance"` | `"relevance"` (score DESC) 또는 `"recent"` (collected_at DESC) |
| `include_deleted` | bool | false | true 시 `status='deleted'` 문서 포함 |

`sortOrder()` 화이트리스트 (`store/document.go:188`): `"recent"` 외 모든 값은 `"score DESC"` — SQL 인젝션 방지.

### sortOrder 동작 차이

| sort 값 | SQL | 의미 |
|---|---|---|
| `"recent"` | `collected_at DESC` | 최근 수집 순 (파일 ModTime 기준) |
| `"relevance"` 또는 그 외 | `rrf.score DESC` | RRF 점수 내림차순 |

---

## 9. 배포 아키텍처

### Kubernetes 리소스 맵

```mermaid
flowchart TB
    subgraph Cluster["minikube Cluster"]
        NS["Namespace: second-brain"]

        subgraph NS
            CM["ConfigMap\nsecond-brain-config\n환경변수 (비민감)"]
            SEC1["Secret\nsecond-brain-secret\nDB PASSWORD, API_KEY 등"]
            SEC2["Secret\ncliproxy-auth-secret\nauth.json (out-of-band)"]

            PV["PV\nsecond-brain-drive-pv\nhostPath /mnt/drive\n100Gi ReadOnlyMany"]
            PVC_PG["PVC (StatefulSet)\npgdata 10Gi\nReadWriteOnce"]

            PGSVC["Service\npostgres\nClusterIP :5432"]
            PGSF["StatefulSet\npostgres\npgvector/pgvector:pg16\nreadiness: pg_isready"]

            BRSVC["Service\nsecond-brain\nNodePort 30920→:9200"]
            BRDEP["Deployment\nsecond-brain\nsecond-brain:dev ~34.5MB\nrequests: 128Mi/100m\nlimits: 512Mi/500m"]
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
```

### K8s 리소스 목록 (`deploy/k8s/`)

| 파일 | 리소스 종류 | 역할 |
|---|---|---|
| `namespace.yaml` | Namespace | `second-brain` 네임스페이스 |
| `second-brain-configmap.yaml` | ConfigMap | 비민감 설정 (`COLLECT_INTERVAL=5m`, `MAX_EMBED_CHARS=8000` 등) |
| `second-brain-secret.yaml` | Secret | placeholder (`POSTGRES_PASSWORD`, `API_KEY`, `SLACK_BOT_TOKEN`, `GITHUB_TOKEN`) |
| `second-brain-pv.yaml` | PersistentVolume | hostPath `/mnt/drive` 100Gi ReadOnlyMany (클러스터 스코프) |
| `second-brain-deployment.yaml` | Deployment | second-brain 앱, initContainer, volume mounts, probe |
| `second-brain-service.yaml` | Service | NodePort 30920 → 컨테이너 9200 |
| `second-brain-web-deployment.yaml` | Deployment | vibers-web, BRAIN_API_URL 참조 |
| `second-brain-web-service.yaml` | Service | NodePort 30300 → 컨테이너 3000 |
| `postgres-statefulset.yaml` | StatefulSet | pgvector:pg16, PVC 10Gi, `pg_isready` probe |
| `postgres-service.yaml` | Service | ClusterIP :5432 |
| `kustomization.yaml` | Kustomize | 리소스 목록 |

### 이미지 빌드

macOS docker driver 환경에서는 `eval $(minikube docker-env)`로 minikube의 Docker 데몬을 사용해야 한다:

```bash
eval $(minikube docker-env)
docker build -t second-brain:dev .
docker build -t vibers-web:dev ./web
```

| 이미지 | 빌드 베이스 | 런타임 베이스 | 크기 | UID |
|---|---|---|---|---|
| `second-brain:dev` | `golang:1.24-alpine` | `alpine:3.21` | ~34.5 MB | 10001 |
| `vibers-web:dev` | `node:22-alpine` | `node:22-alpine` (standalone) | ~195 MB | 10001 |

### 초기 배포 순서

```bash
# 1. minikube 마운트 (백그라운드 유지)
minikube mount "/Users/sangyi/Google Drive/공유 드라이브/Vibers.AI:/mnt/drive" --uid=10001 --gid=10001 &

# 2. PV 먼저 생성 (클러스터 스코프)
kubectl apply -f deploy/k8s/second-brain-pv.yaml

# 3. 나머지 리소스
kubectl apply -k deploy/k8s/

# 4. out-of-band Secret 수동 생성 (git 미포함)
kubectl create secret generic cliproxy-auth-secret \
  --from-file=auth.json=/path/to/auth.json \
  -n second-brain
```

### 접근 방법 (port-forward)

```bash
# 웹 UI
kubectl port-forward svc/vibers-web 30300:80 -n second-brain

# 백엔드 API
kubectl port-forward svc/second-brain 30920:8080 -n second-brain
```

### Probe 설정

| 컨테이너 | Probe 종류 | 엔드포인트 | initialDelay | period |
|---|---|---|---|---|
| second-brain | readiness | `GET /health :9200` | 10s | 10s |
| second-brain | liveness | `GET /health :9200` | 30s | 15s |
| postgres | readiness | `pg_isready -U brain -d second_brain` | 10s | 5s |
| postgres | liveness | `pg_isready -U brain -d second_brain` | 30s | 10s |

---

## 10. 웹 UI 아키텍처

### Next.js App Router 구조

```
web/src/app/
├── page.tsx                    # 검색 메인 — 검색바, 필터, 결과 카드 목록
├── layout.tsx                  # 헤더 네비게이션, dark mode 지원
├── api-docs/page.tsx           # API 레퍼런스 — 9개 엔드포인트 카드
├── documents/[id]/page.tsx     # 문서 상세 — 포맷별 렌더링 분기
├── documents/[id]/MarkdownContent.tsx  # react-markdown + remark-gfm + rehype-highlight
├── documents/[id]/XlsxTable.tsx        # ##SHEET TSV 파싱, 시트당 최대 200행
└── api/                        # Next.js API 라우트 (백엔드 프록시)
    ├── search/route.ts         # GET+POST → BRAIN /api/v1/search
    ├── documents/route.ts      # GET → BRAIN /api/v1/documents
    ├── documents/[id]/route.ts # GET → BRAIN /api/v1/documents/{id}
    ├── documents/[id]/raw/route.ts # GET → BRAIN /api/v1/documents/{id}/raw
    └── stats/route.ts          # GET → BRAIN /api/v1/stats
```

### API 프록시 패턴 (`web/src/app/api/search/route.ts`)

```typescript
const BACKEND_URL =
  process.env.BRAIN_API_URL          // 1순위: K8s 서비스 URL
  ?? process.env.NEXT_PUBLIC_API_URL // 2순위: 공개 URL
  ?? "http://localhost:9200";        // 3순위: 로컬 개발 기본값

const API_KEY = process.env.API_KEY ?? "";  // 백엔드 Bearer 토큰 서버사이드 주입
```

클라이언트에 백엔드 주소나 API 키를 노출하지 않는 서버사이드 프록시 패턴.

### 필터 옵션 (`web/src/app/page.tsx:18`)

```typescript
const FILTER_OPTIONS: FilterOption[] = [
  { value: "all",        label: "전체"   },
  { value: "filesystem", label: "Drive"  },
  { value: "slack",      label: "Slack"  },
  { value: "github",     label: "GitHub" },
];
```

카운터 표시: `전체` 필터 선택 시 Slack 카운트를 제외한 합계 표시 (`page.tsx:43` — `total - by_source.slack`).

### 반응성 패턴

검색 결과 자동 재요청 트리거:
- `[submittedQuery, activeFilter, sort]` 변경 — 검색 모드
- `[activeFilter, isSearchMode]` 변경 — 초기 문서 목록 모드

`cancelled` 플래그로 응답 경쟁 상태 방지. 초기 상태(검색어 없음): `listRecentDocuments(10, source)` 호출.

### 문서 상세 렌더링 분기

`getRenderKind(ext)` 함수가 확장자로 렌더링 방식 결정:

| 종류 | 확장자 | 렌더러 |
|---|---|---|
| `image` | `.png`, `.jpg`, `.gif` 등 | `<img>` 태그 |
| `markdown` | `.md`, `.mdx` | `MarkdownContent` — react-markdown + remark-gfm + rehype-highlight |
| `xlsx` | `.xlsx` | `XlsxTable` — `##SHEET` TSV 파싱, 시트당 최대 200행 |
| `code` | `.ts`, `.go`, `.py` 등 | 코드 블록 (highlight.js github-dark) |
| `text` | 그 외 | `<pre>` plain text |

### match_type 배지 (`page.tsx:37`)

| match_type | 한국어 배지 |
|---|---|
| `fulltext` | 전문 |
| `vector` | 의미 |
| `hybrid` | 복합 |

---

## 11. 설정 및 환경 변수

### 백엔드 (`internal/config/config.go`)

| 환경 변수 | 필수 | 기본값 | 사용 위치 | 설명 |
|---|---|---|---|---|
| `DATABASE_URL` | 선택 | `postgres://brain:brain@localhost:5432/second_brain?sslmode=disable` | `store/postgres.go` | Postgres 연결 문자열 |
| `PORT` | 선택 | `9200` | `cmd/server/main.go:113` | HTTP 서버 포트 |
| `EMBEDDING_API_URL` | 선택 | `https://api.openai.com/v1` | `search/embed.go:92` | OpenAI 호환 엔드포인트 |
| `EMBEDDING_API_KEY` | 선택 | — | `search/embed.go:95` | Static API 키 (1순위 토큰) |
| `EMBEDDING_MODEL` | 선택 | `text-embedding-3-small` | `search/embed.go:101` | 임베딩 모델 |
| `CLIPROXY_AUTH_FILE` | 선택 | — | `search/embed.go:98` | auth.json 경로 (2순위 토큰) |
| `COLLECT_INTERVAL` | 선택 | `10m` | `scheduler/scheduler.go:73` | 수집 주기 (Go duration) |
| `MAX_EMBED_CHARS` | 선택 | `8000` | `scheduler/scheduler.go:29` | 임베딩 입력 최대 문자 수 |
| `MIGRATIONS_DIR` | 선택 | 자동 탐지 | `cmd/server/main.go:153` | SQL 마이그레이션 디렉토리 |
| `FILESYSTEM_PATH` | 선택 | — | `collector/filesystem.go:101` | 파일시스템 수집 루트 |
| `FILESYSTEM_ENABLED` | 선택 | `false` | `cmd/server/main.go:77` | 파일시스템 수집기 활성화 |
| `SLACK_BOT_TOKEN` | 선택 | — | `collector/slack.go:28` | Slack Bot User OAuth 토큰 |
| `SLACK_TEAM_ID` | 선택 | — | `collector/slack.go:28` | Slack 팀 ID |
| `GITHUB_TOKEN` | 선택 | — | `collector/github.go:26` | GitHub Personal Access Token |
| `GITHUB_ORG` | 선택 | — | `collector/github.go:26` | GitHub 조직명 |
| `GDRIVE_CREDENTIALS_JSON` | 선택 | — | `collector/gdrive.go` | Google Drive 서비스 계정 JSON |
| `NOTION_TOKEN` | 선택 | — | `config/config.go:37` | Notion 토큰 (현재 미사용) |
| `API_KEY` | 선택 | — | `api/middleware.go:15` | Bearer 토큰 인증 (빈 문자열 시 비활성) |

### ConfigMap에서 주입되는 값 (`deploy/k8s/second-brain-configmap.yaml`)

| 키 | 값 |
|---|---|
| `COLLECT_INTERVAL` | `5m` |
| `MAX_EMBED_CHARS` | `8000` |
| `EMBEDDING_API_URL` | `https://api.openai.com/v1` |
| `EMBEDDING_MODEL` | `text-embedding-3-small` |
| `CLIPROXY_AUTH_FILE` | `/etc/cliproxy/auth.json` |
| `FILESYSTEM_PATH` | `/data/drive` |
| `FILESYSTEM_ENABLED` | `true` |
| `PORT` | `9200` |
| `MIGRATIONS_DIR` | `/app/migrations` |

### Secret 관리 원칙

| 종류 | 관리 방법 |
|---|---|
| `second-brain-secret` | `deploy/k8s/second-brain-secret.yaml` placeholder → 배포 시 실제 값 치환 |
| `cliproxy-auth-secret` | out-of-band 수동 `kubectl create secret` — git 미포함 |

### 프론트엔드 (`web/.env.example` 기반)

| 환경 변수 | 필수 | 설명 |
|---|---|---|
| `BRAIN_API_URL` | 선택 | 백엔드 서비스 URL (K8s 내부: `http://second-brain:9200`) |
| `NEXT_PUBLIC_API_URL` | 선택 | 클라이언트 접근 가능 URL (미사용 권장) |
| `API_KEY` | 선택 | 백엔드 Bearer 토큰 (서버사이드만 사용) |

### 11.1 cliproxy 통합

**목적**: OpenAI 호환 API 호출(embeddings, chat completions)을 ChatGPT Pro / Claude Pro / Gemini OAuth 기반으로 프록시. second-brain은 cliproxy를 **로컬 HTTP 프록시**로 사용하며, cliproxy 자체가 OAuth 토큰 갱신 · 라우팅 · rate limit을 담당한다.

#### 배포 모델

```mermaid
flowchart LR
    pod[second-brain Pod<br/>:9200] -- "HTTP<br/>Bearer inbound key" --> cliproxy[cliproxy<br/>127.0.0.1:8317]
    cliproxy -- "OAuth access_token<br/>refresh 자동" --> upstream[(OpenAI API<br/>chat.openai.com)]

    subgraph host["host (baekenough-ubuntu24)"]
      cliproxy
      auth["~/.cli-proxy-api/<br/>codex-*.json<br/>claude-*.json<br/>gemini-*.json"]
      cliproxy -.-> auth
    end

    subgraph minikube["minikube docker driver"]
      pod
    end

    pod -->|host.minikube.internal:8317| cliproxy
```

#### 인증 계층 (2단계)

1. **Inbound (second-brain → cliproxy)**: 정적 API key — cliproxy `config.yaml`의 `api-keys` 리스트에 등록된 값. 환경변수 `LLM_API_KEY`, `EMBEDDING_API_KEY`에 저장.
2. **Upstream (cliproxy → OpenAI/Claude/Gemini)**: OAuth access_token (`~/.cli-proxy-api/*.json`). cliproxy가 자동 갱신하므로 second-brain은 개입 불필요.

#### 환경변수 매핑

| 변수 | 용도 | 서버 값 예시 |
|---|---|---|
| `LLM_API_URL` | chat completions 엔드포인트 | `http://host.minikube.internal:8317/v1` |
| `LLM_API_KEY` | inbound API key | cliproxy `config.yaml`의 키 |
| `LLM_MODEL` | 모델 식별자 | `gpt-codex-5.3` |
| `EMBEDDING_API_URL` | embeddings 엔드포인트 (cliproxy 미지원 시 404 → FTS 폴백) | 동일 |
| `EMBEDDING_API_KEY` | inbound API key (LLM과 동일 가능) | 동일 |
| `CLIPROXY_AUTH_FILE` | 폐기 예정 — static key 경로가 우선 | unused |

#### 지원 엔드포인트

| Path | cliproxy | OpenAI 직접 | 현재 second-brain |
|---|---|---|---|
| `/v1/chat/completions` | ✅ | ✅ | cliproxy 사용 |
| `/v1/embeddings` | ❌ (404) | ✅ | FTS 폴백 중, 결정 대기 (#34) |
| `/v1/models` | ✅ | ✅ | 미사용 |

#### 실패 모드

| 상황 | 증상 | 대응 |
|---|---|---|
| cliproxy 다운 | 모든 LLM 호출 실패 → Discord 멘션 응답 에러 | `pm2 restart cli-proxy-api` |
| inbound key 불일치 | `401 Invalid API key` | `LLM_API_KEY` / `EMBEDDING_API_KEY` 와 cliproxy `config.yaml` 키 일치 확인 |
| embeddings 호출 | `404 page not found` | 정상 — FTS 폴백으로 처리됨 |
| OAuth 토큰 만료 | cliproxy가 자동 refresh | second-brain 영향 없음 |

#### 운영 명령

```bash
# cliproxy 상태 확인
pm2 list | grep cli-proxy-api
pm2 logs cli-proxy-api

# cliproxy 재시작
pm2 restart cli-proxy-api

# systemd user service (현재 disabled, 전환 가능)
systemctl --user status cliproxy.service
```

**관련 이슈**: #33 (본 문서화) · #34 (embedding 라우팅 결정) · #4 (JWT 폐기 — cliproxy로 대체됨)

---

## 12. 설계 결정 ADR

### ADR-001: pgvector 하이브리드 검색 (RRF 결합)

**Context**: 자연어 쿼리에서 의미 검색(벡터)만으로는 키워드 정확도가 떨어지고, 전문 검색(FTS)만으로는 의미적 유사 문서를 놓친다.

**Decision**: Reciprocal Rank Fusion(RRF) 알고리즘으로 FTS와 벡터 검색 결과를 결합한다. k=60 상수, FULL OUTER JOIN으로 한쪽에만 존재하는 결과도 포함한다 (`store/document.go:296`).

**Consequences**: 검색 품질이 단일 방식 대비 향상. 쿼리당 OpenAI 임베딩 API 호출이 필요하여 지연 증가. 임베딩 실패 시 FTS 폴백으로 가용성 유지. rerank/HyDE는 Phase 3 계획.

---

### ADR-002: pgvector on Postgres 16 단일 DB

**Context**: 벡터 스토어와 관계형 메타데이터를 분리할 경우(Pinecone + RDS 등) 운영 복잡도 급증.

**Decision**: `pgvector/pgvector:pg16` StatefulSet 단일 인스턴스. PVC(10Gi ReadWriteOnce) 기반 데이터 영속성.

**Consequences**: 운영 단순화, 단일 백업 대상. Go + pgx/v5 + pgvector-go 스택으로 타입 안전한 벡터 조작. 수억 벡터 수준에서는 전용 벡터 DB 마이그레이션 검토 필요.

---

### ADR-003: Go + Next.js 분리 서비스, App Router 프록시

**Context**: 백엔드와 프론트엔드를 단일 서버에 통합하면 개발 속도와 배포 독립성 저하.

**Decision**: Go HTTP 서버(백엔드)와 Next.js App Router(프론트엔드)를 독립 서비스로 분리. `web/src/app/api/` 라우트가 BRAIN 백엔드로 서버사이드 프록시하여 CORS 문제 회피, 클라이언트에 백엔드 주소 미노출.

**Consequences**: 서비스 독립 배포 가능. Next.js API 라우트라는 추가 레이어가 생기지만 클라이언트 코드 단순화.

---

### ADR-004: minikube docker driver + port-forward

**Context**: 프로덕션 환경과 동일한 K8s 매니페스트로 로컬 개발이 가능해야 하며, 멀티 컨테이너 오케스트레이션 필요.

**Decision**: docker driver 기반 minikube를 표준 로컬 환경으로 채택. macOS docker driver는 NodePort를 호스트에 직접 노출하지 않으므로 `kubectl port-forward`를 권장한다.

**Consequences**: 프로덕션 매니페스트와 로컬 환경 동일 유지. `eval $(minikube docker-env)`로 이미지 빌드 시 내부 데몬 사용 필수.

---

### ADR-005: CliProxy OAuth 토큰을 K8s Secret 볼륨 마운트

**Context**: ChatGPT Plus Codex OAuth 토큰은 정기 갱신되며, 환경변수 주입 시 재배포 없이 갱신이 어렵다.

**Decision**: `cliproxy-auth-secret` Secret을 out-of-band 관리, Pod의 `/etc/cliproxy/auth.json`에 볼륨 마운트. `cliProxyToken`이 5분 TTL로 파일 재읽기 (`embed.go:46`).

**Consequences**: 토큰 갱신 시 Pod 재시작 불필요. Secret은 git 미포함, 배포마다 수동 `kubectl create secret` 필요.

---

### ADR-006: `-trimpath` 빌드 + `MIGRATIONS_DIR` env

**Context**: Go 바이너리에 절대 경로 포함 시 빌드 재현성 저하, 경로 노출 보안 이슈.

**Decision**: `go build -trimpath` 빌드. 마이그레이션 경로는 `MIGRATIONS_DIR` 환경변수로 주입 (`cmd/server/main.go:153`). ConfigMap에서 `/app/migrations`로 설정.

**Consequences**: 빌드 재현성 향상. `runtime.Caller(0)` 결과가 모듈 상대 경로가 되어 `filepath.IsAbs()` 검사로 폴백 처리 필요 (main.go:163).

---

### ADR-007: `atomic.Bool` CAS scheduler 뮤텍스

**Context**: 수집 작업 실행 중 cron 틱 또는 수동 트리거가 중복 실행을 시도할 수 있다.

**Decision**: `sync/atomic.Bool.CompareAndSwap(false, true)`로 논블로킹 skip 구현 (`scheduler.go:57`). `sync.Mutex` 대신 CAS를 선택하여 교착 상태 위험 제거.

**Consequences**: 코드 단순, 교착 상태 없음. 분산 환경(멀티 Pod)에서는 Redis 등 외부 잠금 필요. 현재 단일 Pod이므로 충분.

---

### ADR-008: `SanitizeText` 공유 함수 (Postgres 0x00 회피)

**Context**: Postgres `text` 타입은 NULL 바이트(`\x00`)를 저장 불가. PDF, Office 파일에서 유효하지 않은 UTF-8 포함 가능.

**Decision**: 모든 추출기 출력에 `SanitizeText` 공통 적용 (`extractor.go:92`). NULL 바이트 제거 → 유효 UTF-8 치환 → 과도한 개행 압축 순서.

**Consequences**: DB 저장 오류 제거. 미미한 정보 손실(NULL 바이트, 잘못된 바이트)이 있으나 검색 품질에 영향 없음.

---

### ADR-009: `MAX_EMBED_CHARS` env (청킹 전 임시 완화책)

**Context**: OpenAI 임베딩 API 토큰 한도(8191 토큰). 대형 파일 전체 내용이 초과 가능.

**Decision**: `MAX_EMBED_CHARS` 환경변수(기본 8000자)로 문자 수 기준 절단 (`scheduler.go:25`). Phase 1 청킹 구현 전 임시 완화책.

**Consequences**: 긴 문서 뒷부분이 임베딩에서 누락, 검색 재현율 저하. 청킹 구현 후 이 환경변수 제거 예정 (Phase 1 TODO 주석: scheduler.go:213).

---

### ADR-010: OpenAI ChatGPT Codex OAuth JWT 직접 Bearer

**Context**: OpenAI API 키 없이 ChatGPT Plus Codex OAuth 토큰 활용. 미들웨어(`cli-proxy-api` 데몬) 필요로 알려져 있었음.

**Decision**: `iss: auth.openai.com`, `aud: api.openai.com/v1` JWT를 `/v1/embeddings` Bearer 헤더에 직접 사용. 데몬 없이 동작 직접 검증 완료.

**Consequences**: 인프라 복잡도 감소 (데몬 불필요). 비공식 동작으로 OpenAI 정책 변경 시 중단 가능. 프로덕션 전환 시 정식 API 키 권장.

---

### ADR-011: Slack DM 비수집 (프라이버시 설계)

**Context**: Slack DM은 개인 대화로, 수집 시 프라이버시 침해 우려.

**Decision**: `users.conversations` API에 `types=public_channel,private_channel` 파라미터를 명시적으로 전달하여 IM(DM) 및 MPIM(그룹 DM) 채널을 열거 대상에서 제외 (`slack.go:183`). 봇이 member인 채널만 수집하므로 초대받지 않은 채널도 자동 제외.

**Consequences**: 팀원 간 DM은 검색 불가. 팀 지식베이스는 공개/비공개 채널 대화만 포함. 명시적 제외가 코드로 문서화됨.

---

### ADR-012: Google Drive FS 스캔 (vs Drive API 전체)

**Context**: Google Drive 전체를 Drive API로 스캔하면 API 할당량 소모, 인증 복잡도 증가.

**Decision**: minikube mount로 Drive 폴더를 파일시스템으로 노출하고 `FilesystemCollector`가 로컬 파일처럼 스캔. Google Workspace stub 파일(`.gdoc` 등)은 ADC가 설정된 경우에만 `DriveExporter`를 통해 내용 export (`filesystem.go:389`). ADC 미설정 시 URL 메타데이터만 저장.

**Consequences**: API 할당량 문제 없음. minikube mount가 실행 중이어야 함 (백그라운드 프로세스). Workspace 파일 내용은 ADC 설정 여부에 따라 선택적 색인.

---

## 13. 알려진 이슈

GitHub 이슈로 추적 중. 전체 목록: https://github.com/baekenough/second-brain/issues

### P0 버그 (즉시 수정 필요)

| # | 제목 | 관련 파일 |
|---|---|---|
| [#1](https://github.com/baekenough/second-brain/issues/1) | Scheduler run() 동시 실행 방지 | `internal/scheduler/scheduler.go` |
| [#2](https://github.com/baekenough/second-brain/issues/2) | PDF 다단계 fallback 체인 구현 | `internal/collector/*/pdf.go` |
| [#3](https://github.com/baekenough/second-brain/issues/3) | 8KB 텍스트 절단 제거 및 chunks 기반 임베딩 전환 | `internal/scheduler/scheduler.go` |
| [#4](https://github.com/baekenough/second-brain/issues/4) | OpenAI 임베딩 API 키 영구 전환 | 운영 |
| [#5](https://github.com/baekenough/second-brain/issues/5) | Slack rate limit 지수 backoff + Retry-After 존중 | `internal/collector/slack.go` |
| [#6](https://github.com/baekenough/second-brain/issues/6) | minikube hostPath Google Drive 호스트 종속 제거 | `deploy/k8s/*pv*.yaml` |
| [#7](https://github.com/baekenough/second-brain/issues/7) | 9p mount 한글 긴 파일명 lstat 실패 대응 | `internal/collector/filesystem.go` |

### P1 버그

| # | 제목 |
|---|---|
| [#8](https://github.com/baekenough/second-brain/issues/8) | extraction_failures 추적 테이블 + 재시도 워커 |

### 결정 대기

| # | 제목 |
|---|---|
| [#25](https://github.com/baekenough/second-brain/issues/25) | PDF OCR 번들링 전략 결정 |
| [#26](https://github.com/baekenough/second-brain/issues/26) | 요약용 LLM 모델/API/비용 예산 결정 |

### Chore / 블로커

| # | 제목 |
|---|---|
| [#21](https://github.com/baekenough/second-brain/issues/21) | CI/CD 구축 (.github/workflows/) |
| [#22](https://github.com/baekenough/second-brain/issues/22) | GitHub 수집 연동 활성화 |
| [#23](https://github.com/baekenough/second-brain/issues/23) | Notion 수집 연동 활성화 |
| [#24](https://github.com/baekenough/second-brain/issues/24) | Google Workspace export 활성화 |

---

## 14. 로드맵

GitHub 이슈로 추적. 단계별 epic은 아래 이슈들로 구성됨.

### Phase 0: Baseline (현재 측정)

| # | 제목 |
|---|---|
| [#10](https://github.com/baekenough/second-brain/issues/10) | Baseline 지표 측정 |

### Phase 1: RAG 기초 + 버그 수정

| # | 제목 |
|---|---|
| [#9](https://github.com/baekenough/second-brain/issues/9) | chunks 테이블 마이그레이션 및 청크 단위 임베딩 |
| [#1](https://github.com/baekenough/second-brain/issues/1)~[#8](https://github.com/baekenough/second-brain/issues/8) | P0/P1 버그 수정 (위 섹션 13 참조) |

### Phase 2: 의미 강화

| # | 제목 |
|---|---|
| [#11](https://github.com/baekenough/second-brain/issues/11) | 섹션/헤더 기반 의미 청킹 |
| [#12](https://github.com/baekenough/second-brain/issues/12) | LLM 요약 컬럼 추가 (title_summary, bullet_summary) |
| [#13](https://github.com/baekenough/second-brain/issues/13) | 요약 전용 임베딩 파이프라인 |

### Phase 3: 검색 품질

| # | 제목 |
|---|---|
| [#14](https://github.com/baekenough/second-brain/issues/14) | BGE-reranker cross-encoder 통합 |
| [#15](https://github.com/baekenough/second-brain/issues/15) | HyDE 쿼리 확장 |
| [#16](https://github.com/baekenough/second-brain/issues/16) | 하이브리드 검색 가중치 자동 튜닝 |

### Phase 4: 자기진화 루프

| # | 제목 |
|---|---|
| [#17](https://github.com/baekenough/second-brain/issues/17) | 사용자 피드백 테이블 + 수집 API |
| [#18](https://github.com/baekenough/second-brain/issues/18) | eval set 자동 구축 |
| [#19](https://github.com/baekenough/second-brain/issues/19) | nightly eval + 회귀 감지 파이프라인 |
| [#20](https://github.com/baekenough/second-brain/issues/20) | 임계치 기반 자동 재인덱싱 |

---

*Last updated: 2026-04-14*
