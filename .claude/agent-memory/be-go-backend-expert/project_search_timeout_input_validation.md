---
name: search-timeout-input-validation
description: Issue #282 — GraphQL guard (alias/fragment-cycle crash) + search-path ctx timeout (SEARCH_REQUEST_TIMEOUT_SECONDS) + common input validation; pgx v5.9.2 default ALREADY cancels server-side on ctx done (verified on real DB), so no pool change needed
metadata:
  type: project
---

#282(v0.25.1): 검색 경로 타임아웃과 입력 검증. 공통 검증은 `search.ValidateInputText`/`ValidateQueryInput`(internal/search/query_input.go), 타임아웃은 핸들러 단위 `context.WithTimeout`(api `searchWithTimeout`, MCP `registerSearchTool` 인자). 서비스 `search()` 첫 줄에 내용(UTF-8/NUL)만 보는 방어선이 있고 길이는 안 본다 — /ask 질문 예산이 4KB 라서.

**pgx v5.9.2 기본 ctxwatch(DeadlineContextWatcherHandler)는 서버 측 문장도 취소한다.** 소켓 데드라인으로 호출자를 돌려보낸 뒤 `asyncClose()` 안에서 `CancelRequest` 를 보낸다(연결은 버림). 처음엔 "기본값은 서버에서 계속 돈다"고 가정해 풀에 CancelRequestContextWatcherHandler 를 넣었는데, 실DB 음성 대조군이 그 가정을 반박해 되돌렸다. 고정 테스트: internal/store `TestPool_ContextTimeoutCancelsServerSide`(탐지 쿼리 양성 대조군 포함).

**Why:** 사용자는 DB/풀 전역 statement_timeout·풀 설정 변경을 원치 않는다(마이그레이션·백필·collector 가 같은 풀). 기본 동작으로 충분하다는 증거가 있으니 풀을 건드릴 이유가 없다.

**How to apply:**
- 타임아웃 판정은 오류 체인이 아니라 타임아웃 ctx 의 `ctx.Err()` 로 한다 — 레인 오류가 context 오류를 잃을 수 있다(테스트에 unwrapped_57014 케이스).
- Go `encoding/json` 은 문자열 속 잘못된 UTF-8 을 U+FFFD 로 조용히 바꾼다. POST 본문 UTF-8 거부는 디코딩 전 `utf8.Valid(body)` 로만 가능(`decodeBoundedJSON`). GET 쿼리스트링(%FF/%00)은 날 바이트가 그대로 들어오는 실제 경로.
- 기본 60초는 #195 콜드 스타트(>30초) 때문; 워밍업이 들어가면 낮출 것.
- 결함 주입 시 핸들러 검증만 지우면 NUL 케이스는 서비스 방어선이 막아 통과한다(길이 케이스만 FAIL) — 두 층을 다 지워야 NUL 테스트가 FAIL 한다.

**보안 리뷰 후속(같은 날):** GraphQL 은 `guardGraphQL`(internal/api/graphql_limits.go)이 graphql-go 앞에서 요청을 직접 풀어(`gqlhandler.NewRequestOptions`, 쿼리스트링 우선 — 본문만 감싸면 `?query=` 로 우회됨) AST 를 검사한다: search 필드 ≤5(별칭·fragment 전개), curated ≤1, RawQuery·본문 64KB, 요청 전체 ctx 타임아웃. **graphql-go v0.8.1 검증기는 순환 fragment 에서 stack overflow(fatal, recover 불가)로 프로세스를 죽인다** — 가드가 필드 하위까지 순환을 먼저 거부한다. 요청 ctx 가 끝나면 graphql-go 는 리졸버 오류 대신 "context deadline exceeded" 를 낸다. MCP 본문 상한은 3×note.MaxContentBytes(10MB)+1MB — add_note 가 가장 큰 정상 요청이고 hermes(Python ensure_ascii)가 비 ASCII 를 이스케이프한다.

관련: [[store-sql-test-harness]], [[briefing-timeout-writedeadline]]
