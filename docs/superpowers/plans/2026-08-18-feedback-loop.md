# Part D — 피드백 루프 (`/ask` 통합) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `/ask` 답변의 각 근거 문서에 좋아요/싫어요를 붙여 `(query, document_id, thumbs)` 검색 레이블을 축적하고, 그 레이블로 검색 변경이 개선인지 악화인지 **판정할 수 있게** 만든다.

**Architecture:** 근거 카드 단위 피드백 → `feedback` 테이블(기존, additive 확장) → 질의 해시 기반 고정 분할(train/holdout) → `SearchWeights` 좌표 탐색 최적화기(train 전용) → holdout 게이트 통과 시 승격, 이력 저장, 즉시 롤백. holdout 데이터는 **패키지 경계 밖으로 나가지 않는 폐쇄형 평가 인터페이스**로만 접근된다.

**Tech Stack:** Go 1.23+ (backend, `cmd/tune` 배치 바이너리), PostgreSQL(pgvector), Next.js 16 + React 19 + **bun**(web), Langfuse(관측), Cloudflare Access JWT(`web/src/proxy.ts`).

**Spec:** `docs/superpowers/specs/2026-08-17-knowledge-graph-actions-feedback-design.md` §8 (Part D). 전제 명세: `docs/superpowers/specs/2026-08-17-ask-pipeline-design.md` §5.1(SSE `sources` 이벤트).

## Global Constraints

- **LLM은 원격 API 호출만.** 로컬 추론/파인튜닝 금지 (spec §9).
- **기존 `feedback` 50행을 파괴하지 않는다.** 모든 마이그레이션은 additive only — 컬럼 삭제·타입 변경·기존 컬럼 NOT NULL 승격 금지.
- **`internal/config/config.go`는 이 계획에서 수정하지 않는다** (동시 작업 중). 신규 플래그는 `os.Getenv`로 읽는다 — 기존 선례: `internal/model/document.go:133`의 `SUMMARY_VEC_COVERAGE_THRESHOLD`, `ENTITY_EXTRACTION_ENABLED`.
- **동시 작업 중이라 이 계획이 건드리지 않는 파일:** `internal/graph/**`, `internal/worker/graph_projection_worker*.go`, `internal/store/graph_source*.go`, `internal/model/projection.go`, `cmd/collector/main.go`, `internal/llm/client.go`, `internal/config/config.go`, `internal/store/document.go`, `internal/api/ask_retrieval.go`. Task 10은 이 중 `ask_retrieval.go`를 **건드리지 않는 경로**(서버 조립 지점)로 설계되어 있다.
- **네트워크 제약(실측 확인):** macmini 컨테이너는 ubuntu1의 Tailscale IP에 도달하지 못하고, 역방향도 막혀 있다. 따라서 `cmd/tune`은 **Postgres와 같은 호스트에서 실행**한다(§배포). 리랭커 HTTP 서비스에 의존하지 않는다.
- **개인 데이터 취급:** 질의 원문(`feedback.query`)에 개인정보가 포함될 수 있다. 로그·Langfuse에 질의 원문을 그대로 싣지 않는다 — `md5(query)` 앞 8자(`query_hash`)만 식별자로 쓴다.
- **패키지 매니저는 bun.** `npm install` 금지 — `package-lock.json`이 생기면 CI(`bun --frozen-lockfile`)가 깨진다.
- 기능 플래그 2개 모두 기본 off. off 상태에서 시스템 동작은 오늘과 **완전히 동일**해야 한다.

---

## 1. 왜 지금 이것을 만드는가 — 판정 불가 상태의 종료

오늘 검색 품질을 기준선과 비교했더니 **평균 overlap@10이 4.68/10**이었다. 즉 후보 집합의 절반 이상이 갈렸다. 그런데 그 변화가 개선인지 악화인지 **판정할 수단이 없다.** 정답 레이블이 없기 때문이다.

이 계획의 1차 목표는 "가중치를 자동 튜닝한다"가 아니라 **"변경이 나아진 것인지 답할 수 있게 만든다"** 이다. 자동 튜닝은 판정 능력이 생긴 뒤에 따라오는 2차 결과물이며, 그래서 Task 1~5(레이블 축적)가 Task 6~10(최적화·승격)보다 먼저 오고, 승격은 표본이 30 질의에 도달하기 전까지 **제안만** 한다.

현재 자산 실측:

| 자산 | 상태 | 위치 |
|---|---|---|
| `feedback` 테이블 | 존재, 약 50행 | `migrations/005_feedback.sql` |
| `POST /api/v1/feedback` | 존재 (범용 핸들러) | `internal/api/feedback.go`, `router.go:287` |
| `FeedbackStore.Record` / `.Upsert` | 존재 | `internal/store/feedback.go:47,74` |
| 레이블→평가쌍 변환 | 존재 (`BuildFromFeedback`) | `internal/store/eval.go:50` |
| 측정 함수 (NDCG/MRR/FP penalty) | 존재 | `internal/search/tune.go` |
| 가중치 주입 경로 | 존재 (`WithWeights`) | `internal/search/search.go:82` |
| `/ask` 근거 카드 UI | 존재 (`SourceCard`) | `web/src/app/ask/page.tsx:39` |
| 대화 세션 보존 | 존재 (`ask_sessions`) | `migrations/024_ask_sessions.sql` |
| **가중치 최적화기** | **없음** | — |
| **train/holdout 분할** | **없음** | — |
| **가중치 변경 이력·롤백** | **없음** | — |

빠진 것은 넷이다: (1) 근거 단위 수집 경로, (2) 오염되지 않은 분할, (3) 탐색기, (4) 승격·롤백 이력.

---

## 2. 확정된 설계 판단 (실행자는 이 절을 근거로 삼는다)

### 2.1 `feedback` 테이블은 그대로 쓸 수 있는가 — **쓸 수 있다. 단 두 가지 additive 보강이 필요하다.**

현재 스키마(`migrations/005_feedback.sql`):

```
id, query, document_id, chunk_id, source, session_id, user_id,
thumbs, comment, metadata, created_at    -- CHECK (thumbs BETWEEN -1 AND 1)
```

`(query, document_id, thumbs)`라는 레이블 삼중항이 이미 1급 컬럼으로 존재한다. 새 테이블을 만들 이유가 없다. 다만 두 가지가 없다:

**(a) 멱등성 보장이 없다.** `Record`는 항상 INSERT다. 기존 `Upsert`(`internal/store/feedback.go:74`)는 `(user_id, session_id, source)` 튜플의 **모든 행을 DELETE**한 뒤 하나를 INSERT한다 — Discord 리액션 토글용 설계다. 이걸 근거 카드에 그대로 쓰면 **같은 대화에서 앞서 매긴 다른 문서의 레이블이 전부 지워진다.** 근거 단위 피드백에는 `(session_id, query, document_id)` 단위 멱등 경로가 따로 필요하다.

**(b) 분할 컬럼이 없다.** train/holdout을 매번 계산하면 재현성이 규약에 의존한다. 컬럼으로 못박아 저장해야 알고리즘이 나중에 바뀌어도 과거 행의 분할이 고정된다.

**보강 방식 (additive only):**

| 변경 | 안전한 이유 |
|---|---|
| `ALTER TABLE feedback ADD COLUMN split TEXT` (nullable, 기본 NULL) | 기존 50행에 영향 없음. NOT NULL 승격 안 함 |
| 결정적 backfill `UPDATE ... WHERE split IS NULL AND query IS NOT NULL` | 값 추가만. 행 삭제·컬럼 변경 없음 |
| **부분** UNIQUE 인덱스 `WHERE source = 'ask_evidence'` | 기존 50행의 `source`는 `'search'`/`'discord_bot'`/`'api'`이므로 **인덱스 대상이 0행** → 중복 충돌로 마이그레이션이 실패할 수 없다 |

전역 UNIQUE 인덱스를 걸지 않는 이유가 핵심이다. 시드 50행에 `(query, document_id)` 중복이 있으면 전역 인덱스 생성이 **마이그레이션 시점에 실패**한다. 신규 소스 값 `'ask_evidence'`로 대상을 좁히면 그 실패 가능성이 구조적으로 0이다.

### 2.2 분할은 질의 단위로, 결정적으로

행 단위가 아니라 **정규화된 질의 문자열 단위**로 분할한다. 행 단위로 나누면 같은 질의가 양쪽 분할에 동시에 등장해 holdout이 곧바로 오염된다.

- 정규화: `lower(btrim(query))`
- 해시: `md5('sbfeedback:v1:' || normalized)` 의 앞 4바이트를 big-endian uint32로 읽고 `% 100`
- `< 70` → `train`, 아니면 `holdout` (70/30)
- 시드 문자열 `sbfeedback:v1:`은 상수. 바꾸면 과거 분할이 흔들리므로 **절대 변경 금지**(변경이 필요하면 `v2` 접두사와 새 컬럼으로).

