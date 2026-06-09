# Phase 1 설계 명세 — MCP 공개 노출 + 파일 ingest

**작성일**: 2026-06-09
**단계**: Phase 1 of 2 (Phase 2 = Galaxy Flip6 push app + SMS/call/recording ingest)
**상태**: 설계 확정 — 구현 전

---

## 1. 개요 및 목표

second-brain은 이미 MCP 서버(`cmd/mcp`)를 운영 중이다. 현재는 Mac mini 로컬호스트(127.0.0.1:8090)에만 바인딩되어 외부 AI 에이전트가 접근할 수 없다. Phase 1은 다음 두 가지를 달성한다.

1. **MCP 공개 노출** — 터널(Cloudflare Tunnel 또는 역방향 프록시)을 통해 원격 AI 에이전트가 `search`, `get_document`, `stats`, `add_note` 도구를 Bearer 인증 하에 사용할 수 있게 한다.
2. **파일 ingest 엔드포인트 신설** — `POST /api/v1/ingest/file`로 임의 파일을 업로드하면 기존 extractor 파이프라인으로 텍스트를 추출하고, `add_note`와 동일한 경로로 문서화·임베딩한다.

두 기능은 모두 **Bearer 인증을 필수**로 하며, TLS는 터널이 종단한다. 사용자가 직접 터널을 구성하고 `API_KEY` 환경 변수를 설정한다.

### 비목표 (Phase 2)

- Android 앱(`mobile/second-brain-push/`)
- `POST /api/v1/ingest/messages` (SMS·통화 기록 배치)
- `POST /api/v1/ingest/recording` (통화 녹음 .m4a 업로드)

---

## 2. 현황 분석

### 2.1 MCP 서버 현황

| 항목 | 현재 상태 |
|------|----------|
| 바이너리 | `cmd/mcp` |
| 프로토콜 | Streamable HTTP: `POST /mcp` + `GET /mcp/sse` |
| 포트 | `MCP_PORT` 환경 변수 (기본 8090) |
| 바인드 주소 | `MCP_BIND_ADDR` 환경 변수 (기본 `127.0.0.1`) |
| 컨테이너 | `second-brain-local-mcp-1` |
| 공개 도구 | `search`, `get_document`, `stats`, `add_note` |

`mcpAuthContextFunc`은 모든 HTTP 요청에서 Bearer 토큰 검증 결과를 context에 주입한다. `crypto/subtle.ConstantTimeCompare`로 타이밍 공격을 방지한다. 그러나 **`isAuthorized(ctx)` 검사 코드는 `add_note` 핸들러에만 존재한다.** `search`, `get_document`, `stats` 핸들러는 context에 주입된 인증 결과를 전혀 읽지 않는다.

### 2.2 인증 공백 — 소스 코드 확인 결과

`cmd/mcp/main.go`를 직접 확인한 결과:

```go
// add_note 전용 — 다른 세 도구에는 이 검사가 없다
if !isAuthorized(ctx) {
    return mcp.NewToolResultError("unauthorized: Bearer token required"), nil
}
```

`registerSearchTool`, `registerGetDocumentTool`, `registerStatsTool` 함수에는 `isAuthorized(ctx)` 호출이 없다. 결과적으로 터널로 노출할 경우 인증 없이 지식베이스 전체를 조회하고 통계를 열람할 수 있는 프라이버시 홀이 존재한다.

### 2.3 `API_KEY` 빈 값 동작

`mcpAuthContextFunc`은 `API_KEY`가 빈 문자열이면 모든 요청을 인증된 것으로 처리한다(`enabled = len(apiKey) > 0`). 이는 개발 환경 편의를 위한 설계다. **프로덕션(터널 공개)에서는 반드시 `API_KEY`를 설정해야 한다.** `API_KEY`를 설정하지 않은 상태로 터널을 열면 모든 도구가 인증 없이 노출된다.

### 2.4 Extractor 현황

`internal/collector/extractor/extractor.go`의 `NewRegistry()`가 반환하는 내장 Extractor:

