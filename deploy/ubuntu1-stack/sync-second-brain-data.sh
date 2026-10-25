#!/usr/bin/env bash
# macmini(OneDrive 게이트웨이) → ubuntu1 데이터 동기화
#
# 2026-08-25 스택 이전 후, macmini 는 OneDrive 클라이언트와 onedrive-bridge
# LaunchAgent 만 유지하고 컴퓨트는 전부 ubuntu1 로 옮겼다. 이 스크립트는
# macmini 가 스테이징한 파일을 ubuntu1 로 끌어온다.
#
# macmini 의 rsync 는 openrsync(protocol 29) 이므로 --info 등 rsync 3.x
# 전용 옵션을 쓰면 원격이 usage 를 뱉고 죽는다. -az 만 사용할 것.
#
# 자격증명은 동기화하지 않는다(gmail/calendar 소스 폐기). stage/call, stage/sms
# 만 끌어오며 collector 는 ro 로 읽기만 한다.
set -uo pipefail

REMOTE="llm-memory-hub"   # macmini (Tailscale 100.100.80.74)
LOCAL_BASE="$HOME/.second-brain"
LOG_TAG="sb-sync"

log() { logger -t "$LOG_TAG" "$*"; echo "$(date -Is) $*"; }

sync_one() {
  local src="$1" dst="$2" label="$3"
  mkdir -p "$dst"
  if rsync -az --timeout=300 "$REMOTE:$src" "$dst" 2>&1; then
    log "ok: $label"
  else
    log "FAILED: $label (exit $?)"
    return 1
  fi
}

rc=0
sync_one ".second-brain/stage/call/"                  "$LOCAL_BASE/stage/call/"          "stage/call"     || rc=1
sync_one ".second-brain/stage/sms/"                   "$LOCAL_BASE/stage/sms/"           "stage/sms"      || rc=1

# 오디오/스테이징은 collector 가 ro 로 읽기만 하므로 그룹 읽기 권한이면 충분
chmod -R g+rX "$LOCAL_BASE/stage" 2>/dev/null

log "sync complete rc=$rc"
exit $rc
