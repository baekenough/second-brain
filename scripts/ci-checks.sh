#!/usr/bin/env bash
# Local-and-CI parity hygiene checks. Source-of-truth invoked by both
# .githooks/pre-push and .github/workflows/ci.yml (yaml-lint job).
# Each check prints its name, runs, and on failure prints actionable error.
set -euo pipefail

fail() { echo "::error::$*" >&2; echo "FAIL: $*" >&2; exit 1; }
ok()   { echo "OK: $*"; }

# --- 1. kustomization must not reference *secret*.yaml (#29) ---
echo "[1/7] kustomization secret reference"
if command -v yq >/dev/null 2>&1; then
  if yq '.resources[]' deploy/k8s/kustomization.yaml 2>/dev/null | grep -iE 'secret\.yaml$'; then
    fail "kustomization.yaml must not reference secret YAMLs (prevents Secret overwrite #29)"
  fi
else
  # Fallback parser for environments without yq (local dev)
  if awk '/^resources:/{flag=1; next} /^[^[:space:]-]/{flag=0} flag' deploy/k8s/kustomization.yaml | grep -iE 'secret\.yaml\s*$' | grep -v '^[[:space:]]*#'; then
    fail "kustomization.yaml must not reference secret YAMLs (prevents Secret overwrite #29)"
  fi
fi
ok "kustomization secret guard"

# --- 2. Discord handler hardcoded placeholder (#31) ---
echo "[2/7] discord handler hardcoded strings"
if grep -rn 'api/v1/search 엔드포인트를 사용' internal/collector/; then
  fail "hardcoded placeholder response in Discord handler (#31)"
fi
ok "discord handler clean"

# --- 3. sync-env_test.sh destructive kubectl (#39) ---
echo "[3/7] sync-env_test destructive guard"
if [[ -f scripts/sync-env_test.sh ]]; then
  if grep -nE '^[[:space:]]*kubectl[[:space:]]+(apply|create|delete|patch)' scripts/sync-env_test.sh; then
    fail "sync-env_test.sh must not contain destructive kubectl commands (#39)"
  fi
fi
ok "sync-env_test safe"

# --- 4. No committed real secrets (defensive) ---
echo "[4/7] no committed live secrets"
# Look for likely real OpenAI/Slack/GitHub token shapes in tracked files (excluding .example, docs, tests)
if git ls-files -z | xargs -0 grep -l -E '(sk-[a-zA-Z0-9]{40,}|xoxb-[0-9]{10,}-[0-9]{10,}|ghp_[a-zA-Z0-9]{36})' \
      2>/dev/null | grep -vE '(\.example|README|test|docs/|.github/)' ; then
  fail "Possible live secret token detected in tracked files (review match above)"
fi
ok "no obvious live secrets"

# --- 5. .env files not tracked (excluding .example templates) ---
echo "[5/7] .env not tracked"
if git ls-files | grep -E '^\.env$|^\.env\.[a-z]' | grep -v '\.example$'; then
  fail ".env file is tracked — should be gitignored"
fi
ok ".env not tracked"

# --- 6. compose volumes: no bare absolute host paths (#162) ---
echo "[6/7] compose volumes: no bare absolute host paths"
_bare_found=0
for _f in docker-compose*.yml docker-compose*.yaml; do
  [ -f "$_f" ] || continue
  _in_volumes=0
  while IFS= read -r _raw; do
    _line="${_raw#"${_raw%%[! ]*}"}"          # strip leading whitespace
    case "$_line" in "#"*|"") continue ;; esac # skip comments + blank
    if [ "$_line" = "volumes:" ]; then
      _in_volumes=1; continue
    fi
    # top-level (non-indented) key → leave volumes block
    _first="${_raw:0:1}"
    if [ "$_first" != " " ] && [ "$_first" != "	" ] && [ "$_first" != "-" ]; then
      _in_volumes=0
    fi
    if [ "$_in_volumes" = "1" ]; then
      case "$_line" in
        "- "*)
          _entry="${_line#- }"
          _host="${_entry%%:*}"          # source path before first ':'
          case "$_host" in
            /Users/*|/home/*)
              echo "  BARE PATH [$_f]: $_host" >&2
              _bare_found=1
              ;;
          esac
          ;;
      esac
    fi
  done < "$_f"
done
if [ "$_bare_found" = "1" ]; then
  fail "docker-compose volumes contain bare absolute host paths (/Users/ or /home/). Wrap with \${VAR:-/path} (#162)"
fi
ok "compose volumes no bare paths"

# --- 7. 새로 추가된 주석은 한글을 포함해야 한다 (경고, 비차단, #285) ---
# 한국어 주석 규칙(CLAUDE.md/R000)을 위임 프롬프트에 명시해도 위반이 발생했다
# (v0.25.0 #274). 이번 diff 에서 새로 추가된 주석 줄 중 한글이 하나도 없는
# 줄을 찾아 경고한다. 차단하지 않는다 — exit code 에 영향을 주지 않는다.
#
# CI(GITHUB_ACTIONS=true)에서는 커밋 diff(<base>..HEAD)만 본다 — PR 은 항상
# 커밋된 상태로 돌기 때문이다. 로컬에서는 그렇지 않다: 커밋 전 작업트리
# 상태로 이 스크립트를 돌리는 게 정상 흐름인데, 커밋 diff 만 보면 검사
# 대상이 0건이라 항상 통과하는 가짜 녹색이 된다(#285 보완). 그래서 로컬
# 모드에서는 (a) 기준 브랜치 대비 작업트리 변경(스테이징 여부 무관)과
# (b) 아직 추적되지 않은 새 파일까지 모두 diff 에 포함시킨다.
echo "[7/7] 새 주석 한글 포함 검사 (경고, 비차단)"
_base_ref=""
_target_branch="${GITHUB_BASE_REF:-main}"
set +e
if git rev-parse --verify "origin/${_target_branch}" >/dev/null 2>&1; then
  _base_ref="origin/${_target_branch}"
elif git fetch --no-tags --depth=1 origin "${_target_branch}" >/dev/null 2>&1; then
  _base_ref="FETCH_HEAD"
fi
set -e

if [ -z "$_base_ref" ]; then
  echo "  (기준 브랜치(${_target_branch})를 찾지 못해 건너뜀)"
  echo "검사한 파일 수: 0, 경고: 0 (기준 브랜치 없음)"
else
  set +e
  if [ "${GITHUB_ACTIONS:-}" = "true" ]; then
    # CI: PR head 커밋 대 base 커밋 (기존 동작 유지)
    git diff --no-color "$_base_ref" HEAD -- '*.go' '*.sh' '*.yml' '*.yaml' 'Makefile' 2>/dev/null \
      | python3 scripts/check-korean-comments.py
  else
    {
      # 추적 파일: 기준 브랜치 대비 작업트리(스테이징+미스테이징 모두 포함)
      git diff --no-color "$_base_ref" -- '*.go' '*.sh' '*.yml' '*.yaml' 'Makefile' 2>/dev/null
      # 신규 파일: 아직 추적되지 않은 새 파일도 검사 대상에 넣는다
      # (git diff 는 untracked 파일을 기본적으로 보여주지 않는다).
      while IFS= read -r _new_file; do
        case "$_new_file" in
          *.go|*.sh|*.yml|*.yaml|Makefile|*/Makefile)
            git diff --no-color --no-index -- /dev/null "$_new_file" 2>/dev/null || true
            ;;
        esac
      done < <(git ls-files -o --exclude-standard)
    } | python3 scripts/check-korean-comments.py
  fi
  set -e
fi

echo ""
echo "All ci-checks passed."
