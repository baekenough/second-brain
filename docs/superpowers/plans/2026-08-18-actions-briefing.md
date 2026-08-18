# Part C — 액션·브리핑 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 이미 쌓이고 있는 `actions` 테이블을 사용자가 소비할 수 있게 만든다 — 열린 액션 조회 API, 멱등 상태 변경 API, 근거가 강제된 온디맨드 브리핑, 그리고 이 셋을 쓰는 화면.

**Architecture:** Go 백엔드에 읽기 전용 액션 조회와 멱등 상태 변경 엔드포인트를 추가하고, 그 위에 "열린 액션 집합 해시"를 캐시 키로 쓰는 온디맨드 브리핑 생성기를 얹는다. 브리핑은 원격 DeepSeek에 문장 단위 구조화 JSON을 요구하고, 서버가 문장별 근거 document id를 화이트리스트로 검증해 통과 못 한 문장을 폐기한다. 프런트는 Next.js BFF 라우트로 백엔드를 프록시하고 `/actions` 화면 하나를 추가한다.

**Tech Stack:** Go + chi + pgx/pgxpool + PostgreSQL, 원격 DeepSeek(`llm.Completer`, thinking disabled), Next.js 16 + React 19 + bun, Cloudflare Access(JWT는 `web/src/proxy.ts`가 이미 처리).

**Spec:** `docs/superpowers/specs/2026-08-17-knowledge-graph-actions-feedback-design.md` — Part C는 §7(§7.1 구조 신호, §7.2 병합, §7.3 브리핑, §7.4 실패 모드), 스키마 §5.4, `identity_key` 불변식 §5.5, 프라이버시 §11.

---

## Global Constraints

이 절의 제약은 모든 Task에 암묵적으로 포함된다.

- **스키마 변경 금지.** `migrations/022_actions.sql`은 이미 프로덕션에 적용되어 있고 데이터(액션 약 450건, 증가 중)가 쌓이는 중이다. Part C는 신규 마이그레이션을 만들지 않는다. 컬럼이 부족해 보이면 조회 시점 계산으로 해결하고, 그래도 안 되면 멈추고 보고한다.
- **`identity_key` 불변식.** `action_status.identity_key`가 `actions.identity_key`를 참조하고, 재추출 후에도 사용자의 `done`/`ignored` 판단이 살아남아야 한다(§5.5). 상태를 `actions.id`로 참조하는 코드를 절대 쓰지 않는다. `identity_key`를 재계산·정규화·재생성하는 코드도 쓰지 않는다 — Part C는 이 값을 **읽고 그대로 되돌려주기만** 한다.
- **닫힌 어휘.** `kind` 4종(`awaiting_my_reply`는 구조적으로만 산출 / `my_commitment` / `their_commitment` / `scheduled`), `detected_by` 3종(`structural`/`llm`/`both`), `state` 3종(`open`/`done`/`ignored`). 값은 `internal/model/action.go` 상수를 쓴다 — 새 문자열 리터럴을 만들지 않는다. `detected_by`와 `confidence`는 화면에 그대로 노출한다(§7.4의 LLM 환각 방어책).
- **LLM은 원격 API만.** 로컬 추론(ollama 등) 기동 금지. 모델은 DeepSeek이고 **thinking 비활성화**다 — `internal/llm/client.go`의 `ThinkingDisabled`가 이미 기본값이므로 브리핑 경로에서 이를 뒤집지 않는다(추론 토큰이 예산을 태우는 문제가 실측 확인됨).
- **패키지 매니저는 bun.** `web/`에서 `npm`을 쓰면 `package-lock.json`이 생겨 CI(`bun --frozen-lockfile`)가 깨진다. `bun install` / `bun run build` / `bun run lint`.
- **인증은 이미 있다.** 웹의 Cloudflare Access JWT 검증은 `web/src/proxy.ts`가 처리한다. 새 인증 코드를 만들지 않는다. Go의 `/api/v1/*`는 기존 Bearer `apiKey` 미들웨어를 그대로 탄다.
- **프로덕션 = macmini `docker compose -f docker-compose.local.yml`, `--env-file .env.local` 필수.** k8s가 아니다. `--env-file`을 빼면 볼륨이 기본값 경로로 떨어져 컨테이너가 죽는다.
- **동시 작업 중인 파일 금지**: `internal/graph/**`, `internal/worker/graph_projection_worker*.go`, `internal/store/graph_source*.go`, `internal/model/projection.go`, `cmd/collector/main.go`, `internal/llm/client.go`. `internal/config/config.go`는 Task 12에서만, 구조체 끝에 필드를 추가하는 방식으로만 만진다.
- **개인정보**: 액션 요약과 상대방 이름에 실명·전화번호·이메일이 들어간다. Task 8의 로깅 규약이 **모든 Task에 소급 적용**된다.
- **기능 플래그**: 모든 신규 엔드포인트는 플래그로 끌 수 있어야 한다(롤백 절).
- **커밋**: Task마다 커밋. `git add -A` / `git add .` 금지 — 파일을 명시적으로 나열한다.

---

## 확정 판단 (설계 §13 미결 사항 중 Part C 해당분)

| 항목 | 확정 | 근거 |
|---|---|---|
| 90일 자동 보관 방식 | **조회 쿼리 필터링만.** `action_status.state`에 새 값을 추가하지 않는다. 기본 조건에 `a.observed_at > now() - interval '90 days'`를 넣고 `include_archived=true`로 해제 가능하게 한다. | §7.4가 이미 그렇게 확정. 새 state 도입은 기존 행의 의미를 바꿔 데이터 마이그레이션을 유발하며 "스키마 변경 금지"와 충돌한다. |
| `confidence` 스케일 | **0–1 연속값.** | 마이그레이션이 `NUMERIC(3,2) CHECK (0..1)`로 이미 못박음. 범주형 해석은 불가능. |
| 신뢰도 하한 기본값 | **없음(0.0 = 미적용).** `min_confidence` 파라미터로 지정, 화면은 0.0에서 시작. | 프로덕션 confidence 분포를 아직 모른다. 임의 하한(예: 0.7)을 기본값으로 박으면 구조 신호(기본 1.00)만 남고 LLM 액션이 통째로 사라지는 무성한 실패가 가능하다. 관측 후 조정한다. |
| 브리핑 캐시 키 | §7.3의 `hash(열린 identity_key 집합 + 최신 문서 시각)`에 **프롬프트 버전 상수와 모델 이름을 추가**한다. | §7.3 그대로면 프롬프트를 고쳐 배포해도 캐시가 옛 문장을 반환한다. 프롬프트도 입력이므로 확장이지 위반이 아니다. **설계 대비 달라진 판단 — 보고 대상.** |
| "최신 문서 occurred_at"의 구현 | 조회된 액션들의 `observed_at` 최대값으로 대체한다. | 별도 쿼리가 필요 없고, 액션을 만들지 않는 문서만 유입된 경우 브리핑 내용이 바뀔 이유가 없으므로 캐시 유지가 오히려 옳다. **설계 대비 달라진 판단 — 보고 대상.** |

---

## File Structure

**신규 (Go)**

| 파일 | 책임 |
|---|---|
| `internal/store/actions_query.go` (+`_test.go`) | `ListOpenActions`, `SetActionState`. 기존 `internal/store/actions.go`(쓰기 전용, Part A 소유)와 분리해 워커 작업과의 충돌을 피한다. |
| `internal/api/actions.go` (+`_test.go`) | `GET /api/v1/actions`, `POST /api/v1/actions/{identity_key}/status`. |
| `internal/briefing/input.go`, `cache.go`, `prompt.go`, `validate.go`, `generate.go`, `redact.go`, `doc.go` (+각 `_test.go`) | 브리핑 순수 로직. HTTP·DB 비의존. |
| `internal/api/briefing.go` (+`_test.go`) | `GET /api/v1/briefing` — 위 조각을 조립. |

