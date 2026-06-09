# FOR-AGENTS — second-brain MCP 사용 가이드

> 이 문서는 외부 AI 에이전트(Claude Code, Codex 등 다른 세션/도구)가  
> second-brain 을 MCP를 통해 지식원으로 활용하는 방법을 안내합니다.  
> v0.17.0 기준 — Bearer 인증, 원격 터널 연결, 쓰기 도구 포함.

---

## 1. second-brain 이란

second-brain은 개인 지식 베이스 RAG 시스템입니다.

| 항목 | 내용 |
|------|------|
| 저장소 | PostgreSQL + pgvector (벡터 검색) + pg_bigm (한국어 N-gram) |
| 수집 대상 | Gmail·SMS·통화기록·통화녹취·캘린더 (직접 수집), LLM 세션 메모리, 에이전트 업로드, 그 외 Slack·GitHub·Notion 등 |
| 검색 방식 | 하이브리드: BM25 전문 검색 + 벡터 유사도 + pg_bigm 한국어 N-gram, RRF(Reciprocal Rank Fusion) 병합 |
| MCP 트랜스포트 | Streamable HTTP (POST /mcp + GET /mcp/sse) |

**에이전트가 이 시스템을 사용하는 이유**

- 사용자의 과거 대화·메모(llm-memory)를 불러와 현재 작업 맥락을 보강할 때
- 사용자의 연락 기록, 통화 내용, SMS, 이메일 등 개인 데이터를 검색할 때
- 사용자가 "저번에 X 했던 것" 또는 "Y 관련 메모" 같은 기억 기반 질의를 할 때

---

## 2. 연결 방법

### 포트 및 엔드포인트

| 서비스 | 호스트 포트 | 컨테이너 포트 | 용도 |
|--------|------------|--------------|------|
| mcp | **8090** | 8090 | MCP Streamable HTTP |
| server | 8081 | 8080 | REST API (파일 인제스트 등) |

로컬 MCP 엔드포인트: `http://localhost:8090/mcp`

### 인증

v0.17.0부터 서버에 `API_KEY` 환경변수가 설정되어 있으면 **모든 MCP 도구**(`search`, `get_document`, `stats`, `add_note`)가 Bearer 토큰을 요구합니다.

| 환경 | 인증 요구 여부 |
|------|--------------|
| `API_KEY` 미설정 (개발용 로컬) | 인증 없이 접근 가능 (dev bypass) |
| `API_KEY` 설정 (로컬 또는 원격) | 모든 도구에 Bearer 토큰 필수 |
| 공개 터널 경유 원격 접근 | Bearer 토큰 + TLS 필수 |

v0.17.0부터 Cloudflare Tunnel 또는 역방향 프록시를 통해 공개 TLS URL로 MCP를 원격 노출할 수 있습니다. 이 경우 `API_KEY` 설정은 선택이 아닌 **필수**입니다.

### .mcp.json 등록 예시

다른 Claude Code 세션의 `.mcp.json`에 아래와 같이 등록합니다.

**로컬 (API_KEY 미설정, 개발 전용)**

```json
{
  "mcpServers": {
    "second-brain": {
      "type": "http",
      "url": "http://localhost:8090/mcp"
    }
  }
}
```

**로컬 또는 원격 (API_KEY 설정 시)**

```json
{
  "mcpServers": {
    "second-brain": {
      "type": "http",
      "url": "https://<your-tunnel-host>/mcp",
      "headers": { "Authorization": "Bearer <API_KEY>" }
    }
  }
}
```

로컬에서 API_KEY를 설정한 경우에도 `url`만 `http://localhost:8090/mcp`로 바꾸고 `headers`는 동일하게 포함해야 합니다.

- `type`: `http` (Streamable HTTP transport)
- `url`: MCP 서버 베이스 URL (로컬 또는 터널 URL)
- `protocolVersion`: `2024-11-05`

---

## 3. 제공 도구 (Tools)

