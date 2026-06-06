---
name: feedback-rebase-rebuild
description: text-clean rebase 후에도 full build/test 재실행 필수 — semantic-clean 보장 안됨
metadata:
  type: feedback
---

PR rebase가 충돌 없이(text-clean) 완료되어도, **semantic-clean은 보장되지 않는다.** rebase 완료 후 full build + test를 재실행해야 한다.

**Why:** v0.11.0 파이프라인 실행 중 원격에서 PR #73이 merge됨. mgr-gitnerd가 rebase를 충돌 없이 완료했지만, orchestrator는 이후 전체 build/test를 다시 수행했다. stale-base guard의 "rebuild after rebase" 경로가 실제로 필요했음.

**How to apply:**
- rebase 성공 메시지만 보고 `[Done]` 선언 금지
- `git rebase` 완료 직후 → `go build ./...` + `go test ./...` 재실행 → 통과 확인 후 push
- stale-base auto-dev guard의 sync-check 단계와 연동: rebase가 일어났다면 반드시 빌드 재검증 포함

[[feedback-auto-dev-stale-base]] (기존 stale-base guard 참조)