| Extractor | 지원 확장자 |
|-----------|------------|
| `HTMLExtractor` | `.html`, `.htm` |
| `PDFExtractor` | `.pdf` |
| `DocxExtractor` | `.docx` |
| `XlsxExtractor` | `.xlsx` |
| `PptxExtractor` | `.pptx` |
| `HwpxExtractor` | `.hwpx` |

플레인 텍스트(`.md`, `.txt`)는 extractor 없이 직접 처리한다.

---

## 3. 아키텍처

### 3.1 터널 경유 전체 흐름

```
Remote AI agent / browser
        │
        │  HTTPS (TLS terminated at tunnel)
        │  Authorization: Bearer <token>
        ▼
┌─────────────────────────────┐
│  Cloudflare Tunnel          │
│  (or reverse proxy)         │
│                             │
│  /mcp      → :8090          │  MCP streamable HTTP
│  /mcp/sse  → :8090          │  MCP SSE transport
│  /api/v1/* → :8081          │  REST API (ingest/file)
└─────────────────────────────┘
        │                   │
        ▼                   ▼
  MCP server           API server
  cmd/mcp :8090        cmd/server :8081
  (containerized)      (containerized)
        │                   │
        └────────┬──────────┘
                 ▼
          PostgreSQL + pgvector
          (shared DB, same network)
```

터널은 사용자가 직접 구성한다. 앱 계층(MCP 서버, API 서버)의 Bearer 인증이 모든 인가 판단을 담당한다. 터널은 TLS 종단만 수행하고 인증 로직을 갖지 않는다.

### 3.2 라우트 매핑

| 공개 경로 | 내부 대상 | 설명 |
|-----------|----------|------|
| `POST /mcp` | MCP 서버 :8090 `/mcp` | Streamable HTTP 요청 |
| `GET /mcp/sse` | MCP 서버 :8090 `/mcp/sse` | SSE 스트림 |
| `POST /api/v1/ingest/file` | API 서버 :8081 | 신규 파일 ingest (Phase 1) |
| `POST /api/v1/ingest/messages` | API 서버 :8081 | 메시지 배치 (Phase 2) |
| `POST /api/v1/ingest/recording` | API 서버 :8081 | 녹음 파일 (Phase 2) |

Phase 1에서는 `/api/v1/ingest/messages`와 `/api/v1/ingest/recording`은 터널에 추가하지 않는다.

---

## 4. MCP 인증 강화 (MUST)

### 4.1 변경 범위

`cmd/mcp/main.go`의 세 핸들러 등록 함수에 `isAuthorized(ctx)` 검사를 추가한다.

**변경 대상 함수**: `registerSearchTool`, `registerGetDocumentTool`, `registerStatsTool`

각 핸들러 함수 본문 첫 줄에 다음 패턴을 삽입한다:

```go
// 기존 add_note 패턴과 동일
if !isAuthorized(ctx) {
    return mcp.NewToolResultError("unauthorized: Bearer token required"), nil
}
```

### 4.2 변경 후 도구별 인증 상태

| 도구 | 변경 전 | 변경 후 |
|------|--------|--------|
| `add_note` | `isAuthorized` 검사 있음 | 변경 없음 |
| `search` | 검사 없음 — 공개 상태 | `isAuthorized` 검사 추가 |
| `get_document` | 검사 없음 — 공개 상태 | `isAuthorized` 검사 추가 |
| `stats` | 검사 없음 — 공개 상태 | `isAuthorized` 검사 추가 |

### 4.3 개발 환경 하위 호환성

`API_KEY` 환경 변수가 비어 있으면 `mcpAuthContextFunc`은 모든 요청을 인증된 것으로 표시하므로, `isAuthorized(ctx)` 검사를 추가해도 기존 로컬 개발 흐름은 변하지 않는다. `API_KEY`를 설정하지 않은 채로 터널을 열면 모든 도구가 인증 없이 공개된다는 사실은 명시적으로 문서화한다.

### 4.4 운영 필수 조건

터널을 활성화하기 전 반드시 충족해야 하는 조건:

