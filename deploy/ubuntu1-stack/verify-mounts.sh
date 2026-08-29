#!/usr/bin/env bash
# second-brain — bind mount 쓰기 가능 여부 + compose env 파일 존재 실측 검증 (ubuntu1)
#
# 권한 숫자(ls -ldn)가 맞아 보여도 실제로 쓸 수 있는지는 별개다
# (2026-08-26 사고: drwxr-xr-x 여도 uid가 다르면 못 씀). 이 스크립트는
# 컨테이너 안에서 직접 touch/rm을 실행해 실측한다.
#
# 2026-08-29 사고: 재배포 때 web/.env.local(gitignore 대상)이 소스 동기화로
# 유실됐는데, 당시 web 컨테이너가 이미 환경변수를 물고 떠 있어서 아무 증상이
# 없었다. 이틀 뒤 다음 `up -d`에서야 "env file ... not found"로 배포 자체가
# 실패했다. bind mount와 동일하게 "지금 떠 있음"이 "다음 배포도 될 것"을
# 보장하지 않는 사례라 같은 스크립트에 편입한다.
#
# 사용처:
#   1. 배포 직후 수동 실행 (README "기동 방법" 참고)
#   2. sb-permcheck.timer 가 주기적으로 실행 (드리프트 조기 발견,
#      2026-08-26/2026-08-29 사고처럼 한참 뒤에야 발견되는 일을 막기 위함)
#
# 종료 코드: 0 = 전부 통과, 1 = 하나 이상 실패 (systemd가 실패로 기록 →
# `systemctl --user status sb-permcheck.service` / journalctl 로 즉시 확인 가능)
set -uo pipefail

PROJECT="second-brain-ubuntu1"
LOG_TAG="sb-permcheck"
APP_DIR="${HOME}/second-brain-app"
COMPOSE_FILE="${APP_DIR}/docker-compose.ubuntu1.yml"

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

# compose 파일의 `env_file:` 블록에서 파일 경로 목록을 뽑는다. YAML 파서 없이
# grep/sed로 처리하되, "무매치가 초록으로 보이는" 사고를 반복하지 않기 위해
# 추출 결과가 0건이면 그 자체를 실패로 취급한다(아래 호출부에서 처리).
#
# 매칭 대상: `env_file:` 다음 줄부터 이어지는 `  - path` 리스트 항목.
# 다른 `key:` 라인(들여쓰기가 같거나 얕은)이 나오면 블록이 끝난 것으로 본다.
extract_env_files() {
  local compose_file="$1"
  awk '
    /^[[:space:]]*env_file:[[:space:]]*$/ { in_block=1; next }
    in_block && /^[[:space:]]*-[[:space:]]*.+/ {
      line=$0
      sub(/^[[:space:]]*-[[:space:]]*/, "", line)
      sub(/[[:space:]]*#.*$/, "", line)
      gsub(/^["'\'']|["'\'']$/, "", line)
      if (line != "") print line
      next
    }
    in_block { in_block=0 }
  ' "$compose_file"
}

check_env_file() {
  local raw="$1" resolved
  case "$raw" in
    /*) resolved="$raw" ;;
    *)  resolved="${APP_DIR}/${raw}" ;;
  esac

  if [ ! -e "$resolved" ]; then
    log "FAIL: env_file missing ($raw → $resolved not found) — gitignore 대상 파일이라 소스 동기화(재배포)로 유실될 수 있음, 실행 중 컨테이너의 환경변수에서 복원 필요"
    return 1
  fi
  if [ ! -s "$resolved" ]; then
    log "FAIL: env_file empty ($raw → $resolved size 0) — gitignore 대상 파일이라 소스 동기화(재배포)로 유실될 수 있음, 실행 중 컨테이너의 환경변수에서 복원 필요"
    return 1
  fi
  log "OK: env_file present and non-empty ($raw)"
  return 0
}

rc=0
check "${PROJECT}-server-1"    "/data/call/ingest"   "server:/data/call/ingest"   || rc=1
check "${PROJECT}-server-1"    "/data/call/complete" "server:/data/call/complete" || rc=1
check "${PROJECT}-server-1"    "/data/call/to-text"  "server:/data/call/to-text"  || rc=1

if [ ! -f "$COMPOSE_FILE" ]; then
  log "FAIL: compose file not found ($COMPOSE_FILE) — env_file 추출 자체가 불가능하므로 실패로 처리"
  rc=1
else
  env_file_count=0
  # mapfile 대신 while-read를 쓴다 — bash 버전에 덜 의존적이고, 명령이
  # 조용히 실패해도(예: 오래된 bash) 카운터가 0으로 남아 아래 "0건 추출"
  # 분기가 그대로 실패를 잡아낸다.
  while IFS= read -r f; do
    [ -n "$f" ] || continue
    env_file_count=$((env_file_count + 1))
    check_env_file "$f" || rc=1
  done < <(extract_env_files "$COMPOSE_FILE" | sort -u)

  if [ "$env_file_count" -eq 0 ]; then
    log "FAIL: extracted 0 env_file entries from $COMPOSE_FILE — 추출 로직이 깨졌거나 compose 형식이 바뀐 것으로 간주(무매치를 통과로 처리하지 않음)"
    rc=1
  fi
fi

if [ "$rc" -eq 0 ]; then
  log "all mount write + env file checks passed"
else
  log "one or more checks FAILED — see above"
fi
exit "$rc"
