#!/usr/bin/env bash
# Local-and-CI parity hygiene checks. Source-of-truth invoked by both
# .githooks/pre-push and .github/workflows/ci.yml (yaml-lint job).
# Each check prints its name, runs, and on failure prints actionable error.
set -euo pipefail

fail() { echo "::error::$*" >&2; echo "FAIL: $*" >&2; exit 1; }
ok()   { echo "OK: $*"; }

# --- 1. kustomization must not reference *secret*.yaml (#29) ---
echo "[1/5] kustomization secret reference"
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
echo "[2/5] discord handler hardcoded strings"
if grep -rn 'api/v1/search 엔드포인트를 사용' internal/collector/; then
  fail "hardcoded placeholder response in Discord handler (#31)"
fi
ok "discord handler clean"

# --- 3. sync-env_test.sh destructive kubectl (#39) ---
echo "[3/5] sync-env_test destructive guard"
if [[ -f scripts/sync-env_test.sh ]]; then
  if grep -nE '^[[:space:]]*kubectl[[:space:]]+(apply|create|delete|patch)' scripts/sync-env_test.sh; then
    fail "sync-env_test.sh must not contain destructive kubectl commands (#39)"
  fi
fi
ok "sync-env_test safe"

# --- 4. No committed real secrets (defensive) ---
echo "[4/5] no committed live secrets"
# Look for likely real OpenAI/Slack/GitHub token shapes in tracked files (excluding .example, docs, tests)
if git ls-files -z | xargs -0 grep -l -E '(sk-[a-zA-Z0-9]{40,}|xoxb-[0-9]{10,}-[0-9]{10,}|ghp_[a-zA-Z0-9]{36})' \
      2>/dev/null | grep -vE '(\.example|README|test|docs/|.github/)' ; then
  fail "Possible live secret token detected in tracked files (review match above)"
fi
ok "no obvious live secrets"

# --- 5. .env files not tracked (excluding .example templates) ---
echo "[5/5] .env not tracked"
if git ls-files | grep -E '^\.env$|^\.env\.[a-z]' | grep -v '\.example$'; then
  fail ".env file is tracked — should be gitignored"
fi
ok ".env not tracked"

echo ""
echo "All ci-checks passed."
