#!/usr/bin/env bash
# second-brain — bind mount 쓰기 가능 여부 실측 검증 (ubuntu1)
#
# 권한 숫자(ls -ldn)가 맞아 보여도 실제로 쓸 수 있는지는 별개다
# (2026-08-26 사고: drwxr-xr-x 여도 uid가 다르면 못 씀). 이 스크립트는
# 컨테이너 안에서 직접 touch/rm을 실행해 실측한다.
#
# 사용처:
#   1. 배포 직후 수동 실행 (README "기동 방법" 참고)
#   2. sb-permcheck.timer 가 주기적으로 실행 (드리프트 조기 발견,
#      2026-08-26 사고처럼 반나절 뒤 발견되는 일을 막기 위함)
#
# 종료 코드: 0 = 전부 통과, 1 = 하나 이상 실패 (systemd가 실패로 기록 →
# `systemctl --user status sb-permcheck.service` / journalctl 로 즉시 확인 가능)
set -uo pipefail

PROJECT="second-brain-ubuntu1"
LOG_TAG="sb-permcheck"

log() { logger -t "$LOG_TAG" "$*"; echo "$(date -Is) $*"; }

check() {
  local container="$1" path="$2" label="$3"
  if docker exec "$container" sh -c "touch '$path/.permcheck' && rm -f '$path/.permcheck'" >/dev/null 2>&1; then
    log "OK: $label ($container:$path writable)"
    return 0
  else
    log "FAIL: $label ($container:$path NOT writable) — bind mount 권한 드리프트 의심, ls -ldn 으로 호스트측 소유자/모드 확인 필요"
    return 1
  fi
}

rc=0
check "${PROJECT}-server-1"    "/data/call/ingest"   "server:/data/call/ingest"   || rc=1
check "${PROJECT}-server-1"    "/data/call/complete" "server:/data/call/complete" || rc=1
check "${PROJECT}-server-1"    "/data/call/to-text"  "server:/data/call/to-text"  || rc=1

if [ "$rc" -eq 0 ]; then
  log "all mount write checks passed"
else
  log "one or more mount write checks FAILED — see above"
fi
exit "$rc"