md5를 쓰는 이유: Go(`crypto/md5`)와 PostgreSQL(`md5()`)이 **동일한 값**을 내므로, backfill(SQL)과 신규 삽입(Go)이 같은 결과를 낸다. `hashtext()`는 PostgreSQL 내부 함수라 버전 간 안정성이 보장되지 않으므로 쓰지 않는다.

### 2.3 검증셋 격리는 규약이 아니라 **타입과 패키지 경계**로 강제한다

"노출하지 않기로 한다"는 문장은 코드가 아니다. 다음 세 겹으로 막는다.

1. **분리 로더.** `dataset.Load(ctx, src)`가 `(TrainSet, HoldoutSet, error)`를 돌려준다. `WHERE split='train'`과 `WHERE split='holdout'`을 **각각 별도 쿼리**로 읽는다. "전부 읽기" 함수는 존재하지 않는다.
2. **폐쇄형 평가 인터페이스.** `HoldoutSet`은 내부 필드가 전부 unexported이고, 평가쌍을 밖으로 내보내는 exported 접근자가 **하나도 없다**. 유일한 exported 메서드는

   ```go
   func (h *HoldoutSet) Evaluate(run RunFunc) (Metrics, error)
   ```

   호출자는 *검색 함수를 넣고* *집계 스칼라만* 받는다. 최적화기는 holdout의 질의 문자열도, 문서 ID도 손에 넣을 수 없다. (`TrainSet`은 반대로 `Pairs() []store.EvalPair`를 노출한다 — 좌표 탐색은 원본이 필요하다.)
3. **평가 예산.** `HoldoutSet`은 잔여 호출 카운터를 갖고, 기본 예산 2회(baseline 1 + candidate 1)를 넘기면 `ErrHoldoutBudgetExhausted`를 반환한다. holdout을 반복 프로빙해 사실상 학습셋처럼 쓰는 우회를 막는다.

추가로 리플렉션 기반 API 표면 테스트가 `HoldoutSet`의 exported 메서드 집합이 `{Evaluate, Queries}`(`Queries()`는 개수만 반환하는 `int`)임을 고정한다. 누군가 나중에 `Pairs()`를 추가하면 테스트가 깨진다.

### 2.4 최적화기 목적 함수와 배치 위치

- **목적 함수:** `objective = NDCG@10 − 0.5 × FPPenalty@10` (둘 다 `internal/search/tune.go`의 기존 함수. 새 지표를 만들지 않는다 — spec §8.3)
- **탐색 공간:** `FTSWeight, VecWeight, BigmWeight ∈ [0.25, 2.0] step 0.25`; `SummaryVec ∈ [0.05, 1.5] step 0.25`; `EntityWeight ∈ [0.05, 1.5] step 0.25`; `RRFK ∈ {20, 40, 60, 80, 120}`
  - `SummaryVec`의 하한이 0이 아니라 0.05인 이유: `SearchWeights.Defaults()`에서 **0은 "미설정 → 커버리지 게이트에 위임"** 이라는 별도 의미를 갖는다(`internal/model/document.go:99`). 최적화기가 0을 후보로 내면 의도와 다른 동작이 된다. 레인을 끄고 싶으면 `DisableSummaryVec=true`를 별도 후보로 평가한다.
- **탐색 방식:** 좌표 탐색. 한 좌표씩 전 구간을 훑어 최선값 고정 → 다음 좌표. 개선폭 `< 1e-4`이거나 **최대 3 sweep**에서 종료(무한 루프 방지).
- **과적합 방지:** (a) 탐색은 train만 사용, (b) holdout에서 baseline 대비 **NDCG@10 +0.01 이상** 개선 **그리고** FPPenalty@10 악화 `≤ 0.02`일 때만 승격, (c) train↔holdout objective 격차를 이력에 기록해 사후 감시, (d) holdout 평가 예산 2회.
- **배치 위치:** `cmd/tune`은 **Postgres와 같은 호스트에서 실행**한다. macmini↔ubuntu1 상호 도달이 막혀 있으므로 배치를 DB 반대편에 두면 아예 동작하지 않는다. Postgres가 ubuntu1로 이전되면(`docs/superpowers/plans/2026-08-18-postgres-migration-to-ubuntu1.md`) 최적화기도 ubuntu1로 따라간다. 이전 전에는 macmini에서 일회성으로 돌린다.
- **리랭커 미사용:** 배치 계층에서 리랭커 HTTP 서비스 도달성을 보장할 수 없다. `UseRerank=false`로 고정한다. RRF 가중치는 리랭킹 **이전** 융합 단계의 파라미터이고, 후보와 baseline을 **동일 조건**에서 비교하므로 상대 비교의 타당성은 유지된다. 이 사실을 이력 행 `metadata.rerank=false`에 남긴다.
- **주기:** 수동 실행 + 주 1회 cron. 레이블 증가 속도가 느리므로 야간 매일 실행은 낭비다.

### 2.5 승격·롤백

- 신규 테이블 `search_weights_history` (migration 026). `status ∈ {proposed, active, rolled_back, superseded}`.
- **`active`는 최대 1행** — 부분 UNIQUE 인덱스 `WHERE status='active'`가 DB 레벨에서 강제한다.
- **30 질의 게이트:** holdout의 **distinct 질의 수 < 30**이면 승격 경로를 타지 않고 `proposed`로만 기록한다. 이것은 조건문 하나가 아니라 `promote()` 진입 전의 별도 함수 `gate.Eligible(holdoutQueries int) (bool, string)`로 분리해 단위 테스트한다.
- **롤백:** `cmd/tune -rollback` → 현재 `active`를 `rolled_back`으로 바꾸고 직전 `active`였던(=`superseded`) 행을 다시 `active`로 올린다. 되돌릴 행이 없으면 `active` 없음 상태(= 코드 기본값)로 복귀. 플래그를 끄는 것만으로도 즉시 기본값으로 되돌아간다.

---


## 3. 파일 구조

```
migrations/
  025_feedback_evidence.sql        # NEW — feedback additive 보강 (split, 부분 UNIQUE)
  026_search_weights_history.sql   # NEW — 가중치 이력·승격 상태

internal/store/
  feedback.go                      # EDIT — UpsertEvidence + ListEvidenceSplitStats 추가 (기존 함수 불변)
  feedback_evidence_test.go        # NEW — 실 DB 테스트
  weights_history.go               # NEW — SearchWeightsHistoryStore
  weights_history_test.go          # NEW

internal/api/
  feedback_evidence.go             # NEW — POST /api/v1/feedback/evidence 핸들러
  feedback_evidence_test.go        # NEW
  server.go                        # EDIT — 라우트 1줄 + 필드 1개 (router.go는 건드리지 않는다)

internal/dataset/                  # NEW 패키지 — 분할·격리의 경계
  dataset.go                       # Load → (TrainSet, HoldoutSet); HoldoutSet은 폐쇄형
  split.go                         # SplitOf(query) — Go/SQL 동일 md5 규칙
  runner.go                        # RunFunc 타입 + 평가 루프
  dataset_test.go, split_test.go, api_surface_test.go

internal/tune/                     # NEW 패키지 — 좌표 탐색
  coordinate.go                    # Search(train, base) → 후보 가중치
  gate.go                          # Eligible(holdoutQueries) — 30 질의 게이트
  coordinate_test.go, gate_test.go

cmd/tune/
  main.go                          # NEW — -propose / -promote / -rollback / -dry-run

cmd/server/
  main.go                          # 건드리지 않는다 (동시 작업) — Task 10은 internal/api/server.go에서 조립

web/src/
  lib/types.ts                     # EDIT — AskSourceItem에 선택 필드 없음; FeedbackVote 타입 추가
  lib/api.ts                       # EDIT — sendEvidenceFeedback()
  app/ask/page.tsx                 # EDIT — SourceCard에 좋아요/싫어요 + 낙관적 토글
  app/ask/page.test.tsx            # NEW 또는 EDIT
```

**마이그레이션 번호 주의:** 현재 최신은 `024_ask_sessions.sql`이다. 동시 작업(그래프 계열)이 `025`를 먼저 점유했을 수 있으므로, 착수 시점에 `ls migrations`로 확인하고 **비어 있는 두 개의 연속 번호**를 쓴다. 순서(feedback 보강 → weights history)만 유지하면 번호 자체는 무관하다.



## 4. 작업 목록

> Task 1~5는 **판정 능력**(레이블 수집 + 오염 없는 분할)을 만든다. Task 6~10은 그 위에서 최적화·승격을 얹는다. Task 5까지 끝나면 최적화기 없이도 "이 변경이 나아졌는가"에 답할 수 있어야 한다 — 그것이 이 계획의 1차 목표다(§1).

---

## Task 1: `feedback` additive 보강 마이그레이션

**Files:**
- Create: `migrations/025_feedback_evidence.sql`

**설계 메모**

기존 50행을 건드리지 않는다. 이 마이그레이션이 하는 일은 셋뿐이다: 컬럼 1개 추가, 값 backfill, 부분 인덱스 2개 생성. `DROP`·`ALTER TYPE`·`SET NOT NULL`은 **하나도 없다**.

