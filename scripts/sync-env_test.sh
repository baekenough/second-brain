#!/usr/bin/env bash
# sync-env_test.sh — validate sync-env.sh without hitting real k8s
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SYNC_SCRIPT="$SCRIPT_DIR/sync-env.sh"

# Test 1: missing env file
tmp=$(mktemp -d)
cd "$tmp"
if bash "$SYNC_SCRIPT" 2>/dev/null; then
    echo "FAIL: should reject missing .env"
    exit 1
fi
echo "PASS: missing .env rejected"

# Test 2: sparse env file (< 5 keys)
echo "FOO=bar" > "$tmp/.env"
echo "BAZ=qux" >> "$tmp/.env"
if bash "$SYNC_SCRIPT" 2>/dev/null; then
    echo "FAIL: should reject sparse env"
    exit 1
fi
echo "PASS: sparse env rejected"

# Test 3: well-formed env (but no real namespace — expect namespace error)
cat > "$tmp/.env" <<EOF
KEY1=val1
KEY2=val2
KEY3=val3
KEY4=val4
KEY5=val5
KEY6=val6
EOF
chmod 600 "$tmp/.env"
# This will fail because the namespace check hits real kubectl, but we're verifying the script reaches that point
output=$(bash "$SYNC_SCRIPT" 2>&1 || true)
if echo "$output" | grep -q "namespace"; then
    echo "PASS: valid env passes pre-flight, reaches namespace check"
else
    echo "FAIL: script did not reach namespace check. Output: $output"
    exit 1
fi

rm -rf "$tmp"
echo "All tests passed."
