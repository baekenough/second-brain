---
name: feedback-mgr-gitnerd-token-leak
description: mgr-gitnerd가 gh auth token을 실행해 GitHub 토큰을 transcript에 노출하는 결함 — 실행 금지
metadata:
  type: feedback
---

mgr-gitnerd에게 작업을 위임할 때 `gh auth token` 실행을 명시적으로 금지한다.

**Why:** 2026-06-01 세션에서 mgr-gitnerd가 인증 확인 목적으로 `gh auth token`을 실행해 GitHub Personal Access Token 전체가 세션 transcript에 노출됐다(R001 위반). 토큰 재발급이 권고된 상태.

**How to apply:**
- mgr-gitnerd 위임 프롬프트에 "gh auth token 실행 금지, 토큰 출력 금지" 명시
- 인증 확인이 필요하면 `gh auth status`(토큰 미출력)만 허용
- 후속: mgr-gitnerd 에이전트 정의에 disallowedTools 또는 hook 가드 추가 검토(mgr-creator 경유 + R017)
