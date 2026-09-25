# second-brain

LLM 큐레이션 프라이빗 검색 엔진. 다양한 소스의 지식을 수집·임베딩하여 AI 에이전트에게 큐레이션된 검색 결과를 제공합니다.

> English: [README.en.md](README.en.md)

---

## 목차

1. [주요 기능](#주요-기능)
2. [아키텍처 개요](#아키텍처-개요)
3. [빠른 시작](#빠른-시작)
4. [프로젝트 구조](#프로젝트-구조)
5. [API 레퍼런스](#api-레퍼런스)
6. [환경 변수](#환경-변수)
7. [수집 소스 상태](#수집-소스-상태)
8. [운영](#운영)
9. [개발](#개발)
10. [알려진 이슈](#알려진-이슈)
11. [관련 문서](#관련-문서)
12. [라이선스](#라이선스)

---

## 주요 기능

- **LLM 큐레이션** — 검색 결과를 LLM이 재랭킹하고 경량 요약을 생성. 원본 데이터 항상 포함
- **한국어 검색** — pg_bigm 2-gram 인덱스 + bigm_similarity 정렬 + HyDE 쿼리 확장으로 조사·어미 무관 검색
- **듀얼 바이너리** — API 서버와 수집 데몬을 독립 실행
- **5-lane 하이브리드 검색** — FTS(tsvector) + pgvector 코사인 + pg_bigm + 요약 임베딩 + 엔티티 RRF로 높은 리콜과 정밀도를 동시에 달성. BGE cross-encoder rerank + HyDE 쿼리 확장 옵션 지원
- **rune 기반 청킹** — 한국어·영어 동일한 정보 밀도로 청크 분할 (issue #145). 헤딩 인식, 단락·문장 경계 분할, 오버랩
- **멀티 소스 수집** — 파일시스템, Slack, GitHub, SMS/통화/녹음(Android), secretary SQLite, Gmail, Calendar, llm-memory 등
- **다형식 문서 추출** — HTML, PDF, DOCX, XLSX, PPTX, HWPX 등 주요 오피스 포맷에서 텍스트 자동 추출
- **엔티티 추출** — LLM 기반 지식 그래프 엔티티(PERSON/ORG/CONCEPT/OTHER) 추출 및 RRF 검색 레인 통합
- **데이터 가드** — 3계층 soft-delete 대량 삭제 방지: filesystem 루트 stat / 50% 삭제 비율 가드 / document.go empty no-op
- **신선도 모니터** — collection_log staleness 모니터 + `GET /api/v1/collect/status` 엔드포인트
- **eval 자기개선 루프** — nightly eval-runner(03:00 KST), NDCG/MRR 회귀 감지, 웹훅 알림, reindex 추천
- **OpenAI 호환 임베딩** — API Key 또는 ChatGPT Codex OAuth JWT(CliProxy) 모두 Bearer 토큰으로 수용
- **SMS stable source_id** — `sms:{ms}:{addrHash}:{direction}` 형식으로 멱등 upsert 보장 (issue #144)
- **소프트 삭제** — 소스에서 제거된 문서를 플래그로 관리하여 이력 보존
- **마이그레이션 advisory lock** — 다중 인스턴스 동시 기동 시 마이그레이션 중복 실행 방지
- **경량 이미지** — Server ~34.5 MB, Collector ~34.5 MB (alpine 멀티스테이지)

---

## 아키텍처 개요

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
    OpenAIWhisper["OpenAI\ngpt-4o-transcribe-diarize\n전사 + 화자분리 (기본)"]
    Whisper["whisper (opt-in)\nlocal-inference profile\n음성인식 :8000"]
    Diarization["diarization (opt-in)\nlocal-inference profile\n화자분리 :8001"]
    Sources["수집 소스\nFilesystem / Slack / GitHub\nSMS·통화·녹음 / Gmail\nCalendar / llm-memory"]

    Agent -->|REST /api/v1| Server
    Agent -->|MCP streamable HTTP| MCP
    Server -->|SQL + pgvector| PG
    Server -->|curation| LLM
    MCP -->|SQL + pgvector| PG
    Collector -->|SQL + pgvector| PG
    Collector -->|POST /embeddings| OpenAI
    Collector -->|수집| Sources
    Collector -->|음성 파일 전사| OpenAIWhisper
    Collector -.->|opt-in 로컬 폴백| Whisper
    Collector -.->|opt-in 로컬 폴백| Diarization
    EvalRunner -->|eval query| PG
    EvalRunner -->|회귀 웹훅| LLM

    class Server,Collector focal;
    class PG data;
    class LLM,OpenAI,OpenAIWhisper,Sources ext;
    class Whisper,Diarization muted;
    class Agent,MCP,EvalRunner muted;
```

![System Runtime Topology](docs/diagrams/01-system-runtime-topology.png)
> 위 PNG는 whisper/diarization을 항상 기동되는 서비스로 그린 구 토폴로지 렌더링입니다. 최신 상태는 PNG 위의 mermaid 소스를 기준으로 합니다 (whisper/diarization은 `local-inference` profile opt-in, 기본 전사 경로는 OpenAI 클라우드).

서버(API)와 수집기(Collector)는 독립된 바이너리로 분리되어 있습니다. 수집기는 소스별 `COLLECT_INTERVAL`로 실행되며, 수집된 텍스트는 rune 기반 chunker로 분할 후 임베딩되어 `pgvector` 컬럼에 저장됩니다. 프로덕션은 **Mac mini docker-compose** (`docker-compose.local.yml`) 기반이며 `deploy/k8s/`는 향후 Kubernetes 전환용입니다.

### 주요 컴포넌트

| 컴포넌트 | 이미지 | 베이스 | uid |
|----------|--------|--------|-----|
| second-brain server (API) | `second-brain-server:local` | golang:alpine → alpine | 10001 |
| second-brain collector (수집 데몬) | `second-brain-collector:local` | golang:alpine → alpine | 10001 |
| second-brain mcp (MCP 서버) | `second-brain-mcp:local` | golang:alpine → alpine | 10001 |
| second-brain eval (nightly eval) | `second-brain-eval:local` | golang:alpine → alpine | — |
| postgres | `second-brain-postgres:local` | pgvector/pgvector:pg16 + pg_bigm | — |
| ollama | `ollama/ollama:latest` | — | — |
| whisper (`local-inference` profile, opt-in) | `fedirz/faster-whisper-server:latest-cpu` | faster-whisper (int8 CPU) | — |
| diarization (`local-inference` profile, opt-in) | `second-brain-diarization:local` | pyannote.audio | — |
| web | `second-brain-web:local` | node:alpine (Next.js standalone) | 10001 |

---

## 빠른 시작

```bash
# 1. 설정 마법사로 .env 생성 (권장)
go run -tags setup ./cmd/collector/ setup

# 또는 수동 설정
cp .env.local.example .env.local
# .env.local 파일을 편집하여 필수 값 설정

# 2. 서비스 기동 (로컬 docker-compose)
docker compose -f docker-compose.local.yml up -d --build

# 3. 헬스 체크
curl http://localhost:8081/health

# 4. 검색 테스트
curl -X POST http://localhost:8081/api/v1/search \
  -H "Content-Type: application/json" \
  -d '{"query": "온보딩 가이드", "limit": 5}'

# 5. 수집 신선도 확인
curl http://localhost:8081/api/v1/collect/status
```

> **참고**: 마법사는 `-tags setup` 빌드 태그로 빌드된 바이너리에서만 동작합니다.

---

## 프로젝트 구조

```
second-brain/
├── cmd/
│   ├── server/
│   │   └── main.go              # API 서버 엔트리포인트 (포트 8080)
│   ├── collector/
│   │   └── main.go              # 수집 데몬 엔트리포인트
│   ├── mcp/
│   │   └── main.go              # MCP streamable HTTP 서버 (포트 8090)
│   └── eval/
│       └── main.go              # nightly eval 바이너리
├── internal/
│   ├── api/                     # HTTP 핸들러 + 라우터
│   │   ├── collect_status.go    # GET /api/v1/collect/status + FreshnessChecker
│   │   ├── reindex_check.go     # GET /api/v1/reindex/check
│   │   └── search.go            # POST|GET /api/v1/search (HyDE, rerank 옵션)
│   ├── chunker/                 # rune 기반 텍스트 청킹 (#145)
│   │   ├── chunker.go           # Options{TargetSize, MaxSize, Overlap, HeadingAware}
│   │   └── adaptive.go          # SourceType별 Options 자동 선택
│   ├── collector/
│   │   ├── extractor/           # 파일 포맷 추출기
│   │   ├── filesystem.go        # 로컬 파일시스템 수집기
│   │   ├── slack.go             # Slack 수집기 (public_channel only)
│   │   ├── github.go            # GitHub 수집기
│   │   ├── sms.go               # SMS·통화 수집기 (Android push)
│   │   ├── recording.go         # 통화 녹음 수집기 (Whisper ASR)
│   │   ├── secretary.go         # secretary SQLite 수집기
│   │   ├── gmail.go             # Gmail 수집기
│   │   ├── calendar.go          # Calendar 수집기
│   │   └── llm_memory.go        # llm-memory SQLite 수집기
│   ├── config/
│   │   └── config.go            # 환경 변수 파싱
│   ├── curation/                # LLM 큐레이션 레이어
│   ├── llm/                     # LLM client (HyDE, 엔티티 추출)
│   ├── search/
│   │   ├── search.go            # Service.Search() — HyDE, rerank, entity 통합
│   │   ├── embed.go             # EmbedClient (staticToken / cliProxyToken)
│   │   ├── rerank.go            # HTTPReranker (BGE cross-encoder)
│   │   ├── hyde.go              # HyDE 쿼리 확장
│   │   └── tune.go              # 하이브리드 검색 가중치 튜닝
│   ├── store/
│   │   ├── document.go          # DocumentStore — 5-lane hybridSearch CTE
│   │   ├── collection_status.go # CollectionStatus + FreshnessChecker
│   │   ├── entities.go          # EntityStore — upsert, link, fetch
│   │   └── postgres.go          # pgx/v5 연결 + advisory lock 마이그레이션
│   ├── model/                   # Document, SearchQuery, Entity 구조체
│   ├── scheduler/               # 주기적 수집 스케줄러 + 삭제 가드
│   └── worker/                  # 비동기 임베딩 백필 워커
├── migrations/                  # SQL 마이그레이션 001~019 (서버 기동 시 자동 적용)
├── deploy/
│   ├── k8s/                     # Kustomize 매니페스트 (향후 k8s 전환용)
│   ├── postgres/                # pg_bigm 포함 커스텀 PostgreSQL 이미지
│   └── diarization/             # pyannote.audio diarization 서버
├── web/                         # Next.js 프론트엔드
├── mobile/second-brain-push/    # Android 수집 앱 (SMS/통화/녹음)
├── docker-compose.local.yml     # 프로덕션 Compose (Mac mini)
├── Dockerfile                   # 멀티 타겟 빌드 (server/collector/mcp/eval)
└── go.mod                       # Go 모듈 정의
```

---

## API 레퍼런스

모든 엔드포인트는 `/api/v1` 접두사를 사용합니다. `/health`만 예외입니다.

### 엔드포인트 목록

| 메서드 | 경로 | 설명 |
|--------|------|------|
| `GET` | `/health` | 헬스 체크 |
| `GET` | `/api/v1/search` | 하이브리드 검색 (GET, 쿼리 파라미터) |
| `POST` | `/api/v1/search` | 하이브리드 검색 (POST, JSON 바디) |
| `GET` | `/api/v1/documents` | 문서 목록 페이지네이션 |
| `GET` | `/api/v1/documents/{id}` | 단일 문서 상세 조회 |
| `GET` | `/api/v1/documents/{id}/raw` | 원본 파일 스트리밍 (filesystem 전용, 50 MiB 제한) |
| `GET` | `/api/v1/sources` | 등록된 수집기 목록 |
| `GET` | `/api/v1/stats/baseline` | 문서 수·청크·p50/p95 통계 |
| `GET` | `/api/v1/collect/status` | 소스별 수집 신선도 (last_success_at, stale_seconds) |
| `POST` | `/api/v1/collect/trigger` | 수동 수집 트리거 |
| `POST` | `/api/v1/ingest/messages` | SMS·통화 기록 JSON 배치 수신 (Android 앱) |
| `POST` | `/api/v1/ingest/recording` | 통화 녹음 `.m4a` multipart 수신 (Android 앱) |

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

JSON 바디 기반 5-lane 하이브리드 검색. FTS(tsvector BM25) + pgvector 코사인 + pg_bigm + 요약 임베딩 + 엔티티를 RRF로 결합합니다.

![Hybrid Search RRF](docs/diagrams/05-hybrid-search-rrf.png)

**요청 바디**

| 필드 | 타입 | 기본값 | 설명 |
|------|------|--------|------|
| `query` | string | 필수 | 검색 쿼리 |
| `source_type` | string | (전체) | `filesystem` \| `slack` \| `github` \| `sms` 소스 필터 |
| `exclude_source_types` | []string | — | 제외할 소스 목록 |
| `limit` | int | 20 | 반환 결과 수 |
| `sort` | string | `"relevance"` | `"relevance"` (RRF score) \| `"recent"` (collected_at DESC) |
| `include_deleted` | bool | `false` | 소프트 삭제된 문서 포함 여부 |
| `curated` | bool | `false` | LLM 큐레이션 활성화 (재랭킹 + 요약) |
| `use_hyde` | bool | `false` | HyDE 쿼리 확장 활성화 (~1-3s 지연 추가) |
| `use_rerank` | bool | `false` | BGE cross-encoder rerank 활성화 |

```bash
curl -X POST http://localhost:8081/api/v1/search \
  -H "Content-Type: application/json" \
  -d '{"query": "온보딩 가이드", "limit": 5, "sort": "relevance", "use_rerank": true}'
```

```json
{
  "results": [
    {
      "id": "a1b2c3d4-e5f6-7890-abcd-ef1234567890",
      "title": "신규 입사자 온보딩 가이드.docx",
      "content": "입사 첫 주에는 ...",
      "source_type": "filesystem",
      "source_id": "HR/신규 입사자 온보딩 가이드.docx",
      "match_type": "hybrid",
      "score": 0.0312,
      "collected_at": "2026-04-10T09:00:00Z"
    }
  ],
  "count": 1,
  "total": 1,
  "query": "온보딩 가이드",
  "took_ms": 42
}
```

---

### GET /api/v1/collect/status

소스별 수집 신선도를 반환합니다. collection_log 테이블 기반.

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

| 쿼리 파라미터 | 타입 | 기본값 | 설명 |
|---------------|------|--------|------|
| `limit` | int | 20 | 최대 100 |
| `offset` | int | 0 | 시작 오프셋 |
| `source` | string | (전체) | `filesystem` \| `slack` \| `github` 등 |

---

### POST /api/v1/ingest/messages

Android second-brain-push 앱이 SMS·통화 기록을 JSON 배치로 전송합니다. source_id 형식: `sms:{dateMs}:{addrHash}:{direction}`.

**응답 계약(#290).** 앱은 2xx 를 받으면 `errors[]` 를 보지 않고 커서를 배치 끝까지 전진하고, 5xx 를 받으면 커서를 멈추고 같은 배치를 다시 보냅니다. 401/403 이 아닌 4xx 도 다시 보냅니다. 그래서 상태 코드가 곧 "이 배치를 다시 받아야 하는가" 입니다.

| 상태 | 뜻 | 앱 동작 |
|------|----|---------|
| `201` `{accepted, skipped, sanitized, errors[]}` | 배치를 끝까지 처리했습니다. `errors[]` 는 다시 보내도 결과가 같은 **영구 오류**로 건너뛴 레코드입니다(필수 필드 누락, 필드 상한 초과, `date_ms` 가 2000-01-01~서버 시각+7일 밖, 원소 타입 오류, DB SQLSTATE 22·54 클래스와 23502·23514 — 23505 같은 나머지 23 은 동시 쓰기 경합이라 503). 문구는 `sms[3]: body exceeds 65536 bytes` 처럼 배열 이름·인덱스·고정 사유만 담고 입력 값은 담지 않습니다. `skipped` 는 컷오버 이전 레코드와 통화 중복 전사, `sanitized` 는 NUL(U+0000)을 지우고 **저장에 성공한** 레코드 수입니다(거부된 레코드는 errors[] 에만 잡힘). 건너뛴 사유는 서버 로그에 사유 코드별 개수(`error_reasons`·`skip_reasons`, 예: `missing_date_ms`, `db_sqlstate_23514`, `before_cutover`)로 남고, 저장된 레코드 없이 모두 거부되면 앱·서버 필드 계약이 어긋났을 가능성이 커 ERROR 로 남깁니다 | 커서 전진 |
| `503` + `Retry-After: 30` | **일시 오류**입니다. DB 연결 끊김·풀 고갈·요청 예산(45초) 초과·클라이언트 이탈·분류되지 않은 오류, 잘린 업로드, 또는 다른 ingest 요청이 처리 중(동시 1건 게이트, 최대 2초 대기)일 때입니다. 첫 일시 오류에서 멈추고 나머지 레코드는 처리하지 않습니다. 앞서 저장된 레코드는 재전송 때 "변경 없음" 으로 빠르게 지나갑니다 | 같은 배치 재전송 |
| `413` | 본문이 `INGEST_MESSAGES_MAX_BODY_BYTES`(기본 32 MiB)를 넘거나 레코드 수가 `INGEST_MAX_BATCH_MESSAGES`(기본 5000)를 넘었습니다. 정상 앱(300건/배치)은 닿지 않는 크기이며 서버가 `slog.Error` 로 남깁니다 | 재전송(예외로 둔 경우) |
| `401` | API 키가 없거나 틀렸습니다 | 재시도 중단 |
| `400` | JSON 문법 오류·최상위 구조 오류·빈 본문(앱 버그). 레코드 하나의 문제(잘못된 UTF-8·NUL·필드 과대·원소 타입 오류)로는 400 을 주지 않습니다 — 잘못된 UTF-8 은 U+FFFD 로 바뀌어 저장됩니다 | 재전송(예외로 둔 경우) |

레코드 필드 상한: SMS `body` 64 KiB, `address`·`number`·`contact_name` 1 KiB. 넘는 레코드만 건너뜁니다. 문서 upsert 와 청크 교체는 한 트랜잭션이라, 청크 단계에서 실패하면 문서 행도 되돌아가고 재전송 때 다시 만들어집니다. 임베딩은 요청 중에 만들지 않습니다 — 새 청크는 collector 의 임베딩 백필이 다음 수집 주기(`COLLECT_INTERVAL`, 기본 10분)에 채우므로 벡터 검색 반영은 그만큼 늦고, 전문·bigm 검색은 바로 반영됩니다. 로그에는 본문·주소·번호를 남기지 않고 개수·인덱스·SQLSTATE 만 남깁니다.

---

## 환경 변수

`internal/config/config.go` 기반 주요 환경 변수입니다.

### Server

| 키 | 기본값 | 설명 |
|----|--------|------|
| `DATABASE_URL` | `postgres://brain:brain@localhost:5432/second_brain?sslmode=disable` | PostgreSQL 연결 문자열 |
| `PORT` | `8080` | HTTP 서버 포트 |
| `EMBEDDING_API_URL` | `https://api.openai.com/v1` | OpenAI 호환 임베딩 엔드포인트 |
| `EMBEDDING_MODEL` | `text-embedding-3-small` | 임베딩 모델 (1536 차원) |
| `EMBEDDING_API_KEY` | — | Static Bearer 토큰. `CLIPROXY_AUTH_FILE`과 택일 |
| `CLIPROXY_AUTH_FILE` | — | CliProxy OAuth JSON 파일 경로. 5분 TTL 자동 갱신 |
| `LLM_API_URL` | — | LLM 큐레이션·HyDE·엔티티 추출용 chat completions 엔드포인트 |
| `LLM_API_KEY` | — | LLM API 키 |
| `LLM_MODEL` | — | LLM 모델 식별자 |
| `API_KEY` | — | API 인증 Bearer 토큰 |
| `ALERT_WEBHOOK_URL` | — | 수집 신선도 알림 Slack 웹훅 URL |
| `RERANK_API_URL` | — | BGE cross-encoder rerank 엔드포인트 |
| `ENTITY_EXTRACTION_ENABLED` | `false` | 엔티티 추출 + 검색 레인 활성화 |
| `SEARCH_REQUEST_TIMEOUT_SECONDS` | `60` | 검색 요청 하나(임베딩·DB·리랭크)의 제한 시간(초). REST `/api/v1/search`·GraphQL `search`·MCP `search` 에 적용, 초과 시 REST 는 504. DB 전역 `statement_timeout` 이 아니라 요청 context 타임아웃이다(ctx 가 끝나면 pgx 가 서버 측 문장도 취소). `/api/v1/ask` 는 `ASK_TIMEOUT_SECONDS` 가 따로 묶는다. 0·음수·잘못된 값은 기본값 |
| `SEARCH_MAX_CONCURRENCY` | 자동 | 서버 프로세스의 동시 검색 수 상한(#286). 비우면 DB 풀 크기의 절반 `max(1, MaxConns/2)`, 양수면 그 값을 쓰되 `MaxConns-1` 로 잘린다(ingest 같은 비검색 경로에 연결을 남기려는 것). REST `/api/v1/search`·`/api/v1/golden/next` 는 빈 슬롯을 최대 1초 기다린 뒤 503 `search capacity exceeded` + `Retry-After: 2`, GraphQL `search` 는 같은 문구의 필드 오류, `/api/v1/ask` 는 503 없이 기다린다(아래). MCP·collector 는 별도 프로세스·별도 풀이라 적용하지 않는다. 시작 로그 `search: concurrency limit` 에 실제 `max_conns`·`max_concurrency` 가 남는다. 0·음수·잘못된 값은 자동값(제한을 끄는 설정은 없다). 예외: 풀 최대 연결이 1 이하(`pool_max_conns=1`)면 K 도 1 이라 검색 1건이 연결을 전부 쓰므로 ingest 보호가 성립하지 않는다(시작 시 경고) — 그때는 `pool_max_conns` 를 올린다. `/api/v1/ask` 는 검색 호출마다 최대 10초 기다리고 REST 대기자에게 양보하며, 넘으면 `retrieval failed` 로 끝난다 |
| `INGEST_MESSAGES_MAX_BODY_BYTES` | `33554432` (32 MiB) | `POST /api/v1/ingest/messages` 요청 본문 상한(바이트, #288). 넘으면 413. 0·음수·잘못된 값은 기본값(상한을 끄는 설정은 없다). 4 MiB 미만이거나 `INGEST_MAX_BATCH_MESSAGES` 가 폰 앱 배치 크기(300) 미만이면 시작 시 경고한다 — 앱은 413 을 끝없이 재시도하므로 동기화가 멈춘다 |


검색 입력 검증(#282): 검색 질의(`q`, `query`)와 소스 필터·정렬 값은 DB 에 가기 전에 검사한다. 잘못된 UTF-8·NUL(`\x00`) 포함·질의 1024바이트 초과는 400 이고, `POST /api/v1/search` 본문은 16KB(초과 시 413), `/api/v1/ask` 본문은 32KB, `/api/v1/graphql` 본문·쿼리스트링은 각각 64KB(초과 시 413/414)로 제한한다. GraphQL 은 요청당 `search` 필드 5개(별칭·fragment 전개 포함), `curated:true` 검색 1개까지만 실행하고(초과 시 400), 순환 fragment 는 400 으로 거부하며, 요청 전체에 `SEARCH_REQUEST_TIMEOUT_SECONDS` 를 한 번 건다. MCP HTTP 본문은 31MB(add_note 10MB 의 비 ASCII 이스케이프 최악 3배 + 1MB)로 제한한다. `/api/v1/ask` 질문은 같은 검증을 거치되 길이 상한만 4KB 다. 오류 응답과 로그에는 질의 원문을 남기지 않는다.

피드백 입력 검증(#286): 피드백을 저장하는 경로(`POST /api/v1/feedback`, `POST /api/v1/feedback/evidence`, GraphQL `createFeedback`, `POST /api/v1/golden/judgments`, `POST /api/v1/golden/feedback`)도 DB 에 가기 전에 같은 규칙(잘못된 UTF-8·NUL 거부)으로 검사한다. 본문은 feedback·evidence 32KB, golden 64KB(초과 시 413)이고, 잘못된 UTF-8 원 바이트가 든 본문은 400 이다(이전에는 네 REST 경로 모두 U+FFFD 로 바꿔 통과시켰다). 필드 상한은 `query`·`query_text` 4KB(`/ask` 질문 상한과 같은 상수라 답을 받은 질문에는 항상 투표할 수 있다), `comment` 4KB, `source` 64바이트, `session_id`·`user_id`·`conversation_id` 128바이트, `metadata` 직렬화 4KB·중첩 깊이 16(키와 문자열 값 모두 NUL·UTF-8 검사)이며, 판정 배열은 요청당 100개까지이고 판정의 `rank` 는 0~10000 이다. `document_id` 는 UUID 여야 하며 `urn:uuid:`·중괄호·하이픈 없는 32자·대문자 형식은 정규형(소문자 36자)으로 바꿔 저장한다(PostgreSQL 은 urn 형식을 받지 않는다). 존재하지 않는 `document_id`·`chunk_id`·`query_id` 는 500 이 아니라 400 이다(golden 판정은 한 트랜잭션이라 요청 전체가 롤백된다). evidence 의 `layer` 는 `""`·`note`·`observed`·`insight` 만 받는다. GraphQL 은 요청당 `createFeedback` 1개(별칭·fragment 전개 포함)까지만 실행하고, POST 가 아닌 요청(GET 링크 등)이 mutation 을 실행하려 하면 405(`Allow: POST`)로 거부한다 — query 연산의 GET 은 그대로 된다. OpenSearch 레인이 실패하면 오류·로그에는 상태 코드와 `error.type` 만 남기고 응답 본문(질의 조각이 든 `reason`)은 버린다.

### Collector

| 키 | 기본값 | 설명 |
|----|--------|------|
| `DATABASE_URL` | (위와 동일) | PostgreSQL 연결 문자열 |
| `COLLECT_INTERVAL` | `1h` | 기본 수집 주기 (소스별 오버라이드 가능) |
| `MAX_EMBED_CHARS` | `8000` | 임베딩 입력 최대 문자 수 |
| `EMBEDDING_API_URL` | `https://api.openai.com/v1` | OpenAI 호환 임베딩 엔드포인트 |
| `EMBEDDING_API_KEY` | — | Static Bearer 토큰 |
| `FILESYSTEM_PATH` | — | 수집할 로컬 경로 |
| `FILESYSTEM_ENABLED` | `false` | 파일시스템 수집기 활성화 |
| `SLACK_BOT_TOKEN` | — | Slack Bot OAuth Token (`xoxb-...`) |
| `SLACK_TEAM_ID` | — | Slack Workspace ID |
| `GITHUB_TOKEN` | — | GitHub Personal Access Token |
| `GITHUB_ORG` | — | GitHub 조직명 |
| `GDRIVE_CREDENTIALS_JSON` | — | Google ADC JSON 경로 |
| `DIARIZATION_API_URL` | — | (레거시 로컬 전용) pyannote.audio 화자 분리 서버 URL — 기본 클라우드 전사 경로는 사용하지 않음 |
| `DIARIZATION_ENABLED` | `false` | (레거시 로컬 전용) 로컬 pyannote 화자 분리 셀렉터 — 클라우드 화자 분리는 항상 내장되어 무관 |
| `WHISPER_API_URL` | `https://api.openai.com/v1` | 전사 API 엔드포인트 (기본: OpenAI 클라우드) |
| `WHISPER_API_KEY` | — | OpenAI API 키. 필수 — 비어있으면 전사 비활성화 |
| `WHISPER_MODEL` | `gpt-4o-transcribe-diarize` | 전사 + 화자 분리 통합 모델 |
| `WHISPER_CHUNKING_STRATEGY` | `auto` | 화자 분리 세그먼트를 받기 위해 필수 |
| `ENTITY_EXTRACTION_ENABLED` | `false` | 엔티티 추출 활성화 |

> `EMBEDDING_API_KEY`와 `CLIPROXY_AUTH_FILE`이 모두 설정된 경우 `CLIPROXY_AUTH_FILE`이 우선합니다.

---

## 수집 소스 상태

| 소스 | 활성 조건 | 구현 상태 | 비고 |
|------|-----------|-----------|------|
| filesystem | `FILESYSTEM_PATH` + `FILESYSTEM_ENABLED=true` | 완전 동작 | 4,150+ 문서 수집 검증 |
| slack | `SLACK_BOT_TOKEN` 설정 | 구현 완료 | public_channel만, DM 자동 제외 |
| github | `GITHUB_TOKEN` + `GITHUB_ORG` 설정 | 구현 완료 | Issues + PR 수집 |
| sms / call | Android 앱 설치 + 서버 URL/토큰 설정 | 완전 동작 | Galaxy Z Flip6 실기 검증. 앱: [`mobile/second-brain-push/`](mobile/second-brain-push/README.md) |
| recording (통화 녹음) | 위와 동일 + `WHISPER_API_KEY` | 완전 동작 | OpenAI gpt-4o-transcribe-diarize (클라우드) → 텍스트 변환 + 화자 분리 후 저장 |
| secretary | `SECRETARY_DB_HOST_PATH` 마운트 | 완전 동작 | secretary SQLite ro 마운트 |
| gmail | `/data/gmail` 마운트 | 완전 동작 | secretary 연동 Gmail 익스포트 |
| calendar | `/data/calendar` 마운트 | 완전 동작 | secretary 연동 Calendar 익스포트 |
| llm-memory | `LLM_MEMORY_DB_HOST_PATH` 마운트 | 완전 동작 | llm-memory SQLite ro 마운트 |
| gdrive (export) | `GDRIVE_CREDENTIALS_JSON` 설정 | 스캐폴드 | ADC 필요, 기본 disabled |
| notion | — | 제거됨 | main.go 등록 해제 |

### 파일 추출기 (`internal/collector/extractor/`)

| 포맷 | 라이브러리 | 특이 사항 |
|------|-----------|-----------|
| HTML | `golang.org/x/net/html` | 태그 strip 후 텍스트 추출 |
| PDF | `ledongthuc/pdf` | 10초 타임아웃, NUL 바이트 sanitize |
| DOCX | OOXML unzip | `word/document.xml` 파싱 |
| XLSX | `github.com/xuri/excelize/v2` | TSV 출력, `##SHEET {name}` 헤더 + 탭 구분 row, 200 KiB cap |
| PPTX | OOXML unzip | `ppt/slides/*.xml` 파싱 |
| HWPX | OWPML unzip | `Contents/section*.xml` 의 `<hp:t>` 파싱 |
| 공통 | `SanitizeText` | 0x00 제거 + UTF-8 validation + 공백 압축 |

---

## 운영

### 서비스 상태 확인

```bash
docker compose -f docker-compose.local.yml ps
docker compose -f docker-compose.local.yml logs server --tail=100 -f
docker compose -f docker-compose.local.yml logs collector --tail=100 -f
```

### 수집 신선도 확인

```bash
curl http://localhost:8081/api/v1/collect/status | jq '.sources[] | {source_type, consecutive_failures, stale_seconds}'
```

### 데이터베이스 직접 접속

```bash
docker compose -f docker-compose.local.yml exec postgres psql -U brain -d second_brain
```

```sql
-- 소스별 문서 수
SELECT source_type, COUNT(*) FROM documents GROUP BY source_type;

-- 최근 수집된 문서 10건
SELECT title, source_type, collected_at FROM documents ORDER BY collected_at DESC LIMIT 10;

-- 임베딩 누락 문서 수
SELECT COUNT(*) FROM documents WHERE embedding IS NULL AND status = 'active';

-- 수집 로그 최근 20건
SELECT source_type, started_at, documents_count, error FROM collection_log ORDER BY started_at DESC LIMIT 20;
```

### 이미지 재빌드

```bash
docker compose -f docker-compose.local.yml build --no-cache
docker compose -f docker-compose.local.yml up -d
```

### Ollama 모델 초기 다운로드

```bash
docker exec second-brain-ollama ollama pull gemma3:12b-it-qat
```

---

## Contributor Setup

리포에 push 권한이 있다면 한 번만 실행:

```bash
make install-hooks
```

이 명령은 git이 `.githooks/`의 pre-push hook을 사용하도록 설정합니다. push 시 자동으로:

- `scripts/ci-checks.sh` (secret 가드, discord placeholder, .env 추적 검사 등)
- `go vet` / `go build` / `go test`

가 실행됩니다. CI와 동일한 hygiene 검사만 로컬에서 돌리려면:

```bash
make check
```

---

## 개발

### 사전 요구사항

- Go 1.25+
- PostgreSQL 16 + pgvector + pg_bigm 확장
- Docker / docker compose

### 백엔드 로컬 실행

```bash
export DATABASE_URL="postgres://brain:brain@localhost:5432/second_brain?sslmode=disable"
export EMBEDDING_API_KEY="sk-..."

# API 서버 기동 (마이그레이션 자동 적용)
go run ./cmd/server/

# 수집 데몬 기동 (별도 터미널)
export FILESYSTEM_PATH="/path/to/docs"
export FILESYSTEM_ENABLED=true
go run ./cmd/collector/
```

### 백엔드 테스트 및 린트

```bash
go test ./...
go test -race ./...
go vet ./...
gofmt -w .
```

> `go build ./cmd/eval` 는 레포 루트에 추적 중인 `eval/`(평가 픽스처) 디렉터리와 이름이 충돌해 로컬에서 실패합니다.
> `make build-eval` (또는 `make build-all`)을 사용하면 `bin/`(gitignore 대상) 아래로 빌드되어 충돌을 피할 수 있습니다.

### 마이그레이션

마이그레이션 파일은 `migrations/` 디렉터리(001~019)에 위치하며, 서버 기동 시 advisory lock 하에 순서대로 자동 적용됩니다.

---

## 알려진 이슈

| 증상 | 원인 | 우회 방법 |
|------|------|-----------|
| minikube mount에서 일부 파일 수집 skip | 9p 마운트의 한글 긴 파일명(255바이트 초과) `lstat: file name too long` | 파일명 255바이트 이하로 단축 |
| Slack/GitHub 수집기 ERROR 후 skip | 자격증명 환경 변수 미설정 | 해당 `*_TOKEN` / `*_ORG` 환경 변수 설정 |
| gdrive 수집기 미동작 | `GDRIVE_CREDENTIALS_JSON` 미설정 시 기본 disabled | ADC 자격증명 설정 후 활성화 |
| Ollama 첫 기동 느림 | gemma3:12b-it-qat 모델 macOS arm64 CPU 추론 | 최초 1회 모델 pull 후 재사용 |

---

## 인터랙티브 다이어그램

각 다이어그램은 ARCHITECTURE.md의 해당 섹션과 동일하며, [eraser.io](https://app.eraser.io)에서 인터랙티브 편집이 가능합니다.

### 1. 시스템 토폴로지
([eraser.io에서 열기](https://app.eraser.io/workspace/PyHgjPmM97MYtJNoVD5H?diagram=CSpF8N1BRvdpM3l5TW2nd))

### 2. 데이터 모델 ERD
([eraser.io에서 열기](https://app.eraser.io/workspace/O901Iet3HpcIaldLfQ1e?diagram=fo1U7OVZh_I9mVe6OaLfV))

### 3. 수집 파이프라인 시퀀스
([eraser.io에서 열기](https://app.eraser.io/workspace/2NzpSSDbSx4Oh3gCs6DV?diagram=UT0aD63tuIkrRiqLEUgKr))

### 4. 하이브리드 검색 RRF
([eraser.io에서 열기](https://app.eraser.io/workspace/K10FmwBysYGTBVp8E9Wb?diagram=xJgZnmmbfRJL2Ox_bMd_i))

---

## 관련 문서

- [`ARCHITECTURE.md`](ARCHITECTURE.md) — 아키텍처 상세 설명
- [`docs/runbook-deploy.md`](docs/runbook-deploy.md) — 배포 런북
- [`guides/`](guides/) — 운영 및 개발 가이드 모음

---

## 라이선스

이 프로젝트는 [MIT License](LICENSE) 하에 배포됩니다. 기여 방법은 [CONTRIBUTE.md](CONTRIBUTE.md)를 참고하세요.

---

Last updated: 2026-06-13 (v0.20.4)