- `split TEXT` — nullable. `CHECK`는 붙이되 `NOT VALID`로 걸어 기존 행 재검사를 피한다(기존 행은 어차피 backfill로 유효값이 들어가지만, 순서 의존을 없앤다).
- backfill의 해시 규칙은 §2.2 그대로: `md5('sbfeedback:v1:' || lower(btrim(query)))`의 앞 8 hex를 `bit(32)`로 캐스팅해 정수로 읽고 `% 100 < 70` → `train`.
  - PostgreSQL에서 hex→int는 `('x' || substr(md5(...),1,8))::bit(32)::bigint`가 관용구다. 결과는 부호 있는 값이 될 수 있으므로 `abs(...)`로 감싼다(Go 쪽도 uint32로 읽어 동일 결과가 되도록 Task 2에서 맞춘다 — **부호 처리 규칙을 양쪽이 반드시 동일하게** 한다).
- `query IS NULL` 또는 빈 문자열인 행은 분할 대상이 아니다. `split`을 NULL로 남긴다. 분할 로더는 NULL을 무시한다.
- 부분 UNIQUE 인덱스는 `WHERE source = 'ask_evidence'` 로 제한한다. 시드 50행의 `source`는 `'search'`/`'discord_bot'`/`'api'` 뿐이므로 대상 행이 0이고, 따라서 **중복 충돌로 이 마이그레이션이 실패할 수 없다**(§2.1).
- 멱등 키는 `(session_id, md5(normalized query), document_id)`다. 세션이 다르면 별개 레이블이다 — 같은 질문을 다른 대화에서 다시 물어봤다면 그건 독립 신호다.

```sql
ALTER TABLE feedback ADD COLUMN IF NOT EXISTS split TEXT;
ALTER TABLE feedback ADD CONSTRAINT feedback_split_chk
  CHECK (split IS NULL OR split IN ('train','holdout')) NOT VALID;

UPDATE feedback SET split = CASE
    WHEN abs(('x' || substr(md5('sbfeedback:v1:' || lower(btrim(query))),1,8))::bit(32)::bigint) % 100 < 70
    THEN 'train' ELSE 'holdout' END
WHERE split IS NULL AND query IS NOT NULL AND btrim(query) <> '';

CREATE UNIQUE INDEX IF NOT EXISTS uq_feedback_ask_evidence
  ON feedback (session_id, md5(lower(btrim(query))), document_id)
  WHERE source = 'ask_evidence';

CREATE INDEX IF NOT EXISTS idx_feedback_split
  ON feedback (split, thumbs) WHERE source = 'ask_evidence';
```

- [ ] **Step 1: 마이그레이션 파일 작성** — 위 SQL. 파일 상단에 "additive only" 주석과 근거(§2.1) 링크.
- [ ] **Step 2: 로컬 DB에서 dry-run**

Run: `psql "$TEST_DATABASE_URL" -1 -v ON_ERROR_STOP=1 -f migrations/025_feedback_evidence.sql`
Expected: 에러 없이 완료. `ALTER TABLE`/`UPDATE`/`CREATE INDEX` 4개 모두 성공.

- [ ] **Step 3: 파괴성 없음을 증명한다**

Run:
```bash
psql "$TEST_DATABASE_URL" -Atc "SELECT count(*) FROM feedback;"                  # 마이그레이션 전후 동일
psql "$TEST_DATABASE_URL" -Atc "SELECT split, count(*) FROM feedback GROUP BY 1;" # NULL 없음(query 있는 행 한정)
psql "$TEST_DATABASE_URL" -Atc "SELECT count(*) FROM feedback WHERE source='ask_evidence';" # 0
```
Expected: 행 수 불변, `train`/`holdout` 두 값만 등장(그리고 `query IS NULL` 행만 NULL), `ask_evidence` 0행.

- [ ] **Step 4: 재실행 멱등성** — 같은 파일을 두 번 적용해도 실패하지 않고 `split` 값이 바뀌지 않는지 확인한다.
- [ ] **Step 5: 커밋** — `git commit -m "feat(feedback): additive split column and evidence uniqueness index"`

---

## Task 2: 근거 단위 멱등 저장 (`UpsertEvidence`)

**Files:**
- Edit: `internal/store/feedback.go` (추가만 — 기존 `Record`/`Upsert`/`ListRecent`/`Stats`는 한 줄도 바꾸지 않는다)
- Create: `internal/store/feedback_evidence_test.go`

**Interfaces:**
- `type EvidenceVote struct { SessionID, Query, DocumentID string; Thumbs int16; UserID *string; Metadata map[string]any }`
- `func (s *FeedbackStore) UpsertEvidence(ctx context.Context, v EvidenceVote) (id int64, err error)`
- `func SplitOf(query string) string` (`internal/dataset/split.go`, Task 5에서 정의 — 여기서는 store가 그 패키지를 import한다)

**설계 메모**

- **기존 `Upsert`를 재사용하면 안 된다.** 그 함수는 `(user_id, session_id, source)`의 모든 행을 DELETE한다. 근거 카드에 쓰면 같은 대화에서 앞서 매긴 다른 문서의 레이블이 전부 지워진다(§2.1(a)). 새 함수를 만든다.
- 멱등은 DELETE가 아니라 `ON CONFLICT ... DO UPDATE`로 한다. 충돌 타깃은 Task 1의 부분 UNIQUE 인덱스와 **정확히 같은 식**이어야 한다:
  `ON CONFLICT (session_id, md5(lower(btrim(query))), document_id) WHERE source = 'ask_evidence'`
- **재클릭 토글:** 같은 값을 다시 보내면 `thumbs`를 0으로 되돌린다. 이 판단은 SQL에서 한다 — `DO UPDATE SET thumbs = CASE WHEN feedback.thumbs = EXCLUDED.thumbs THEN 0 ELSE EXCLUDED.thumbs END`. 왕복 조회 없이 원자적으로 처리된다.
- **`RETURNING`에서 `EXCLUDED`를 참조하지 않는다.** 과거에 이 프로젝트가 정확히 이 함정에 빠졌다. `RETURNING id, thumbs`만 쓴다(둘 다 대상 행의 컬럼).
- `split`은 INSERT 시 Go에서 계산해 넣는다. 갱신 시에는 건드리지 않는다 — 질의가 같으면 split도 같으므로 재계산이 무의미하고, 재계산하면 규칙 변경 시 과거 행이 흔들린다(§2.2).
- `thumbs = 0` 행은 남겨둔다. 삭제하지 않는다. "본 적 있으나 판단 보류"와 "본 적 없음"은 다른 정보이고, `BuildFromFeedback`은 `thumbs >= 1` / `= -1`만 보므로 0행은 자연히 무시된다.
- `metadata`에는 `{"rank": <카드 순위>, "layer": "<note|observed|insight>"}`를 넣는다. 나중에 위치 편향 보정을 하려면 순위가 필요하다. **질의 원문은 metadata에 중복 저장하지 않는다**(이미 `query` 컬럼에 있다).

- [ ] **Step 1: 실패하는 테스트를 쓴다**

`internal/store/feedback_evidence_test.go`. SQL은 스텁으로 검증할 수 없다 — 실 DB(`TEST_DATABASE_URL`, 미설정 시 `t.Skip`)에 넣고 `t.Cleanup`에서 지운다. 기존 store 테스트의 연결 헬퍼를 재사용한다.

```go
func TestUpsertEvidence_SecondDocInSameSessionIsNotDeleted(t *testing.T)
// 같은 session/query로 docA(+1), docB(-1) 저장 → 두 행 모두 남아 있어야 한다.
// 기존 Upsert 였다면 docA 행이 사라진다. 이 테스트가 §2.1(a)의 회귀 가드다.

func TestUpsertEvidence_RepeatSameVoteTogglesToZero(t *testing.T)
// docA에 +1 두 번 → 행은 1개, thumbs = 0.

func TestUpsertEvidence_OppositeVoteOverwrites(t *testing.T)
// docA에 +1 후 -1 → 행은 1개, thumbs = -1.

func TestUpsertEvidence_DoesNotTouchLegacyRows(t *testing.T)
// source='search' 시드 행을 넣고 evidence upsert 수행 → 시드 행 count 불변.

func TestUpsertEvidence_SplitMatchesSQLBackfill(t *testing.T)
// Go SplitOf(q) 와 Task 1 backfill SQL 식의 결과가 같은 질의 20개에 대해 일치.
// 이것이 Go/SQL 해시 규칙 동치성의 유일한 방어선이다.
```

- [ ] **Step 2: 실패 확인** — Run: `go test ./internal/store/ -run TestUpsertEvidence -v` → FAIL (`undefined: UpsertEvidence`).
- [ ] **Step 3: 구현** — `UpsertEvidence` 추가. `source`는 상수 `SourceAskEvidence = "ask_evidence"`로 고정하고 호출자가 바꿀 수 없게 한다(부분 인덱스가 이 값에 의존한다).
- [ ] **Step 4: 통과 확인** — Run: `go test ./internal/store/ -run TestUpsertEvidence -v` → PASS. 이어서 `go test ./internal/store/` 전체로 기존 feedback 테스트 회귀 없음 확인.
- [ ] **Step 5: 커밋** — `git commit -m "feat(feedback): idempotent per-evidence upsert with deterministic split"`

