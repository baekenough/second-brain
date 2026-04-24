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
- **한국어 검색** — pg_bigm 2-gram 인덱스 + HyDE 쿼리 확장으로 조사/어미 무관 검색
- **듀얼 바이너리** — API 서버와 수집 데몬을 독립 실행
- **하이브리드 검색** — BM25 전문(Full-text) 검색(`ts_rank_cd`)과 pgvector 코사인 유사도 검색을 RRF(Reciprocal Rank Fusion)로 결합하여 높은 리콜과 정밀도를 동시에 달성
- **멀티 소스 수집** — 파일시스템, Slack, GitHub, Google Drive(Export) 수집기를 통한 자동 문서 수집 및 주기적 갱신
- **다형식 문서 추출** — HTML, PDF, DOCX, XLSX, PPTX 등 주요 오피스 포맷에서 텍스트 자동 추출
- **OpenAI 호환 임베딩** — API Key 또는 ChatGPT Codex OAuth JWT(CliProxy) 모두 Bearer 토큰으로 수용
- **소프트 삭제** — 소스에서 제거된 문서를 즉시 삭제하지 않고 플래그로 관리하여 이력 보존
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
    Collector -->|수집| FS
    Collector -->|수집| Slack
    Collector -->|수집| GH
    Collector -->|수집| GD

    class Server,Collector focal;
    class PG data;
    class LLM,OpenAI,Slack,GH,GD,FS ext;
    class Agent muted;
```

서버(API)와 수집기(Collector)는 독립된 바이너리로 분리되어 있습니다. 수집기는 스케줄러(기본 1시간 주기)가 실행하며, 수집된 텍스트는 최대 8,000자로 절단 후 임베딩되어 `pgvector` 컬럼에 저장됩니다.

### 주요 컴포넌트

| 컴포넌트 | 이미지 | 베이스 | 크기 | uid |
|----------|--------|--------|------|-----|
| second-brain server (API) | `second-brain:dev` | golang:1.24-alpine → alpine:3.21 | ~34.5 MB | 10001 |
| second-brain collector (수집 데몬) | `second-brain-collector:dev` | golang:1.24-alpine → alpine:3.21 | ~34.5 MB | 10001 |
| postgres | `pgvector/pgvector:pg16` | PostgreSQL 16 + pgvector + pg_bigm | — | — |

---

## 빠른 시작

```bash
# 1. 환경 변수 설정
cp .env.example .env
# .env 파일을 편집하여 필수 값 설정 (DATABASE_URL, EMBEDDING_API_KEY 등)

# 2. 서비스 기동
docker compose up -d

# 3. 헬스 체크
curl http://localhost:8080/health

# 4. 검색 테스트
curl -X POST http://localhost:8080/api/v1/search \
  -H "Content-Type: application/json" \
  -d '{"query": "온보딩 가이드", "limit": 5}'
```

---

## 프로젝트 구조

```
second-brain/
├── cmd/
│   ├── server/
│   │   └── main.go              # API 서버 엔트리포인트 (포트 8080)
│   └── collector/
│       └── main.go              # 수집 데몬 엔트리포인트
├── internal/
│   ├── collector/
│   │   ├── extractor/           # 파일 포맷 추출기
│   │   │   ├── extractor.go     # 인터페이스 + SanitizeText
│   │   │   ├── html.go          # x/net/html 태그 strip
│   │   │   ├── pdf.go           # ledongthuc/pdf, 10초 타임아웃
│   │   │   ├── docx.go          # OOXML word/document.xml
│   │   │   ├── xlsx.go          # excelize/v2 TSV, 200 KiB cap
│   │   │   └── pptx.go          # OOXML ppt/slides
│   │   ├── filesystem.go        # 로컬 파일시스템 수집기
│   │   ├── slack.go             # Slack 수집기 (public_channel only)
│   │   ├── github.go            # GitHub 수집기
│   │   └── gdrive_export.go     # Google Drive Export 수집기
│   ├── config/
│   │   └── config.go            # 환경 변수 파싱
│   ├── curation/                # LLM 큐레이션 레이어
│   ├── db/                      # pgvector 초기화, 마이그레이션
│   ├── embedding/               # OpenAI 호환 임베딩 클라이언트
│   ├── handler/                 # HTTP 핸들러 (search, documents, sources)
│   ├── model/                   # Document, SearchResult 구조체
│   └── scheduler/               # 주기적 수집 스케줄러 (mutex 중복 방지)
├── migrations/                  # SQL 마이그레이션 (서버 기동 시 자동 적용)
├── deploy/
│   └── k8s/                     # Kustomize 매니페스트
├── Dockerfile                   # 멀티 타겟 빌드 (server + collector)
├── docker-compose.yml           # 로컬 개발용 Compose
└── go.mod                       # Go 1.25 모듈 정의
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

