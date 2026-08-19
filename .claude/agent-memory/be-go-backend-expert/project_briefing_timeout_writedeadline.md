---
name: briefing-timeout-writedeadline
description: 브리핑 degraded 원인(30초 2중 절단)과 해결 패턴 — BRIEFING_TIMEOUT_SECONDS + 핸들러 단위 write deadline 연장(ResponseController), HTTP_WRITE_TIMEOUT_SECONDS
metadata:
  type: project
---

`GET /api/v1/briefing`가 프로덕션에서 항상 `degraded=true`(문장 1개)였던 원인은 **두 겹의 30초**였다: `internal/api/briefing.go`의 하드코딩 상수와 `cmd/server/main.go`의 전역 `http.Server.WriteTimeout`. 하나만 늘리면 다른 하나가 잘랐다.

채택한 구조 (2026-08-18):
- `BRIEFING_TIMEOUT_SECONDS`(기본 120s) → `config.BriefingTimeout()`. api 패키지가 요청마다 호출한다. `api.Server` 구조체(router.go)에 필드를 추가하지 못하는 상황(동시 작업 중 파일)이라 env-per-request로 갔고, 브리핑은 캐시로 보호되는 저빈도 경로라 비용이 없다.
- 전역 `WriteTimeout`은 **느린 클라이언트 방어 장치**라서 최장 엔드포인트에 맞춰 올리지 않는다. 대신 브리핑 핸들러가 `http.NewResponseController(w).SetWriteDeadline(...)`로 자기 응답만 연장한다. chi `middleware.NewWrapResponseWriter`는 `Unwrap()`을 구현하므로 실제 소켓까지 도달한다(실측 검증: WriteTimeout 500ms + 1.5s 핸들러 → 연장 시 200/본문 정상, 미연장 시 클라이언트 EOF).
- `HTTP_WRITE_TIMEOUT_SECONDS`(기본 90s)는 전역값을 설정화한 것. 기존 30s는 `/ask`(ASK_TIMEOUT_SECONDS 기본 60s, SSE)와 콜드스타트 검색(이슈 #195)을 이미 자르고 있었다 — #195의 "빈 응답"은 위 negative control과 같은 EOF 증상이다.

**Why:** 200 + degraded=true는 상태코드만으로는 실패가 보이지 않아 오래 방치됐다. 상한은 코드가 아니라 운영자가 조절해야 한다.

**How to apply:** 오래 걸리는 단일 라우트가 생기면 전역 WriteTimeout을 올리지 말고 핸들러 단위 deadline 연장을 쓴다. 0/음수는 "무제한"이 아니라 기본값으로 폴백시킨다(`context.WithTimeout(0)`은 즉시 만료).