> MCP는 읽기 도구(`search` / `get_document` / `stats`)와 쓰기 도구(`add_note`)를 제공합니다.  
> 모든 도구는 `API_KEY` 설정 시 Bearer 인증이 적용됩니다.

### 3-1. `search` — 하이브리드 검색

**용도**: 전문 검색(BM25·pg_bigm)과 벡터 의미 검색을 동시에 수행하고, 관련도 점수 순으로 결과를 반환합니다.

#### 입력 인자

| 인자 | 타입 | 필수 | 기본값 | 설명 |
|------|------|------|--------|------|
| `query` | string | **필수** | — | 검색 질의 텍스트 |
| `limit` | number | 선택 | `10` | 최대 반환 건수 (1–50; 50 초과 시 50으로 고정) |
| `source` | string | 선택 | 없음 (전체) | 소스 타입 필터. 허용값: `slack` `github` `gdrive` `notion` `filesystem` `discord` `telegram` `secretary` `llm-memory` `gmail` `calendar` `sms` `call-log` `call-transcript` `upload` |

#### 출력 구조

```json
{
  "query": "검색어",
  "count": 3,
  "results": [
    {
      "document_id": "uuid-string",
      "title": "문서 제목",
      "source_type": "secretary",
      "score": 0.87,
      "match_type": "hybrid",
      "snippet": "본문 앞 최대 500자(rune 기준)"
    }
  ]
}
```

- `match_type` 가능한 값: `"fulltext"` | `"vector"` | `"hybrid"` | `"chunk-fts"`
- `snippet`은 `content` 앞 500 rune 으로 잘림. 전문이 필요하면 `get_document` 사용.

#### 호출 예시

```json
{
  "tool": "search",
  "arguments": {
    "query": "지난주 스탠드업 회의 내용",
    "limit": 5,
    "source": "llm-memory"
  }
}
```

#### 응답 예시

```json
{
  "query": "지난주 스탠드업 회의 내용",
  "count": 2,
  "results": [
    {
      "document_id": "a1b2c3d4-0000-0000-0000-000000000001",
      "title": "standup 2026-05-21",
      "source_type": "llm-memory",
      "score": 0.92,
      "match_type": "hybrid",
      "snippet": "어제 완료: auth 모듈 리팩토링. 오늘 예정: PR 리뷰..."
    }
  ]
}
```

---

### 3-2. `get_document` — 단일 문서 전문 조회

**용도**: UUID로 특정 문서의 전체 내용(본문·메타데이터)을 가져옵니다. `search` 결과의 `document_id`를 이 도구에 전달하면 됩니다.

#### 입력 인자

| 인자 | 타입 | 필수 | 기본값 | 설명 |
|------|------|------|--------|------|
| `id` | string | **필수** | — | 문서 UUID (예: `123e4567-e89b-12d3-a456-426614174000`) |

#### 출력 구조

```json
{
  "id": "uuid-string",
  "source_type": "secretary",
  "source_id": "원본 시스템 내 ID",
  "title": "문서 제목",
  "content": "전체 본문",
  "metadata": { "임의 키": "임의 값" },
  "status": "active",
  "collected_at": "2026-05-20T09:00:00Z"
}
```

- `status` 가능한 값: `"active"` | `"deleted"` | `"moved"`
- `embedding` 필드는 MCP 응답에 포함되지 않음(크기가 크고 LLM 호출자에게 불필요)
- 문서를 찾을 수 없거나 UUID가 잘못된 경우 오류 텍스트 반환

#### 호출 예시

```json
{
  "tool": "get_document",
  "arguments": {
    "id": "a1b2c3d4-0000-0000-0000-000000000001"
  }
}
```

---

### 3-3. `stats` — 소스별 통계

**용도**: 지식 베이스에 수집된 문서/청크 수를 소스별로 확인합니다. 어떤 소스에 얼마나 많은 데이터가 있는지 파악할 때 사용합니다.

#### 입력 인자

없음 (인자 불필요)

#### 출력 구조

