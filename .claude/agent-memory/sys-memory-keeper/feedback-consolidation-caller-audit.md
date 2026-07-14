---
name: feedback-consolidation-caller-audit
description: DB→Go 계산 이전 시 모든 caller가 prior state를 threads하는지 감사 필수 (struct 기본값 0 silent bug)
metadata:
  type: feedback
---

DB 계산 로직을 Go로 옮길 때 기존 caller들이 prior state (예: Attempts 필드)를 명시적으로 전달하는지 전수 감사해야 한다.

**Why:** v0.13.0 #82 리팩터 — backoff 계산을 SQL에서 Go로 이전하면서 `recordFailure` 호출부가 `f.Attempts`를 누락해 항상 Attempts=0으로 Record() 호출 → NextRetryAt 항상 5분, dead-letter(10회) 절대 발동 안 됨. deep-verify(adversarial review)가 발견했고 `Attempts: f.Attempts` 한 줄 추가로 해결. struct 기본값이 0이라 컴파일 오류 없이 silent하게 잘못 동작.

**How to apply:** DB side-effect computation(backoff, retry count, state machine transition)을 Go로 이전할 때:
1. 새 함수의 모든 input 필드 나열
2. 기존 caller grep → 각 caller가 해당 필드를 실제로 채우는지 확인
3. 기본값(0, false, nil)이 올바른 동작인지 단위 테스트로 커버

[[feedback-deep-verify-high-surface]]
