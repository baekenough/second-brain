---
name: feedback-goal-loop-no-new-issues
description: /goal "이슈 전부 해결" 루프 중 follow-up 이슈 자동 생성 금지 — 루프 무한 트리거 방지
metadata:
  type: feedback
---

`/goal "모든 이슈가 해결될 때까지"` 패턴의 자동화 루프 실행 중, 구현 과정에서 발견된 follow-up 작업을 **GitHub 이슈로 생성하지 않는다.**

**Why:** v0.11.0 파이프라인에서 chunk embedding backfill이 추가 작업으로 식별됨. 이슈로 등록했다면 goal 조건("이슈 0건")이 재충족되지 않아 루프가 무한히 재트리거됨.

**How to apply:**
- goal-loop 실행 중 발견된 후속 작업 → 사용자에게 **텍스트 권고사항**으로 보고
- 이슈 생성은 루프 완전 종료 후 사용자 승인 하에만 수행
- goal 조건 평가 전 `gh issue list --state open` 재확인

**예외**: goal 조건이 "특정 이슈 해결"이 아닌 "전체 오픈 이슈 해결"일 때만 이 규칙이 적용됨. 이슈 번호 지정 목표(예: "#71 완료")는 해당 없음.
