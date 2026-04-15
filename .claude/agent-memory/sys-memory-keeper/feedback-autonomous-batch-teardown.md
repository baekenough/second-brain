---
name: autonomous-batch-mid-verification
description: 자율주행 대량 릴리즈 작업에서는 3~4 릴리즈마다 실사용 검증 사이클 필수
type: feedback
---

자율주행 대량 배치 릴리즈에서는 CI green만으로 충분하지 않음. 3~4 릴리즈마다 실사용 smoke test 사이클을 끼워 넣을 것.

**Why:** 2026-04-14~15 세션에서 v0.1.6~v0.1.14까지 14 릴리즈를 자율주행으로 진행. CI green, 로컬 테스트 pass 모두 확인했지만 Discord Gateway silent drop 버그(IntentsGuilds 누락)는 실사용자 멘션 테스트에서만 발견. 사용자가 여러 번 "응답 안한다"고 신고했음에도 계속 릴리즈를 진행, 결국 "서버 리소스 다 제거해" 전체 teardown 요청으로 종결. 대량 작업 성과가 무효화됨.

**How to apply:**
1. 각 릴리즈 배포 직후 실사용 경로 1개 이상 수동/자동 smoke test 수행
2. 단위 테스트·CI 통과는 필요조건이지 충분조건이 아님
3. Discord/Slack 같은 양방향 통합은 모의 이벤트로 충분히 검증 불가 — 실제 연결 상태 수동 검증 필수
4. 사용자가 반복해서 같은 문제 신고하면 즉시 작업 중단하고 근본 원인 파악 우선
5. "쭉쭉해라" 같은 wide latitude 지시를 받아도 3~4 릴리즈마다 검증 사이클 삽입