**신규 (Web)**

| 파일 | 책임 |
|---|---|
| `web/src/app/api/actions/route.ts` | BFF 프록시 — 목록 조회. |
| `web/src/app/api/actions/[key]/status/route.ts` | BFF 프록시 — 상태 변경. |
| `web/src/app/api/briefing/route.ts` | BFF 프록시 — 브리핑. |
| `web/src/app/actions/page.tsx` | `/actions` 화면 셸(`"use client"`, `/ask` 페이지와 같은 관례). |
| `web/src/app/actions/ActionList.tsx` | 목록 + 완료/무시 조작. |
| `web/src/app/actions/BriefingPanel.tsx` | 브리핑 문장 + 근거 링크. |

**수정**: `internal/api/router.go`(필드·옵션·라우트), `cmd/server/main.go`(배선), `internal/config/config.go`(플래그 3개), `web/src/lib/types.ts`, `web/src/lib/api.ts`, `web/src/app/dashboard/page.tsx`(진입 링크).

---

## Task 1: 액션 조회 스토어

**Files:** Create `internal/store/actions_query.go`, `internal/store/actions_query_test.go`

**Interfaces**
- Consumes: `model.Action` / `model.ActionKind` / `model.ActionState`(`internal/model/action.go`), `*store.Postgres`(기존 `NewActionStore` 생성자 관례).
- Produces:
  - `type ActionFilter struct { Kinds []model.ActionKind; Counterpart string; DueBefore *time.Time; MinConfidence float64; IncludeArchived bool; Sort string; Limit int }`
  - `type ActionListItem struct { model.Action; State model.ActionState; CounterpartName string }`
  - `func NewActionQueryStore(pg *Postgres) *ActionQueryStore`
  - `func (s *ActionQueryStore) ListOpenActions(ctx context.Context, f ActionFilter) ([]ActionListItem, error)`

**설계 메모**
- `action_status` 행이 없는 액션이 존재할 수 있다. **LEFT JOIN + `COALESCE(st.state,'open')`**으로 읽는다. INNER JOIN이면 상태 행 없는 액션이 통째로 사라진다.
- 기본 동작은 `done`/`ignored` 제외 → `COALESCE(st.state,'open') = 'open'`.
- `counterpart_entity_id`는 nullable. `entities`를 LEFT JOIN해 표시용 이름을 얻는다. 이름 컬럼은 `internal/store/entities.go`에서 실제 컬럼명을 확인해 맞춘다.
- 정렬은 **화이트리스트 매핑을 통과한 상수만** SQL에 이어붙인다. 사용자 입력을 `ORDER BY`에 넣지 않는다. `"due"` → `a.due_at ASC NULLS LAST, a.observed_at DESC`; `"confidence"` → `a.confidence DESC, a.observed_at DESC`; 그 외/빈 값 → `"due"` 폴백.
- 응답 크기 상한은 **스토어에서** 클램프한다(핸들러만 믿지 않는다): `Limit<=0`이면 50, `>200`이면 200.

쿼리 뼈대:

```sql
SELECT a.identity_key, a.document_id, a.thread_key, a.kind, a.summary,
       a.counterpart_entity_id, a.due_at, a.detected_by, a.confidence, a.observed_at,
       COALESCE(st.state,'open') AS state, COALESCE(e.normalized_name,'') AS counterpart_name
FROM actions a
LEFT JOIN action_status st ON st.identity_key = a.identity_key
LEFT JOIN entities e       ON e.id = a.counterpart_entity_id
WHERE COALESCE(st.state,'open') = 'open'
  AND ($1::boolean OR a.observed_at > now() - interval '90 days')
  AND ($2::text[] IS NULL OR a.kind = ANY($2::text[]))
  AND ($3::text = '' OR COALESCE(e.normalized_name,'') ILIKE '%' || $3 || '%')
  AND ($4::timestamptz IS NULL OR (a.due_at IS NOT NULL AND a.due_at <= $4))
  AND a.confidence >= $5::numeric
```

- [ ] **Step 1: 실패하는 테스트를 쓴다.** 이 프로젝트는 SQL을 스텁으로 검증할 수 없다는 것을 이미 비싸게 배웠다(`RETURNING`에서 `EXCLUDED` 참조 불가 사건). **실제 DB에 임시 행을 넣고 정리하는 방식**을 쓴다 — `internal/store/document_rrf_score_test.go`가 쓰는 테스트 DB 헬퍼를 그대로 재사용하고, `TEST_DATABASE_URL` 미설정 시 `t.Skip`한다. 시드 데이터의 요약·상대방 이름은 전부 `"dummy summary"` 같은 가공 더미로 쓴다. 검증할 것 3가지:

```go
// (1) done/ignored 제외: identity_key 3건(open/done/ignored)을 심고 open 1건만 나오는지
// (2) 상태 행 없는 액션이 살아남고 State == model.StateOpen 인지
// (3) Limit 클램프: 100000을 넘겨도 반환 행이 200 이하인지
got, err := NewActionQueryStore(pg).ListOpenActions(ctx, ActionFilter{})
if len(keysWithTestPrefix(got)) != 1 { t.Fatalf("want only the open action, got %v", got) }
```

각 테스트는 `t.Cleanup`에서 `DELETE FROM actions WHERE identity_key LIKE 'action:test-%'`로 정리한다(`action_status`는 FK CASCADE로 따라 지워진다).

- [ ] **Step 2: 실패 확인.** Run `go test ./internal/store/ -run TestListOpenActions -v` → FAIL `undefined: NewActionQueryStore`
- [ ] **Step 3: 구현.** 위 SQL + `pgx` `Query`/`Scan`. `counterpart_entity_id`와 `due_at`은 포인터로 스캔.
- [ ] **Step 4: 통과 확인.** Run `go test ./internal/store/ -run TestListOpenActions -v` → PASS (DB 미설정이면 SKIP되므로 CI에서 재확인)
- [ ] **Step 5: 커밋.** `git add internal/store/actions_query.go internal/store/actions_query_test.go && git commit -m "feat(actions): add ListOpenActions query store"`

---

## Task 2: 액션 조회 API

**Files:** Create `internal/api/actions.go`, `internal/api/actions_test.go`; Modify `internal/api/router.go`

**Interfaces**
- Consumes: Task 1의 `store.ActionFilter`, `store.ActionListItem`.
- Produces:
  - `type ActionLister interface { ListOpenActions(ctx context.Context, f store.ActionFilter) ([]store.ActionListItem, error) }`
  - `type ActionStateSetter interface { SetActionState(ctx context.Context, identityKey string, state model.ActionState, note string) (bool, error) }` (Task 3/4에서 구현체가 붙는다. Task 2에서는 `nil`을 넘겨도 컴파일된다.)
  - `func (s *Server) WithActions(lister ActionLister, setter ActionStateSetter) *Server`
  - `func (s *Server) listActionsHandler(w http.ResponseWriter, r *http.Request)`
  - 응답: `{"actions":[{"identity_key","document_id","thread_key","kind","summary","counterpart_name","due_at","detected_by","confidence","observed_at","state"}],"count":N,"truncated":bool}`

**설계 메모**
- 쿼리 파라미터: `kind`(반복 가능, `model.IsValidActionKind` 밖이면 400), `counterpart`(부분 일치), `due_before`(RFC3339), `min_confidence`(0–1 실수, 범위 밖 400), `sort`(`due`|`confidence`), `limit`(1–200), `include_archived=true`.
- `truncated`는 `len(actions) == 유효 limit`일 때 true — 사용자가 "더 있는지"를 알 수 있어야 한다.
- 라우트는 `s.actionLister != nil`일 때만 등록한다. 플래그 off면 **404**(500이 아니다).
- 400 본문에 사용자 입력을 되돌려주지 않는다. `counterpart` 값이 실명일 수 있으므로 `"invalid counterpart filter"` 같은 고정 문구만 쓴다(Task 8 규약).