```
PRODUCTION GATE
├── API_KEY 설정 여부 확인 (비어 있으면 배포 불가)
├── 모든 MCP 도구에 isAuthorized 검사 존재 확인
└── 터널 경유 Bearer 인증 E2E 검증 (curl로 토큰 없는 요청 → 401)
```

---

## 5. 신규 엔드포인트: `POST /api/v1/ingest/file`

### 5.1 목적

원격 AI 에이전트 또는 사용자가 임의 파일을 second-brain으로 직접 업로드한다. 기존 extractor 파이프라인이 텍스트를 추출하고, `add_note`와 동일한 Upsert + 임베딩 경로로 저장한다.

### 5.2 요청 형식

```
POST /api/v1/ingest/file
Content-Type: multipart/form-data
Authorization: Bearer <token>
```

| 파트명 | Content-Type | 필수 | 설명 |
|--------|-------------|------|------|
| `file` | (파일 본래 MIME) | 필수 | 업로드할 파일 바이너리 |
| `title` | `text/plain` | 선택 | 문서 제목; 생략 시 원본 파일명 사용 |
| `source_id` | `text/plain` | 선택 | 멱등성 키; 생략 시 `upload:{hash}` 자동 생성 |
| `metadata` | `application/json` | 선택 | 임의 키-값 JSON |

### 5.3 SourceID 생성 (idempotent)

`source_id` 파트를 전달하지 않으면 서버가 자동으로 결정론적 ID를 생성한다.

```
source_id = "upload:" + hex(SHA-256(filename + raw_bytes))
```

동일 파일을 반복 업로드해도 같은 `source_id`가 생성되므로 Upsert가 중복 문서를 만들지 않는다. 명시적 `source_id`를 전달하면 그 값을 그대로 사용한다.

### 5.4 파일 크기 제한

| 환경 변수 | 기본값 | 적용 범위 |
|----------|-------|---------|
| `INGEST_MAX_FILE_BYTES` | 104,857,600 (100 MiB) | 파일 파트 바이트 수 |

이 한계를 초과하면 `413 Request Entity Too Large`를 즉시 반환한다. 요청 본문을 버퍼에 읽기 전에 `http.MaxBytesReader`로 제한을 적용한다.

### 5.5 처리 흐름

```
POST /api/v1/ingest/file
        │
        ▼
1. Bearer 인증 검사 (requireAPIKey 미들웨어)
   └── 실패: 401 Unauthorized
        │
        ▼
2. multipart.ParseMultipartForm (MaxBytesReader 적용)
   └── 크기 초과: 413 Request Entity Too Large
        │
        ▼
3. 파일 파트 읽기 → 파일명, 바이트 획득
        │
        ▼
4. source_id 결정
   ├── 파트 있음: 사용자 제공값 사용
   └── 파트 없음: "upload:" + hex(SHA-256(filename + bytes))
        │
        ▼
5. 확장자 추출 → extractor.Registry.Find(ext)
   ├── 지원 확장자: Extract(ctx, tmpfile)
   └── 미지원 확장자 (.md/.txt): string(bytes)
        │
        ▼
6. extractedText == "" → 400 Bad Request ("no extractable text")
        │
        ▼
7. model.Document 생성
   ├── source_type = "upload"
   ├── source_id   = (4에서 결정된 값)
   ├── title       = title 파트 또는 파일명
   ├── content     = extractedText
   ├── status      = "active"
   └── collected_at = time.Now().UTC()
        │
        ▼
8. store.Upsert(doc)
        │
        ▼
9. chunker.Split(content) → chunks
   → chunkStore.ReplaceDocument(doc.ID, chunks)
        │
        ▼
10. embedClient.Enabled() → EmbedBatch(texts)
    → chunkStore.UpdateChunkEmbeddings(embeddings)
    (임베딩 실패는 비치명적 — 저장은 완료됨)
        │
        ▼
11. 응답: 200 OK
    {
      "id": "<uuid>",
      "source_id": "<source_id>",
      "chunks_created": N,
      "embedding_created": true|false,
      "filename": "<original filename>"
    }
```

### 5.6 지원 파일 형식