---

## Task 3: 피드백 수집 API — `POST /api/v1/feedback/evidence`

**Files:**
- Create: `internal/api/feedback_evidence.go`, `internal/api/feedback_evidence_test.go`
- Edit: `internal/api/server.go` (필드 1개 + 라우트 1줄). **`internal/api/router.go`는 동시 작업 중이므로 열지 않는다** — 라우트 등록 지점이 `router.go`에만 있다면 그 파일을 수정하는 대신, `server.go`에 `RegisterFeedbackEvidenceRoutes(mux)` 형태의 등록 함수를 두고 착수 시점에 동시 작업이 끝난 뒤 한 줄로 연결한다.

**Interfaces:**
- 요청: `{ "conversation_id": string, "query": string, "document_id": string, "thumbs": -1|0|1, "rank": int, "layer": string }`
- 응답 `200`: `{ "thumbs": -1|0|1 }` — **서버가 판정한 최종 상태**를 돌려준다. 클라이언트가 토글 결과를 추측하지 않게 하기 위함이다.
- `type EvidenceVoter interface { UpsertEvidence(ctx, store.EvidenceVote) (int64, error) }` — 스텁 주입용.

**설계 메모**

- **기존 `POST /api/v1/feedback`은 그대로 둔다.** 그 핸들러는 `Record`(항상 INSERT)를 호출하는 범용 경로이고 Discord 봇/검색 UI가 쓰고 있다. 근거 단위 경로는 요구사항(멱등, 질의 필수, source 고정)이 다르므로 별도 엔드포인트가 맞다. 기존 핸들러에 분기를 심으면 `source` 값에 따라 멱등성이 달라지는 숨은 계약이 생긴다.
- **인증은 새로 만들지 않는다.** `web/src/proxy.ts`의 Cloudflare Access JWT 검증이 `/api/*`를 이미 감싼다. 핸들러는 인증을 가정한다.
- **`user_id`는 서버가 채우지 않는다.** Access가 넘겨주는 신원을 굳이 레이블에 붙일 이유가 없다(단일 사용자 시스템). NULL로 둔다 — 개인 데이터 최소화.
- **검증:** `conversation_id`/`query`/`document_id` 빈 값 → 400. `thumbs` 범위 밖 → 400. `document_id`가 UUID 형식이 아니면 → 400(DB FK 에러를 500으로 흘리지 않는다).
- **기능 플래그:** `FEEDBACK_EVIDENCE_ENABLED`(기본 off). off일 때 이 엔드포인트는 **404**를 반환한다(501이 아니라 404 — 존재를 드러내지 않는다). `os.Getenv`로 읽는다(`internal/config/config.go`는 동시 작업 중이라 수정 금지, Global Constraints).
- **관측:** 핸들러에서 OTel 스팬 `feedback.evidence`를 연다(`internal/telemetry` 재사용). 속성은 `query_hash`(md5 앞 8자), `thumbs`, `rank`, `layer`, `split`. **질의 원문·문서 제목은 스팬에 넣지 않는다**(Global Constraints, 개인 데이터).
- 저장 실패는 500이되 로그에도 질의 원문을 남기지 않는다 — `query_hash`만.

- [ ] **Step 1: 실패하는 테스트를 쓴다**

`internal/api/feedback_evidence_test.go`. 스텁 `EvidenceVoter`로 DB 없이 검증한다(여기서 검증하는 것은 HTTP 계약이지 SQL이 아니다 — SQL은 Task 2가 실 DB로 검증했다).

```go
func TestEvidenceHandler_DisabledReturns404(t *testing.T)   // 플래그 off → 404, 스텁 호출 0회
func TestEvidenceHandler_RejectsBadThumbs(t *testing.T)     // thumbs=2 → 400
func TestEvidenceHandler_RejectsNonUUIDDocument(t *testing.T)
func TestEvidenceHandler_RejectsEmptyQuery(t *testing.T)
func TestEvidenceHandler_PassesRankAndLayerToMetadata(t *testing.T) // 스텁이 받은 Metadata에 rank/layer 존재
func TestEvidenceHandler_ReturnsServerResolvedThumbs(t *testing.T)  // 스텁이 0을 리턴하면 응답도 0
```

- [ ] **Step 2: 실패 확인** — Run: `go test ./internal/api/ -run TestEvidenceHandler -v` → FAIL.
- [ ] **Step 3: 구현** — 핸들러 + 등록 함수. 플래그 체크를 핸들러 **최상단**에 둔다(디코딩보다 먼저).
- [ ] **Step 4: 통과 확인** — Run: `go test ./internal/api/ -v -run TestEvidenceHandler` → PASS. 이어서 `go build ./...`.
- [ ] **Step 5: 수동 계약 확인 (로컬 서버)**

Run:
```bash
curl -s -o /dev/null -w '%{http_code}\n' -XPOST localhost:8080/api/v1/feedback/evidence -d '{}'   # 플래그 off → 404
FEEDBACK_EVIDENCE_ENABLED=1 <서버 재기동 후>
curl -s -XPOST localhost:8080/api/v1/feedback/evidence \
  -H 'content-type: application/json' \
  -d '{"conversation_id":"c1","query":"테스트 질의","document_id":"<유효 UUID>","thumbs":1,"rank":1,"layer":"observed"}'
# → {"thumbs":1}  같은 요청 재전송 → {"thumbs":0}
```

- [ ] **Step 6: 커밋** — `git commit -m "feat(api): per-evidence feedback endpoint behind FEEDBACK_EVIDENCE_ENABLED"`

---

## Task 4: `/ask` 근거 카드 좋아요/싫어요 UI

**Files:**
- Edit: `web/src/lib/types.ts` (`EvidenceVote`, `EvidenceFeedbackRequest` 타입)
- Edit: `web/src/lib/api.ts` (`sendEvidenceFeedback`)
- Edit: `web/src/app/ask/page.tsx` (`SourceCard`, `SourceLayerGroup`, `AssistantBubble` 프롭 배선)
- Create: `web/src/app/ask/evidenceFeedback.test.ts`

**설계 메모**

- **문서 단위여야 한다.** 답변 단위 좋아요는 `(query, thumbs)`뿐이라 어떤 문서가 좋았는지 알 수 없고, 레이블로 쓸 수 없다. 버튼은 반드시 `SourceCard` 안에 있어야 한다(요구사항 1).
- `SourceCard`는 현재 `{ source }`만 받는다. `{ source, rank, conversationId, query, vote, onVote }`로 확장한다. `rank`는 **레이어 필터링 전 원본 `turn.sources` 배열의 인덱스**여야 한다 — 레이어별로 나눈 뒤의 인덱스를 쓰면 실제 노출 순위와 어긋난다. `AssistantBubble`에서 `sourcesByLayer`를 만들기 전에 `turn.sources`에 대해 `rank` 맵을 먼저 만든다.
- **`query`는 그 턴의 질문**(`turn.question`)이지 현재 입력창 값이 아니다. 과거 턴의 카드에 나중에 투표할 수 있으므로 반드시 턴에서 읽는다.
- **투표 상태는 턴 로컬 state**로 둔다: `Record<string /*docId*/, -1|0|1>`을 `Turn`에 추가하거나 `page.tsx` 상단에 `Map<turnIndex, Record<docId, vote>>`로 둔다. 서버에서 기존 투표를 되읽어오지 않는다 — 대화 재로드 시 투표 표시가 비는 것은 v1에서 허용한다(요구사항은 수집이지 표시 복원이 아니다). 이 절충을 코드 주석에 남긴다.
- **낙관적 갱신 + 서버 판정 반영:** 클릭 즉시 로컬 상태를 바꾸되, 응답의 `thumbs`로 **덮어쓴다**. 토글 판정의 단일 진실 공급원은 서버다(Task 3). 실패 시 이전 값으로 되돌리고 카드에 조용한 에러 틴트만 준다 — 토스트를 띄우지 않는다(대화 흐름을 끊는다).
- **접근성:** `aria-pressed`로 현재 투표 상태를 노출하고 `aria-label`은 `"이 근거가 도움이 됐어요"` / `"도움이 안 됐어요"`. 아이콘만 두지 않는다.
- **비활성화 시 렌더링:** `NEXT_PUBLIC_FEEDBACK_EVIDENCE_ENABLED !== "1"`이면 버튼을 아예 렌더링하지 않는다. 서버 플래그와 별개로 UI 쪽 게이트가 있어야 백엔드 배포 전에 프런트가 404를 때리지 않는다.
- **패키지 매니저는 bun.** `npm install` 금지 — `package-lock.json`이 생기면 CI(`bun --frozen-lockfile`)가 깨진다.

- [ ] **Step 1: 실패하는 테스트를 쓴다**

`web/src/app/ask/evidenceFeedback.test.ts`. DOM 렌더링 전체가 아니라 **순수 함수 두 개**를 뽑아 검증한다 — 그래야 테스트가 값싸고 회귀에 강하다.

