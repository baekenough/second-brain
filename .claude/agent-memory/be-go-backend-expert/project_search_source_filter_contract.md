---
name: search-source-filter-contract
description: 검색 계층 소스 필터 계약 — SourceType(단수)+SourceTypes(복수)는 union, include/exclude 충돌은 exclude 우선, /ask는 호출 전 insight를 사전 조정, 창+청크는 OccurredRangeChecker로 사후 검증
metadata:
  type: project
---

검색 계층 소스 필터의 세 가지 계약을 2026-08-19에 확정했다(쿼리 계획기 선행 조건, spec §5 / R4).

**1. 단수 `SourceType` 과 복수 `SourceTypes` 는 union 이다.** `model.SearchQuery.IncludeSourceTypes()` 가 유일한 정의.
**Why:** 단수 필드를 쓰는 호출자(`internal/api/search.go`, `graphql.go`, `cmd/mcp`)를 수정할 수 없었고, union 만이 monotone — 다른 레이어가 복수 필드를 채워도 기존 호출자가 문서를 잃지 않는다. intersection/precedence 는 둘 다 "조용히 빈 결과 / 조용히 무시" 실패 모드가 있는데 그게 바로 #196 의 형태다.
**How to apply:** 새 코드에서 두 필드를 직접 읽어 포함 여부를 판단하지 마라. 항상 `IncludeSourceTypes()`.

**2. include/exclude 충돌은 exclude 우선.**
**Why:** exclude 는 정책(insight echo-chamber 가드는 서비스와 /ask 가 주입, 사용자 요청 아님), include 는 요청이다. 요청이 정책을 이기면 계획기가 소스 이름만 대서 가드를 열 수 있다.
**How to apply:** 전면 충돌(모든 include 가 exclude 됨)은 `warnOnSourceFilterConflict` 로 로그를 남긴다. `/ask` 는 `splitInsightLane` 에서 검색 호출 **전에** 조정해서 충돌 자체를 만들지 않는다. include 가 [insight] 뿐이면 본검색을 아예 건너뛴다 — insight 를 빼면 빈 include 셋이 되고 빈 셋은 "전체 소스"라 코퍼스 전체로 조용히 넓어지기 때문.

**3. R4(창 설정 시 청크 레인 스킵)는 "사후 검증"으로 해결했다.** `store.DocumentStore.FilterIDsByOccurredRange` + `search.OccurredRangeChecker`.
**Why:** 청크 행은 `occurred_at` 을 안 실어와서 스킵이 유일한 안전책이었지만, 계획기가 창 산출을 늘리면 청크 재현율이 그만큼 사라진다. 후보 id 를 `documents` 에 되짚어 확인하면 PK 조회 1회로 둘 다 지킬 수 있다.
**How to apply:** 검증자가 없거나 검증이 실패하면 **청크 후보 전량 폐기**(fail-closed). 서비스는 `s.store` 를 타입 단언해 검증자를 찾으므로 `cmd/**` 배선 변경이 필요 없다 — 프로덕션은 자동으로 켜지고, `DocumentSearcher` 만 구현한 테스트 더블은 예전 스킵을 유지한다.

관련: [[store-sql-test-harness]], [[project-second-brain]]