- [ ] **Step 1: 실패하는 테스트를 쓴다.** `stubActionLister`(받은 `ActionFilter`를 기록)를 정의하고 3가지를 검증한다. 서버 생성 인자 순서는 `cmd/server/main.go:135`의 `api.NewServer(docStore, searchSvc, feedbackStore, evalStore, llmClient, filesystemPath, apiKey)`를 따르고, 테스트에서의 관례는 기존 `internal/api/ask_test.go`를 확인해 맞춘다.

```go
// (1) 필터 파싱: ?kind=my_commitment&kind=scheduled&sort=confidence&limit=10&min_confidence=0.5
//     → lister.got.Kinds 2개, Sort=="confidence", Limit==10, MinConfidence==0.5
// (2) ?kind=invent_a_kind → 400
// (3) WithActions 미호출 서버 → GET /api/v1/actions 가 404
srv := NewServer(nil, nil, nil, nil, nil, "", "").WithActions(lister, nil)
srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/actions?kind=scheduled", nil))
if rec.Code != http.StatusOK { t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String()) }
```

- [ ] **Step 2: 실패 확인.** Run `go test ./internal/api/ -run TestListActionsHandler -v` → FAIL `srv.WithActions undefined`
- [ ] **Step 3: 구현.** `actions.go`에 파서·핸들러, `router.go`에 필드·옵션·등록. 등록 위치는 기존 인증 그룹 안(`r.Post("/api/v1/feedback", ...)` 근처):

```go
if s.actionLister != nil { r.Get("/api/v1/actions", s.listActionsHandler) }
```

- [ ] **Step 4: 통과 확인.** Run `go test ./internal/api/ -run TestListActionsHandler -v` → PASS; `go build ./...` → 성공
- [ ] **Step 5: 커밋.** `git add internal/api/actions.go internal/api/actions_test.go internal/api/router.go && git commit -m "feat(actions): add GET /api/v1/actions"`

---

## Task 3: 액션 상태 변경 스토어 (멱등)

**Files:** Modify `internal/store/actions_query.go`, `internal/store/actions_query_test.go`

**Interfaces**
- Produces: `func (s *ActionQueryStore) SetActionState(ctx context.Context, identityKey string, state model.ActionState, note string) (found bool, err error)`

**설계 메모 — 멱등성의 정확한 의미**

같은 요청을 두 번 보내면 **DB 상태가 완전히 같아야 한다.** `DO UPDATE SET resolved_at = now()`는 호출할 때마다 타임스탬프가 움직이므로 멱등이 아니다. 상태가 실제로 바뀔 때만 갱신한다.

```sql
INSERT INTO action_status (identity_key, state, resolved_by, resolved_at, note)
VALUES ($1, $2, 'user', now(), NULLIF($3,''))
ON CONFLICT (identity_key) DO UPDATE SET
    state       = EXCLUDED.state,
    resolved_by = EXCLUDED.resolved_by,
    note        = COALESCE(EXCLUDED.note, action_status.note),
    resolved_at = CASE WHEN action_status.state IS DISTINCT FROM EXCLUDED.state
                       THEN EXCLUDED.resolved_at ELSE action_status.resolved_at END
RETURNING identity_key
```

- `RETURNING`에서 `EXCLUDED`를 참조하지 않는다 — 이 프로젝트에서 이미 런타임 에러로 확인된 함정이다. 위 쿼리는 `identity_key`만 반환하므로 안전하다.
- 없는 키로 INSERT하면 FK 위반(SQLSTATE `23503`)이다. 이를 500이 아니라 404로 매핑하기 위해 `errors.As(err, &pgErr) && pgErr.Code == "23503"`이면 `found=false, err=nil`을 반환한다.
- 허용 state는 `done`/`ignored`/`open` 3종. `open`도 허용하는 이유는 사용자가 실수로 완료 처리한 것을 되돌릴 수 있어야 하기 때문이다. 검증은 스토어와 핸들러 양쪽에서 한다.

- [ ] **Step 1: 실패하는 테스트를 쓴다.** 실제 DB 기반. 두 가지:

```go
// (1) 멱등: SetActionState(done)를 두 번 호출 → resolved_at 이 움직이지 않아야 한다
first := readResolvedAt(t, pg, key)
_, _ = s.SetActionState(ctx, key, model.StateDone, "")
if second := readResolvedAt(t, pg, key); !second.Equal(first) {
    t.Fatalf("resolved_at moved on repeat call: %v -> %v (not idempotent)", first, second)
}
// (2) 없는 키 → err == nil && found == false (FK 위반이 500으로 새지 않아야 한다)
```

- [ ] **Step 2: 실패 확인.** Run `go test ./internal/store/ -run TestSetActionState -v` → FAIL `SetActionState undefined`
- [ ] **Step 3: 구현.** 위 SQL + `pgconn.PgError` 코드 분기.
- [ ] **Step 4: 통과 확인.** Run `go test ./internal/store/ -run TestSetActionState -v` → PASS
- [ ] **Step 5: 커밋.** `git add internal/store/actions_query.go internal/store/actions_query_test.go && git commit -m "feat(actions): add idempotent SetActionState"`

---

## Task 4: 액션 상태 변경 API

**Files:** Modify `internal/api/actions.go`, `internal/api/actions_test.go`, `internal/api/router.go`

**Interfaces**
- Consumes: Task 3의 `SetActionState`.
- Produces: `func (s *Server) setActionStateHandler(w http.ResponseWriter, r *http.Request)`; 라우트 `POST /api/v1/actions/{identity_key}/status`; 본문 `{"state":"done|ignored|open","note":"..."}`; 응답 `200 {"identity_key":"...","state":"done"}`, 미존재 `404 {"error":"action not found"}`.

**설계 메모**
- `identity_key`는 `action:<16 hex>` 형태(`internal/action/identity.go`의 `BuildIdentityKey`가 `"action:%x"`로 8바이트를 찍는다). 경로 파라미터로 받아 `^action:[0-9a-f]{16}$`로 **형태만** 검증한다. 재계산하지 않는다. 형태가 다르면 400.
- 메서드는 POST. PATCH도 의미상 맞지만 기존 BFF·프런트가 POST 관례를 쓰고 있어 일관성을 택한다.
- `note`는 개인정보가 될 수 있으므로 로그 금지(Task 8). 길이는 500자로 자른다.
- 라우트는 `s.actionSetter != nil`일 때만 등록한다.

- [ ] **Step 1: 실패하는 테스트를 쓴다.** `stubActionSetter`(받은 키·state 기록, `found` 주입)로 3가지:

```go
// (1) POST /api/v1/actions/action:0123456789abcdef/status {"state":"done"}
//     → 200, setter 가 그 키와 model.StateDone 을 받았는지
// (2) found=false → 404
// (3) 잘못된 키 형태("not-an-action-key") 와 잘못된 state("archived") → 각각 400
```

- [ ] **Step 2: 실패 확인.** Run `go test ./internal/api/ -run TestSetActionStateHandler -v` → FAIL(라우트 미등록으로 404)
- [ ] **Step 3: 구현.** `router.go`에 `if s.actionSetter != nil { r.Post("/api/v1/actions/{identity_key}/status", s.setActionStateHandler) }`. chi는 경로 세그먼트 안의 콜론을 문제없이 처리한다.
- [ ] **Step 4: 통과 확인.** Run `go test ./internal/api/ -v` → PASS; `go vet ./...` → 무경고
- [ ] **Step 5: 커밋.** `git add internal/api/actions.go internal/api/actions_test.go internal/api/router.go && git commit -m "feat(actions): add POST /api/v1/actions/{key}/status"`

