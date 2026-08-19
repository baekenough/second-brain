---
name: project-sort-recent-vs-rrf
description: "검색 정렬 계약 — RRF는 선택, Sort는 표시순서. recent 방향 규칙은 model.SearchQuery.RecencyAscending 단일 정의를 store SQL과 search Go 정렬이 함께 참조"
metadata:
  type: project
---

`Sort="recent"` 의 방향 판정(미래 창이면 `occurred_at ASC`, 그 외 `COALESCE(occurred_at, collected_at) DESC`)은
`internal/model/document.go` 의 `SearchQuery.RecencyAscending(now)` / `SortsByRecency()` 에 **한 번만** 정의된다.
`internal/store` 의 `sortOrder`(SQL ORDER BY)와 `internal/search` 의 융합 후 Go 정렬(`sortByRecency`)이 둘 다 이 함수를 호출한다.

**Why:** 청크 레인 결과는 스토어의 ORDER BY를 통과하지 않으므로 `mergeRRF` 후에는 Go에서 같은 순서를 다시 세워야 한다.
규칙이 두 벌이 되면 "청크 병합이 발동했는지"에 따라 응답 순서가 달라지고, 응답만 봐서는 두 경우를 구분할 수 없다
(#212 이전에 이 필드의 주석과 실제 동작이 갈라진 전례가 있다).

**How to apply:**
- 정렬/랭킹을 건드릴 때 `q.Sort == "recent"` 문자열 비교를 새로 쓰지 말고 `SortsByRecency()` / `RecencyAscending()` 을 쓴다.
- 융합 후 정렬은 `chunkFused` 일 때만 적용한다 — 스토어 단독 결과에 다시 정렬을 걸면 DB가 더 많은 정보로 매긴 순서를 덮어쓴다.
- **(해결, #215)** 청크 전용 문서가 timestamp를 못 들고 오던 문제는 `OccurredRangeChecker` 를 바꾸는 대신
  **청크 조인 SELECT 에 `d.occurred_at` / `d.collected_at` 을 추가**해서 닫았다(`store.ChunkSearchResult.DocumentOccurredAt/CollectedAt`
  → `search.chunkTimestamps`). checker 인터페이스는 그대로다 — 창은 여전히 WHERE 술어여야 하고,
  청크 레인은 자기 LIMIT 으로 이미 자른 뒤라 사후 필터링은 후보 축소가 아니라 **페이지 축소**가 된다.
  즉 조인은 ORDER 를, checker 는 MEMBERSHIP 을 담당한다.
- 동점은 시각 → Score DESC → id ASC 전순서로 깬다(`mergeRRF` 가 map을 평탄화하므로 안정정렬만으로는 비결정적, #191과 같은 부류).

관련: [[project_search_source_filter_contract]], [[active-weights-serving]]
