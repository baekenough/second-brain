---
name: partc-actions-briefing
description: Part C 액션·브리핑 백엔드(Task 1~8) 구현 시 내린 비자명한 판단 — 모델명을 LLM_MODEL env로 읽는 이유, resolved_at 멱등 규칙, 근거 없는 문장 폐기 계약
metadata:
  type: project
---

Part C(액션 조회/상태변경 API + 온디맨드 브리핑)를 2026-08-18에 구현했다. 계획서: `docs/superpowers/plans/2026-08-18-actions-briefing.md`. 코드에서 읽어낼 수 없는 판단만 기록한다.

- **브리핑 캐시 키의 모델명은 `os.Getenv("LLM_MODEL")` 로 읽는다.** `llm.Completer` 인터페이스에 모델명 접근자가 없고(`Enabled`/`CompleteWithMessages` 뿐) 계획서가 `WithBriefing(cache, maxActions)` 시그니처를 고정했기 때문. 모델 교체 시 캐시가 자동 무효화되어야 한다는 요구를 만족시키는 최소 우회.
- **`action_status.resolved_at` 은 상태가 실제로 바뀔 때만 전진시킨다.** `DO UPDATE SET resolved_at = now()` 는 재전송마다 타임스탬프를 밀어 멱등이 아니게 되고, 프런트 버그로 대량 오처리가 났을 때 "사고 시간대 행"을 골라내는 유일한 단서를 잃는다(롤백 절차가 이 컬럼에 의존).
- **브리핑은 LLM 실패를 절대 에러로 올리지 않는다.** truncated(`llm.ErrTruncated`)·타임아웃·비활성 전부 결정적 집계 폴백(`Degraded=true`). 잘린 JSON을 부분 파싱해 살리려는 시도는 금지 — 근거 없는 문장이 새는 경로가 정확히 그것.
- **`identity_key` 는 형태만 검증(`^action:[0-9a-f]{16}$`)하고 재계산하지 않는다.** 사용자의 done/ignored 판단이 재추출을 넘어 살아남는 근거가 이 값이다.

관련: [[store-sql-test-harness]], [[project-second-brain]]