| 확장자 | 처리 방법 |
|--------|---------|
| `.pdf` | `PDFExtractor` (extractor.Registry) |
| `.docx` | `DocxExtractor` |
| `.xlsx` | `XlsxExtractor` |
| `.pptx` | `PptxExtractor` |
| `.hwpx` | `HwpxExtractor` |
| `.html`, `.htm` | `HTMLExtractor` |
| `.md`, `.txt` | 직접 `string(bytes)` |

위 목록 외 확장자는 `400 Bad Request` ("unsupported file type: .{ext}")를 반환한다. 알 수 없는 확장자를 무작위 바이너리로 저장하지 않는다.

### 5.7 임시 파일 처리

`PDFExtractor`를 비롯한 일부 extractor는 파일 경로를 인자로 받는다. 업로드된 바이트를 `os.CreateTemp`로 임시 파일에 기록하고, extractor 완료 후 즉시 `os.Remove`로 정리한다. `defer`로 정리를 보장하며, 추출 실패 시에도 임시 파일이 남지 않는다.

### 5.8 라우터 등록 위치

`internal/api/router.go`의 Bearer 인증 미들웨어 그룹에 라우트를 추가한다.

```go
// 기존 인증 그룹 내부
r.Post("/api/v1/ingest/file", s.ingestFileHandler)
```

조건부 등록이 아닌 무조건 등록 — 의존성(extractor.Registry, chunker, embedClient)은 서버 초기화 시 항상 주입된다.

---

## 6. 데이터 모델

### 6.1 업로드 Document 필드 매핑

| `model.Document` 필드 | 값 |
|----------------------|-----|
| `source_type` | `"upload"` |
| `source_id` | `"upload:{hex(SHA-256(filename+bytes))}"` 또는 명시적 값 |
| `title` | 요청 `title` 파트 또는 원본 파일명 |
| `content` | extractor 추출 텍스트 (최대 512 KiB, `extractor.MaxExtractedBytes`) |
| `metadata` | 요청 `metadata` 파트; 없으면 `{"filename": "...", "original_size_bytes": N}` |
| `status` | `"active"` |
| `occurred_at` | nil (업로드 시점 이벤트 없음; 정렬은 `collected_at` 기준) |
| `collected_at` | `time.Now().UTC()` |

`add_note`의 `source_type=llm-memory`와 구별하기 위해 `source_type="upload"`를 신규 도입한다.

### 6.2 Chunking · 임베딩 경로

`add_note` 핸들러의 `handleAddNote` 함수가 chunking + 임베딩 로직을 이미 분리(`handleAddNote`)하고 있다. `ingestFileHandler`는 동일한 `handleAddNote` 함수를 재사용하거나 동일 패턴을 따른다. 코드 중복을 피하기 위해 공통 로직을 `internal/api/ingest_common.go`에 분리하는 것을 권장한다.

---

## 7. 보안 및 프라이버시

### 7.1 인증 계층

| 계층 | 책임 |
|------|------|
| 터널(Cloudflare Tunnel 등) | TLS 종단; 인증 로직 없음 |
| MCP 서버 (`cmd/mcp`) | `mcpAuthContextFunc` → `isAuthorized(ctx)` (4개 도구 모두) |
| API 서버 (`cmd/server`) | `requireAPIKey` 미들웨어 → `subtle.ConstantTimeCompare` |

두 서버 모두 `API_KEY` 환경 변수를 공유한다. 단일 Bearer 토큰으로 MCP와 REST API 양쪽을 인증한다.

### 7.2 키 관리

- `API_KEY`는 `.env` 파일 또는 Docker compose 환경 변수로 주입한다.
- `.env` 파일은 `.gitignore`에 포함되어 있어 git에 커밋되지 않는다.
- 터널 활성화 전 `API_KEY` 설정 여부를 반드시 확인한다.
- 빈 `API_KEY`는 모든 인증 검사를 무력화한다. 이는 의도된 개발 편의이며 터널 환경에서는 절대 허용하지 않는다.

### 7.3 파일 ingest 보안

