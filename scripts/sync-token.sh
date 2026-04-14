#!/bin/bash
# Sync CliProxyAPI Codex OAuth token from ubuntu-ext server.
# Token auto-refreshes on the server; re-run this script to get latest.
set -e

LOCAL_DIR="$HOME/.cli-proxy-api"
REMOTE="ubuntu24_home_server-ext"
AUTH_FILE="codex-baekenough@gmail.com-pro.json"

mkdir -p "$LOCAL_DIR"
scp "$REMOTE:/home/baekenough/.cli-proxy-api/$AUTH_FILE" "$LOCAL_DIR/$AUTH_FILE"
echo "Token synced to $LOCAL_DIR/$AUTH_FILE"
echo "Expires: $(python3 -c "import json; print(json.load(open('$LOCAL_DIR/$AUTH_FILE'))['expired'])")"
