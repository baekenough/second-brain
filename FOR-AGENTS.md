# FOR-AGENTS — second-brain MCP 사용 가이드

> 이 문서는 외부 AI 에이전트(Claude Code, Codex 등 다른 세션/도구)가  
> second-brain 을 MCP를 통해 검색 지식원으로 활용하는 방법을 안내합니다.

---

## 1. second-brain 이란

second-brain은 개인 지식 베이스 RAG 시스템입니다.

| 항목 | 내용 |
|------|------|
| 저장소 | PostgreSQL + pgvector (벡터 검색) + pg_bigm (한국어 N-gram) |
| 수집 대상 | secretary (Gmail·SMS·통화기록·통화녹취·캘린더), LLM 세션 메모리, 그 외 Slack·GitHub·Notion 등 |
| 검색 방식 | 하이브리드: BM25 전문 검색 + 벡터 유사도 + pg_bigm 한국어 N-gram, RRF(Reciprocal Rank Fusion) 병합 |
| MCP 트랜스포트 | Streamable HTTP (POST /mcp + GET /mcp/sse) |

**에이전트가 이 시스템을 사용하는 이유**

- 사용자의 과거 대화·메모(llm-memory)를 불러와 현재 작업 맥락을 보강할 때
- 사용자의 연락 기록, 통화 내용, SMS, 이메일 등 개인 데이터를 검색할 때
- 사용자가 "저번에 X 했던 것" 또는 "Y 관련 메모" 같은 기억 기반 질의를 할 때

---

## 2. 연결 방법

### 로컬 포트

| 서비스 | 호스트 포트 | 컨테이너 포트 | 용도 |
|--------|------------|--------------|------|
| mcp | **8090** | 8090 | MCP Streamable HTTP |
| server | 8081 | 8080 | REST API (에이전트 직접 사용 불필요) |

MCP 엔드포인트: `http://localhost:8090/mcp`

### .mcp.json 등록 예시

다른 Claude Code 세션의 `.mcp.json`에 아래와 같이 등록합니다.

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

- `type`: `http` (Streamable HTTP transport)
- `url`: MCP 서버 베이스 URL
- `protocolVersion`: `2024-11-05`
- 인증 헤더 불필요 (로컬 전용)

---

## 3. 제공 도구 (Tools)

### 3-1. `search` — 하이브리드 검색

**용도**: 전문 검색(BM25·pg_bigm)과 벡터 의미 검색을 동시에 수행하고, 관련도 점수 순으로 결과를 반환합니다.

#### 입력 인자

| 인자 | 타입 | 필수 | 기본값 | 설명 |
|------|------|------|--------|------|
| `query` | string | **필수** | — | 검색 질의 텍스트 |
| `limit` | number | 선택 | `10` | 최대 반환 건수 (1–50; 50 초과 시 50으로 고정) |
| `source` | string | 선택 | 없음 (전체) | 소스 타입 필터. 허용값: `slack` `github` `gdrive` `notion` `filesystem` `discord` `telegram` `secretary` `llm-memory` |

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

## 4. source_type 레퍼런스

| `source_type` 값 | 포함 데이터 |
|-----------------|------------|
| `secretary` | Gmail·SMS·통화기록·통화녹취(전사 텍스트)·캘린더 일정. 개인 연락 및 일정 전반. |
| `llm-memory` | LLM 세션 메모리. 이전 Claude/GPT 대화 중 저장된 메모, 작업 로그, 프로젝트 노트. |
| `slack` | Slack 메시지·채널 내용 |
| `github` | GitHub 이슈, PR, 코드 코멘트, 리포지터리 파일 |
| `gdrive` | Google Drive 문서 |
| `notion` | Notion 페이지·데이터베이스 |
| `filesystem` | 로컬 파일시스템 파일 (마크다운, 텍스트 등) |
| `discord` | Discord 메시지 |
| `telegram` | Telegram 메시지 |

---

## 5. 검색 활용 팁

### source 필터 사용 시점

| 상황 | 권장 source 필터 |
|------|----------------|
| "저번에 ChatGPT에서 정리했던 내용" | `llm-memory` |
| "상대방과 나눈 통화 내용 / 문자" | `secretary` |
| "특정 Slack 채널에서 논의한 내용" | `slack` |
| "내 노트북에 저장한 마크다운 메모" | `filesystem` |
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

---

## 6. 주의사항

### 읽기 전용

에이전트는 **검색(조회)만** 가능합니다. MCP 도구에 쓰기·수정·삭제 기능은 없습니다. 지식 베이스 내용 변경은 collector 서비스가 소스 시스템에서 자동 수집하는 방식으로만 이루어집니다.

### 민감 정보 취급

`secretary` 소스에는 **SMS 원문, 통화 녹취 전사, 개인 이메일** 등 고도로 민감한 개인정보가 포함됩니다.

- 검색 결과를 외부 시스템·API에 전달하지 마십시오.
- 로그에 본문(`content`)을 그대로 기록하지 마십시오.
- 필요한 맥락만 발췌해 사용하고, 원문을 불필요하게 노출하지 마십시오.
- 이 MCP 서버는 로컬 전용이며 인터넷에 노출되지 않아야 합니다.

### 서비스 가용성

MCP 서버는 Docker Compose로 실행됩니다. 서버가 내려가 있으면 연결이 거부됩니다.

```bash
# 상태 확인
docker compose -f docker-compose.local.yml ps

# 시작
docker compose -f docker-compose.local.yml up -d mcp
```