- 파일 크기 상한(`INGEST_MAX_FILE_BYTES`)으로 대용량 DoS를 방지한다.
- `http.MaxBytesReader`를 요청 바디 파싱 전에 적용한다.
- 임시 파일은 extractor 완료 직후 삭제한다 (`defer os.Remove`).
- 지원 확장자 목록 외 파일은 거부한다 (알 수 없는 형식을 실행하거나 저장하지 않음).
- 업로드된 파일의 원본 바이너리는 저장하지 않는다 — 추출된 텍스트만 DB에 저장한다.

### 7.4 MCP 바인드 주소

현재 `MCP_BIND_ADDR` 기본값은 `127.0.0.1`이다. 터널이 로컬호스트의 8090 포트를 목적지로 설정하면 컨테이너 외부 노출 없이 동작한다. 컨테이너 내부 네트워크에서 터널이 접근 가능한 경우 `0.0.0.0` 바인딩이 필요할 수 있으나, 이 경우 방화벽 규칙으로 외부 직접 접근을 차단해야 한다.

---

## 8. 터널 구성 (사용자 수행)

### 8.1 Cloudflare Tunnel 권장 구성 예시

Cloudflare Tunnel을 사용할 경우 `config.yml` 또는 Cloudflare 대시보드에서 다음 ingress 규칙을 설정한다.

```yaml
ingress:
  - hostname: brain.example.com
    path: /mcp
    service: http://localhost:8090
  - hostname: brain.example.com
    path: /mcp/sse
    service: http://localhost:8090
  - hostname: brain.example.com
    path: /api/v1
    service: http://localhost:8081
  - service: http_status:404
```

경로 기반 라우팅을 지원하지 않는 터널 솔루션을 사용할 경우, nginx/Caddy 역방향 프록시로 동일한 라우트 매핑을 구현한다.

```nginx
location /mcp {
    proxy_pass http://127.0.0.1:8090;
    proxy_set_header Authorization $http_authorization;
}
location /api/v1 {
    proxy_pass http://127.0.0.1:8081;
    proxy_set_header Authorization $http_authorization;
}
```

### 8.2 원격 AI 에이전트 MCP 클라이언트 설정 예시

원격 AI 에이전트(예: Claude Desktop, Cursor)의 MCP 설정:

```json
{
  "mcpServers": {
    "second-brain": {
      "url": "https://brain.example.com/mcp",
      "headers": {
        "Authorization": "Bearer <API_KEY>"
      }
    }
  }
}
```

### 8.3 배포 전 점검 목록

```
터널 활성화 전 필수 확인 사항
├── [ ] API_KEY 환경 변수가 비어 있지 않음
├── [ ] MCP 서버: search/get_document/stats 인증 검사 추가됨
├── [ ] 터널 없이 curl로 Bearer 없는 요청 → 401 확인
├── [ ] 터널 없이 curl로 올바른 Bearer → 200 확인
└── [ ] 터널 경유 E2E: https://brain.example.com/mcp Bearer 없이 → 401 확인
```

---

## 9. 구현 파일 레이아웃

```
second-brain/
└── cmd/
│   └── mcp/
│       └── main.go              # 수정 — search/get_document/stats에 isAuthorized 추가
│
└── internal/
    └── api/
        ├── router.go            # 수정 — /api/v1/ingest/file 라우트 추가
        ├── ingest_file.go       # 신규 — 파일 ingest 핸들러
        └── ingest_common.go     # 신규 (권장) — chunking·임베딩 공통 로직 추출
```

`ingest_file.go`는 기존 `internal/collector/extractor` 패키지를 import한다. 새 패키지 생성 없이 기존 코드를 재사용한다.

---

## 10. 테스트 전략

### 10.1 MCP 인증 테스트 (`cmd/mcp/main_test.go`)

기존 `main_test.go` 패턴을 확장한다.

| 테스트 케이스 | 검증 항목 |
|-------------|---------|
| `search` Bearer 없음 | `"unauthorized: Bearer token required"` 오류 반환 |
| `get_document` Bearer 없음 | `"unauthorized: Bearer token required"` 오류 반환 |
| `stats` Bearer 없음 | `"unauthorized: Bearer token required"` 오류 반환 |
| `add_note` Bearer 없음 | 기존 동작 유지 (회귀 방지) |
| `API_KEY` 비어 있을 때 `search` | 인증 없이 통과 (개발 모드 유지) |
| 올바른 Bearer로 `search` | 정상 결과 반환 |

