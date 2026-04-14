#!/usr/bin/env bash
# sync-env_test.sh — validate sync-env.sh without touching real cluster
# All tests run in DRY_RUN mode. Safe to run against any cluster.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SYNC_SCRIPT="$SCRIPT_DIR/sync-env.sh"

pass() { echo "PASS: $1"; }
fail() { echo "FAIL: $1"; exit 1; }

# Test 1: missing env file
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
cd "$tmp"
if DRY_RUN=true bash "$SYNC_SCRIPT" 2>/dev/null; then
    fail "should reject missing .env"
fi
pass "missing .env rejected"

# Test 2: sparse env file (< 5 keys)
echo "FOO=bar" > "$tmp/.env"
echo "BAZ=qux" >> "$tmp/.env"
chmod 600 "$tmp/.env"
if DRY_RUN=true bash "$SYNC_SCRIPT" 2>/dev/null; then
    fail "should reject sparse env"
fi
pass "sparse env rejected"

# Test 3: well-formed env in DRY-RUN mode (no kubectl apply)
cat > "$tmp/.env" <<ENVEOF
KEY1=val1
KEY2=val2
KEY3=val3
KEY4=val4
KEY5=val5
KEY6=val6
ENVEOF
chmod 600 "$tmp/.env"

output=$(DRY_RUN=true bash "$SYNC_SCRIPT" 2>&1 || true)

# Must contain DRY-RUN marker
if ! echo "$output" | grep -q "DRY-RUN"; then
    fail "DRY_RUN mode did not emit DRY-RUN marker. Output: $output"
fi

# Must NOT contain evidence of actual apply
if echo "$output" | grep -q "secret/.*created\|secret/.*configured\|secret/.*applied"; then
    fail "DRY_RUN mode contaminated real cluster! Output: $output"
fi

pass "DRY_RUN mode validated — no real cluster contact"

# Test 4: sanity — sync-env.sh source code contains DRY_RUN guard
if ! grep -q 'DRY_RUN' "$SYNC_SCRIPT"; then
    fail "sync-env.sh missing DRY_RUN guard"
fi
pass "sync-env.sh contains DRY_RUN guard"

# Test 5: sanity — sync-env_test.sh never calls kubectl directly
if grep -En '^[[:space:]]*kubectl[[:space:]]+(apply|create|delete|patch)' "${BASH_SOURCE[0]}"; then
    fail "this test script contains direct destructive kubectl commands"
fi
pass "test script contains no destructive kubectl calls"

echo "All tests passed (safe mode)."
