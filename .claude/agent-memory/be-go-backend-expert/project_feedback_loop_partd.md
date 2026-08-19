---
name: feedback-loop-partd
description: Part D 피드백 루프 백엔드 — store↔dataset 임포트 사이클 회피(split을 SQL에서 계산), PG bit(32)::bigint가 unsigned라는 실측, SearchQuery.Weights per-request 주입, holdout 격리 검증법
metadata:
  type: project
---

Part D(피드백 루프) 백엔드 Task 1/2/3/5/6/7 구현에서 얻은, 코드만 읽어서는 안 나오는 사실들.

**1. `internal/store` 는 `internal/dataset` 을 import 할 수 없다 (사이클).**
`dataset` 이 `store.EvalPair` 를 쓰기 때문에 계획서가 지시한 "store 가 dataset.SplitOf 를 호출" 은 컴파일되지 않는다. 해결: evidence INSERT 시 `split` 을 **SQL 식으로 계산**(`internal/store/feedback_evidence.go` 의 `evidenceSplitExpr`)하고, migration 025 backfill 과 같은 식을 쓴다. Go 쪽 `dataset.SplitOf` 는 미러이고, 동치성은 `internal/store/feedback_evidence_test.go`(**`package store_test`** — 외부 테스트 패키지여야 dataset import 가능)가 실 DB로 고정한다.
**Why:** 삽입 시점과 backfill 시점이 다른 split 을 주면 같은 질의가 train/holdout 양쪽에 들어가 holdout 이 오염된다. 이 계획 전체가 막으려는 그 하나의 실패다.
**How to apply:** split 규칙을 손대야 하면 SQL 식(migration + evidenceSplitExpr)과 `dataset/split.go` 를 **함께** 바꾸고 `TestUpsertEvidence_SplitMatchesSQLBackfill` 로 재확인한다.

**2. PostgreSQL `('x'||substr(md5(...),1,8))::bit(32)::bigint` 는 UNSIGNED 값이다** (실측: `c01cfb8b` → 3223124875). 따라서 Go 의 `binary.BigEndian.Uint32` 와 그대로 일치하고 `abs()` 는 no-op 이다. signed int32 로 가정하면 split 이 어긋난다.
**Why:** 계획서는 "부호 처리를 양쪽이 동일하게" 라고만 적고 어느 쪽인지 확정하지 않았다.

**3. `dataset.Normalize` 는 `strings.TrimSpace` 가 아니라 `strings.Trim(q, " ")` 이다.** PG `btrim` 기본 trim set 이 공백 하나뿐이라 TrimSpace(탭/개행까지 제거)를 쓰면 Go/SQL 이 갈린다.

**4. 가중치 주입은 `WithWeights` 가 아니라 `model.SearchQuery.Weights`.**
`search.Service.Search` 는 `q.Weights` 가 zero 가 아니면 그걸 우선한다(`search.go` 의 "Apply service-level weights when the caller has not set explicit weights"). `WithWeights` 는 서비스를 **뮤테이트**하므로 후보를 연달아 평가하면 오염된다. `internal/tune/harness.go` 의 `RunnerFor` 는 per-request 주입 + `Defaults()` 적용으로 인스턴스 재사용을 안전하게 만든다.

**5. holdout 격리 검증법.** `internal/tune` 에 `_ = h.pairs; _ = h.Pairs()` 를 쓴 임시 파일을 만들고 `go build ./internal/tune/` 가 실패하는지 본다(실패해야 정상). 상시 가드는 `internal/dataset/api_surface_test.go` 의 리플렉션 표면 고정 테스트 두 개.

**6. 일회성 pgvector 컨테이너로 `internal/store` 전체를 통과시키려면** 최소 스키마(documents/chunks) + `005_feedback.sql` 외에 **`017_entities.sql` 과 `022_actions.sql`** 도 적용해야 한다. 안 그러면 Part C 액션 테스트 7건이 `relation "entities" does not exist` 로 깨져서 내 변경 탓처럼 보인다. 관련: [[store-sql-test-harness]]

**7. 미배선 상태.** `Server.WithEvidenceFeedback` / `store.NewWeightsHistoryStore` 를 `cmd/server/main.go` 에서 부르는 코드는 아직 없다(파일 소유권상 손대지 않음). 라우트는 `evidenceVoter != nil` 일 때만 등록되고 핸들러는 `FEEDBACK_EVIDENCE_ENABLED` 기본 off 로 404 를 낸다.