### 10.2 파일 ingest 핸들러 테스트 (`internal/api/ingest_file_test.go`)

기존 `internal/api/eval_test.go` · `feedback_test.go` 패턴을 따른다. 인메모리 mock store 사용.

| 테스트 케이스 | 검증 항목 |
|-------------|---------|
| Bearer 없는 요청 | `401 Unauthorized` |
| 올바른 Bearer + 유효한 .txt 파일 | `200 OK`, `id` 필드 존재, Document Upsert 호출됨 |
| 올바른 Bearer + 유효한 .md 파일 | `200 OK`, 내용 저장됨 |
| 크기 초과 파일 | `413 Request Entity Too Large` |
| 지원하지 않는 확장자 (.xyz) | `400 Bad Request`, 메시지에 `.xyz` 포함 |
| 추출 결과 빈 파일 (공백만) | `400 Bad Request`, "no extractable text" |
| `source_id` 명시 후 동일 요청 반복 | 두 번째 Upsert가 오류 없이 통과, 동일 `id` 반환 |
| `source_id` 미지정 시 동일 파일 반복 | SHA-256 기반 `source_id` 동일 → Upsert 멱등성 확인 |
| `title` 파트 생략 | `title`이 파일명으로 설정됨 |
| 올바른 Bearer + .pdf (PDFExtractor mock) | `200 OK`, `filename` 필드 일치 |
| 임베딩 비활성화 환경 | `embedding_created: false`, 저장은 완료됨 |

### 10.3 Extractor 재사용 확인

`internal/collector/extractor/extractor_test.go`는 변경 없이 그대로 통과해야 한다. `ingest_file.go`는 extractor 패키지를 새로 구현하지 않고 import만 한다.

---

## 11. 미결 사항

### 11.1 MCP 바인드 주소 컨테이너 설정

`MCP_BIND_ADDR`의 기본값(`127.0.0.1`)이 Cloudflare Tunnel 컨테이너 토폴로지에서 정상 동작하는지 확인 필요. 터널 컨테이너와 MCP 컨테이너가 별도 Docker 네트워크에 있을 경우 `0.0.0.0`으로 변경해야 할 수 있다.

### 11.2 `source_type="upload"` 검색 필터 노출

MCP `search` 도구의 `source` 파라미터 허용 목록(`allowedSourceTypes`)에 `upload` 타입 추가 여부를 결정해야 한다. Phase 1에서는 저장만 지원하고 필터는 Phase 2에서 추가하는 방안도 가능하다.

### 11.3 추출 텍스트 크기 상한

현재 `extractor.MaxExtractedBytes`는 512 KiB다. 대용량 PDF 업로드 시 이 한도에 걸리는 경우가 잦을 수 있다. `INGEST_MAX_EXTRACT_BYTES` 환경 변수로 별도 조정 가능하도록 구현하는 것을 검토한다.

---

## 12. 출시 순서

```
구현 순서
1. MCP 인증 강화
   └── search / get_document / stats 에 isAuthorized 검사 추가
   └── 테스트 통과 확인

2. /api/v1/ingest/file 구현
   └── ingest_file.go 작성 (extractor.Registry 재사용)
   └── 핸들러 테스트 통과 확인
   └── router.go에 라우트 등록

3. API_KEY 설정 후 로컬 E2E 검증
   └── curl Bearer 없음 → 401 (MCP 4개 도구 모두)
   └── curl Bearer 있음 → 200 (MCP search, ingest/file)

4. 터널 구성 (사용자 수행)
   └── Cloudflare Tunnel 또는 역방향 프록시 설정
   └── 배포 전 점검 목록 완료 확인 (8.3절)

5. Phase 2 (별도 명세)
   └── Galaxy Flip6 Android 앱
   └── /api/v1/ingest/messages, /api/v1/ingest/recording
```