```json
{
  "total_documents": 12483,
  "documents_by_source": {
    "secretary": 10200,
    "llm-memory": 1500,
    "filesystem": 783
  },
  "chunks": {
    "total": 58920,
    "avg_chunks_per_document": 4.7,
    "avg_chunk_size_bytes": 1024
  }
}
```

> **참고**: 청크 통계 쿼리가 실패하면 `documents_by_source` 만 포함된 축약 응답이 반환됩니다.

#### 호출 예시

```json
{
  "tool": "stats",
  "arguments": {}
}
```

---

### 3-4. `add_note` — 노트 쓰기 (쓰기 도구)

**용도**: 에이전트 또는 사용자가 노트·메모리 텍스트를 지식 베이스에 직접 추가합니다. 저장 시 `source_type=llm-memory`로 분류되며, 청크로 분할 후 FTS·벡터 인덱스에 등록됩니다. 동일한 `source_id`로 재호출하면 기존 노트를 덮어씁니다(upsert).

**인증**: 읽기 도구와 동일하게 `API_KEY` 설정 시 Bearer 토큰 필수, 미설정 시 dev bypass가 적용됩니다.

#### 입력 인자

| 인자 | 타입 | 필수 | 기본값 | 설명 |
|------|------|------|--------|------|
| `title` | string | **필수** | — | 노트 제목 (공백만인 문자열 불가, 최대 1 KiB) |
| `content` | string | **필수** | — | 노트 전문 텍스트 (최대 10 MiB) |
| `source_id` | string | 선택 | 랜덤 UUID | 노트 고유 식별자. 동일 값으로 재호출 시 기존 노트 갱신(upsert). |
| `metadata` | object | 선택 | `null` | 임의 키-값 쌍 JSON 객체. 노트에 태그·출처 등 부가 정보 첨부 시 사용. |
| `embed` | boolean | 선택 | `true` | 청크에 임베딩 벡터를 생성할지 여부. 임베딩 API 미사용 시 `false` 설정. |

#### 출력 구조

```json
{
  "id": "uuid-string",
  "chunks_created": 4,
  "embedding_created": true
}
```

- `id`: 저장된 문서 UUID (`get_document`에 바로 사용 가능)
- `chunks_created`: 생성된 청크 수
- `embedding_created`: 벡터 임베딩 성공 여부 (`false`여도 FTS 검색은 가능)

> **청크 수 제한**: 단일 노트가 2,000개를 초과하는 청크를 생성하면 임베딩이 생략됩니다(`embedding_created: false`). 노트는 정상 저장되고 전문 검색(BM25·pg_bigm)은 정상 동작합니다.

#### 호출 예시

```json
{
  "tool": "add_note",
  "arguments": {
    "title": "v0.17.0 배포 완료 메모",
    "content": "2026-06-09 MCP Bearer 인증 및 원격 터널 기능 배포 완료. 관련 이슈 #88 close.",
    "source_id": "release-memo-v0.17.0",
    "metadata": { "version": "0.17.0", "tags": "release,mcp" }
  }
}
```

---

### 3-5. `POST /api/v1/ingest/file` — 파일 업로드 인제스트 (HTTP 엔드포인트)

이 경로는 MCP 도구가 아닌 REST API 엔드포인트입니다. 문서 파일을 업로드하면 텍스트를 추출해 `source_type=upload`로 저장합니다. 동일 파일을 재업로드하면 SHA-256 기반 `source_id`로 idempotent하게 upsert됩니다.

- **URL**: `POST /api/v1/ingest/file` (server 포트 8081 경유, 또는 터널 경유 시 공개 URL)
- **인증**: `Authorization: Bearer <API_KEY>` 헤더 필수
- **Content-Type**: `multipart/form-data`
- **파일 크기 제한**: 기본 100 MiB (`INGEST_MAX_FILE_BYTES` 환경변수로 조정 가능; `0` 설정 시 무제한)

#### 폼 필드

| 필드 | 필수 | 설명 |
|------|------|------|
| `file` | **필수** | 업로드할 파일 (아래 지원 형식 참고) |
| `title` | 선택 | 문서 표시 제목. 생략 시 원본 파일명 사용. |
| `source` | 선택 | 출처 레이블 (메타데이터에 저장됨) |
| `tags` | 선택 | 쉼표 구분 태그 문자열 (메타데이터에 저장됨) |