---

### GET /health

서버가 기동 중이면 200을 반환합니다.

```bash
curl http://localhost:8080/health
```

```json
{"status":"ok"}
```

---

### GET /api/v1/search

쿼리 파라미터 기반 하이브리드 검색입니다.

| 쿼리 파라미터 | 타입 | 기본값 | 설명 |
|---------------|------|--------|------|
| `q` | string | 필수 | 검색 쿼리 |
| `source_type` | string | (전체) | `filesystem` \| `slack` \| `github` 소스 필터 |
| `limit` | int | 10 | 반환 결과 수 |
| `curated` | bool | `false` | LLM 큐레이션 활성화 (재랭킹 + 요약) |

```bash
curl "http://localhost:8080/api/v1/search?q=온보딩+가이드&limit=5&curated=true"
```

---

### POST /api/v1/search

JSON 바디 기반 하이브리드 검색입니다. 전문(BM25 `ts_rank_cd`) + 벡터(pgvector `<=>` 코사인)를 RRF로 결합합니다.

**요청 바디**

| 필드 | 타입 | 기본값 | 설명 |
|------|------|--------|------|
| `query` | string | 필수 | 검색 쿼리 |
| `source_type` | string | (전체) | `filesystem` \| `slack` \| `github` 소스 필터 |
| `limit` | int | 10 | 반환 결과 수 |
| `sort` | string | `"relevance"` | `"relevance"` (RRF score 내림차순) \| `"recent"` (collected_at 내림차순) |
| `include_deleted` | bool | `false` | 소프트 삭제된 문서 포함 여부 |
| `curated` | bool | `false` | LLM 큐레이션 활성화 (재랭킹 + 요약) |

```bash
curl -X POST http://localhost:8080/api/v1/search \
  -H "Content-Type: application/json" \
  -d '{"query": "온보딩 가이드", "limit": 5, "sort": "relevance", "curated": true}'
```

```json
{
  "results": [
    {
      "id": "a1b2c3d4-e5f6-7890-abcd-ef1234567890",
      "title": "신규 입사자 온보딩 가이드.docx",
      "content": "입사 첫 주에는 ...",
      "source": "filesystem",
      "source_url": "/data/drive/HR/신규 입사자 온보딩 가이드.docx",
      "collected_at": "2026-04-10T09:00:00Z",
      "score": 0.0312
    }
  ],
  "count": 1,
  "total": 1,
  "query": "온보딩 가이드",
  "took_ms": 42
}
```

---

### GET /api/v1/documents

| 쿼리 파라미터 | 타입 | 기본값 | 설명 |
|---------------|------|--------|------|
| `limit` | int | 20 | 최대 100 |
| `offset` | int | 0 | 시작 오프셋 |
| `source` | string | (전체) | `filesystem` \| `slack` \| `github` |

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

단일 문서의 메타데이터와 전체 콘텐츠를 JSON으로 반환합니다.

```bash
curl http://localhost:8080/api/v1/documents/a1b2c3d4-e5f6-7890-abcd-ef1234567890
```

---

### GET /api/v1/documents/{id}/raw

원본 파일 바이트를 스트리밍합니다. filesystem 소스 전용이며 Content-Type은 확장자 기반으로 자동 설정됩니다. 50 MiB 초과 파일은 413 반환.

