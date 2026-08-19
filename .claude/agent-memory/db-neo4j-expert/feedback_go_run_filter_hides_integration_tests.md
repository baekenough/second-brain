---
name: go-run-filter-hides-integration-tests
description: `go test -run Integration` 은 이름에 Integration이 없는 테스트를 조용히 제외한다 — 통합 테스트는 파일이 아니라 함수 이름으로 골라진다
metadata:
  type: feedback
---

Go 통합 테스트를 `-run Integration` 같은 이름 필터로 선별한다면, **테스트 함수 이름에 그 토큰이 들어가야 한다.** 파일 이름(`*_integration_test.go`)은 `-run` 과 아무 관계가 없다.

**Why:** 2026-08-18 Part B 검증에서 계획서가 지정한 검증 명령 `go test ./internal/graph/ -run Integration` 이 통합 테스트 8건 중 **5건(`TestProjector_*`)을 실행조차 하지 않았다.** 출력은 PASS 3건으로 초록이었고, 실행되지 않은 5건은 SKIP 으로도 보이지 않았다 — 필터에서 빠진 테스트는 아예 나타나지 않기 때문에 "돌았는데 통과"와 "안 돌았음"이 육안으로 구분되지 않는다. 하필 그 5건이 멱등성 등 설계의 토대를 검증하는 테스트였다.

**How to apply:** 통합 테스트를 만들거나 리뷰할 때 (a) 함수 이름에 `Integration` 을 넣고, (b) 검증 명령을 실행한 뒤 **PASS 개수가 기대한 테스트 개수와 일치하는지** 센다. 환경변수를 뺀 실행에서 같은 개수가 SKIP 으로 나오는지도 함께 확인하면 필터 누락이 즉시 드러난다.