```ts
// page.tsx에서 export 하는 순수 헬퍼
export function buildRankMap(sources: AskSourceItem[]): Record<string, number>
export function nextVote(current: -1|0|1|undefined, clicked: 1|-1): -1|0|1  // 낙관적 예측값

describe("buildRankMap", () => {
  it("레이어와 무관하게 원본 배열 순서로 0-based rank를 매긴다");
  it("중복 문서 ID가 있으면 첫 등장 순위를 유지한다");
});
describe("nextVote", () => {
  it("같은 방향 재클릭은 0으로 토글한다");
  it("반대 방향 클릭은 그 값으로 덮어쓴다");
  it("undefined에서 클릭하면 클릭 값이 된다");
});
```

- [ ] **Step 2: 실패 확인** — Run: `cd web && bun test evidenceFeedback` → FAIL.
- [ ] **Step 3: 구현** — 헬퍼 → `sendEvidenceFeedback` → `SourceCard` 버튼 → 프롭 배선 순서로.
- [ ] **Step 4: 통과 확인**

Run: `cd web && bun test evidenceFeedback && bunx tsc --noEmit && bun run lint`
Expected: 전부 통과. **`bun.lockb` 외의 lockfile이 생기지 않았는지 `git status`로 확인한다.**

- [ ] **Step 5: 브라우저 렌더 검증 (R020 — 타입체크만으로는 완료가 아니다)**

Run: `cd web && bun run dev` → `/ask`에서 질문 1건 → 근거 그룹 펼침 → 카드에 버튼 2개 보임 → 좋아요 클릭 시 상태 반영, 재클릭 시 해제. 콘솔 에러 0. 플래그를 끈 상태에서 버튼이 사라지는 것도 함께 확인한다.

- [ ] **Step 6: 커밋** — `git commit -m "feat(ask): per-evidence thumbs on source cards"`

---

## Task 5: `internal/dataset` — 고정 분할과 **구조적** 검증셋 격리

**Files:**
- Create: `internal/dataset/split.go`, `dataset.go`, `runner.go`
- Create: `internal/dataset/split_test.go`, `dataset_test.go`, `api_surface_test.go`

**Interfaces:**

```go
func SplitOf(query string) string            // "train" | "holdout"; 빈 질의는 ""
func Normalize(query string) string          // strings.ToLower(strings.TrimSpace(q))

type Source interface {                       // store.EvalStore가 만족
    EvalPairsBySplit(ctx context.Context, split string) ([]store.EvalPair, error)
}
func Load(ctx context.Context, src Source) (*TrainSet, *HoldoutSet, error)

func (t *TrainSet) Pairs() []store.EvalPair   // 좌표 탐색은 원본이 필요하다 — 노출한다
func (t *TrainSet) Queries() int

type RunFunc func(ctx context.Context, query string, k int) ([]string, error)
type Metrics struct { NDCG10, MRR10, FPPenalty10, Objective float64; Pairs, Queries int }

func (h *HoldoutSet) Evaluate(ctx context.Context, run RunFunc) (Metrics, error)
func (h *HoldoutSet) Queries() int             // 개수만. 질의 문자열이 아니다.
var ErrHoldoutBudgetExhausted = errors.New(...)
```

**격리를 코드가 어떻게 막는가 — 네 겹 (§2.3)**

1. **분리 로더.** `Load`가 `WHERE split='train'`과 `WHERE split='holdout'`을 **별개 쿼리 두 번**으로 읽는다. "전부 읽어 나중에 나눈다"는 경로가 존재하지 않으므로, 실수로 전체를 최적화기에 넘길 방법이 없다. `Source` 인터페이스 자체가 split 인자를 요구한다.
2. **필드 unexported + 접근자 부재.** `HoldoutSet{ pairs []store.EvalPair; budget int }` — 전부 소문자. exported 메서드는 `Evaluate`와 `Queries` **둘뿐**이고 어느 쪽도 `EvalPair`·질의 문자열·문서 ID를 반환하지 않는다. `internal/tune`은 다른 패키지이므로 Go 컴파일러가 접근을 **차단한다**. 규약이 아니라 타입 시스템이 막는 것이다.
3. **평가 예산.** `budget` 기본 2(baseline 1 + candidate 1). `Evaluate`가 호출될 때마다 감소하고 0에서 `ErrHoldoutBudgetExhausted`를 반환한다. holdout을 반복 프로빙해 사실상 학습셋으로 쓰는 우회를 막는다. 예산은 생성자에서만 정해지고 exported setter가 없다.
4. **API 표면 고정 테스트.** 리플렉션으로 `HoldoutSet`의 exported 메서드 집합이 정확히 `{Evaluate, Queries}`임을 단언한다. 누군가 나중에 편의상 `Pairs()`를 추가하면 **테스트가 깨지고** 그 순간 격리가 무너졌음이 드러난다.

`Metrics.Objective = NDCG10 − 0.5 × FPPenalty10`(§2.4). 계산은 `internal/search`의 기존 `NDCGK`/`MRRK`/`FalsePositivePenalty`를 호출한다 — 새 지표를 만들지 않는다.

`EvalPairsBySplit`은 `store.EvalStore`에 추가한다. 기존 `BuildFromFeedback`을 복제하지 말고 **split 필터를 인자로 받는 내부 쿼리로 리팩터**하되, `BuildFromFeedback()`은 시그니처를 유지한 얇은 래퍼로 남긴다(호출자 회귀 방지).

- [ ] **Step 1: 실패하는 테스트를 쓴다**

```go
// split_test.go
func TestSplitOf_Deterministic(t *testing.T)          // 같은 질의 1000회 → 같은 값
func TestSplitOf_NormalizationInsensitive(t *testing.T) // " Foo "와 "foo"가 같은 split
func TestSplitOf_RoughlySeventyThirty(t *testing.T)   // 10k 랜덤 질의 → train 비율 0.65~0.75
func TestSplitOf_KnownVectors(t *testing.T)           // 하드코딩 5쌍 — 규칙이 바뀌면 즉시 깨진다

// dataset_test.go
func TestLoad_QueriesEachSplitSeparately(t *testing.T) // 스텁 Source가 받은 split 인자 = ["train","holdout"]
func TestLoad_NoQueryAppearsInBothSets(t *testing.T)   // TrainSet.Pairs()의 질의 ∩ holdout 질의 = ∅
                                                       // (스텁 Source 쪽에서 대조 — HoldoutSet은 안 열린다)
func TestHoldout_BudgetExhausted(t *testing.T)         // 3번째 Evaluate → ErrHoldoutBudgetExhausted
func TestHoldout_EvaluateReturnsScalarsOnly(t *testing.T)

// api_surface_test.go
func TestHoldoutSet_ExportedSurfaceIsFrozen(t *testing.T) {
    got := exportedMethodNames(reflect.TypeOf(&HoldoutSet{}))
    want := []string{"Evaluate", "Queries"}
    // 불일치 시: "HoldoutSet의 공개 표면이 바뀌었다. 검증셋 격리(§2.3)가 깨졌을 수 있다."
}
```

- [ ] **Step 2: 실패 확인** — Run: `go test ./internal/dataset/ -v` → FAIL (패키지 없음).
- [ ] **Step 3: 구현** — `split.go`(md5 → uint32 → %100, **Task 1 SQL과 부호 처리 동일**) → `dataset.go` → `runner.go` 순.
- [ ] **Step 4: 통과 확인** — Run: `go test ./internal/dataset/ ./internal/store/ -v` → PASS.
- [ ] **Step 5: 격리를 컴파일러로 증명한다**

Run: 임시 파일 `internal/tune/leak_check.go`에 `_ = h.pairs` 및 `_ = h.Pairs()`를 써 넣고 `go build ./internal/tune/`.
Expected: **컴파일 실패** (`h.pairs undefined (cannot refer to unexported field)`, `h.Pairs undefined`). 확인 후 파일 삭제. 이 단계를 건너뛰면 §2.3은 주장에 머문다.

- [ ] **Step 6: 커밋** — `git commit -m "feat(dataset): deterministic split with closed-form holdout isolation"`

---

## Task 6: 가중치 후보 평가 하네스 (`RunFunc` 어댑터)

**Files:**
- Create: `internal/tune/harness.go`, `internal/tune/harness_test.go`

**Interfaces:**
- `type Searcher interface { WithWeights(model.SearchWeights) *search.Service; Search(ctx, search.Query) (search.Results, error) }` — 실제 타입에 맞춰 착수 시 조정한다.
- `func RunnerFor(svc *search.Service, w model.SearchWeights) dataset.RunFunc`
- `func Objective(m dataset.Metrics) float64` — `m.NDCG10 - 0.5*m.FPPenalty10`

**설계 메모**