**지원 파일 형식**: `.pdf`, `.docx`, `.xlsx`, `.pptx`, `.hwpx`, `.html`, `.htm`, `.txt`, `.md`, `.text`

#### 응답

```json
{
  "document_id": "uuid-string",
  "accepted": true
}
```

#### curl 예시

```bash
curl -X POST https://<your-tunnel-host>/api/v1/ingest/file \
  -H "Authorization: Bearer <API_KEY>" \
  -F "file=@report.pdf" \
  -F "title=2026 Q2 보고서" \
  -F "tags=report,q2"
```

---

## 4. source_type 레퍼런스

`search` 도구의 `source` 필터에 아래 값을 사용합니다 (v0.17.0 기준 전체 목록).

| `source_type` 값 | 포함 데이터 | 수집 방식 |
|-----------------|------------|---------|
| `gmail` | Gmail 메일 원문 | 직접 수집 (2026-05-30 이후) |
| `calendar` | 캘린더 일정 | 직접 수집 (2026-05-30 이후) |
| `sms` | SMS 메시지 | 직접 수집 (2026-05-30 이후) |
| `call-log` | 통화기록 (발신·수신 목록) | 직접 수집 (2026-05-30 이후) |
| `call-transcript` | 통화 녹취 전사 텍스트 | 직접 수집 (2026-05-30 이후) |
| `secretary` | Gmail·SMS·통화기록·통화녹취·캘린더 **통합 아카이브** (2026-05-30 이전 레거시 데이터) | 레거시 (이관 완료 후 점진 축소) |
| `llm-memory` | LLM 세션 메모리. 이전 Claude/GPT 대화 중 저장된 메모, 작업 로그, 프로젝트 노트. `add_note`로 추가된 노트 포함. | MCP 쓰기 / 자동 수집 |
| `upload` | 에이전트·사용자가 `POST /api/v1/ingest/file`로 업로드한 파일 | HTTP 업로드 |
| `slack` | Slack 메시지·채널 내용 | 커넥터 |
| `github` | GitHub 이슈, PR, 코드 코멘트, 리포지터리 파일 | 커넥터 |
| `gdrive` | Google Drive 문서 | 커넥터 |
| `notion` | Notion 페이지·데이터베이스 | 커넥터 |
| `filesystem` | 로컬 파일시스템 파일 (마크다운, 텍스트 등) | 커넥터 |
| `discord` | Discord 메시지 | 커넥터 |
| `telegram` | Telegram 메시지 | 커넥터 |

> **레거시 `secretary` 소스**: 2026-05-30 이전 수집된 Gmail·SMS·통화·캘린더 데이터는 `secretary` 단일 소스로 저장되어 있습니다. 커트오버 이후 신규 데이터는 `gmail`·`sms`·`call-log`·`call-transcript`·`calendar`로 분리 저장됩니다. 두 소스를 함께 검색하려면 `source` 필터를 생략하고 전체 검색을 사용하십시오.

---

## 5. 검색 활용 팁

### source 필터 사용 시점

| 상황 | 권장 source 필터 |
|------|----------------|
| "저번에 ChatGPT에서 정리했던 내용" | `llm-memory` |
| "최근 문자 / 통화 내용 (2026-05-30 이후)" | `sms` / `call-log` / `call-transcript` |
| "최근 이메일 (2026-05-30 이후)" | `gmail` |
| "일정·스케줄 정보" | `calendar` |
| "2026-05-30 이전 통화·문자·이메일·일정" | `secretary` (레거시) |
| "특정 Slack 채널에서 논의한 내용" | `slack` |
| "내 노트북에 저장한 마크다운 메모" | `filesystem` |
| "에이전트가 업로드한 파일" | `upload` |
| 출처를 모르거나 여러 소스를 동시에 | 필터 생략 (전체 검색) |

### 질의 유형별 전략

