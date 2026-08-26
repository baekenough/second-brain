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
# 컨테이너(appuser uid 10001)가 자격증명을 읽어야 하므로 동기화 후 매번
# 그룹을 다시 부여한다 — rsync -a 가 권한을 원본대로 되돌리기 때문이다.
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
sync_one "workspace/private/secretary/gmail/"         "$LOCAL_BASE/secretary/gmail/"     "gmail"          || rc=1
sync_one "workspace/private/secretary/calendar/"      "$LOCAL_BASE/secretary/calendar/"  "calendar"       || rc=1

# 컨테이너 읽기 권한 복구 (rsync 가 600 으로 되돌려 놓는다)
for f in "$LOCAL_BASE/secretary/gmail/credentials.json" \
         "$LOCAL_BASE/secretary/gmail/token.json" \
         "$LOCAL_BASE/secretary/calendar/credentials.json" \
         "$LOCAL_BASE/secretary/calendar/token.json"; do
  [ -f "$f" ] || continue
  sudo -n chgrp 10001 "$f" 2>/dev/null && sudo -n chmod 640 "$f" 2>/dev/null \
    || log "WARN: could not restore group on $f"
done

# 오디오/스테이징은 collector 가 ro 로 읽기만 하므로 그룹 읽기 권한이면 충분
chmod -R g+rX "$LOCAL_BASE/stage" 2>/dev/null

log "sync complete rc=$rc"
exit $rc
