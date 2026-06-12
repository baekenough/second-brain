#!/bin/bash
#
# release-ci-gate.sh — PreToolUse hook (Bash matcher)
#
# Purpose:
#   Hard guard that BLOCKS release commands (gh release create / `v*` tag push)
#   whenever CI is not green on HEAD. Enforces R020 "Release CI Gate".
#
#   exit 0 → allow the tool to run (graceful degradation paths use this too)
#   exit 2 → BLOCK the tool; stderr message is shown to the model
#
#   Triggered by: #133 / CI-gate request (v0.19.0–v0.19.2 were released on red CI).
#
# Behavior:
#   - Reads hook JSON from stdin, extracts .tool_input.command.
#   - If jq missing, command empty, or command is not a release command → exit 0.
#   - Resolves HEAD sha; queries the latest CI run for it via gh.
#   - completed/success → allow. red / in-progress / no-run → block (exit 2).
#   - Missing gh / git failures → exit 0 (never break the session).
#
set -uo pipefail

# --- Read stdin ---------------------------------------------------------------
input=$(cat 2>/dev/null) || exit 0

# --- jq availability (graceful degradation) -----------------------------------
if ! command -v jq >/dev/null 2>&1; then
  exit 0
fi

cmd=$(printf '%s' "$input" | jq -r '.tool_input.command // ""' 2>/dev/null) || exit 0
[ -z "$cmd" ] && exit 0

# --- Is this a release command? (case-insensitive) ----------------------------
# Match: `gh release create`, OR a `git push` that carries a v<digit> tag /
# `--tags` / `refs/tags` token. Be specific to avoid blocking unrelated pushes.
lower=$(printf '%s' "$cmd" | tr '[:upper:]' '[:lower:]')

is_release=0
if printf '%s' "$lower" | grep -Eq 'gh[[:space:]]+release[[:space:]]+create'; then
  is_release=1
elif printf '%s' "$lower" | grep -Eq 'git[[:space:]]+push'; then
  if printf '%s' "$lower" | grep -Eq '(v[0-9]|--tags|refs/tags)'; then
    is_release=1
  fi
fi

[ "$is_release" -eq 0 ] && exit 0

# --- Resolve HEAD sha ---------------------------------------------------------
sha=$(git rev-parse HEAD 2>/dev/null) || exit 0
[ -z "$sha" ] && exit 0

# --- gh availability ----------------------------------------------------------
if ! command -v gh >/dev/null 2>&1; then
  echo "[release-ci-gate] gh CLI not available — cannot verify CI for HEAD ($sha). Proceeding WITHOUT a CI gate; verify CI is green manually before releasing." >&2
  exit 0
fi

# --- Query latest CI run for this commit --------------------------------------
status=$(gh run list --commit "$sha" --workflow CI --limit 1 \
  --json status,conclusion \
  --jq '.[0] | "\(.status)/\(.conclusion)"' 2>/dev/null) || status=""

# Fallback: some gh versions lack --commit; match latest run on main by headSha.
if [ -z "$status" ] || [ "$status" = "/" ]; then
  status=$(gh run list --branch main --limit 1 \
    --json headSha,status,conclusion \
    --jq ".[0] | select(.headSha == \"$sha\") | \"\(.status)/\(.conclusion)\"" 2>/dev/null) || status=""
fi

# Normalize "null/null" or "/" to empty (no usable run found).
case "$status" in
  "" | "/" | "null/null" | "null/" | "/null")
    status=""
    ;;
esac

# --- Decision -----------------------------------------------------------------
if [ -z "$status" ]; then
  echo "[release-ci-gate] No completed CI run for HEAD ($sha). Wait for CI to finish green before releasing." >&2
  exit 2
fi

run_status="${status%%/*}"

if [ "$run_status" = "completed" ]; then
  if [ "$status" = "completed/success" ]; then
    exit 0
  fi
  echo "[release-ci-gate] CI is RED for HEAD ($sha): $status. Fix all CI failures before releasing (R020 Release CI Gate)." >&2
  exit 2
fi

# status != completed → still running / queued.
echo "[release-ci-gate] CI still running for HEAD. Wait for green before releasing." >&2
exit 2