- 이것이 "변경이 나아졌는가"에 답하는 실제 기계다. Task 5가 자로 눈금을 새겼다면 이 Task는 자를 대상에 대는 손이다.
- `WithWeights`는 `*Service`를 **뮤테이트하고 자기 자신을 반환**한다(`internal/search/search.go`). 후보마다 같은 인스턴스를 재사용하면 이전 후보의 가중치가 남는 오염이 생긴다. **후보마다 서비스 인스턴스를 새로 조립**하거나, 최소한 매 호출 직전에 `WithWeights`를 다시 적용한다. 어느 쪽을 택했는지 주석에 남긴다.
- **`UseRerank=false` 고정**(§2.4). 배치 계층에서 리랭커 HTTP 도달성을 보장할 수 없다. baseline과 candidate를 동일 조건에서 재므로 상대 비교의 타당성은 유지된다.
- **HyDE도 끈다.** LLM 호출이 들어가면 같은 질의가 실행마다 다른 결과를 내 재현성이 사라지고, 수백 회 탐색에 원격 API 비용이 붙는다.
- `k`는 10 고정(NDCG@10/FP@10과 맞춘다). 검색은 상위 10개 문서 ID만 순서대로 뽑아 반환한다.
- **동시성:** 질의별 평가를 `errgroup`으로 최대 4 병렬. Postgres 커넥션 풀 크기를 넘지 않게 상한을 상수로 둔다. 결과 순서는 인덱스로 복원한다(맵 순회 순서에 의존하지 않는다 — 결정성).
- 검색 에러는 삼키지 않는다. 한 질의라도 실패하면 `Evaluate` 전체를 에러로 끝낸다. 부분 실패를 0점으로 처리하면 "가중치가 나쁘다"와 "DB가 흔들렸다"가 구분되지 않는다.

- [ ] **Step 1: 실패하는 테스트** — 스텁 `Searcher`로:
  - `TestRunnerFor_AppliesWeightsPerCandidate` — 서로 다른 가중치 2개를 연속 실행하면 스텁이 받은 weights가 각각 다르다(오염 없음).
  - `TestRunnerFor_ForcesRerankAndHyDEOff` — 스텁이 받은 Query의 `UseRerank`/`UseHyDE`가 false.
  - `TestObjective_PenalisesFalsePositives` — FP가 커질수록 objective가 단조 감소.
  - `TestRunnerFor_PropagatesSearchError` — 스텁 에러 → 에러 전파.
- [ ] **Step 2: 실패 확인** — Run: `go test ./internal/tune/ -v` → FAIL.
- [ ] **Step 3: 구현.**
- [ ] **Step 4: 통과 확인** — Run: `go test ./internal/tune/ ./internal/dataset/ -v` → PASS.
- [ ] **Step 5: 커밋** — `git commit -m "feat(tune): deterministic search harness for weight evaluation"`

---

## Task 7: 가중치 이력 테이블과 스토어

**Files:**
- Create: `migrations/026_search_weights_history.sql`
- Create: `internal/store/weights_history.go`, `internal/store/weights_history_test.go`

**Interfaces:**
- `func (s *WeightsHistoryStore) Insert(ctx, rec WeightsRecord) (int64, error)`
- `func (s *WeightsHistoryStore) Active(ctx) (*WeightsRecord, error)` — 없으면 `(nil, nil)`
- `func (s *WeightsHistoryStore) Promote(ctx, id int64) error` — 기존 active를 `superseded`로, 대상을 `active`로 **한 트랜잭션 안에서**
- `func (s *WeightsHistoryStore) Rollback(ctx) (*WeightsRecord, error)` — 현재 active를 `rolled_back`, 직전 `superseded`를 `active`로

**스키마 (migration 026)**

```sql
CREATE TABLE IF NOT EXISTS search_weights_history (
    id              BIGSERIAL PRIMARY KEY,
    weights         JSONB       NOT NULL,          -- model.SearchWeights 직렬화
    status          TEXT        NOT NULL,          -- proposed|active|rolled_back|superseded
    train_objective DOUBLE PRECISION,
    holdout_ndcg10  DOUBLE PRECISION,
    holdout_fp10    DOUBLE PRECISION,
    baseline_ndcg10 DOUBLE PRECISION,
    baseline_fp10   DOUBLE PRECISION,
    holdout_queries INTEGER     NOT NULL DEFAULT 0,
    gate_reason     TEXT,                          -- 승격 거부 사유 (사람이 읽는다)
    metadata        JSONB       NOT NULL DEFAULT '{}'::jsonb,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    promoted_at     TIMESTAMPTZ,
    CHECK (status IN ('proposed','active','rolled_back','superseded'))
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_search_weights_active
  ON search_weights_history ((status)) WHERE status = 'active';
CREATE INDEX IF NOT EXISTS idx_search_weights_created
  ON search_weights_history (created_at DESC);
```

**설계 메모**

- **`active`가 둘일 수 없음을 DB가 강제한다**(부분 UNIQUE 인덱스). 애플리케이션 조건문에 맡기면 동시 실행 두 번에 깨진다.
- `Promote`는 반드시 하나의 트랜잭션. 순서는 (1) 기존 active → `superseded`, (2) 대상 → `active` + `promoted_at=now()`. 순서를 뒤집으면 부분 UNIQUE 인덱스가 중간 상태에서 터진다.
- `metadata`에 `{"rerank": false, "train_holdout_gap": <float>, "sweeps": <int>, "tool_version": "..."}`를 남긴다. `train_holdout_gap`은 과적합 사후 감시용이다(§2.4(c)).
- 승격되지 못한 후보도 **반드시 `proposed`로 기록**한다. 기록하지 않으면 "왜 안 올라갔는가"를 나중에 물을 수 없다. `gate_reason`에 사람이 읽을 문장을 넣는다(예: `"holdout distinct queries 12 < 30"`).
- 개인 데이터 금지: 질의 원문·문서 ID를 이 테이블에 넣지 않는다. 집계 스칼라만.

- [ ] **Step 1: 실패하는 테스트** (실 DB, `t.Cleanup`으로 정리):
  - `TestPromote_LeavesExactlyOneActive` — 3회 연속 promote 후 `status='active'` 행이 정확히 1개.
  - `TestPromote_IsAtomicUnderDuplicateActive` — 수동으로 active 2행을 넣으려 하면 **DB가 거부**한다.
  - `TestRollback_RestoresPreviousActive` — promote A → promote B → rollback → active == A, B는 `rolled_back`.
  - `TestRollback_NoPreviousReturnsNilNoError` — 되돌릴 행 없으면 active 없음 상태로 복귀.
  - `TestActive_ReturnsNilWhenEmpty`.
- [ ] **Step 2: 실패 확인** — Run: `go test ./internal/store/ -run TestPromote -run TestRollback -v` → FAIL.
- [ ] **Step 3: 마이그레이션 적용 + 구현** — Run: `psql "$TEST_DATABASE_URL" -1 -v ON_ERROR_STOP=1 -f migrations/026_search_weights_history.sql`
- [ ] **Step 4: 통과 확인** — Run: `go test ./internal/store/ -v` → PASS.
- [ ] **Step 5: 커밋** — `git commit -m "feat(store): search weights history with single-active invariant"`

---

## Task 8: 좌표 탐색 최적화기 + 승격 게이트

**Files:**
- Create: `internal/tune/coordinate.go`, `internal/tune/gate.go`
- Create: `internal/tune/coordinate_test.go`, `internal/tune/gate_test.go`

**Interfaces:**

```go
type SearchSpace struct{ FTS, Vec, Bigm, SummaryVec, Entity []float64; RRFK []float64 }
func DefaultSpace() SearchSpace
type Result struct { Best model.SearchWeights; BestObjective float64; Sweeps, Evals int; Trace []string }
func Coordinate(ctx context.Context, train *dataset.TrainSet,
    base model.SearchWeights, space SearchSpace,
    eval func(context.Context, model.SearchWeights) (dataset.Metrics, error)) (Result, error)

func Eligible(holdoutQueries int) (bool, string)
type GateInput struct{ BaseNDCG10, CandNDCG10, BaseFP10, CandFP10 float64; HoldoutQueries int }
func PassesPromotion(in GateInput) (bool, string)
```

**설계 메모**

- **탐색 공간**(§2.4): `FTS/Vec/Bigm ∈ [0.25, 2.0] step 0.25`, `SummaryVec ∈ [0.05, 1.5] step 0.25`, `Entity ∈ [0.05, 1.5] step 0.25`, `RRFK ∈ {20,40,60,80,120}`. 좌표 6개, sweep당 약 45회 평가.
- **`SummaryVec`의 하한이 0이 아니라 0.05인 것이 핵심이다.** `SearchWeights.Defaults()`에서 0은 "미설정 → 커버리지 게이트에 위임"이라는 **별도 의미**를 갖는다(`internal/model/document.go`). 최적화기가 0을 후보로 내면 의도와 다른 동작이 되고, 그 결과를 "최적 가중치"라 부르면 거짓말이 된다. 레인을 끄고 싶으면 `DisableSummaryVec=true`를 **별도 후보 1개**로 따로 평가한다. `Entity`도 음수가 "명시적 비활성"이므로 하한을 양수로 둔다.
- **종료 조건:** 한 sweep의 개선폭이 `< 1e-4`이거나 sweep 3회. 둘 다 상수로 두고 exported 하지 않는다 — 늘리고 싶은 유혹이 곧 과적합이다.
- **탐색은 train만 본다.** `Coordinate`의 시그니처에 `*dataset.HoldoutSet`이 **없다**. holdout을 넣을 자리가 아예 없으므로 실수로 넘길 수 없다.
- **결정성:** 좌표 순회 순서와 후보 값 순서를 고정한다. 동점이면 **먼저 나온 값**을 유지한다(맵 순회·부동소수 비교로 결과가 흔들리지 않게).
- **승격 게이트**(§2.5): `PassesPromotion`은 (a) `Eligible(HoldoutQueries)`, (b) `CandNDCG10 - BaseNDCG10 >= 0.01`, (c) `CandFP10 - BaseFP10 <= 0.02` 셋을 모두 요구한다. **거부 사유 문자열을 반드시 반환**한다 — 이력의 `gate_reason`에 그대로 들어간다.
- `Eligible`을 별도 함수로 뽑는 이유: 30 질의 게이트는 `if`문 하나로 두면 나중에 "이번만"이라며 우회된다. 독립 함수 + 독립 테스트로 못박는다.

