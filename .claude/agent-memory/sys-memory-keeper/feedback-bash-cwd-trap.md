---
name: feedback-bash-cwd-trap
description: Bash 툴의 persistent cwd가 디렉토리 이동/삭제 후 잠기는 문제 — 세션 재시작 외 복구 불가
type: feedback
---

Bash 툴은 세션 시작 시 cwd를 캡처하고 모든 후속 호출 전에 chdir을 시도한다. cwd가 mv/rm으로 사라지면 모든 Bash/Glob 호출이 `Path "..." does not exist` 로 영구 차단된다.

**Why**: 2026-04-13 second-brain 세션에서 `mv ~/workspace/baekenough/vibers-brain ~/workspace/vibers-brain` 직후 모든 Bash 명령이 차단됨. Glob도 ripgrep posix_spawn 실패. 오직 Read/Write/Edit (절대 경로) 만 동작.

**How to apply**:
- 현재 작업 디렉토리 자체를 mv/rm 하기 전에 사용자에게 세션 재시작이 필요함을 사전 고지
- 디렉토리 이동은 세션 종료 직전이나 새 세션 첫 작업으로 미루기
- 부득이하게 mid-session에 해야 하면, mv 대신 새 위치에 cp + 검증 + 옛 위치 보존 후 다음 세션에서 정리
- 이동 후 Read/Write/Edit는 절대 경로로 정상 동작 — 대안 경로로 활용