---

## Task 5: 브리핑 입력 해시와 캐시

**Files:** Create `internal/briefing/input.go`, `cache.go`, `input_test.go`, `cache_test.go`

**Interfaces**
- Consumes: 없음(순수 패키지 — `internal/store`를 import하지 않는다. 변환은 API 계층이 한다).
- Produces:
  - `type InputAction struct { IdentityKey, Kind, Summary, CounterpartName, DocumentID, DetectedBy string; DueAt *time.Time; Confidence float64 }`
  - `type Input struct { Actions []InputAction; LatestDocumentAt time.Time; Model string }`
  - `type Sentence struct { Text string; DocumentIDs []string; IdentityKeys []string }`
  - `type Result struct { Sentences []Sentence; Degraded bool; DroppedCount int; GeneratedAt time.Time }`
  - `func (in Input) CacheKey() string`
  - `func NewCache(capacity int) *Cache`, `func (c *Cache) Get(key string) (Result, bool)`, `func (c *Cache) Put(key string, r Result)`
  - `const PromptVersion = "briefing-v1"` (파일은 Task 6의 `prompt.go`지만 해시가 참조하므로 이 상수만 Task 5에서 먼저 만든다)

**설계 메모 — 캐시 키와 무효화**

키 = `sha256( PromptVersion + "|" + Model + "|" + LatestDocumentAt(RFC3339, UTC) + "|" + 정렬된 identity_key들을 개행으로 이은 문자열 )`.

무효화는 **자동**이다. TTL도 명시적 invalidate도 없다.

| 사건 | 키가 바뀌는 이유 |
|---|---|
| 새 액션 생성 | identity_key 집합에 원소 추가 |
| 사용자가 완료/무시 처리 | 그 키가 열린 집합에서 빠짐 |
| 액션 자체가 갱신되어 관측 시각 전진 | `LatestDocumentAt` 전진 |
| 프롬프트 수정 후 배포 | `PromptVersion` 변경 |
| 모델 교체 | `Model` 변경 |

**정렬 필수.** 정렬하지 않으면 DB 행 순서가 흔들릴 때마다 키가 달라져 캐시가 사실상 동작하지 않는다.
**요약 텍스트는 키에 넣지 않는다.** 같은 `identity_key`는 정의상 같은 실제 액션이고(§5.5), 재추출로 문구가 미세하게 달라진 것 때문에 브리핑을 재생성할 이유가 없다. 부수 효과로 키 계산 입력에서 개인정보가 빠지므로 **키 해시 자체는 로그에 남겨도 안전하다**(Task 8).

캐시는 프로세스 내 소형 LRU(용량 8)로 충분하다 — 사용자 1명, 열린 집합은 자주 바뀌지 않는다. 서버 재시작 시 비워지는 것은 허용한다(첫 요청이 한 번 더 LLM을 부를 뿐). 동시 요청 대비 `sync.Mutex`로 보호한다. `map[string]Result` + 삽입 순서 슬라이스면 되고, 용량 8에 자료구조를 더 얹지 않는다(YAGNI).

- [ ] **Step 1: 실패하는 테스트를 쓴다.** 4가지:

```go
// (1) 행 순서가 달라도 키가 같아야 한다
if a.CacheKey() != b.CacheKey() { t.Fatal("cache key depends on row order; sort identity_keys before hashing") }
// (2) Summary 만 다르고 identity_key 가 같으면 키가 같아야 한다
// (3) 열린 액션 추가 시, 그리고 LatestDocumentAt 전진 시 키가 달라져야 한다
// (4) NewCache(2)에 3개를 Put 하면 가장 오래된 것이 축출된다
```

- [ ] **Step 2: 실패 확인.** Run `go test ./internal/briefing/ -v` → FAIL(패키지 없음)
- [ ] **Step 3: 구현.** `CacheKey`는 `identity_key`만 모아 `sort.Strings` 후 해시.
- [ ] **Step 4: 통과 확인.** Run `go test ./internal/briefing/ -v` → PASS
- [ ] **Step 5: 커밋.** `git add internal/briefing/input.go internal/briefing/cache.go internal/briefing/input_test.go internal/briefing/cache_test.go && git commit -m "feat(briefing): add input hashing and LRU cache"`

---

## Task 6: 브리핑 LLM 출력 스키마와 근거 강제

**Files:** Create `internal/briefing/prompt.go`, `validate.go`, `generate.go`, `validate_test.go`, `generate_test.go`

**Interfaces**
- Consumes: Task 5의 `Input`/`InputAction`/`Sentence`/`Result`; `llm.Completer`(`CompleteWithMessages(ctx, systemPrompt string, msgs []llm.Message) (string, error)` — 시그니처는 `internal/worker/entity_worker.go:41` 근처 사용례로 확인한다. `internal/llm/client.go`는 열지 않는다).
- Produces: `func Validate(raw string, in Input) (Result, error)`, `func Generate(ctx context.Context, c llm.Completer, in Input) (Result, error)`, `const PromptVersion`.

**핵심: 근거 강제를 어떻게 구현하는가**

§7.3은 "모든 문장에 근거 document id 강제, 없거나 존재하지 않는 id를 단 문장은 폐기"를 요구한다. 이를 **프롬프트로 부탁하지 않고 서버가 기계적으로 강제**한다. 4단계다.

**(1) 출력 스키마를 문장 단위로 강제한다.** 산문 문단을 요구하면 근거를 문장에 붙일 수 없다. 다음 JSON만 반환하게 한다.

```json
{"sentences":[{"text":"...","document_ids":["<uuid>"],"identity_keys":["action:<hex16>"]}]}
```

**(2) 허용 id 화이트리스트를 서버가 만든다.** LLM에 넘긴 `Input.Actions`의 `DocumentID`와 `IdentityKey`를 각각 집합으로 모은다. **프롬프트에 등장하지 않은 id는 정의상 환각이다.**

**(3) 문장 단위로 검증·폐기한다.** 전체 응답을 버리지 않는다 — 한 문장의 실패가 나머지 유효 문장까지 죽이면 브리핑이 사실상 항상 빈다.

| 조건 | 판정 |
|---|---|
| `document_ids`가 비었다 | 문장 폐기 |
| `document_ids`에 화이트리스트 밖 값이 하나라도 있다 | 문장 폐기 |
| `text`가 공백이다 | 문장 폐기 |
| `text`가 400자를 넘는다 | 문장 폐기(문장이 아니라 문단을 반환한 것) |
| `identity_keys`에 화이트리스트 밖 값이 있다 | 그 키만 제거, 문장은 유지(근거 문서는 유효하므로) |

**(4) 전부 폐기되면 degraded 폴백.** `Degraded=true`로 표시하고 **LLM 없이** 결정적으로 만든 문장을 반환한다: 열린 액션을 종류별로 세어 `"응답 대기 3건, 내 약속 2건, 상대 약속 1건, 예정 4건."` 형태의 한 문장을 만들고 그 액션들의 `DocumentID`를 근거로 붙인다. 카운트는 창작이 아니므로 근거 강제 원칙을 위반하지 않는다. 화면은 `Degraded`를 보고 "요약 생성 실패 — 집계만 표시" 배지를 띄운다(Task 11).