- [ ] **Step 1: 실패하는 테스트**
  - `TestCoordinate_FindsKnownOptimum` — 합성 objective(예: `-(fts-1.5)²-(vec-0.5)²`)를 주면 격자 위 최적점에 수렴.
  - `TestCoordinate_NeverProposesZeroSummaryVec` — 전 탐색 궤적에서 `SummaryVec == 0`인 후보가 나오지 않는다.
  - `TestCoordinate_TerminatesWithinThreeSweeps` — 개선이 없는 평평한 objective에서도 유한 종료(`Sweeps <= 3`).
  - `TestCoordinate_IsDeterministic` — 같은 입력 5회 → 같은 `Best`.
  - `TestEligible_Under30Rejects` / `TestEligible_At30Accepts` — 경계값 29/30.
  - `TestPassesPromotion_RejectsSmallGain`(+0.005), `_RejectsFPRegression`(+0.03), `_AcceptsClearWin`, `_ReasonIsNonEmptyOnReject`.
- [ ] **Step 2: 실패 확인** — Run: `go test ./internal/tune/ -run 'TestCoordinate|TestEligible|TestPasses' -v` → FAIL.
- [ ] **Step 3: 구현.**
- [ ] **Step 4: 통과 확인** — Run: `go test ./internal/tune/ -v` → PASS.
- [ ] **Step 5: holdout 미접근을 컴파일러로 재확인** — `Coordinate` 시그니처에 `HoldoutSet`이 없고, `internal/tune` 어디에서도 `dataset.HoldoutSet`의 unexported 필드를 참조하지 않음을 `grep -rn "HoldoutSet" internal/tune/`로 확인한다. `cmd/tune`(Task 9)의 `Evaluate` 호출 외에는 등장하지 않아야 한다.
- [ ] **Step 6: 커밋** — `git commit -m "feat(tune): coordinate search with explicit promotion gate"`

---

## Task 9: 관측 (Langfuse 계측)

**Files:**
- New: `internal/telemetry/feedback_attrs.go` — 신규 span 이름·속성 키 상수
- New: `internal/telemetry/feedback_attrs_test.go` — 속성 키 허용목록 고정 테스트
- Edit: `internal/api/feedback_evidence.go`(Task 5 산출물) — 근거 카드 피드백 기록 시 span 1개
- Edit: `internal/api/feedback_evidence_test.go`(Task 5 산출물) — span 방출 테스트 추가
- **호출 계약만 명시**(파일이 아직 없어 이 Task의 편집 대상이 아님): `cmd/tune/main.go`가 구현될 때 아래 세 지점에 반드시 넣는다 — `dataset.Load` 직후, `tune.Coordinate` 반환 직후, `gate.PassesPromotion` 판정 직후. `internal/store/weights_history.go`의 promote/rollback 처리 끝에도 한 지점 필요.

**Interfaces:**

```go
// internal/telemetry/feedback_attrs.go
const (
    SpanFeedbackEvidence  = "feedback.evidence.recorded"
    SpanTuneDatasetLoaded = "tune.dataset.loaded"
    SpanTuneCoordinate    = "tune.coordinate.completed"
    SpanTunePromotion     = "tune.promotion.decision"
    SpanWeightsAction     = "weights.action.applied"
)

const (
    AttrFeedbackDocumentID = "feedback.document_id" // doc UUID, PII 아님
    AttrFeedbackThumbs     = "feedback.thumbs"       // -1 | 1
    AttrFeedbackSplit      = "feedback.split"        // "train" | "holdout"
    AttrTuneTrainQueries   = "tune.train_queries"
    AttrTuneHoldoutQueries = "tune.holdout_queries"
    AttrTuneTotalRows      = "tune.total_feedback_rows"
    AttrTuneSweeps         = "tune.sweeps"
    AttrTuneEvals          = "tune.evals"
    AttrTuneBaseObjective  = "tune.base_objective"
    AttrTuneBestObjective  = "tune.best_objective"
    AttrTuneGatePassed     = "tune.gate_passed"
    AttrTuneGateReason     = "tune.gate_reason"      // gate.go/coordinate.go 고정 enum만
    AttrTuneNDCGGain       = "tune.ndcg_gain"
    AttrTuneFPDelta        = "tune.fp_delta"
    AttrWeightsHistoryID   = "weights.history_id"    // int64 PK
    AttrWeightsAction      = "weights.action"        // "propose"|"promote"|"rollback"
    AttrWeightsPreviousID  = "weights.previous_history_id"
)
```

**설계 메모**

- **왜 span 속성이지 별도 메트릭 시스템이 아닌가.** `internal/telemetry`는 이미 OTel 트레이스만 Langfuse OTLP(`/v1/traces`)로 내보낸다(`otel.go`) — 메트릭 익스포터는 없다. 새 파이프라인 대신 기존 `tracer()` 패턴(`internal/llm/client.go:36,48`)을 재사용한다. 승격 이벤트마다 `tune.ndcg_gain`을 span 속성으로 실으면, Langfuse 트레이스 타임라인 자체가 "검증셋 점수 추이" 그래프가 된다 — 별도 대시보드가 필요 없다.
- **PII 회피가 이 Task의 존재 이유다.** Global Constraints(§0)는 로그·Langfuse에 `query_hash`(md5 앞 8자)까지는 허용하지만, **이 Task는 그보다 보수적으로 간다.** Langfuse는 외부 호스팅 서비스이고 보존 기간이 로컬 로그보다 길다 — 트레이스 속성에는 `query_hash`조차 싣지 않는다. `feedback.document_id`(문서 UUID)와 집계 스칼라만 싣는다. 재현이 필요하면 DB의 `feedback.split` 컬럼과 `search_weights_history`만으로 충분하다.
- **`tune.gate_reason`은 자유 텍스트가 아니라 `gate.go`/`coordinate.go`(Task 8)가 반환하는 고정 enum 문자열이다.** 자유 텍스트를 속성에 실으면 어딘가에서 사용자 입력이 우연히 섞여 들어갈 경로가 열린다 — enum 고정으로 그 경로를 원천 차단한다.
- **속성 키 허용목록 테스트.** `feedback_attrs_test.go`는 `go/parser`로 이 파일의 `const` 블록을 읽어 문자열 값 전체 집합이 하드코딩된 `attributeKeyAllowlist`와 **정확히 일치**하는지 검사한다(§2.3 리플렉션 기반 API 표면 테스트와 동일 패턴). 새 속성을 추가하려는 사람은 테스트와 allowlist를 함께 고쳐야 하므로 "일단 문서 제목도 넣자" 같은 실수가 CI에서 걸린다.
- `feedback.thumbs ∈ {-1,1}`, `feedback.split ∈ {"train","holdout"}` — 둘 다 §2.1/§2.2에서 이미 확정된 닫힌 도메인이라 새로운 노출면이 아니다.

- [ ] **Step 1: 실패하는 테스트**
  - `TestAttrKeys_MatchAllowlist` — `internal/telemetry`의 `const` 블록 파싱 결과가 하드코딩 allowlist와 정확히 일치.
  - `TestFeedbackEvidence_EmitsSpan` — 근거 카드 피드백 핸들러 호출 후, 테스트 전용 in-memory span recorder(`sdktrace.NewTracerProvider` + `tracetest.NewInMemoryExporter`)에 `SpanFeedbackEvidence` 1개, 속성 3개(`document_id`/`thumbs`/`split`)만 존재.
  - `TestFeedbackEvidence_SpanHasNoQueryAttribute` — 방출된 span의 속성 키 중 `"query"`를 부분 문자열로 포함하는 키가 **없다**.
