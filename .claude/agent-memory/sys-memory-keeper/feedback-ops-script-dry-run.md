---
name: ops-script-dry-run-enforcement
description: 프로덕션 클러스터/DB/secret을 건드리는 ops 스크립트 자동 테스트는 DRY_RUN만 허용
type: feedback
---

프로덕션 클러스터·DB·secret을 건드리는 ops 스크립트의 테스트는 반드시 `DRY_RUN=true`만 사용. 실제 apply 절대 금지.

**Why:** 2026-04-14 `sync-env_test.sh`(이슈 #39)가 Test 3에서 `kubectl apply`를 실제 실행해 운영 `second-brain-secret`을 테스트 더미 키 6개로 덮어써 파괴. `.env`에서 27개 키로 수동 복구. 프로덕션 다운타임 발생. v0.1.11 hotfix로 `DRY_RUN=true` 가드 추가 + CI yaml-lint에 destructive-command 금지 lint.

**How to apply:**
1. ops 스크립트(`sync-env.sh`, deploy 스크립트 등)는 `DRY_RUN=true` 모드 제공
2. 테스트 스크립트는 항상 `DRY_RUN=true`로만 실행
3. CI에 `grep 'kubectl apply' scripts/*_test.sh` 금지 lint 추가
4. 의심스러우면 `--dry-run=client`만 쓰는 완전 분리된 namespace 사용