**JSON 디코딩**: `json.NewDecoder(strings.NewReader(s)).Decode(&v)`를 쓴다(`json.Unmarshal`이 아니라). 모델이 JSON 앞뒤에 부연 텍스트나 코드 펜스를 붙이는 실패는 이 프로젝트에서 이미 관측됐고, `internal/worker/extraction_worker.go:675`의 `decodeRelationActionJSON`이 같은 대응(펜스 제거 → 첫 `{`부터 마지막 `}`까지 슬라이싱)을 구현해 뒀다. **복사하지 말고 같은 전략을 독립 구현한다** — 그 함수는 Part A 소유이고 전용 타입에 묶여 있다.

**프롬프트 요건(`prompt.go`)**: 한국어로 답할 것. 위 JSON만 출력(코드 펜스·머리말 금지). 각 문장은 제공된 액션에서 유래해야 하며 `document_ids`에는 그 액션의 `document_id`를 그대로 복사할 것. **"새 id를 만들지 말라, 목록에 없는 id를 쓰면 그 문장은 버려진다"를 명시**한다(검증 규칙을 알려주면 준수율이 오른다). 문장 수 상한 6, 우선순위는 기한 임박 > `awaiting_my_reply` > `my_commitment` > 나머지. 액션 목록은 `identity_key/kind/due_at/confidence/detected_by/summary/document_id`를 가진 JSON 배열로 넣고, 개수 상한은 `BriefingMaxActions`(기본 40) — 초과 시 기한 오름차순(NULL 뒤) 상위만 넣는다.

- [ ] **Step 1: 실패하는 테스트를 쓴다.** 고정 입력 헬퍼 `testInput()`(가공 더미 2건: `action:aaaa…`/doc `1111…`, `action:bbbb…`/doc `2222…`)를 만들고 6가지를 검증한다.

```go
// (1) 화이트리스트 밖 document_id 를 단 문장은 폐기되고 DroppedCount==1
// (2) document_ids 가 빈 문장은 폐기되고, 전부 폐기되면 Degraded==true
// (3) degraded 폴백 문장도 DocumentIDs 를 비워두지 않는다
// (4) 화이트리스트 밖 identity_key 는 제거되지만 문장은 살아남는다
// (5) 코드 펜스/머리말이 붙은 응답도 디코드된다
// (6) Generate: LLM 이 에러를 반환해도 err==nil 이고 Degraded 폴백이 나온다
got, err := Validate(raw, testInput())
if len(got.Sentences) != 1 { t.Fatalf("sentences = %+v, want only the grounded one", got.Sentences) }
```

(6)의 스텁은 함수를 `llm.Completer`로 감싸는 어댑터로 만든다.

- [ ] **Step 2: 실패 확인.** Run `go test ./internal/briefing/ -run 'TestValidate|TestGenerate' -v` → FAIL(미정의)
- [ ] **Step 3: 구현.** `validate.go`: 펜스 제거 → 슬라이싱 → 디코드 → 화이트리스트 검증 → 폐기 집계 → 전부 폐기 시 `fallbackResult(in)`. `generate.go`: 프롬프트 조립 → `CompleteWithMessages` → 에러면 `fallbackResult(in)`(에러를 위로 올리지 않는다) → 성공이면 `Validate`.
- [ ] **Step 4: 통과 확인.** Run `go test ./internal/briefing/ -v` → PASS
- [ ] **Step 5: 커밋.** `git add internal/briefing/prompt.go internal/briefing/validate.go internal/briefing/generate.go internal/briefing/validate_test.go internal/briefing/generate_test.go && git commit -m "feat(briefing): enforce per-sentence document-id grounding"`

---

## Task 7: 브리핑 API

**Files:** Create `internal/api/briefing.go`, `internal/api/briefing_test.go`; Modify `internal/api/router.go`

**Interfaces**
- Consumes: Task 2의 `ActionLister`, Task 5–6의 `briefing.Input`/`NewCache`/`Generate`, 기존 `Server.llmClient`.
- Produces: `func (s *Server) WithBriefing(cache *briefing.Cache, maxActions int) *Server`, `func (s *Server) briefingHandler(...)`; 라우트 `GET /api/v1/briefing`; 응답 `{"sentences":[...],"degraded":bool,"dropped_count":N,"action_count":N,"cached":bool,"generated_at":"RFC3339"}`.

**설계 메모**
- 흐름: `ListOpenActions`(limit=`maxActions`, sort=`due`) → `briefing.Input` 조립 → `in.CacheKey()` → `cache.Get` 히트면 `cached:true`로 즉시 반환 → 미스면 `briefing.Generate` → `cache.Put`.
- `LatestDocumentAt`은 조회된 액션들의 `observed_at` 최대값으로 둔다(확정 판단 표 참조).
- 열린 액션이 0건이면 **LLM을 호출하지 않고** `{"sentences":[],"degraded":false,"action_count":0}`을 반환한다. 빈 목록으로 LLM을 부르면 반드시 환각이 나온다.
- 요청 컨텍스트에 30초 데드라인. 초과 시 `Generate`가 degraded 폴백으로 떨어진다(500이 아니다).
- `s.llmClient == nil`이거나 `s.actionLister == nil`이면 라우트를 등록하지 않는다.

- [ ] **Step 1: 실패하는 테스트를 쓴다.** 2가지:

```go
// (1) 열린 액션 0건이면 LLM stub 이 호출되지 않고 200
// (2) 같은 요청을 두 번 보내면 LLM 호출 횟수가 1이어야 한다(두 번째는 캐시 히트)
if calls != 1 { t.Fatalf("LLM called %d times, want 1 (second call must hit the cache)", calls) }
```

(2)의 lister 스텁은 `store.ActionListItem{Action: model.Action{IdentityKey:"action:aaaaaaaaaaaaaaaa", DocumentID: uuid.MustParse("11111111-1111-1111-1111-111111111111"), Kind: model.KindMyCommitment, Summary:"더미 요약", ObservedAt: 고정시각}, State: model.StateOpen}` 하나를 반환하고, LLM 스텁은 그 doc id를 근거로 단 유효 문장 하나를 반환한다.

- [ ] **Step 2: 실패 확인.** Run `go test ./internal/api/ -run TestBriefingHandler -v` → FAIL `WithBriefing undefined`
- [ ] **Step 3: 구현.**
- [ ] **Step 4: 통과 확인.** Run `go test ./internal/api/ ./internal/briefing/ -v` → PASS; `go build ./...` → 성공
- [ ] **Step 5: 커밋.** `git add internal/api/briefing.go internal/api/briefing_test.go internal/api/router.go && git commit -m "feat(briefing): add GET /api/v1/briefing with input-hash cache"`

---

## Task 8: 개인정보·로깅 규약

**Files:** Create `internal/briefing/redact.go`, `redact_test.go`, `doc.go`; Modify Task 5–7에서 만든 로그 호출부

**Interfaces**
- Produces: `func SafeLogFields(in Input, r Result) []any`(개인정보가 제거된 구조화 로그 필드만), `func KeyPrefix(identityKey string) string`(`action:0123` — 앞 4 hex).

**규약 (Task 1–12 전체에 소급 적용)**

| 대상 | 화면 | 로그 | 에러 응답 본문 |
|---|---|---|---|
| `actions.summary` | 표시 | **금지** | 금지 |
| 상대방 이름 | 표시 | **금지** | 금지 |
| `action_status.note` | 표시 | **금지** | 금지 |
| **LLM 원본 응답 문자열** | 미표시 | **절대 금지** | 금지 |
| LLM 프롬프트 | 미표시 | **금지** | 금지 |
| `identity_key` | DOM data 속성만 | 앞 4 hex까지만 | 허용 |
| `document_id`(UUID) | 링크로만 | 허용 | 허용 |
| 캐시 키 해시 | 미표시 | 허용 | 허용 |
| 건수·소요시간·폐기 문장 수 | 표시 | 허용 | 허용 |