- [ ] **Step 2: 실패 확인** — Run: `go test ./internal/telemetry/... ./internal/api/... -run 'TestAttrKeys|TestFeedbackEvidence_Emits|TestFeedbackEvidence_SpanHasNo' -v` → FAIL.
- [ ] **Step 3: 구현.** `internal/telemetry/feedback_attrs.go` 상수 정의 + `internal/api/feedback_evidence.go`에 `tracer().Start(ctx, telemetry.SpanFeedbackEvidence, oteltrace.WithAttributes(...))` 삽입(기존 `tracerName`/`tracer()` 패턴 재사용, `internal/api` 전용 tracerName 신설, 요청 실패 시 span을 만들지 않는다 — 실패는 HTTP 계층 로그로 충분).
- [ ] **Step 4: 통과 확인** — Run: `go test ./internal/telemetry/... ./internal/api/... -v` → PASS.
- [ ] **Step 5: 계약 남기기** — `cmd/tune`가 구현될 때 넣어야 할 세 호출 지점은 이미 위 "Files"·"설계 메모"에 기술돼 있다. 이 Task는 그 파일을 새로 만들지 않는다 — 존재하지 않는 파일을 편집 대상으로 넣지 않기 위함(§금지 사항).
- [ ] **Step 6: 커밋** — `git commit -m "feat(telemetry): PII-free spans for feedback evidence and tune events"`

---

## 배포

프로덕션은 macmini의 `docker compose -f docker-compose.local.yml --env-file .env.local`이다(k8s 아님). **`--env-file .env.local`을 절대 빼지 않는다** — 빼면 `${VAR}` 치환이 기본값으로 떨어져 볼륨 마운트가 어긋나고 컨테이너가 죽는다([[feedback_macmini_compose_envfile_flag]] 전례).

**마이그레이션:** `cmd/server/main.go`가 기동 시 `pg.RunMigrations`로 `migrations/` 디렉토리를 자동 적용한다 — 수동 `psql` 실행이 필요 없다. 순서는 파일명 사전순으로 고정되므로 §3 "번호 주의"대로 착수 시점에 `ls migrations`로 실제 번호를 확인해 feedback 보강이 weights history보다 먼저 오게만 해 두면 나머지는 자동이다.

- [ ] 적용 전: `docker compose ... exec postgres psql -U "$POSTGRES_USER" -d "$POSTGRES_DB" -c "SELECT count(*) FROM feedback;"` — 배포 전 행 수 기록(약 50 + 그간 누적분).
- [ ] `docker compose -f docker-compose.local.yml --env-file .env.local up -d --no-deps --build server` — `postgres`·`web`은 대상에서 제외해 무손상 유지.
- [ ] 적용 후: 같은 count 쿼리가 **감소 없이** 배포 전과 같거나 큰지 확인. `SELECT count(*) FROM feedback WHERE split IS NOT NULL;`이 backfill 대상 행 수와 일치하는지 확인.
- [ ] `\d feedback`에 `split` 컬럼과 `WHERE source = 'ask_evidence'` 부분 UNIQUE 인덱스가 보이는지, `\d search_weights_history`에 `WHERE status='active'` 부분 UNIQUE 인덱스가 보이는지 확인.
- [ ] 플래그 off 상태 회귀 확인: `curl -s -o /dev/null -w '%{http_code}\n' localhost:9200/api/v1/feedback/evidence` → 404. 기존 `/api/v1/feedback`, `/api/v1/ask`는 200대 그대로.
- [ ] **플래그는 배포와 분리된 별도 조작, 한 번에 하나씩.** `.env.local`에 `FEEDBACK_EVIDENCE_ENABLED=true` → `up -d --no-deps server` → `POST /api/v1/feedback/evidence` 200/201 확인 + Langfuse에서 `feedback.evidence.recorded` span이 `document_id`/`thumbs`/`split` 세 속성만 갖고 나타나는지 육안 확인(질의 텍스트 없음).
- [ ] 이어서 `NEXT_PUBLIC_FEEDBACK_EVIDENCE_ENABLED=true` → `up -d --no-deps --build web` → `/ask` 근거 카드 좋아요/싫어요 렌더·낙관적 토글 확인.
- [ ] `cmd/tune`을 postgres와 같은 macmini 호스트에서 1회 수동 실행(§2.4 배치 위치) → Langfuse에 `tune.dataset.loaded`/`tune.coordinate.completed`/`tune.promotion.decision` span이 잡히는지, 표본 30 미만이면 `gate_reason`이 그 사실을 보고하는지 확인.

> 이 절의 명령은 **운영자가 실행한다.** 에이전트는 macmini에 SSH하거나 컨테이너를 조작하지 않는다.

---

## 롤백

- [ ] **1단계(즉시, 플래그만):** `.env.local`에서 `FEEDBACK_EVIDENCE_ENABLED=false`(및 `NEXT_PUBLIC_FEEDBACK_EVIDENCE_ENABLED=false`) → `up -d --no-deps server`(또는 `web`). 엔드포인트가 다시 404가 되고 UI 버튼이 사라진다. 기존 `/ask`·`/api/v1/feedback`은 영향 없음.
- [ ] **2단계(가중치가 이미 승격된 경우, 즉시 롤백 경로):** `cmd/tune -rollback` 실행 → `search_weights_history`에서 현재 `active` 행이 `rolled_back`으로, 직전 `superseded` 행이 다시 `active`로 전이된다(§2.5). 되돌릴 `superseded` 행이 없으면 `active` 없음 상태(=`SearchWeights.Defaults()`)로 복귀 — 코드 기본값과 동일하므로 **서버 재기동 없이도 다음 검색 요청부터 기본 가중치로 돌아간다**(검색 경로가 매 요청 `active` 행을 조회하는 설계일 때. Task 10에서 캐싱을 도입한다면 캐시 무효화 훅이 반드시 필요하다 — 이 문장을 Task 10 담당자에게 그대로 전달할 것).
- [ ] **3단계(이미지 회귀, 위 두 단계로 부족할 때만):** 직전 태그로 `up -d --no-deps --build server web`. `feedback`·`search_weights_history` 데이터는 그대로 남는다.
- [ ] **마이그레이션은 되돌리지 않는다.** 025/026(§3, 실제 번호는 착수 시점 확인)은 전부 additive(nullable 컬럼 추가, 신규 테이블, 부분 인덱스)이므로 앱 코드가 롤백돼도 스키마는 무해하게 남는다 — `DROP COLUMN`/`DROP TABLE`이 이 계획의 어떤 마이그레이션에도 없다.
- [ ] 하지 말 것: `feedback` 또는 `search_weights_history` 행 삭제. 사용자가 이미 매긴 평가와 승격 이력이며 재수집으로 복구되지 않는다.

---

## Self-Review

구현 완료 후 아래를 직접 확인한다. 하나라도 아니면 완료가 아니다.

- [ ] 기존 `feedback` 50행이 파괴되지 않았다: 이번 브랜치에서 추가된 마이그레이션만 대상으로 `git diff --name-only main...HEAD -- migrations/ | xargs grep -in 'drop column\|alter column .* type\|set not null'` → 신규 컬럼의 최초 정의 라인 외 출력 없음.
- [ ] **검증셋 격리가 코드로(규약이 아니라) 강제된다:** `internal/dataset`의 리플렉션 기반 `api_surface_test.go`가 `HoldoutSet`의 exported 메서드 집합을 `{Evaluate, Queries}`로 고정하는지 확인, `grep -rn "HoldoutSet" internal/tune/`이 `Evaluate` 호출 외에 등장하지 않는지 확인(§Task 8 Step 5), `tune.Coordinate` 시그니처에 `*dataset.HoldoutSet` 파라미터가 없는지 확인.
- [ ] 질의 30개 미만에서 자동 승격이 차단된다: `TestEligible_Under30Rejects`/`TestEligible_At30Accepts` 통과, `gate.Eligible`이 `promote()` 진입 **전** 독립 호출인지(인라인 `if` 아님) 확인.
- [ ] 모든 기능 플래그 기본값이 off다: 환경변수를 지운 상태에서 `FEEDBACK_EVIDENCE_ENABLED`/`NEXT_PUBLIC_FEEDBACK_EVIDENCE_ENABLED` 관련 테스트가 off를 강제.
- [ ] 로그·트레이스에 질의 텍스트·문서 본문·실명이 없다: `TestFeedbackEvidence_SpanHasNoQueryAttribute` 통과, `TestAttrKeys_MatchAllowlist` 통과, `grep -rn "Attr(Feedback|Tune|Weights)" internal/telemetry/feedback_attrs.go | grep -iw query` → 출력 없음.
- [ ] `web/package-lock.json`이 생기지 않았다: `git status --porcelain web/package-lock.json` → 출력 없음. bun만 사용.
- [ ] `go test ./... && go vet ./...` 통과, `cd web && bun run lint && bun run build` 통과.
- [ ] 동시 작업 파일(§Global Constraints 목록, `internal/graph/**`·`internal/api/graph.go`·`internal/api/router.go`·`internal/store/graph_source.go`·`internal/worker/graph_projection_worker.go`·`internal/llm/client.go`·`internal/config/config.go`·`internal/worker/extraction_worker.go`·`cmd/server/main.go`·`cmd/collector/main.go` 등)을 건드리지 않았다: `git diff --name-only main...HEAD`로 확인.
- [ ] `internal/config/config.go`를 수정하지 않았다: 신규 플래그는 전부 `os.Getenv` 직접 호출.
