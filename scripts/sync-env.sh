#!/usr/bin/env bash
# sync-env.sh — sync ~/second-brain/.env to k8s Secret
# Usage: bash scripts/sync-env.sh [namespace]
set -euo pipefail

NAMESPACE="${1:-second-brain}"
SECRET_NAME="second-brain-secret"
ENV_FILE="${ENV_FILE:-.env}"

# ---------- pre-flight ----------
if [ ! -f "$ENV_FILE" ]; then
    echo "[ERR] $ENV_FILE not found (cwd: $(pwd))" >&2
    exit 1
fi

if ! command -v kubectl >/dev/null 2>&1; then
    echo "[ERR] kubectl not installed" >&2
    exit 1
fi

if ! kubectl get namespace "$NAMESPACE" >/dev/null 2>&1; then
    echo "[ERR] namespace $NAMESPACE does not exist" >&2
    exit 1
fi

# ---------- safety checks ----------
# .env should be chmod 600 for security
mode=$(stat -c '%a' "$ENV_FILE" 2>/dev/null || stat -f '%A' "$ENV_FILE" 2>/dev/null || echo "")
if [ -n "$mode" ] && [ "$mode" != "600" ]; then
    echo "[WARN] $ENV_FILE mode is $mode, recommended 600" >&2
fi

# Count keys for sanity
key_count=$(grep -cE '^[A-Z_][A-Z0-9_]*=' "$ENV_FILE" || echo 0)
if [ "$key_count" -lt 5 ]; then
    echo "[ERR] only $key_count keys found in $ENV_FILE — refusing to create sparse secret" >&2
    exit 1
fi

echo "[INFO] syncing $key_count keys from $ENV_FILE to $NAMESPACE/$SECRET_NAME"

# ---------- dry-run first ----------
rendered=$(kubectl create secret generic "$SECRET_NAME" \
    --from-env-file="$ENV_FILE" \
    --namespace "$NAMESPACE" \
    --dry-run=client -o yaml)

# Sanity: rendered secret should have data field
if ! echo "$rendered" | grep -q '^data:'; then
    echo "[ERR] rendered secret has no data field — refusing to apply" >&2
    exit 1
fi

# ---------- apply ----------
echo "$rendered" | kubectl apply -f -

# ---------- post-check ----------
actual_count=$(kubectl -n "$NAMESPACE" get secret "$SECRET_NAME" -o jsonpath='{.data}' | jq 'length' 2>/dev/null || echo "?")
echo "[OK] secret now has $actual_count keys"

# Optional: trigger rollout
if [ "${AUTO_ROLLOUT:-false}" = "true" ]; then
    echo "[INFO] AUTO_ROLLOUT=true — restarting deployment"
    kubectl -n "$NAMESPACE" rollout restart deployment/second-brain
    kubectl -n "$NAMESPACE" rollout status deployment/second-brain --timeout=3m
fi
