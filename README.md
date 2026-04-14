# second-brain

팀의 지식을 한 곳에서 검색하는 사내 AI 검색 엔진. Google Drive, Slack, GitHub, 로컬 파일시스템에 흩어진 문서를 수집·임베딩하여 하이브리드 벡터 + 전문(全文) 검색을 제공합니다.

> English: [README.en.md](README.en.md)

---

## 목차

1. [주요 기능](#주요-기능)
2. [아키텍처 개요](#아키텍처-개요)
3. [빠른 시작 (minikube)](#빠른-시작-minikube)
4. [프로젝트 구조](#프로젝트-구조)
5. [API 레퍼런스](#api-레퍼런스)
6. [환경 변수](#환경-변수)
7. [수집 소스 상태](#수집-소스-상태)
8. [웹 UI 가이드](#웹-ui-가이드)
9. [운영](#운영)
10. [개발](#개발)
11. [알려진 이슈](#알려진-이슈)
12. [관련 문서](#관련-문서)
13. [라이선스](#라이선스)

---

## 주요 기능

- **하이브리드 검색** — BM25 전문(Full-text) 검색(`ts_rank_cd`)과 pgvector 코사인 유사도 검색을 RRF(Reciprocal Rank Fusion)로 결합하여 높은 리콜과 정밀도를 동시에 달성
- **멀티 소스 수집** — 파일시스템, Slack, GitHub, Google Drive(Export) 수집기를 통한 자동 문서 수집 및 주기적 갱신
- **다형식 문서 추출** — HTML, PDF, DOCX, XLSX, PPTX 등 주요 오피스 포맷에서 텍스트 자동 추출
- **OpenAI 호환 임베딩** — API Key 또는 ChatGPT Codex OAuth JWT(CliProxy) 모두 Bearer 토큰으로 수용
- **소프트 삭제** — 소스에서 제거된 문서를 즉시 삭제하지 않고 플래그로 관리하여 이력 보존
- **Next.js 웹 UI** — 검색·필터·정렬, 포맷별 전문 렌더링(Markdown prose, XLSX 테이블, 코드 하이라이트) 제공
- **경량 이미지** — Backend ~34.5 MB (alpine 멀티스테이지), Frontend ~195 MB (Next.js standalone)
- **Kubernetes 배포** — minikube 기반 로컬 k8s 클러스터에 Kustomize로 선언적 배포

---

## 아키텍처 개요

```mermaid
flowchart LR
    Browser["브라우저\n(localhost:3000)"]
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
    Backend -->|수집| FS
    Backend -->|수집| Slack
    Backend -->|수집| GH
    Backend -->|수집| GD
```

수집기는 스케줄러(기본 1시간 주기)가 실행하며, 트리거 API로 즉시 실행도 가능합니다. 수집된 텍스트는 최대 8,000자로 절단 후 임베딩되어 `pgvector` 컬럼에 저장됩니다.

### 주요 컴포넌트

| 컴포넌트 | 이미지 | 베이스 | 크기 | uid |
|----------|--------|--------|------|-----|
| second-brain (백엔드) | `second-brain:dev` | golang:1.24-alpine → alpine:3.21 | ~34.5 MB | 10001 |
| vibers-web (프론트엔드) | `vibers-web:dev` | node:22-alpine (standalone) | ~195 MB | 10001 |
| postgres | `pgvector/pgvector:pg16` | PostgreSQL 16 + pgvector | — | — |

### Kubernetes 리소스 (namespace: `second-brain`)

| 리소스 | 이름 | 비고 |
|--------|------|------|
| Namespace | `second-brain` | — |
| ConfigMap | `second-brain-config` | 비밀 외 환경 설정 |
| Secret | `second-brain-secret` | DB 자격증명 |
| Secret (out-of-band) | `cliproxy-auth-secret` | OAuth JWT, git 미포함 — 수동 생성 필수 |
| PersistentVolume | — | hostPath 100 Gi, Pod `/data/drive`, minikube mount uid 10001 필수 |
| StatefulSet | `postgres` | PostgreSQL 16 + pgvector |
| Service | `postgres` | ClusterIP 5432 |
| Deployment | `second-brain` | initContainer: wait-for-postgres |
| Service | `second-brain` | NodePort 30920 |
| Deployment | `vibers-web` | Next.js standalone |
| Service | `vibers-web` | NodePort 30300 |

---

## 빠른 시작 (minikube)

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

> **macOS Docker driver 주의**: NodePort(30920, 30300)는 macOS docker driver 환경에서 외부 직접 접근이 제한됩니다. 반드시 위 `port-forward` 명령을 사용하세요.

---

## 프로젝트 구조

```
second-brain/
├── cmd/
│   └── server/
│       └── main.go              # 엔트리포인트, 라우터, 수집기 등록 (포트 9200)
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
│   ├── db/                      # pgvector 초기화, 마이그레이션
│   ├── embedding/               # OpenAI 호환 임베딩 클라이언트
│   ├── handler/                 # HTTP 핸들러 (search, documents, sources)
│   ├── model/                   # Document, SearchResult 구조체
│   └── scheduler/               # 주기적 수집 스케줄러 (mutex 중복 방지)
├── migrations/                  # SQL 마이그레이션 (서버 기동 시 자동 적용)
├── deploy/
│   └── k8s/                     # Kustomize 매니페스트
│       ├── kustomization.yaml
│       ├── namespace.yaml
│       ├── configmap.yaml
│       ├── postgres/            # StatefulSet, Service
│       └── app/                 # Deployment, Service (NodePort)
├── web/                         # Next.js 16 프론트엔드
│   ├── app/                     # App Router 페이지
│   ├── components/              # 검색 UI, 문서 카드, 상세 뷰어
│   └── Dockerfile               # standalone 빌드
├── Dockerfile                   # 백엔드 멀티스테이지 빌드
└── go.mod                       # Go 1.25 모듈 정의
```

---

## API 레퍼런스

모든 엔드포인트는 `/api/v1` 접두사를 사용합니다. `/health`만 예외입니다.
웹 UI의 `/api-docs` 페이지에서도 7개 엔드포인트 레퍼런스를 확인할 수 있습니다.

### 엔드포인트 목록

| 메서드 | 경로 | 설명 |
|--------|------|------|
| `GET` | `/health` | 헬스 체크 |
| `POST` | `/api/v1/search` | 하이브리드 검색 (BM25 + pgvector, RRF) |
| `GET` | `/api/v1/documents` | 문서 목록 페이지네이션 |
| `GET` | `/api/v1/documents/{id}` | 단일 문서 상세 조회 |
| `GET` | `/api/v1/documents/{id}/raw` | 원본 파일 스트리밍 (filesystem 전용, 50 MiB 제한) |
| `POST` | `/api/v1/collect/trigger` | 수집 즉시 실행 (스케줄러 mutex 중복 방지) |
| `GET` | `/api/v1/sources` | 등록된 수집기 목록 |

---

### GET /health

서버가 기동 중이면 200을 반환합니다.

```bash
curl http://localhost:9200/health
```

```json
{"status":"ok"}
```

---

### POST /api/v1/search

전문(BM25 `ts_rank_cd`) + 벡터(pgvector `<=>` 코사인) 하이브리드 검색을 RRF로 결합합니다.

**요청 바디**

| 필드 | 타입 | 기본값 | 설명 |
|------|------|--------|------|
| `query` | string | 필수 | 검색 쿼리 |
| `source_type` | string | (전체) | `filesystem` \| `slack` \| `github` 소스 필터 |
| `limit` | int | 10 | 반환 결과 수 |
| `sort` | string | `"relevance"` | `"relevance"` (RRF score 내림차순) \| `"recent"` (collected_at 내림차순) |
| `include_deleted` | bool | `false` | 소프트 삭제된 문서 포함 여부 |

```bash
curl -X POST http://localhost:9200/api/v1/search \
  -H "Content-Type: application/json" \
  -d '{"query": "온보딩 가이드", "limit": 5, "sort": "relevance"}'
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

단일 문서의 메타데이터와 전체 콘텐츠를 JSON으로 반환합니다.

```bash
curl http://localhost:9200/api/v1/documents/a1b2c3d4-e5f6-7890-abcd-ef1234567890
```

---

### GET /api/v1/documents/{id}/raw

원본 파일 바이트를 스트리밍합니다. filesystem 소스 전용이며 Content-Type은 확장자 기반으로 자동 설정됩니다. 50 MiB 초과 파일은 413 반환.

```bash
# 파일 다운로드
curl -O -J http://localhost:9200/api/v1/documents/a1b2c3d4-e5f6-7890-abcd-ef1234567890/raw

# 브라우저 인라인 (이미지, PDF 등)
open "http://localhost:9200/api/v1/documents/a1b2c3d4-e5f6-7890-abcd-ef1234567890/raw"
```

---

### POST /api/v1/collect/trigger

모든 활성 수집기를 즉시 실행합니다. 수집이 진행 중이면 `409 Conflict`를 반환합니다.

```bash
curl -X POST http://localhost:9200/api/v1/collect/trigger
```

---

### GET /api/v1/sources

등록된 수집기 목록과 상태를 반환합니다.

```bash
curl http://localhost:9200/api/v1/sources
```

---

## 환경 변수

`internal/config/config.go` 기반 전체 환경 변수 목록입니다.

| 키 | 기본값 | 설명 |
|----|--------|------|
| `DATABASE_URL` | `postgres://brain:brain@localhost:5432/second_brain?sslmode=disable` | PostgreSQL 연결 문자열 |
| `PORT` | `9200` | HTTP 서버 포트 |
| `COLLECT_INTERVAL` | `1h` | 수집 스케줄러 주기 (Go duration 형식) |
| `MAX_EMBED_CHARS` | `8000` | 임베딩 입력 최대 문자 수. 초과 시 WARN 로그 후 절단 |
| `EMBEDDING_API_URL` | `https://api.openai.com/v1` | OpenAI 호환 임베딩 엔드포인트 |
| `EMBEDDING_MODEL` | `text-embedding-3-small` | 임베딩 모델 (1536 차원) |
| `EMBEDDING_API_KEY` | — | Static Bearer 토큰. `CLIPROXY_AUTH_FILE`과 택일 |
| `CLIPROXY_AUTH_FILE` | — | CliProxy OAuth JSON 파일 경로. `access_token` 5분 TTL 자동 갱신 |
| `MIGRATIONS_DIR` | runtime.Caller fallback | Docker 이미지 내부: `/app/migrations` |
| `FILESYSTEM_PATH` | — | 수집할 로컬 경로. Pod 내 경로: `/data/drive` |
| `FILESYSTEM_ENABLED` | `false` | `true`로 설정 시 파일시스템 수집기 등록 |
| `SLACK_BOT_TOKEN` | — | Slack Bot OAuth Token (`xoxb-...`) |
| `SLACK_TEAM_ID` | — | Slack Workspace ID |
| `GITHUB_TOKEN` | — | GitHub Personal Access Token |
| `GITHUB_ORG` | — | GitHub 조직명 |
| `GDRIVE_CREDENTIALS_JSON` | — | Google ADC JSON 경로. 미설정 시 gdrive 수집기 disabled |
| `API_KEY` | — | 웹 프록시가 백엔드 호출 시 Bearer로 전달하는 API 키 |

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

## 웹 UI 가이드

브라우저에서 `http://localhost:3000` 접속.

### 검색 화면

- 초기 진입 시 최근 문서 10건 자동 표시
- **소스 필터**: 전체 / Drive / Slack / GitHub (필터 변경 시 반응형 갱신)
- **정렬**: 관련도순 (RRF score 내림차순) / 최신순 (수집일 내림차순)
- 문서 카드: 제목, 소스 뱃지, 설명(최대 180자), 이미지 썸네일, 타임스탬프(생성/수정)

### 문서 상세 렌더링

| 포맷 | 렌더러 |
|------|--------|
| Markdown (`.md`) | react-markdown + remark-gfm + Tailwind Typography (`prose`) |
| XLSX | TSV 파싱 후 시트별 HTML 테이블 |
| TXT / LOG | 모노스페이스 `<pre>` 블록 |
| 코드 (json, yaml, go, ts 등 30+ 확장자) | 펜스 블록 + highlight.js 구문 강조 |
| 이미지 | `/raw` 엔드포인트를 통한 원본 인라인 표시 |

타임스탬프 3종(파일 수정일, 최초 수집일, 최근 갱신일) 표시.

### API 문서

헤더 네비게이션의 `/api-docs` 경로에서 7개 엔드포인트 레퍼런스를 확인할 수 있습니다.

---

## 운영

### Pod 상태 확인

```bash
kubectl -n second-brain get pods
kubectl -n second-brain get pods -w   # 실시간 감시
```

### 로그 확인

```bash
# 백엔드 (수집 로그, 임베딩, 검색 쿼리)
kubectl -n second-brain logs -l app=second-brain --tail=100 -f

# 웹 프론트엔드
kubectl -n second-brain logs -l app=vibers-web --tail=50 -f

# PostgreSQL
kubectl -n second-brain logs -l app=postgres --tail=50 -f
```

### port-forward

```bash
# 로컬 전용
kubectl -n second-brain port-forward svc/vibers-web 3000:3000 &
kubectl -n second-brain port-forward svc/second-brain 9200:9200 &

# LAN 공유
kubectl -n second-brain port-forward --address 0.0.0.0 svc/vibers-web 3000:3000 &
kubectl -n second-brain port-forward --address 0.0.0.0 svc/second-brain 9200:9200 &
```

macOS docker 드라이버에서는 NodePort 외부 직접 접근에 제한이 있으므로 port-forward를 사용하세요.

### 수집 즉시 트리거

```bash
curl -X POST http://localhost:9200/api/v1/collect/trigger
```

수집이 진행 중이면 `409 Conflict` 반환. 완료 후 재시도하세요.

### 데이터베이스 직접 접속

```bash
kubectl -n second-brain exec -it statefulset/postgres -- \
  psql -U brain -d second_brain
```

```sql
-- 소스별 문서 수
SELECT source, COUNT(*) FROM documents GROUP BY source;

-- 최근 수집된 문서 10건
SELECT title, source, collected_at FROM documents ORDER BY collected_at DESC LIMIT 10;

-- 임베딩 누락 문서 (벡터 검색 제외 대상)
SELECT COUNT(*) FROM documents WHERE embedding IS NULL;
```

### 이미지 재빌드 및 롤링 재시작

```bash
eval $(minikube docker-env)

# 백엔드
docker build -t second-brain:dev .
kubectl -n second-brain rollout restart deployment/second-brain
kubectl -n second-brain rollout status deployment/second-brain

# 프론트엔드
docker build -t vibers-web:dev -f web/Dockerfile web/
kubectl -n second-brain rollout restart deployment/vibers-web
kubectl -n second-brain rollout status deployment/vibers-web
```

### CliProxy OAuth Secret 갱신

토큰이 만료되거나 `auth.json`이 교체된 경우:

```bash
kubectl -n second-brain delete secret cliproxy-auth-secret
kubectl -n second-brain create secret generic cliproxy-auth-secret \
  --from-file=auth.json=$HOME/.cli-proxy-api/codex-*.json
kubectl -n second-brain rollout restart deployment/second-brain
```

---

## 개발

### 사전 요구사항

- Go 1.25+
- Node.js 22+ / pnpm
- PostgreSQL 16 + pgvector 확장
- Docker / minikube (k8s 배포 시)

### 백엔드 로컬 실행

```bash
export DATABASE_URL="postgres://brain:brain@localhost:5432/second_brain?sslmode=disable"
export EMBEDDING_API_KEY="sk-..."
export FILESYSTEM_PATH="/path/to/docs"
export FILESYSTEM_ENABLED=true

# 서버 기동 (마이그레이션 자동 적용)
go run ./cmd/server/
```

### 백엔드 테스트 및 린트

```bash
go test ./...
go test -race ./...
go vet ./...
gofmt -w .
```

### 프론트엔드 개발 서버

```bash
cd web
pnpm install
pnpm dev
```

### 프론트엔드 빌드 및 린트

```bash
cd web
pnpm build
pnpm lint
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
| — | macOS Docker driver에서 NodePort 직접 접근 불가 | macOS docker driver 네트워크 제한 | `kubectl port-forward` 사용 |
| — | gdrive 수집기 미동작 | `GDRIVE_CREDENTIALS_JSON` 미설정 시 기본 disabled | ADC 자격증명 설정 후 활성화 가능 (현재 스캐폴드 단계) |

---

## 관련 문서

- [`deploy/k8s/README.md`](deploy/k8s/README.md) — Kubernetes 매니페스트 상세 설명
- [`guides/`](guides/) — 운영 및 개발 가이드 모음
- `wiki/` — 프로젝트 위키 (에이전트, 스킬, 룰 레퍼런스)

---

## 라이선스

이 저장소에는 라이선스가 아직 명시되지 않았습니다. 사용·배포 전 프로젝트 관리자에게 문의하세요.

---

Last updated: 2026-04-13