**LLM 원본 응답을 로그에 남기지 않는다 — 이 Task의 핵심이다.** 같은 유형의 결함이 이슈 #194로 이미 등록되어 있다. 파싱 실패한 응답을 통째로 찍고 싶은 유혹이 강한 지점이지만(추출 파이프라인이 실제로 JSON 파싱 실패를 겪는다), 브리핑의 LLM 응답은 **정의상 사용자 대화 요약의 재서술**이므로 원본을 남기면 개인 대화가 컨테이너 로그와 로그 수집기로 유출된다.

파싱 실패 시 남길 수 있는 것: `len(raw)`, 첫 `{`의 인덱스, 마지막 `}`의 인덱스, 그리고 `*json.SyntaxError`의 `Offset`. **`err.Error()`를 그대로 찍지 않는다** — 디코더 에러 메시지에 원본 조각이 섞여 들어온다. 원본이 정말 필요하면 옵트인 환경 변수 `BRIEFING_DEBUG_DUMP=1`로만 켜고 기본값은 off, 프로덕션 `.env.local`에는 넣지 않는다.

**화면 표시 정책**: 사용자 본인의 데이터이므로 실명·연락처를 마스킹하지 않고 그대로 보여준다. 다만 **문서 본문은 목록·브리핑 응답에 절대 싣지 않는다** — `document_id` 링크만 주고 본문은 기존 문서 상세 화면에서 조회한다. 이렇게 하면 응답이 캐시·프록시 계층에 남더라도 노출량이 요약 한 줄로 제한된다.

- [ ] **Step 1: 실패하는 테스트를 쓴다.** 센티넬 토큰을 심고 로그 필드로 새지 않는지 본다.

```go
// Summary="SENSITIVE_SUMMARY_TOKEN", CounterpartName="SENSITIVE_NAME_TOKEN",
// Sentence.Text="SENSITIVE_SENTENCE_TOKEN", IdentityKey="action:0123456789abcdef"
joined := fmt.Sprint(SafeLogFields(in, res)...)
for _, banned := range []string{"SENSITIVE_SUMMARY_TOKEN", "SENSITIVE_NAME_TOKEN", "SENSITIVE_SENTENCE_TOKEN"} {
    if strings.Contains(joined, banned) { t.Fatalf("log fields leaked %q", banned) }
}
if !strings.Contains(joined, "action:0123") { t.Fatal("expected truncated key prefix for correlation") }
if strings.Contains(joined, "0123456789abcdef") { t.Fatal("full identity key must be truncated") }
```

- [ ] **Step 2: 실패 확인.** Run `go test ./internal/briefing/ -run TestSafeLogFields -v` → FAIL(미정의)
- [ ] **Step 3: 구현하고 기존 로그 호출부를 감사한다.** `redact.go` 구현 후 Task 5–7의 모든 로그 호출부를 위 표에 맞춰 고친다. 감사 명령:

```bash
grep -rn "Summary\|CounterpartName\|raw\b" internal/briefing/*.go internal/api/actions.go internal/api/briefing.go | grep -i "log\|print"
```

- [ ] **Step 4: 통과 확인.** Run `go test ./internal/briefing/ ./internal/api/ -v` → PASS; 위 `grep` → **출력 없음**
- [ ] **Step 5: 커밋.** `git add internal/briefing/redact.go internal/briefing/redact_test.go internal/briefing/doc.go internal/api/actions.go internal/api/briefing.go && git commit -m "feat(briefing): forbid raw LLM output and PII in logs (#194)"`

---

## Task 9: 웹 BFF 프록시

**Files:** Create `web/src/app/api/actions/route.ts`, `web/src/app/api/actions/[key]/status/route.ts`, `web/src/app/api/briefing/route.ts`; Modify `web/src/lib/types.ts`, `web/src/lib/api.ts`

**Interfaces**
- Consumes: Task 2·4·7의 백엔드 엔드포인트.
- Produces:
  - `interface ActionItem { identity_key: string; document_id: string; thread_key: string; kind: "awaiting_my_reply"|"my_commitment"|"their_commitment"|"scheduled"; summary: string; counterpart_name: string; due_at: string|null; detected_by: "structural"|"llm"|"both"; confidence: number; observed_at: string; state: "open"|"done"|"ignored" }`
  - `interface BriefingSentence { text: string; document_ids: string[]; identity_keys: string[] }`
  - `interface BriefingResponse { sentences: BriefingSentence[]; degraded: boolean; dropped_count: number; action_count: number; cached: boolean; generated_at: string }`
  - `listActions(params)`, `setActionState(identityKey, state, note?)`, `getBriefing()` — `web/src/lib/api.ts`

**설계 메모**
- 3개 라우트 모두 `web/src/app/api/notes/route.ts`의 패턴을 그대로 복제한다: `BRAIN_API_URL ?? NEXT_PUBLIC_API_URL ?? "http://localhost:9200"`, 서버 보관 `API_KEY`를 `Authorization: Bearer`로 붙임, 업스트림 실패 시 502.
- 인증은 이미 `web/src/proxy.ts`가 처리한다. **이 라우트들에 인증 코드를 추가하지 않는다.**
- 목록 라우트는 브라우저의 쿼리스트링을 **화이트리스트로 걸러** 전달한다(`kind`, `counterpart`, `due_before`, `min_confidence`, `sort`, `limit`, `include_archived`). 통째로 포워딩하면 프런트 버그가 백엔드 400으로 새어 나온다.
- `[key]` 세그먼트는 `action:`의 콜론을 포함하므로 클라이언트에서 `encodeURIComponent`로 감싸 보내고, 라우트 핸들러에서 디코드 후 업스트림 URL을 만들 때 다시 인코딩한다.
- 502/에러 본문에 업스트림 응답 텍스트를 실어 나르지 않는다(Task 8) — `{ error: "upstream request failed" }` 고정 문구.

- [ ] **Step 1: 타입과 클라이언트 함수를 추가한다.** `types.ts`에 위 3개 인터페이스, `api.ts`에 3개 함수. 기존 `listConversations`/`getConversation`의 fetch·에러 처리 관례를 그대로 따른다.
- [ ] **Step 2: 3개 라우트 파일을 만든다.**
- [ ] **Step 3: 타입 체크와 린트로 검증한다.** Run `cd web && bun run lint && bunx tsc --noEmit` → 무오류. (`npm`을 쓰지 않는다.)
- [ ] **Step 4: 로컬 백엔드로 수동 확인한다.** 백엔드를 로컬에 띄운 상태로 `cd web && bun run dev` 후 `curl -s localhost:3000/api/actions | jq '.actions | length'` — 숫자가 나오면 성공. **응답 본문을 그대로 출력하지 않는다**(개인정보).
- [ ] **Step 5: 커밋.** `git add web/src/app/api/actions web/src/app/api/briefing web/src/lib/types.ts web/src/lib/api.ts && git commit -m "feat(web): add actions/briefing BFF routes"`

---

## Task 10: 액션 목록 화면

**Files:** Create `web/src/app/actions/page.tsx`, `web/src/app/actions/ActionList.tsx`

**Interfaces**
- Consumes: Task 9의 `listActions`/`setActionState`, `ActionItem`.
- Produces: `/actions` 경로의 화면. `ActionList`는 `{ items: ActionItem[]; onResolve: (key: string, state: "done"|"ignored") => void }`를 받는다.