```bash
# 파일 다운로드
curl -O -J http://localhost:8080/api/v1/documents/a1b2c3d4-e5f6-7890-abcd-ef1234567890/raw

# 브라우저 인라인 (이미지, PDF 등)
open "http://localhost:8080/api/v1/documents/a1b2c3d4-e5f6-7890-abcd-ef1234567890/raw"
```

---

### GET /api/v1/sources

등록된 수집기 목록과 상태를 반환합니다.

```bash
curl http://localhost:8080/api/v1/sources
```

---

## 환경 변수

`internal/config/config.go` 기반 전체 환경 변수 목록입니다.

### Server 환경 변수

| 키 | 기본값 | 설명 |
|----|--------|------|
| `DATABASE_URL` | `postgres://brain:brain@localhost:5432/second_brain?sslmode=disable` | PostgreSQL 연결 문자열 |
| `PORT` | `8080` | HTTP 서버 포트 |
| `EMBEDDING_API_URL` | `https://api.openai.com/v1` | OpenAI 호환 임베딩 엔드포인트 |
| `EMBEDDING_MODEL` | `text-embedding-3-small` | 임베딩 모델 (1536 차원) |
| `EMBEDDING_API_KEY` | — | Static Bearer 토큰. `CLIPROXY_AUTH_FILE`과 택일 |
| `CLIPROXY_AUTH_FILE` | — | CliProxy OAuth JSON 파일 경로. `access_token` 5분 TTL 자동 갱신 |
| `LLM_API_URL` | — | LLM 큐레이션용 chat completions 엔드포인트 |
| `LLM_API_KEY` | — | LLM API 키 |
| `LLM_MODEL` | — | LLM 모델 식별자 |
| `MIGRATIONS_DIR` | runtime.Caller fallback | Docker 이미지 내부: `/app/migrations` |
| `API_KEY` | — | API 인증 Bearer 토큰 |

### Collector 환경 변수

| 키 | 기본값 | 설명 |
|----|--------|------|
| `DATABASE_URL` | (위와 동일) | PostgreSQL 연결 문자열 |
| `COLLECT_INTERVAL` | `1h` | 수집 스케줄러 주기 (Go duration 형식) |
| `MAX_EMBED_CHARS` | `8000` | 임베딩 입력 최대 문자 수. 초과 시 WARN 로그 후 절단 |
| `EMBEDDING_API_URL` | `https://api.openai.com/v1` | OpenAI 호환 임베딩 엔드포인트 |
| `EMBEDDING_MODEL` | `text-embedding-3-small` | 임베딩 모델 |
| `EMBEDDING_API_KEY` | — | Static Bearer 토큰 |
| `CLIPROXY_AUTH_FILE` | — | CliProxy OAuth JSON 파일 경로 |
| `FILESYSTEM_PATH` | — | 수집할 로컬 경로. Pod 내 경로: `/data/drive` |
| `FILESYSTEM_ENABLED` | `false` | `true`로 설정 시 파일시스템 수집기 등록 |
| `SLACK_BOT_TOKEN` | — | Slack Bot OAuth Token (`xoxb-...`) |
| `SLACK_TEAM_ID` | — | Slack Workspace ID |
| `GITHUB_TOKEN` | — | GitHub Personal Access Token |
| `GITHUB_ORG` | — | GitHub 조직명 |
| `GDRIVE_CREDENTIALS_JSON` | — | Google ADC JSON 경로. 미설정 시 gdrive 수집기 disabled |

> `EMBEDDING_API_KEY`와 `CLIPROXY_AUTH_FILE`이 모두 설정된 경우 `CLIPROXY_AUTH_FILE`이 우선합니다. 둘 중 하나만 설정하세요.

---

## 수집 소스 상태

| 소스 | 활성 조건 | 구현 상태 | 비고 |
|------|-----------|-----------|------|
| filesystem | `FILESYSTEM_PATH` 설정 + `FILESYSTEM_ENABLED=true` | 완전 동작 | 4,150+ 문서 수집 검증 |
| slack | `SLACK_BOT_TOKEN` 설정 | 구현 완료 | public_channel만, DM 자동 제외. 미설정 시 ERROR 후 skip |
| github | `GITHUB_TOKEN` + `GITHUB_ORG` 설정 | 구현 완료 | 미설정 시 ERROR 후 skip |
| gdrive (export) | `GDRIVE_CREDENTIALS_JSON` 설정 | 스캐폴드 | ADC 필요, 기본 disabled |
| notion | — | 제거됨 | main.go 등록 해제 |