| 질의 유형 | 전략 |
|----------|------|
| 키워드형 ("OAuth 에러", "배포 실패") | 짧고 구체적인 단어를 그대로 사용. BM25·pg_bigm이 효과적. |
| 의미형 ("그때 결정한 아키텍처 방향") | 자연어 문장 그대로 전달. 벡터 검색이 의미 근접도를 계산. |
| 한국어 혼합 질의 | 한·영 혼합 그대로 사용. pg_bigm이 한국어 N-gram을 처리. |

### limit 권장값

| 용도 | 권장 limit |
|------|-----------|
| 단순 확인 (존재 여부 파악) | `3` |
| 일반 참조 (요약·맥락 보강) | `5`–`10` (기본값) |
| 종합 탐색 (여러 문서 비교) | `20`–`30` |
| 전수 조사 | `50` (최대) |

### search → get_document 2단계 흐름

1. `search`로 관련 문서 목록과 snippet 확인
2. snippet만으로 부족하면 `document_id`를 `get_document`에 전달해 전문 조회
3. `stats`로 특정 소스에 데이터가 실제로 존재하는지 먼저 확인 후 검색하면 효율적

```
stats()                          → 소스별 데이터 규모 파악
  ↓
search(query, source=...)        → 관련 문서 목록 + snippet
  ↓
get_document(document_id)        → 전문 + 메타데이터
```

### 노트·파일 추가 흐름

에이전트가 지식 베이스에 새 정보를 기록해야 할 때는 두 가지 경로를 사용합니다.

```
[짧은 텍스트 노트]
add_note(title, content, source_id?, metadata?)
  → 저장 완료 → id 반환 → 즉시 search로 조회 가능

[파일 문서]
POST /api/v1/ingest/file  (multipart: file, title?, source?, tags?)
  → 텍스트 추출 → source_type=upload 로 저장 → document_id 반환
```

---

## 6. 주의사항

### 읽기/쓰기 범위

MCP 읽기 도구(`search` / `get_document` / `stats`)는 지식 베이스를 변경하지 않습니다.  
**쓰기 경로는 두 가지**가 존재합니다:

- **`add_note`** (MCP 도구): 텍스트 노트를 `source_type=llm-memory`로 추가합니다.
- **`POST /api/v1/ingest/file`** (HTTP 엔드포인트): 파일을 업로드해 `source_type=upload`로 추가합니다.

두 쓰기 경로 모두 `API_KEY` 설정 여부와 관계없이 Bearer 인증이 항상 적용됩니다.  
삭제·수정 API는 에이전트에 노출되지 않습니다.

### 민감 정보 취급

`secretary`·`sms`·`call-log`·`call-transcript`·`gmail`·`calendar` 소스에는 **SMS 원문, 통화 녹취 전사, 개인 이메일, 일정** 등 고도로 민감한 개인정보가 포함됩니다.

- 검색 결과를 외부 시스템·API에 전달하지 마십시오.
- 로그에 본문(`content`)을 그대로 기록하지 마십시오.
- 필요한 맥락만 발췌해 사용하고, 원문을 불필요하게 노출하지 마십시오.
- **공개 노출 시 반드시 `API_KEY`(Bearer 토큰)로 보호하고 TLS 터널을 사용하십시오. 무인증 공개는 절대 금지입니다.**

### 보안 체크리스트 (원격 노출 시)

- [ ] `API_KEY` 환경변수 설정
- [ ] TLS 터널(Cloudflare Tunnel 등) 또는 HTTPS 역방향 프록시 적용
- [ ] `.mcp.json`의 `Authorization` 헤더에 올바른 Bearer 토큰 입력
- [ ] 무인증 로컬 개발 서버를 공개 인터넷에 직접 노출하지 않을 것

### 서비스 가용성

MCP 서버는 Docker Compose로 실행됩니다. 서버가 내려가 있으면 연결이 거부됩니다.

```bash
# 상태 확인
docker compose -f docker-compose.local.yml ps

# 시작
docker compose -f docker-compose.local.yml up -d mcp
```