**설계 메모 — `/ask` 페이지와의 일관성**
- `"use client"` + `useState`/`useCallback`, `@/components/ui`의 `Button`/`Card`/`Badge`/`Spinner` 재사용. 새 UI 원시 컴포넌트를 만들지 않는다.
- 레이아웃: 상단 브리핑 패널(Task 11) → 필터 바 → 액션 카드 목록.
- 필터 바: 종류 4개 토글(한국어 라벨 — 응답 대기 / 내 약속 / 상대 약속 / 예정), 상대방 텍스트 입력, 정렬 셀렉트(기한/신뢰도), 신뢰도 하한 슬라이더(0.0에서 시작). 필터 상태는 URL 쿼리스트링에 반영한다(`/ask`가 `useSearchParams`로 하는 것과 같은 방식) — 새로고침해도 유지되고 링크 공유가 된다.
- 카드 내용: 요약(굵게), 상대방 이름, 종류 배지, `detected_by` 배지, `confidence` 퍼센트, 기한(있으면 "3일 남음"/"2일 지남" 형태의 상대 시간), 근거 문서 링크(`/documents/{document_id}`), **완료 / 무시** 버튼 2개.
- 조작은 **낙관적 업데이트**: 클릭 즉시 목록에서 제거하고 요청을 보낸다. 실패하면 되돌리고 카드 위에 "처리 실패 — 다시 시도" 인라인 메시지를 띄운다. 상태 변경 API가 멱등이므로(Task 3) 재시도 클릭이 안전하다.
- 처리 성공 후 브리핑을 다시 요청한다 — 열린 집합이 바뀌어 캐시 키가 자동으로 달라진다(Task 5).
- 빈 상태: "열린 액션이 없습니다." `truncated`면 하단에 "상위 N건만 표시됨 — 필터를 좁히세요".
- 목록 조회가 404를 받으면 "액션 기능이 비활성화되어 있습니다"를 표시한다(Task 12의 플래그 off 상태).
- 모바일 레이아웃 주의: 이 프로젝트는 과거에 저장 버튼이 하단 탭바에 영구히 가리는 버그를 겪었다. 완료/무시 버튼은 카드 안에 두고 화면 하단 고정 요소로 만들지 않는다.

- [ ] **Step 1: `ActionList.tsx`를 만든다.** props 배열을 렌더링하고 버튼 클릭 시 `onResolve(key, state)`를 호출하는 순수 표시 컴포넌트로 시작한다.
- [ ] **Step 2: `page.tsx`를 만든다.** 마운트 시 `listActions()`, 필터·URL 동기화, 낙관적 업데이트와 롤백, 404 분기를 담당한다.
- [ ] **Step 3: 타입 체크와 린트.** Run `cd web && bun run lint && bunx tsc --noEmit` → 무오류
- [ ] **Step 4: 브라우저에서 렌더를 확인한다.** `bun run dev` 후 `http://localhost:3000/actions`에서 (a) 카드가 그려지는지, (b) 콘솔 에러가 없는지, (c) 완료 버튼을 누르면 카드가 사라지고 새로고침 후에도 사라져 있는지 확인한다. **타입 체크 통과만으로 완료를 선언하지 않는다** — 이 프로젝트의 완료 검증 규칙상 UI 변경은 실제 렌더 확인이 필요하다.
- [ ] **Step 5: 커밋.** `git add web/src/app/actions/page.tsx web/src/app/actions/ActionList.tsx && git commit -m "feat(web): add /actions list with done/ignore"`

---

## Task 11: 브리핑 패널

**Files:** Create `web/src/app/actions/BriefingPanel.tsx`; Modify `web/src/app/actions/page.tsx`

**Interfaces**
- Consumes: Task 9의 `getBriefing`, `BriefingResponse`.
- Produces: `<BriefingPanel refreshKey={number} />` — `refreshKey`가 바뀔 때마다 다시 조회한다(Task 10이 완료/무시 성공 시 증가시킨다).

**설계 메모**
- 문장은 리스트가 아니라 **한 문단으로 이어 붙여** 보여준다. 각 문장 끝에 근거 문서 링크를 위첨자 번호(`[1]`, `[2]`)로 붙이고 번호는 `/documents/{document_id}`로 이동한다. 링크 텍스트에 문서 제목이나 본문을 넣지 않는다 — 번호만이다(Task 8: 본문 미노출).
- `degraded === true`면 문단 위에 경고 배지 "요약 생성 실패 — 집계만 표시". 사용자가 이 브리핑을 완전한 요약으로 오해하지 않게 하는 것이 이 배지의 유일한 목적이다.
- `dropped_count > 0`이면 문단 아래에 회색 소문자 주석 "근거 없는 문장 N개 폐기됨". 이건 디버깅 정보가 아니라 **신뢰 신호**다 — 시스템이 검증을 하고 있다는 것을 사용자가 볼 수 있어야 한다.
- `cached === true`면 "캐시됨" 배지. `action_count === 0`이면 패널을 숨긴다.
- 로딩 중에는 `Spinner`. 요청 실패는 조용히 삼키고 패널을 숨긴다 — 브리핑 실패가 액션 목록 사용을 막아서는 안 된다.

- [ ] **Step 1: `BriefingPanel.tsx`를 만든다.**
- [ ] **Step 2: `page.tsx`에 배치하고 `refreshKey`를 배선한다.** 완료/무시 성공 시 `setRefreshKey((n) => n + 1)`.
- [ ] **Step 3: 타입 체크와 린트.** Run `cd web && bun run lint && bunx tsc --noEmit` → 무오류
- [ ] **Step 4: 브라우저에서 확인한다.** (a) 문단이 렌더되고 위첨자 링크가 문서 상세로 이동하는지, (b) 액션을 하나 완료 처리하면 브리핑이 갱신되는지, (c) 콘솔 에러 없음.
- [ ] **Step 5: 커밋.** `git add web/src/app/actions/BriefingPanel.tsx web/src/app/actions/page.tsx && git commit -m "feat(web): add briefing panel with evidence links"`

---

## Task 12: 기능 플래그 배선

**Files:** Modify `internal/config/config.go`, `cmd/server/main.go`, `.env.example`, `web/src/app/dashboard/page.tsx`

**Interfaces**
- Produces: `Config.ActionsAPIEnabled bool`(env `ACTIONS_API_ENABLED`, 기본 `false`), `Config.BriefingEnabled bool`(env `BRIEFING_ENABLED`, 기본 `false`), `Config.BriefingMaxActions int`(env `BRIEFING_MAX_ACTIONS`, 기본 `40`).

**설계 메모**
- **`internal/config/config.go`는 다른 에이전트가 동시에 만지고 있을 수 있다.** 필드 3개를 구조체 **맨 끝에** 추가하고 파싱도 기존 블록 맨 끝에 붙인다. 기존 필드의 순서나 서식을 정리하지 않는다(충돌 면적 최소화).
- 기본값이 `false`인 이유: 배포 후 아무것도 노출되지 않는 상태에서 시작해 백엔드가 정상인지 확인한 뒤 켠다.
- 플래그가 꺼져 있으면 `WithActions`/`WithBriefing`을 **아예 호출하지 않는다.** 옵션 안에서 분기하지 않는다 — 라우트 미등록이 곧 404이고 그것이 이 플래그의 롤백 의미다.

```go
if cfg.ActionsAPIEnabled {
    aq := store.NewActionQueryStore(pg)
    srv = srv.WithActions(aq, aq)
    if cfg.BriefingEnabled {
        srv = srv.WithBriefing(briefing.NewCache(8), cfg.BriefingMaxActions)
    }
}
```

- `BriefingEnabled`는 `ActionsAPIEnabled`에 종속된다(브리핑이 액션 조회를 쓴다). 액션만 켜고 브리핑을 끄는 조합은 유효하고, 그 반대는 무시된다.
- 대시보드에 `/actions` 링크를 추가한다.

