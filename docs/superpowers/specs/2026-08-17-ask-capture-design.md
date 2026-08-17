# Ask & Capture 설계 명세

**작성일**: 2026-08-17
**대상**: `web/` (Next.js) + Go API 서버 (`cmd/server`) + 컬렉터 데몬 (`cmd/collector`)
**상태**: 설계 확정 — 구현 전

> **개정 (2026-08-17)**: §7 프론트엔드 설계의 UI 라이브러리 결정을 뒤집었다. 원래 채택했던 AWS Cloudscape 대신, `DESIGN.md`(https://github.com/VoltAgent/awesome-design-md, linear.app 항목) 토큰 스펙을 `web/`이 이미 쓰고 있는 Tailwind 4 위에 직접 적용한다. §2·§4·§7·§12·§14의 Cloudscape 관련 서술을 모두 이 결정으로 교체했다 — 취소선이나 원문 보존 없이 교체하되, 무엇이 왜 바뀌었는지는 §7.1에 남긴다. **모바일 레이아웃 요구사항(Ask 하단 고정 입력창, Capture 단일 필드+큰 저장 버튼)은 이 개정으로 사라지지 않는다** — 오히려 컴포넌트 라이브러리의 기본값을 거스를 필요가 없어져 구현이 더 쉬워졌다.

---

## 1. 개요

second-brain은 지금까지 SMS·통화·Gmail·Slack·GitHub·파일시스템 등을 자동 수집해 검색 가능한 RAG로 축적해 왔다. 사용자가 직접 이 데이터에게 "질문"하거나 "생각을 남기는" 진입점은 없었다 — `web/`은 대시보드·거버넌스·문서 뷰어이고, 유일한 노트 작성 경로는 MCP `add_note` 도구(AI 에이전트 전용)뿐이다.

본 명세는 두 가지 사용자 대면 기능을 기존 `web/` Next.js 앱에 추가하는 설계를 기술한다.

1. **Ask** — 자연어로 질문하면 사용자 자신의 색인된 데이터에 근거한 LLM 답변을 클릭 가능한 출처와 함께 받는다.
2. **Capture** — 생각을 적으면 그대로 저장되고, 이후 비동기로 LLM이 제목/요약/태그/엔티티를 정리하고, 암묵지(tacit knowledge)를 별도로 추적되는 추론으로 추출한다.

두 기능 모두 프론트엔드는 `web/`에 있지만, 검색·생성·저장 로직은 전부 Go API 서버(`cmd/server`)와 컬렉터 데몬(`cmd/collector`)에 있다. Next.js는 인증 프록시 이상의 역할을 하지 않는다.

---

## 2. 목표 및 비목표

### 목표

- 46,000+ 문서 코퍼스에 대해 근거 기반 자연어 질의응답을 제공한다(Ask).
- 순간적인 생각을 마찰 없이 기록하고, 이를 잃어버리지 않으면서도 검색 가능한 지식으로 발전시킨다(Capture).
- 사용자의 원문과 LLM의 해석(정리/추론)을 스키마 레벨에서 영구히 구분한다.
- 모바일에서도 Ask·Capture를 편하게 쓸 수 있게 한다.
- 기존 인증·배포·워커 패턴을 재사용해 새 구조를 만들지 않는다.

### 비목표

- `dashboard`, `governance`, `api-docs`, `login`, `documents/[id]` 다섯 개 기존 페이지에 신규 디자인 토큰(§7.1)을 전면 적용하는 것 — 후속 작업으로 별도 추적. (2026-08-17 개정: 이전에는 "Cloudscape 마이그레이션"이었으나, Cloudscape 자체를 채택하지 않기로 하면서 대상이 바뀌었다. 다섯 페이지는 이미 Tailwind 커스텀 컴포넌트로 되어 있으므로 이번 개정은 오히려 이 비목표의 범위를 좁힌다 — 새 토큰으로 갈아 끼우는 일만 남는다.)
- 노트가 아닌 다른 소스(SMS, 통화, Gmail 등)로부터의 암묵지 추출 — v1은 노트에서만 추론한다(§10).
- 기존 `llm-memory` 소스(19,933건, MCP `add_note`로 과거 작성된 문서)의 마이그레이션 — 그대로 둔다(§3.3).
- `brain.baekenough.com` 경로 변경 — 폰의 유일한 ingest 경로이며 건드리지 않는다(§8).
- 실시간 협업, 노트 편집 UI — 범위 밖. 노트 삭제와 인사이트 검색 노출 정책은 결정되었다(§6.5).

---

## 3. 데이터 세그멘테이션 모델 (핵심)

이 설계의 핵심은 노트를 어떻게 저장하느냐다. 세 계층으로 나누되, UI 라벨이 아니라 **스키마 레벨**에서 분리한다.

| 계층 | 위치 | 내용 | 변경 가능 여부 |
|------|------|------|----------------|
| 원문(Raw) | 신규 `SourceNote = "note"` 소스 타입, 문서 `Content` | 사용자가 입력한 그대로 | LLM이 절대 수정하지 않음 |
| 정리(Organized) | 같은 노트 문서의 `Metadata` | LLM이 생성한 제목/요약/태그/엔티티 | 재생성 가능 |
| 추론(Inferred) | 별도 문서, 신규 `SourceInsight = "insight"` 소스 타입 | 노트에서 도출한 암묵지 명제 | 원본 노트를 `Metadata`의 provenance로 역참조 |

### 3.1 왜 이렇게 나누는가

- **원문은 영구히 사용자의 것이어야 한다.** "정리" 과정에서 LLM이 문장을 다시 쓰면, 몇 달 후에는 무엇이 사용자의 실제 생각이고 무엇이 모델의 요약인지 아무도 구별할 수 없다. `Content` 필드는 write-once — 정리 파이프라인은 `Content`를 절대 건드리지 않고 `Metadata`에만 쓴다.
- **추론은 관측이 아니다.** "김대표가 예산 얘기를 피했다"는 관측이고, "자금 사정이 어렵다"는 추론이다. 이 둘을 한 문서에 섞으면, 나중에 추론이 사실로 인용되는 문제가 생긴다. `insight` 문서는 물리적으로 다른 문서다.

### 3.2 에코 체임버 방지

`insight` 문서도 임베딩되어 검색 가능하다 — 즉 하나의 추론이 다음 추론의 근거로 재인용될 수 있다는 뜻이다. 두 가지 가드로 막는다.

1. **insight-of-insight 금지** — `insight` 문서는 그 자체로 다시 enrichment/추출 파이프라인의 입력이 되지 않는다. 정리(§3의 Organized 계층 생성) 및 암묵지 추출(§10의 v1 제약, §6.4의 범위 제한) 모두 `source_type=note`인 문서에만 실행된다.
2. **`/ask` 프롬프트 내 분리된 레인** — 컨텍스트 조립 시 `insight` 문서는 별도로 라벨링된 섹션에 배치되고, "이것은 가설이며 사실로 인용할 수 없다"는 지시문이 함께 주어진다(§5.2).
3. **범위 제한 + 감사 가능성** — 추출 가능한 추론의 종류를 명시적으로 제한하고(§6.4), 모든 인사이트에 confidence 값과 원문 노트의 근거 span을 남겨 사후 감사가 가능하게 한다. 노트당 최대 3건으로 상한을 둔다(§6.4).
4. **`/api/v1/search` 기본 제외(영구 정책)** — `insight` 문서는 일반 검색 API의 기본 결과에서 항상 제외되고, 호출자가 `source_type=insight`를 명시적으로 요청할 때만 반환된다(§6.5). 라벨 없는 일반 검색 결과에 추론이 섞여 나오는 것 자체가 "모델의 추측"이 "내가 알던 사실"로 둔갑하는 경로이기 때문이다.

### 3.3 기존 `llm-memory` 소스와의 관계

`internal/model/document.go`에 정의된 기존 소스 타입은 다음과 같다.

```go
const (
    SourceSlack          SourceType = "slack"
    SourceGitHub         SourceType = "github"
    SourceGDrive         SourceType = "gdrive"
    SourceNotion         SourceType = "notion"
    SourceFilesystem     SourceType = "filesystem"
    SourceDiscord        SourceType = "discord"
    SourceTelegram       SourceType = "telegram"
    SourceSecretary      SourceType = "secretary"
    SourceLLMMemory      SourceType = "llm-memory"
    SourceGmail          SourceType = "gmail"
    SourceCalendar       SourceType = "calendar"
    SourceSMS            SourceType = "sms"
    SourceCallLog        SourceType = "call-log"
    SourceCallTranscript SourceType = "call-transcript"
    SourceUpload         SourceType = "upload"
)
```

여기에 `SourceNote = "note"`와 `SourceInsight = "insight"` 두 개가 추가된다. `llm-memory`는 MCP `add_note` 도구가 지금까지 써온 소스 타입이며, 19,933건이 이미 존재한다. 이 문서들은 **건드리지 않는다** — 마이그레이션도, 재라벨링도 하지 않는다. 이유: `llm-memory` 문서는 3계층 세그멘테이션이 적용되지 않은 채로 저장되어 있어(원문/정리가 한 필드에 섞여 있음) 지금 재분류하려면 각 문서를 사람이 읽고 원문과 정리를 수동으로 분리해야 한다 — 자동화된 마이그레이션은 오분류 위험이 크고, 그 리스크를 감수할 만큼 `llm-memory` 데이터가 활발히 조회되지도 않는다. 새 노트부터 새 모델을 적용하고, 기존 데이터는 별개의 소스로 공존시킨다.

---

## 4. 아키텍처 개요

```
브라우저 (Next.js + Tailwind 4, linear.app 기반 디자인 토큰)
  │  next-auth 세션 쿠키
  ▼
Next.js route handlers (web/src/app/api/v1/ask, web/src/app/api/v1/notes)
  │  Authorization: Bearer ${API_KEY}  (BRAIN_API_URL로 프록시)
  ▼
Go API 서버 (cmd/server, internal/api)
  │
  ├─ POST /api/v1/ask  → search.Service (검색) + llm.Client (SSE 생성)
  └─ POST /api/v1/notes → internal/note (동기 저장, 202 반환)
                              │
                              ▼
                        PostgreSQL (documents, chunks)
                              │
                              ▼  (비동기, 폴링)
                    NoteEnrichmentWorker (cmd/collector, internal/worker)
                        - 정리(title/summary/tags/entities) → Metadata
                        - 암묵지 추출 → 신규 insight 문서
```

Ask와 Capture 모두 생성/저장 로직은 Go에 있다. `search.Service`, `llm.Client`, 임베딩 스택(`internal/search.EmbeddingEngine`)은 이미 Go에 있고 MCP 서버(`cmd/mcp`)와 공유된다. RAG 조립("무엇을 검색하고 어떻게 프롬프트할지")을 Go와 TypeScript 두 곳에 만들면 두 구현이 갈라진다 — 그래서 생성 위치는 Go 신규 엔드포인트로 고정한다.

---

## 5. Ask 설계

### 5.1 `POST /api/v1/ask`

기존 `internal/api/router.go`의 Bearer 인증 그룹(`r.Group` + `requireAPIKey`)에 등록되는 신규 라우트다. `internal/api/document.go`, `search.go` 등 기존 핸들러와 마찬가지로 `Server`에 의존성을 주입해 무조건 등록한다(선택적 기능이 아니므로 `search`/`documents`처럼 `if s.X != nil` 조건부 등록 패턴을 쓰지 않는다).

요청:

```json
{
  "question": "지난달 김대표랑 예산 얘기 어떻게 됐어?"
}
```

응답은 `text/event-stream`(SSE)이다.

```
event: sources
data: {"sources":[{"id":"uuid","title":"...","source_type":"sms","score":0.82},{"id":"uuid","title":"...","source_type":"note","score":0.77}]}

event: token
data: {"text":"김대표는 "}

event: token
data: {"text":"예산 얘기를 피했고,"}

event: done
data: {"finish_reason":"stop"}
```

| 이벤트 | 시점 | 페이로드 |
|--------|------|---------|
| `sources` | 검색 완료 직후, 생성 시작 전 | 컨텍스트로 채택된 문서 목록(id, title, source_type, score) — 프론트엔드가 즉시 "출처" 칩을 렌더링할 수 있게 스트림 최초에 1회 전송 |
| `token` | 생성 중, 반복 | LLM이 생성한 텍스트 조각 |
| `done` | 생성 완료 | `finish_reason` ("stop" \| "error" \| "no_evidence") |
| `error` | 오류 발생 시 (연결은 유지하고 이벤트로 알림) | `{"message": "..."}` |

에러 케이스:

| 상황 | 처리 |
|------|------|
| 인증 실패 | `401 Unauthorized` (SSE 스트림 시작 전 — 기존 `requireAPIKey` 미들웨어) |
| 빈 질문 | `400 Bad Request` |
| 검색 결과 0건 | 스트림은 시작하되 `sources: []` 전송 후, 프롬프트에 "근거 없음"을 명시해 모델이 스스로 "모른다"고 답하게 유도. `done` 이벤트의 `finish_reason`은 `no_evidence` |
| LLM 클라이언트 미설정(`Enabled()==false`) | 스트림 시작 전 `503 Service Unavailable` |
| LLM 호출 중 오류 | `error` 이벤트 전송 후 `done` (`finish_reason: "error"`)로 스트림 정상 종료 — SSE 커넥션을 끊지 않고 클라이언트가 후속 재시도를 결정하게 한다 |

### 5.2 컨텍스트 조립

`search.Service.Search`를 **두 번** 호출한다.

1. **본검색**: `SearchQuery{Query: question, ExcludeSourceTypes: []model.SourceType{model.SourceInsight}, Limit: K}` — `insight` 문서를 제외한 일반 검색(§3.2의 가드 1).
2. **인사이트 검색**: `SearchQuery{Query: question, SourceType: &model.SourceInsight, Limit: M}` — `insight` 문서만 별도로 검색.

K, M은 env var로 조정 가능한 값이며 기본값은 `ASK_CONTEXT_TOP_K=12`(본검색), `ASK_CONTEXT_INSIGHT_M=3`(인사이트 검색)이다. 이 기본값은 시작점이며, 실제 프롬프트 토큰 사용량을 측정한 뒤 튜닝한다(§14) — `gpt-5.6-luna`의 컨텍스트 윈도우 크기나 토큰 단가는 확인된 바 없으므로 본 명세에서 단정하지 않는다.

두 결과는 프롬프트에서 라벨이 다른 두 섹션으로 배치된다.

```
[관측된 사실]
1. (sms, 2026-07-14) "예산 얘기 나오니까 말 돌리시더라고요"
2. (note, 2026-07-15) "김대표 미팅 — 예산 항목만 스킵함"
...

[추론 — 가설이며 사실로 인용 불가]
1. (insight, 출처: note #uuid) "자금 사정이 어려울 가능성"
...

지시: 위 [관측된 사실]에서 답을 찾아라. [추론]은 참고할 수 있으나 그 자체를 사실처럼 서술하지 마라("~할 가능성이 있다는 메모가 있다" 식으로만 인용). 근거가 없으면 "모른다"고 답하라 — 근거 밖으로 추측해서 답하지 마라.
```

시스템 프롬프트에 다음을 고정한다.

- 답은 반드시 제공된 컨텍스트에 근거해야 한다.
- 근거가 불충분하면 "모른다"고 명시적으로 답한다 — 컨텍스트 밖 지식으로 채우지 않는다.
- `insight` 레인의 내용은 가설이며, 사실처럼 인용하지 않는다.

응답 스트림의 `sources` 이벤트에는 본검색 + 인사이트 검색 결과를 합쳐 프론트엔드에서 두 종류를 시각적으로 구분(§7.2)할 수 있도록 `source_type`을 포함한다.

### 5.3 스트리밍 구현

`internal/llm.Client`(`internal/llm/client.go`)의 `Completer` 인터페이스는 현재 다음 두 메서드만 제공한다.

```go
type Completer interface {
    Enabled() bool
    CompleteWithMessages(ctx context.Context, system string, messages []Message) (string, error)
}
```

스트리밍 메서드가 없다 — 이번 작업에서 추가해야 한다. `Client`가 이미 OpenAI 호환 `/v1/chat/completions` 엔드포인트를 호출하므로, `stream: true` + SSE 파싱을 추가하는 확장이다. 새 메서드(가칭 `StreamWithMessages`)를 `Completer` 인터페이스에 추가하면 `internal/api`의 `/ask` 핸들러가 콜백 또는 채널을 통해 토큰을 받아 그대로 클라이언트 SSE로 전달(pass-through)한다. 이 인터페이스 변경은 `EntityWorker`, `SummarizerWorker` 등 기존 `Completer` 소비자에게는 영향이 없다(기존 메서드는 유지, 추가만 함).

**모델**: `gpt-5.6-luna`, OpenAI 호환 엔드포인트, 서버 측 API 키(`internal/config`의 `LLM_API_KEY`/`LLM_API_URL`, 기본값은 `EmbeddingAPIKey`/`EmbeddingAPIURL`에서 파생). 키는 브라우저에 절대 노출되지 않는다 — Next.js route handler는 세션만 검증하고, 실제 LLM 호출은 Go 서버 프로세스 내부에서만 일어난다.

**전용 모델 override**: 신규 env var `LLM_ASK_MODEL`(기본값 `gpt-5.6-luna`)을 도입해 `/ask`와 노트 enrichment 워커(§6.3) 둘 다 이 값을 쓴다. 기존 `LLM_MODEL`(기본값 `gpt-4o-mini`)은 엔티티 추출·요약·HyDE·큐레이션 등 기존 기능에 그대로 남는다. 두 노브로 분리하는 이유: 저 기존 기능들은 46,000+ 문서 코퍼스 전체를 대상으로 대량 실행되므로, `LLM_MODEL`을 조용히 더 큰 모델로 바꾸면 비용이 배수로 늘고 아무도 요청하지 않은 방식으로 기존 출력이 바뀐다. 반면 Ask·Capture enrichment는 대화형·저볼륨이면서 품질에 민감한 워크로드다. 워크로드 성격이 다르므로 노브도 다르게 둔다.

---

## 6. Capture 설계

### 6.1 `POST /api/v1/notes`

원문을 동기적으로 저장하고 즉시 응답한다. Enrichment는 별도 워커가 비동기로 처리한다 — 저장이 LLM 가용성이나 지연에 의존하면 안 된다(폰에서 입력한 생각이 OpenAI 장애에도 살아남아야 한다).

요청:

```json
{
  "title": "김대표 미팅 메모",
  "content": "예산 얘기 나오니까 계속 말을 돌리더라. 다음 안건으로 넘어가자고 두 번이나 제안함."
}
```

`title`이 비어 있으면 서버가 `content` 앞부분으로 임시 제목을 생성하지 않고, enrichment 워커가 정식 제목을 생성할 때까지 빈 문자열로 둔다(추측 제목을 원문처럼 보이게 만들지 않기 위함 — `Content`뿐 아니라 사용자가 명시적으로 입력한 것만 즉시 필드에 반영한다는 원칙의 연장).

응답: `202 Accepted`

```json
{
  "id": "uuid",
  "status": "pending"
}
```

크기 제한은 기존 `add_note`와 동일하게 맞춘다 — `cmd/mcp/main.go`에 정의된 `maxNoteContentBytes = 10 * 1024 * 1024`(10 MiB), `maxNoteTitleBytes = 1024`(1 KiB), `maxEmbedChunks = 2000`. 청크 수가 2000을 넘는 노트는 저장·FTS 색인은 되지만 임베딩은 건너뛴다(기존 `add_note`의 `errEmbedSkipped` 동작과 동일 — 사용자에게 오류로 노출하지 않는다).

에러 케이스:

| 상황 | 처리 |
|------|------|
| 인증 실패 | `401 Unauthorized` |
| `content` 비어있음 | `400 Bad Request` |
| `content` > 10 MiB | `413 Request Entity Too Large` |
| `title` > 1 KiB | `400 Bad Request` |
| DB 저장 실패 | `500 Internal Server Error` |

Enrichment 상태는 노트 문서의 `Metadata`에 기록된다.

```json
{
  "enrichment_status": "pending",   // "pending" | "done" | "failed"
  "enrichment_attempts": 0,          // 시도 횟수, 최대 3
  "enrichment_last_error": null      // failed일 때만 채워짐
}
```

`Document.Status`(`"active"`/`"deleted"`/`"moved"`)는 문서 생명주기 필드로 이미 용도가 정해져 있어(`internal/model/document.go` 주석 참조) enrichment 진행 상태를 여기에 얹지 않는다 — `Metadata.enrichment_status`로 분리해 재시도 가능하고 UI에서 조회 가능하게 한다. 재시도 정책과 `enrichment_attempts`의 소비 방식은 §6.3.

### 6.2 코드 재사용 — `internal/note` 패키지 추출

`cmd/mcp/main.go`(537번 줄)의 `handleAddNote`와 그 헬퍼들은 이미 좁은 인터페이스로 분리되어 있다.

```go
// cmd/mcp/main.go
type NoteDocumentUpserter interface {
    Upsert(ctx context.Context, doc *model.Document) error
}
type NoteChunkWriter interface {
    ReplaceDocument(ctx context.Context, documentID uuid.UUID, chunks []store.Chunk) error
    ListByDocument(ctx context.Context, documentID uuid.UUID) ([]store.Chunk, error)
    UpdateChunkEmbeddings(ctx context.Context, embeddings []store.ChunkEmbedding) error
}
type NoteEmbedder interface {
    Enabled() bool
    EmbedBatch(ctx context.Context, texts []string) ([][]float32, error)
}
```

이 세 인터페이스와 `handleAddNote`, `embedNoteChunks`, 관련 상수(`maxNoteContentBytes`, `maxNoteTitleBytes`, `maxEmbedChunks`, `errEmbedSkipped`)를 신규 패키지 `internal/note`로 추출한다. MCP `add_note` 도구와 신규 `POST /api/v1/notes` 핸들러가 동일한 코드를 호출하게 된다.

**소스 타입 변경**: 현재 `handleAddNote`는 `model.SourceLLMMemory`를 하드코딩한다(`cmd/mcp/main.go:573`). `internal/note`로 옮기면서 소스 타입을 파라미터화해 `SourceNote`를 기본값으로 쓴다. `add_note` MCP 도구는 하위 호환을 위해 계속 `SourceLLMMemory`를 명시적으로 넘긴다 — MCP를 통해 AI 에이전트가 쓰는 메모리와, 사람이 Capture UI로 쓰는 노트는 계속 별개 소스로 유지한다(§3.3). `POST /api/v1/notes`는 `SourceNote`를 넘긴다.

청킹은 기존과 동일하게 `chunker.Split(content, chunker.SelectOptions(*doc))`를 그대로 쓴다.

### 6.3 Enrichment 워커

`internal/worker/entity_worker.go`의 패턴(리스터 인터페이스 + `Run`/`tick`/`markProcessed` + claim 방식)을 따른다. 이 워커도 `internal/worker/summarizer.go`, `entity_worker.go`와 마찬가지로 **컬렉터 데몬**(`cmd/collector/main.go`)에서 기동된다 — API 서버 프로세스가 아니다. 기존 워커들이 전부 `cmd/collector`에서 등록·실행되는 구조를 그대로 따른다.

```go
// internal/worker/note_enrichment_worker.go (신규)
type NoteEnrichmentLister interface {
    ListPendingNotes(ctx context.Context, limit int) ([]*model.Document, error)
    MarkNoteEnriched(ctx context.Context, documentID uuid.UUID, metadata map[string]any) error
    MarkNoteEnrichmentAttemptFailed(ctx context.Context, documentID uuid.UUID, attempts int, reason string) error
    MarkNoteEnrichmentTerminal(ctx context.Context, documentID uuid.UUID, reason string) error
}
```

**단일 LLM 호출 구조**: 노트당 LLM 호출은 **한 번**이다. 정리 필드(제목/요약/태그/엔티티)와 암묵지 후보(§6.4)를 모두 포함하는 구조화된 JSON 스키마를 한 번의 호출로 받는다 — 컨텍스트(노트 원문)를 두 번 보내지 않고, 비용도 노트당 호출 1회로 상한이 걸린다. 엔티티는 응답 JSON의 `entities` 필드로 받아 기존 `internal/worker/entity_extractor.go`의 `model.Entity` 스키마와 동일한 형태로 매핑한 뒤, `EntityLinker.UpsertAndLinkEntities`(기존 `EntityWorker`가 쓰는 저장/연결 경로)로 연결한다 — 즉 엔티티 **추출** 자체는 이번 단일 호출의 일부이고, 엔티티 **저장/링킹** 메커니즘만 기존 코드를 재사용한다(`ExtractEntities`를 별도로 다시 호출하지 않는다).

한 틱에서 처리하는 노트마다:

1. 단일 LLM 호출 → 구조화된 JSON(제목/요약/태그/엔티티/암묵지 후보 최대 3건, §6.4) 파싱.
2. 성공 시: 정리 필드를 노트 문서 `Metadata`에 병합, 엔티티는 `UpsertAndLinkEntities`로 연결, 암묵지 후보는 각각 새 `insight` 문서로 저장(§6.5) → `MarkNoteEnriched`로 `enrichment_status: "done"`.
3. 실패 시(LLM 오류, JSON 파싱 실패 등): `enrichment_attempts`를 증가시키고 `MarkNoteEnrichmentAttemptFailed` 호출.
   - **3회 미만**: 지수 백오프(예: 1분/5분/30분) 후 다음 틱에서 재시도 대상에 다시 포함.
   - **3회 도달**: `MarkNoteEnrichmentTerminal`로 `enrichment_status: "failed"` 확정(터미널 상태) + `enrichment_last_error`에 사유 기록. 이후 워커는 이 노트를 자동으로 재큐잉하지 않는다.
4. **수동 재시도**: 터미널 `failed` 노트는 UI에서 재시도 버튼으로 노출된다(§7.3). 재시도는 `POST /api/v1/notes/{id}/retry-enrichment`(신규, 기존 `requireAPIKey` 그룹에 등록)로 `enrichment_attempts`를 0으로, `enrichment_status`를 `pending`으로 되돌려 다음 워커 틱에서 다시 처리되게 한다.

무제한 재시도는 절대 끝나지 않는 LLM 비용 누수이고, 재시도 0회는 일시적인 429나 네트워크 오류에도 노트가 영구히 미정리 상태로 남는다 — 3회 상한 + 수동 재시도로 두 극단을 피한다.

### 6.4 암묵지 추출 범위 (중요)

암묵지 추출은 "흥미로워 보이는 건 뭐든 추론"하는 방식으로 두면 가장 빠르게 코퍼스를 오염시킨다 — 잘못된 추론도 똑같이 임베딩되고 검색되며, 1년 뒤에는 좋은 추론과 구별되지 않는다. 그래서 추출 범위를 명시적으로 제한한다.

**허용되는 추론**:
- 노트가 암묵적으로 내포하는 사실의 재서술.
- 사용자 자신의 여러 노트에 걸쳐 반복되는 패턴.
- 노트가 전제하고 있지만 명시하지 않은 전제(unstated premise).
- 후속 행동 후보(follow-up action candidate).

**금지되는 추론**:
- 제3자의 감정·동기·성격에 대한 단정.
- 단 한 번의 관찰로부터의 인과관계 단정.
- 노트 표면에 근거가 없는, 특정 인물에 대한 주장.

**감사 가능성**: 모든 인사이트는 `confidence` 값과 원본 노트 내 근거 span(예: 문자 오프셋 범위 또는 인용 문장)을 함께 가져야 한다 — 나중에 "이 추론이 어디서 나왔는지"를 감사할 수 있어야 한다.

**상한**: 노트당 인사이트는 **최대 3건**. 상한이 없으면 모델은 항상 "하나 더" 추론을 찾아내려 하고, 저품질 추론의 양이 늘어나는 것은 아예 없는 것보다 나쁘다 — 검색에서 진짜 근거를 밀어낸다.

### 6.5 `insight` 문서 생성 규칙

- `SourceType: SourceInsight`, `Content`는 추론 명제 자체(예: "자금 사정이 어려울 가능성").
- `Metadata.confidence`(§6.4의 confidence 값), `Metadata.source_span`(근거 span).
- `Metadata.provenance.source_note_id`로 원본 노트를 역참조.
- 임베딩됨 — 검색 가능(§3.2 가드로 재귀 방지).
- **enrichment 파이프라인의 재입력이 되지 않음**: `ListPendingNotes`는 `source_type=note`만 조회하므로 `insight` 문서는 애초에 워커의 처리 대상이 아니다.
- **원본 노트 삭제 시 연쇄 소프트 삭제(영구 정책)**: 노트가 소프트 삭제(`Status: "deleted"`)되면, `Metadata.provenance.source_note_id`로 해당 노트를 가리키는 모든 `insight` 문서도 함께 소프트 삭제된다. 근거가 사라진 추론은 반증 불가능하고 감사 불가능하다 — 고아 인사이트를 남겨두는 것은 형태만 다른 provenance 붕괴다.
- **`/api/v1/search` 기본 결과에서 항상 제외(영구 정책)**: 호출자가 `SourceType: &model.SourceInsight`로 명시적으로 요청하지 않는 한 일반 검색 결과에 포함되지 않는다. `/ask`만이 `insight`를 쓰는 유일한 경로이며, 거기서도 항상 별도 라벨링된 레인으로만 등장한다(§5.2).

---

## 7. 프론트엔드 설계

### 7.1 디자인 토큰 채택 (2026-08-17 개정 — 원래 결정은 AWS Cloudscape였음)

**뒤집힌 결정과 사유**. 이 절의 원래 버전은 AWS Cloudscape Design System을 통째로 도입하는 안이었다. 그 결정을 뒤집는다. 대신 [`DESIGN.md` 토큰 스펙](https://github.com/VoltAgent/awesome-design-md)(`design-md/linear.app/DESIGN.md`)을 `web/`에 `web/DESIGN.md`로 벤더링하고, 이를 `web/`이 이미 쓰고 있는 Tailwind 4의 CSS-first `@theme` 설정(`web/src/app/globals.css`)에 직접 반영한다. 새 컴포넌트 라이브러리를 추가하지 않는다.

사유 세 가지:

1. **Cloudscape는 데스크톱 콘솔 지향이고, 원래 명세 자신이 그 약점을 지적했다.** §7.2·§7.3의 원래 버전은 "Cloudscape는 데스크톱 콘솔 지향의 정보 밀도가 높은 시스템이라 기본 컴포지션이 폰 화면에서 그대로 동작하지 않는다"고 명시하며 모바일을 프론트엔드 리스크 1순위로 꼽았다. 토큰 기반 Tailwind는 애초에 컴포넌트가 강요하는 레이아웃이 없으므로, 이 앱이 필요로 하는 모바일 레이아웃(§7.2·§7.3 유지)을 처음부터 완전히 통제할 수 있다.
2. **Cloudscape는 그 자체로 강한 시각적 정체성을 가진다.** AWS 콘솔류의 정보 밀도·컴포넌트 크롬은 이번에 채택한 linear.app 계열 디자인 언어(§7.1.1)와 정면으로 충돌한다. 두 정체성을 한 앱에 공존시키느니, 처음부터 하나를 선택한다.
3. **컴포넌트 라이브러리를 정당화할 마이그레이션 비용이 애초에 없었다.** `web/`은 이미 Tailwind 4를 쓰고 있고, 커스텀 UI 컴포넌트는 `web/src/components/ui/`의 `Card`, `Badge`, `Button`, `Spinner` 네 개뿐이다(§7.1 원래 버전이 스스로 이 사실을 언급했다). 컴포넌트 라이브러리 도입은 보통 "밑바닥부터 다시 만들 컴포넌트가 너무 많다"는 이유로 정당화되는데, 이 앱에는 그 이유가 없다.

#### 7.1.1 채택 디자인: linear.app

톤은 linear.app — 거의 검정에 가까운 캔버스(`#010102`), 단일 라벤더-블루 액센트(`#5e6ad2`), 헤어라인 보더를 가진 차콜 서페이스, 밀도 높고 기술적인 인상. 이 앱은 매일 쓰는 개인 도구이고(단일 액센트 원칙이 정보 밀도 높은 UI를 차분하게 유지한다), 야간 캡처가 잦으므로(다크 퍼스트가 유리하다) 이 톤이 맞다.

**주의 — 마케팅 사이트에서 추출된 스펙의 한계**: `DESIGN.md` 계열 저장소는 실제 제품 UI가 아니라 마케팅 랜딩 페이지에서 토큰을 추출한다. 색상·서페이스·헤어라인 보더·단일 액센트 원칙은 그대로 채택하되, **타입 스케일(예: display-xl 80px급)은 애플리케이션 UI에 맞지 않으므로 그대로 쓰지 않는다** — 밀도 높은 앱 UI에 맞게 별도로 재도출한다(구체 값은 프론트엔드 구현 계획 `docs/superpowers/plans/2026-08-17-ask-capture-frontend.md`에 있다). 이 한계는 벤더링된 `web/DESIGN.md` 파일 상단 주석에도 명시한다.

이번 작업 범위는 Ask·Capture 화면과 앱 셸(내비게이션, 레이아웃 프레임)뿐이다 — 기존 다섯 페이지(`dashboard`, `governance`, `api-docs`, `login`, `documents/[id]`)에 새 토큰을 전면 적용하는 일은 하지 않는다(후속 작업, §2 비목표).

### 7.2 Ask 화면

**데스크톱**: 질문 입력, 스트리밍 답변, 하단(또는 옆)에 출처 카드 목록 — Tailwind 커스텀 컴포넌트로 직접 구성한다. 출처 카드는 `source_type`에 따라 시각적으로 구분한다(`insight`는 별도 배지로 "추론" 표시, 클릭 시 원본 노트로 이동 가능하도록 `provenance.source_note_id` 활용).

**모바일**: 세로 스택 레이아웃 + 화면 하단 고정 입력창으로 구성한다. (2026-08-17 개정: 원래 이 요구사항은 "Cloudscape 기본값에서 의도적으로 벗어나는" 구성으로 서술되어 있었다. 토큰 기반 Tailwind로 바뀌면서 벗어나야 할 컴포넌트 기본값 자체가 없어졌다 — 요구사항은 그대로이지만 지금은 유일한 구성이지 예외적 이탈이 아니다.)

### 7.3 Capture 화면

**데스크톱**: 단일 텍스트 영역 + 저장 버튼. 저장 후 enrichment 상태(`pending`/`done`/`failed`)를 노트 목록에서 확인 가능. 터미널 `failed`(3회 실패) 노트는 실패 사유와 함께 재시도 버튼이 노출되며, 클릭 시 `POST /api/v1/notes/{id}/retry-enrichment`를 호출한다(§6.3).

**모바일**: 입력 마찰을 최소화하기 위해 필드 하나 + 큰 저장 버튼만 노출한다. (2026-08-17 개정: §7.2와 동일한 이유로 "Cloudscape 기본 데스크톱 폼 컴포지션에서 벗어나는" 서술을 제거했다 — 요구사항 자체는 유지된다.)

### 7.4 인증

기존 패턴을 그대로 재사용한다 — 새 인증 메커니즘을 만들지 않는다.

- 브라우저는 next-auth 세션을 보유(`web/src/auth.ts`) — GitHub OAuth(`GITHUB_CLIENT_ID`/`GITHUB_CLIENT_SECRET`) + `ALLOWED_GITHUB_USERS` 화이트리스트.
- `web/src/app/api/v1/ask/route.ts`, `web/src/app/api/v1/notes/route.ts` 두 신규 route handler가 세션을 검증한 뒤, 기존 `web/src/app/api/search/route.ts` 패턴과 동일하게 `BRAIN_API_URL`(컨테이너 내부에서는 `http://server:8080`) + `Authorization: Bearer ${API_KEY}`로 Go 서버에 프록시한다. `/ask`는 SSE 스트림이므로 route handler는 응답 바디를 버퍼링하지 않고 그대로 pass-through 해야 한다(Next.js Route Handler의 `ReadableStream` 응답으로 구현).

---

## 8. 배포

- 신규 호스트네임 `sb.baekenough.com`을 Mac mini의 `~/.cloudflared/second-brain.yml`에 ingress 엔트리 하나로 추가하고, `web` 컨테이너로 라우팅한다.
- `brain.baekenough.com`은 **변경하지 않는다** — 계속 Go 서버(`localhost:8081`, `docker-compose.local.yml`에서 `server` 서비스가 `8081:8080`으로 바인딩됨)를 직접 가리킨다. 이 경로는 폰(second-brain-push 앱)의 유일한 ingest 진입점이며, 2026년 8월 prod 점검에서 `web` 컨테이너가 폰의 유일한 인터넷 진입점 역할을 하고 있었다는 것이 실측으로 드러나 의도적으로 분리한 구조다(§`feedback_container_hidden_dependency` 참조). `web`을 다시 그 경로에 끼워 넣으면 동일한 단일 장애점이 재현된다.
- `web` 컨테이너는 현재 `docker-compose.local.yml`에 정의는 되어 있으나 prod에서 기동되고 있지 않다(8월 축소 당시 제거됨). 본 작업으로 다시 기동한다.

---

## 9. 오류 처리

### 9.1 `/api/v1/ask`

| 상황 | 처리 |
|------|------|
| 검색 0건 | `sources: []` + `no_evidence` 종료 |
| LLM 스트리밍 중 네트워크 오류 | `error` 이벤트 + `done(finish_reason: "error")`, 커넥션은 정상 종료 |
| 클라이언트 연결 조기 종료 | 서버는 `ctx.Done()`을 감지해 LLM 호출을 취소(컨텍스트 전파) |

### 9.2 `/api/v1/notes`

| 상황 | 처리 |
|------|------|
| enrichment LLM 실패(1~2회차) | `enrichment_attempts` 증가, 지수 백오프 후 다음 틱에서 재시도. 노트 원문·저장은 영향 없음 |
| enrichment LLM 실패(3회차, 터미널) | `enrichment_status: "failed"` 확정 + `enrichment_last_error` 기록, 자동 재큐잉 중단. UI 수동 재시도로만 재개(§6.3) |
| 임베딩 청크 수 초과(2000) | `errEmbedSkipped`와 동일하게 조용히 스킵 — 사용자 오류 아님 |
| `POST /api/v1/notes/{id}/retry-enrichment` 대상 노트가 터미널 `failed`가 아님 | `409 Conflict`(이미 진행 중이거나 완료된 노트에 대한 불필요한 재시도 방지) |

---

## 10. 보안 / 프라이버시

- LLM API 키는 서버 프로세스 내부에만 존재 — `LLM_API_KEY`가 브라우저·Next.js 클라이언트 번들에 노출되지 않는다(Next.js route handler는 서버 사이드에서만 실행되고 브라우저로 키를 전달하지 않는다).
- 노트는 사용자 본인이 직접 입력한 텍스트이므로, SMS/통화 전사에 적용되는 OTP 리댁션 같은 처리는 해당하지 않는다.
- **v1 제약**: 암묵지 추출은 `source_type=note` 문서에서만 수행한다. SMS·통화 전사 등 다른 소스로부터 인사이트를 추론하지 않는다 — 다른 소스는 이미 전사 단계에서 OTP 등을 리댁션했는데, 만약 그 소스에서 인사이트를 추론하면 리댁션된 내용이 인사이트 문서를 통해 다시 표면화될 위험이 있다. 이 위험을 v1 범위에서 원천 차단한다.
- `insight` 문서는 `Metadata.provenance.source_note_id`로 원본을 추적 가능하며, 원본 노트가 소프트 삭제되면 연쇄 소프트 삭제된다(§6.5, 영구 정책).
- `insight` 문서는 `/api/v1/search` 기본 결과에서 항상 제외되고, `/ask`의 별도 레인에서만 사용된다(§6.5, 영구 정책) — 라벨 없는 추론이 사실처럼 검색 결과에 섞여 나오는 경로를 스키마 레벨에서 차단한다.
- 암묵지 추출 자체의 허용/금지 범위(제3자 감정·동기·성격 단정 금지, 단일 관찰로부터의 인과 단정 금지 등)는 §6.4 참조 — 이 또한 넓은 의미의 프라이버시 가드다.

---

## 11. 테스트 전략

### 11.1 Go 핸들러 테스트

기존 `internal/api/ingest_messages_test.go` 패턴(`httptest.NewRequest` + `srv.Handler().ServeHTTP` + 스텁 구현체)을 따른다.

| 대상 | 테스트 케이스 |
|------|--------------|
| `POST /api/v1/ask` | 인증 없음 → 401 / 빈 질문 → 400 / 검색 0건 → `no_evidence` / 정상 흐름에서 `sources` → `token`* → `done` 이벤트 순서 검증(가짜 `Completer`로 스트리밍 모킹) / LLM 미설정 → 503 |
| `POST /api/v1/notes` | 인증 없음 → 401 / 빈 content → 400 / 10 MiB 초과 → 413 / 정상 저장 → 202 + `enrichment_status: pending` |
| `internal/note` 패키지 | 기존 `handleAddNote` 테스트(`cmd/mcp/main_test.go`)를 이관, 소스 타입 파라미터화 검증(기본 `SourceNote`, MCP 경로는 `SourceLLMMemory`) |

### 11.2 노트 파이프라인 테스트

| 대상 | 테스트 케이스 |
|------|--------------|
| `NoteEnrichmentWorker` | `entity_worker_test.go` 패턴 — 가짜 `NoteEnrichmentLister` + 가짜 `Completer`로 tick 단위 처리 검증. 단일 호출 성공 시 `Metadata` 병합 + 엔티티 링킹 + insight 문서 생성이 모두 한 틱에서 일어나는지 확인 |
| 재시도 정책 | 1~2회 실패 시 `enrichment_attempts` 증가 + 비터미널 상태 유지, 3회째 실패 시 `enrichment_status: failed`(터미널) + 자동 재큐잉 중단 확인 |
| 인사이트 상한 | LLM이 4건 이상의 후보를 반환해도 상위 3건만 저장되는지 확인(§6.4) |
| 에코 체임버 가드 | `ListPendingNotes`가 `source_type=note`만 반환하는지, `insight` 문서가 절대 조회 대상에 포함되지 않는지 단위 테스트로 고정 |
| `/api/v1/search` 기본 제외 | `source_type` 미지정 검색 결과에 `insight` 문서가 나타나지 않는지, `source_type=insight` 명시 시에만 반환되는지 확인(§6.5) |
| `POST /api/v1/notes/{id}/retry-enrichment` | 터미널 `failed` 노트 → `enrichment_attempts`가 0으로 리셋되고 `pending`으로 전환 / 비터미널 상태 노트 → `409 Conflict` |

### 11.3 수동 검증

- 모바일 레이아웃(Ask 하단 고정 입력창, Capture 단일 필드)은 실기기/에뮬레이터에서 수동 확인 — 자동화된 시각 회귀 테스트는 범위 밖.
- SSE 스트리밍의 실제 토큰 지연/청크 분할은 브라우저 Network 탭에서 수동 확인.

---

## 12. 파일 레이아웃

```
second-brain/
├── internal/
│   ├── note/                        # 신규 — cmd/mcp/main.go에서 추출
│   │   ├── note.go                  # handleAddNote, embedNoteChunks 이관 (소스 타입 파라미터화)
│   │   ├── note_test.go
│   │   └── limits.go                # maxNoteContentBytes, maxNoteTitleBytes, maxEmbedChunks
│   ├── api/
│   │   ├── router.go                # 수정 — /api/v1/ask, /api/v1/notes, /api/v1/notes/{id}/retry-enrichment 라우트 추가
│   │   ├── ask.go                   # 신규 — SSE 핸들러, 컨텍스트 조립
│   │   ├── ask_test.go
│   │   ├── notes.go                 # 신규 — POST /api/v1/notes + POST /api/v1/notes/{id}/retry-enrichment 핸들러
│   │   └── notes_test.go
│   ├── llm/
│   │   └── client.go                # 수정 — StreamWithMessages 추가, Completer 인터페이스 확장
│   ├── worker/
│   │   ├── note_enrichment_worker.go   # 신규 — entity_worker.go 패턴
│   │   └── note_enrichment_worker_test.go
│   ├── model/
│   │   └── document.go              # 수정 — SourceNote, SourceInsight 추가
│   └── config/
│       └── config.go                # 수정 — LLM_ASK_MODEL, ASK_CONTEXT_TOP_K, ASK_CONTEXT_INSIGHT_M 추가
├── cmd/
│   ├── mcp/main.go                  # 수정 — internal/note 호출로 교체, SourceLLMMemory 유지
│   └── collector/main.go            # 수정 — NoteEnrichmentWorker 등록
└── web/
    ├── DESIGN.md                                # 신규 — linear.app 디자인 토큰 벤더링본 (2026-08-17 개정, §7.1)
    └── src/
        ├── app/
        │   ├── globals.css                      # 수정 — @theme을 linear.app 토큰으로 교체 (§7.1)
        │   ├── ask/page.tsx                     # 신규
        │   ├── capture/page.tsx                 # 신규
        │   └── api/                             # 신규 라우트는 /api/v1/* 가 아니라 /api/* 하위에 둔다
        │       ├── ask/route.ts                 # 신규 — SSE pass-through 프록시 (OAuth 세션 보호, api/search 패턴)
        │       ├── notes/route.ts               # 신규 — 프록시 (OAuth 세션 보호, api/documents 패턴)
        │       └── notes/[id]/
        │           ├── route.ts                 # 신규 — DELETE 프록시
        │           └── retry-enrichment/route.ts  # 신규 — POST 프록시
        └── components/                          # 신규 컴포넌트는 기존 components/ui/ 관례를 따른다 (별도 라이브러리 디렉터리 없음, §7.1)
```

**(2026-08-17 개정) 라우트 경로 주의**: 원래 명세는 `web/src/app/api/v1/ask/route.ts`, `web/src/app/api/v1/notes/route.ts`를 지정했다. 그러나 `web/src/proxy.ts`(Edge Middleware)는 `/api/v1/*` 전체를 OAuth 세션 검사에서 **제외**한다 — 그 경로는 폰 앱이 자기 자신의 Bearer `API_KEY`를 그대로 전달하는 용도로 예약되어 있다(`web/src/app/api/v1/ingest/*`가 이 패턴). Ask·Capture 신규 라우트를 문자 그대로 `api/v1/` 아래 두면 로그인하지 않은 브라우저도 호출할 수 있게 된다 — 서버가 자체 `API_KEY`로 Go 백엔드를 호출하는 한, 세션 검사가 없으면 인증 자체가 우회된다. 그래서 새 라우트는 `api/v1/` 프리픽스가 아니라 `web/src/app/api/ask/route.ts`, `web/src/app/api/notes/route.ts` 아래 둔다 — 이는 이미 이 계층에 존재하는 `web/src/app/api/search/route.ts`, `web/src/app/api/documents/route.ts`와 동일한 패턴(서버가 `API_KEY`를 직접 들고 있고, `proxy.ts`의 캐치올 매처가 세션을 요구)이다. 상세는 프론트엔드 구현 계획 참조.

---

## 13. 미결 사항

이전 초안에서 미결이었던 6개 항목(모델 override, enrichment 재시도 정책, 인사이트 검색 노출, 노트 삭제 시 파생 insight 처리, enrichment 호출 구조, Ask 컨텍스트 크기)은 모두 결정되어 해당 섹션에 반영되었다 — §5.2(K/M 기본값), §5.3(`LLM_ASK_MODEL`), §6.3(단일 호출 구조 + 재시도 정책), §6.4(암묵지 추출 범위), §6.5(insight 검색 노출·삭제 연쇄). 아래는 그 결정들을 반영한 뒤에도 남는, 구현 단계에서 확정해야 할 세부 사항이다.

### 13.1 Confidence 값의 스케일과 임계값

§6.4에서 모든 인사이트에 `confidence` 값을 요구하기로 했지만, 이 값이 0–1 사이의 연속값인지 저/중/고 같은 범주형인지, `/ask` 프롬프트에서 특정 임계값 미만의 인사이트를 아예 컨텍스트에서 제외할지는 정하지 않았다.

### 13.2 재시도 백오프의 정확한 간격

§6.3에서 3회 상한 + 지수 백오프로 정책은 확정했지만, 구체적인 대기 시간(예: 1분/5분/30분)은 예시일 뿐 확정값이 아니다. 실제 LLM 오류율과 워커 틱 주기를 보고 정한다.

### 13.3 `retry-enrichment` 엔드포인트의 남용 방지

수동 재시도 액션(§6.3, §7.3)에 별도의 속도 제한을 둘지, 아니면 기존 Bearer 인증만으로 충분하다고 볼지는 정하지 않았다.

---

## 14. 출시 계획

1. **Go 서버 먼저**: `internal/note` 패키지 추출(기존 `add_note` 동작 회귀 없음을 `cmd/mcp` 테스트로 확인) → `SourceNote`/`SourceInsight` 모델 추가 → `LLM_ASK_MODEL`/`ASK_CONTEXT_TOP_K`/`ASK_CONTEXT_INSIGHT_M` config 추가 → `POST /api/v1/notes` 구현 → `llm.Client.StreamWithMessages` 추가 → `POST /api/v1/ask` 구현 → 핸들러 테스트 통과.
2. **워커**: `NoteEnrichmentWorker` 구현(단일 LLM 호출 + 3회 재시도 정책 + 인사이트 3건 상한, §6.3–§6.4) → `POST /api/v1/notes/{id}/retry-enrichment` 구현 → `cmd/collector`에 등록 → `EntityLinker.UpsertAndLinkEntities` 재사용 검증.
3. **프롬프트 검증(실측 단계)**: 실제 노트 샘플로 enrichment 프롬프트의 출력 품질(정리 필드 정확도, 암묵지 후보가 §6.4 허용 범위를 벗어나지 않는지)과 노트당 비용·지연을 측정한다 — 결과에 따라 `LLM_ASK_MODEL` 프롬프트 템플릿을 조정한다. `ASK_CONTEXT_TOP_K`/`ASK_CONTEXT_INSIGHT_M`(기본 12/3)도 실제 프롬프트 토큰 사용량을 측정해 튜닝한다(§5.2). 이 단계 전까지는 §13의 세부 사항(백오프 간격, confidence 임계값)도 확정하지 않는다.
4. **프론트엔드**: `web/DESIGN.md` 벤더링 + Tailwind `@theme` 토큰 반영(§7.1) → 앱 셸(내비게이션) 갱신 → Capture 페이지 구현(데스크톱 우선, 실패 노트 재시도 UI 포함) → Ask 페이지 구현 → 모바일 레이아웃 추가 → route handler 프록시(SSE pass-through 포함) 구현. 상세 태스크 순서는 `docs/superpowers/plans/2026-08-17-ask-capture-frontend.md` 참조 — Ask 관련 태스크는 `POST /api/v1/ask` 백엔드가 별도 계획(LATER)이므로 맨 뒤로 순서가 밀린다.
5. **배포**: `docker-compose.local.yml`의 `web` 서비스 재기동 → `sb.baekenough.com` cloudflared ingress 추가 → `brain.baekenough.com` 무변경 확인 → Mac mini 배포·검증.
6. **후속**: 기존 5개 페이지에 §7.1 토큰 전면 적용(별도 작업), §13 잔여 세부 사항 순차 확정.
