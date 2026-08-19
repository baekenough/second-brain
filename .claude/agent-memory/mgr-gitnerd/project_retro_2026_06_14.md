---
name: project-retro-2026-06-14
description: 2026-06-14 FSD 세션 회고 이슈 #162 — 찐빠 4건 (deploy-readiness 게이트 부재 High)
metadata:
  type: project
---

2026-06-14 FSD 세션 회고 이슈 #162 생성 완료.

**Why:** 맥미니 prod 점검→FSD 루프(v0.20.5/v0.20.6/v0.21.0) 중 4건의 프로세스 갭 발견.

**How to apply:** 향후 릴리스 시 deploy-readiness 검증(compose 보간 prod 환경 검증) 선행 필요.

핵심 찐빠:
- [High] CI 전부 그린 릴리스 → 맥미니 배포 실패 (compose `/Users/user` 하드코딩). "릴리스됨" ≠ "배포 가능" — R020 위반.
- [Medium] 배포 에이전트 symlink 우회 제안(근본수정 대신) — 오케스트레이터가 차단, #160으로 근본수정.
- [Medium] 서브에이전트 위임 시 prod 접근 승인 컨텍스트 누락 → SECURITY WARNING 반복.
- [Low] FSD homework 게이트를 매 이터레이션→종료 시 통합 1회로 변경(명세 이탈).

하네스 제안: deploy-readiness CI 잡(compose config 검증) + 위임 프롬프트 컨벤션 + FSD homework 통합 모드 문서화.

See [[project_retro_second_brain]] for earlier retro pattern batch.