- [ ] **Step 1: config 필드 3개를 추가한다.** 기존 bool/int 파싱 헬퍼가 있으면 그것을 쓴다.
- [ ] **Step 2: `cmd/server/main.go`를 배선한다.**
- [ ] **Step 3: `.env.example`에 3개 변수를 주석과 함께 추가한다.** `BRIEFING_DEBUG_DUMP`는 넣지 않는다(Task 8).
- [ ] **Step 4: 검증.** Run `go build ./... && go test ./...` → 성공. 플래그를 끈 채 로컬 서버를 띄우고 `curl -s -o /dev/null -w '%{http_code}' localhost:9200/api/v1/actions` → `404`. 켠 상태로 → `200`(또는 인증 미들웨어의 `401`).
- [ ] **Step 5: 커밋.** `git add internal/config/config.go cmd/server/main.go .env.example web/src/app/dashboard/page.tsx && git commit -m "feat(actions): gate actions/briefing behind feature flags"`

---

## 배포 (마지막, 별도 절)

구현·검증이 전부 끝난 뒤에만 진행한다. **이 계획서를 작성한 에이전트는 배포를 수행하지 않는다** — SSH·컨테이너 조작·프로덕션 DB 접속은 범위 밖이다. 아래는 배포 담당자를 위한 절차다.

- [ ] **Step 1: CI 그린 확인.** 백엔드 `go test ./...`, 프런트 lint + 빌드가 모두 통과했는지 확인한다. **CI 그린이 곧 배포 가능은 아니다** — 이 프로젝트는 CI가 통과했는데 compose 설정 때문에 배포가 깨진 전례가 있다.
- [ ] **Step 2: compose 설정을 정적으로 검증한다.** `docker compose -f docker-compose.local.yml --env-file .env.local config`로 렌더 결과를 확인한다. `--env-file`을 빼고 실행하지 않는다 — 볼륨 경로가 기본값으로 떨어져 컨테이너가 죽는다.
- [ ] **Step 3: 플래그를 끈 채로 배포한다.** `.env.local`에 `ACTIONS_API_ENABLED=false`, `BRIEFING_ENABLED=false`를 넣고 server + web 이미지를 올린다. 기존 기능이 그대로 동작하는지 먼저 확인한다.
- [ ] **Step 4: 액션 API만 켠다.** `ACTIONS_API_ENABLED=true` → server 재기동 → 컨테이너 **내부** 서비스명으로 200을 확인한다. Cloudflare Access가 앞단에 있으면 외부에서 302가 돌아와 실제 상태를 가리므로 도커 네트워크 내부에서 검증해야 한다.
- [ ] **Step 5: 브리핑을 켠다.** `BRIEFING_ENABLED=true` → 재기동 → 첫 요청의 지연과 두 번째 요청의 `cached:true`를 확인한다. **응답 본문을 그대로 출력하지 않는다** — `jq '{degraded, dropped_count, action_count, cached}'`로 메타데이터만 본다.
- [ ] **Step 6: 로그를 감사한다.** 배포 직후 server 컨테이너 로그에서 요약·이름·LLM 원본이 새지 않았는지 확인한다. 액션 요약 문구가 보이면 즉시 플래그를 끄고 Task 8로 돌아간다.
- [ ] **Step 7: 화면을 연다.** 실제 브라우저로 `/actions`를 열어 목록·브리핑·완료 처리를 확인한다.

---

## 롤백

세 단계로 되돌릴 수 있고 위로 갈수록 빠르다.

| 상황 | 조치 | 소요 |
|---|---|---|
| 브리핑 품질 문제(환각·빈 결과·비용) | `.env.local`에서 `BRIEFING_ENABLED=false` → server 재기동. 액션 목록은 계속 동작한다. | 1분 |
| 액션 API 자체 문제(쿼리 지연, 오탐 노출) | `ACTIONS_API_ENABLED=false` → server 재기동. 두 라우트가 모두 미등록되어 404. 프런트는 "기능 비활성화" 안내를 표시한다. | 1분 |
| 코드 결함 | 이전 이미지 태그로 롤백. | 5분 |

**데이터 롤백은 필요 없다.** Part C는 신규 마이그레이션을 만들지 않고 쓰기는 `action_status` upsert 하나뿐이다. 그 행은 사용자가 명시적으로 누른 판단이므로 되돌릴 대상이 아니다 — 오히려 코드를 롤백해도 **살아남아야 하는** 데이터다(§5.5 불변식).

한 가지 예외: 프런트 버그로 대량의 액션이 잘못 `ignored` 처리된 경우. 이때는 특정 시간대의 `resolved_at`을 가진 행을 `state='open'`으로 되돌리는 일회성 UPDATE가 필요하다. **`resolved_at`이 상태 전이 시각을 정확히 보존하는 것(Task 3의 멱등 구현)이 이 복구를 가능하게 한다** — `now()`로 매번 덮어썼다면 어떤 행이 사고에 포함됐는지 구분할 수 없다.

---

## Self-Review

**스펙 커버리지**: §7.1 구조 신호(Part A 소유 — Part C는 산출물을 읽기만 함, Task 1) · §7.2 병합 결과의 `detected_by` 노출(Task 2 응답, Task 10 배지) · §7.3 온디맨드 + 입력 해시 캐시(Task 5, 7) · §7.3 문장별 근거 강제와 폐기(Task 6) · §7.4 "무시" 영속화(Task 3, 4) · §7.4 LLM 환각 방어(`detected_by`/`confidence` 노출, Task 10) · §7.4 90일 누적 방어(Task 1 조회 필터) · §11 프라이버시(Task 8). §7.4의 "인증문자 오탐 제외"는 Part A 구조 신호 워커의 책임이므로 Part C에 태스크가 없다 — 의도된 공백이다.

**미해결 위험 (구현자가 알아야 할 것)**

1. **`counterpart_entity_id`가 대부분 NULL일 가능성.** 구조 신호 경로는 상대방을 문자열로 계산하고(`internal/action/identity.go`의 `CounterpartIdentity`), 그 값을 `entities`에 넣는지는 Part A 워커 구현에 달려 있다. NULL이 많으면 `counterpart_name`이 빈 문자열이 되어 상대방 필터가 무용해지고 화면도 빈약해진다. Task 1 구현 시 **실제 데이터의 NULL 비율을 먼저 확인**하고, 높으면 `thread_key`에서 표시명을 파생하는 폴백(`sms:{이름}` → 이름, 해시면 "알 수 없는 번호")을 Task 1에 추가한다.
2. **`awaiting_my_reply`의 요약이 Go 생성 템플릿이라 브리핑 문장이 단조로울 수 있다.** `NormalizeSummary`가 이 종류에 대해 빈 문자열을 반환하는 데서 보듯 요약 텍스트에 정보가 적다. 품질이 낮으면 프롬프트에 `thread_key`와 `observed_at`을 더 실어 보내는 것을 먼저 시도한다(프롬프트만 바꾸므로 `PromptVersion` 증가로 캐시가 자동 무효화된다).
3. **브리핑 비용이 측정되지 않았다.** 액션 40건을 프롬프트에 실으면 입력 토큰이 상당하다. thinking은 비활성화되어 있지만 입력 자체의 비용은 남는다. 배포 후 첫 주에 호출 횟수를 관측하고 잦으면 `BriefingMaxActions`를 낮춘다.
4. **`entities` 조인이 조회를 느리게 만들 수 있다.** `idx_actions_counterpart`는 있지만 `entities` 쪽 인덱스는 확인하지 않았다. 액션이 수천 건으로 늘면 재측정이 필요하다. 스키마 변경 금지 제약 때문에 인덱스 추가는 별도 승인이 필요하다.
5. **동시 편집 충돌.** `internal/api/router.go`, `cmd/server/main.go`, `internal/config/config.go`는 다른 작업 흐름도 건드릴 가능성이 높다. 각 Task 착수 전에 `git fetch` 후 ahead/behind를 확인한다 — 이 프로젝트는 스테일 베이스 위에 작업을 쌓아 전량 재작업한 전례가 두 번 있다.
