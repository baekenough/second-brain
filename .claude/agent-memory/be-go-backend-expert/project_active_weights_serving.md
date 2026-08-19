---
name: active-weights-serving
description: "#214 승격 가중치 서빙 배선 — 캐시 없이 요청마다 읽는 이유(cmd/tune이 별도 프로세스), 우선순위 3단, SEARCH_ACTIVE_WEIGHTS_ENABLED 기본 off, cmd/server에만 배선"
metadata:
  type: project
---

`search_weights_history` 의 `active` 행을 검색 기본 가중치로 쓰는 배선(`internal/search/active_weights.go`).

**캐시를 두지 않고 요청마다 `Active(ctx)` 를 읽는다.**
**Why:** 쓰는 쪽이 `cmd/tune -promote` / `-rollback` 으로 **다른 프로세스**다. 서버 안의 어떤 무효화 훅도 그 write 를 관측할 수 없으므로 캐시가 줄 수 있는 최선은 "TTL 안에"뿐인데, 롤백 절차는 "재기동 없이 다음 요청부터"를 요구한다. 그 요구를 만족시킬 만큼 짧은 TTL 은 사실상 요청마다 읽는 것과 같다. 비용은 partial unique index 위 단일 행 조회 — 같은 요청이 이미 지불하는 임베딩 왕복 + 5레인 CTE 옆에서 무시할 수준.
**How to apply:** 나중에 정말 비싸지면 TTL 이 아니라 **NOTIFY/LISTEN 무효화 + 캐시**로 가야 한다. 맨 TTL 은 이 결정이 배제한 창을 조용히 되살린다.

**우선순위:** `q.Weights`(요청별) > active 행(플래그 on) > `WithWeights` > 컴파일 기본값.
요청별이 최상위인 것이 핵심이다 — `internal/tune/harness.go` 가 후보를 `q.Weights` 로 평가하므로, active 행이 이걸 덮으면 모든 후보가 현직으로 측정돼 튜너가 자기 자신과 비교하고 노이즈로 승격한다. (참고: [[feedback-loop-partd]] 4번)

**플래그 `SEARCH_ACTIVE_WEIGHTS_ENABLED`, 기본 false**, `config.envFlag` 사용(1/true/yes/on). off 는 "조회하고 버림"이 아니라 **아예 조회 안 함** — 테스트가 reader 호출수 0 으로 고정한다.

**배선은 `cmd/server/main.go` 한 곳뿐.** cmd/tune 에 붙이면 baseline 이 직전 승격본으로 흘러가 매 실행이 자기 자신과 비교하게 된다.

실패 시 fail-open(컴파일 기본값) + 분당 1회로 rate-limit 한 `slog.Warn`(`failures_total` 누적 카운트 포함). 026 설계상 이 SELECT 는 파라미터도 질의문도 없어 에러 메시지에 개인정보가 실릴 수 없다.

관련: [[feedback-loop-partd]], [[project-tune-optimizer-task89]]