### 파일 추출기 (`internal/collector/extractor/`)

| 포맷 | 라이브러리 | 특이 사항 |
|------|-----------|-----------|
| HTML | `golang.org/x/net/html` | 태그 strip 후 텍스트 추출 |
| PDF | `ledongthuc/pdf` | 10초 타임아웃. NUL 바이트 sanitize |
| DOCX | OOXML unzip | `word/document.xml` 파싱 |
| XLSX | `github.com/xuri/excelize/v2` | TSV 출력, `##SHEET {name}` 헤더 + 탭 구분 row, 200 KiB cap |
| PPTX | OOXML unzip | `ppt/slides/*.xml` 파싱 |
| 공통 | `SanitizeText` | 0x00 제거 + UTF-8 validation + 공백 압축 |

---

## 운영

### 서비스 상태 확인

```bash
docker compose ps
docker compose logs server --tail=100 -f
docker compose logs collector --tail=100 -f
```

### 데이터베이스 직접 접속

```bash
docker compose exec postgres psql -U brain -d second_brain
```

```sql
-- 소스별 문서 수
SELECT source, COUNT(*) FROM documents GROUP BY source;

-- 최근 수집된 문서 10건
SELECT title, source, collected_at FROM documents ORDER BY collected_at DESC LIMIT 10;

-- 임베딩 누락 문서 (벡터 검색 제외 대상)
SELECT COUNT(*) FROM documents WHERE embedding IS NULL;
```

### 이미지 재빌드

```bash
docker compose build --no-cache
docker compose up -d
```

### CliProxy OAuth Secret 갱신

토큰이 만료되거나 `auth.json`이 교체된 경우 Collector를 재시작합니다.

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

### 마이그레이션

마이그레이션 파일은 `migrations/` 디렉터리에 위치하며, 서버 기동 시 자동으로 순서대로 적용됩니다. 이미 적용된 마이그레이션은 재실행되지 않습니다.

---

## 알려진 이슈

| ID | 증상 | 원인 | 우회 방법 |
|----|------|------|-----------|
| BUG-007 | minikube mount에서 일부 파일 수집 skip | 9p 마운트에서 한글 긴 파일명(255 바이트 초과) `lstat: file name too long` | 파일명을 255바이트 이하로 단축 |
| — | `cliproxy-auth-secret` 누락 시 임베딩 실패 | git 제외 out-of-band Secret | 배포마다 `kubectl create secret --from-file=auth.json=~/.cli-proxy-api/codex-*.json` 수동 생성 |
| — | Slack/GitHub 수집기 ERROR 후 skip | 자격증명 환경 변수 미설정 | 해당 `*_TOKEN` / `*_ORG` 환경 변수 설정. 미설정 시 해당 소스만 skip, 나머지 수집기는 정상 동작 |
| — | 8,000자 초과 문서 임베딩 절단 | `MAX_EMBED_CHARS` 기본값 8,000 | `MAX_EMBED_CHARS` 값 상향 조정. Phase 1 chunks 테이블 도입 전 완화책 |
| — | gdrive 수집기 미동작 | `GDRIVE_CREDENTIALS_JSON` 미설정 시 기본 disabled | ADC 자격증명 설정 후 활성화 가능 (현재 스캐폴드 단계) |

---

## 관련 문서

- [`ARCHITECTURE.md`](ARCHITECTURE.md) — 아키텍처 상세 설명
- [`EXPANSION.md`](EXPANSION.md) — 확장 계획
- [`docs/runbook-deploy.md`](docs/runbook-deploy.md) — 배포 런북
- [`guides/`](guides/) — 운영 및 개발 가이드 모음

---

## 라이선스

이 저장소에는 라이선스가 아직 명시되지 않았습니다. 사용·배포 전 프로젝트 관리자에게 문의하세요.

---

Last updated: 2026-04-15
