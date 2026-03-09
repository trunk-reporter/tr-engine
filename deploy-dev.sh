#!/usr/bin/env bash
# Deploy a dev build to case without a full Docker image rebuild.
#
# How it works:
#   1. Cross-compiles tr-engine for linux/amd64
#   2. Stops the container, SCPs the binary, restarts
#   3. Pushes web files (hot-reloaded, no restart needed)
#
# First-time setup (already done on case):
#   Add binary volume mount to docker-compose.yml under tr-engine service:
#     - ./tr-engine:/usr/local/bin/tr-engine
#
# Usage:
#   ./deploy-dev.sh              # full deploy (binary + web + restart)
#   ./deploy-dev.sh --web-only   # just push web files (no restart needed)
#   ./deploy-dev.sh --binary-only # just push binary + restart
set -euo pipefail

HOST="root@case"
REMOTE_DIR="/data/tr-engine"

WEB=true
BINARY=true

for arg in "$@"; do
  case "$arg" in
    --web-only)    BINARY=false ;;
    --binary-only) WEB=false ;;
    -h|--help)
      sed -n '2,/^set/{ /^#/s/^# \?//p }' "$0"
      exit 0
      ;;
  esac
done

if $BINARY; then
  echo "==> Cross-compiling for linux/amd64..."
  GOOS=linux GOARCH=amd64 bash build.sh

  echo "==> Stopping tr-engine container..."
  ssh "$HOST" "cd ${REMOTE_DIR} && docker compose stop tr-engine"

  echo "==> Uploading binary..."
  scp tr-engine "${HOST}:${REMOTE_DIR}/tr-engine"
  ssh "$HOST" "chmod +x ${REMOTE_DIR}/tr-engine"
  rm tr-engine

  echo "==> Starting tr-engine container..."
  ssh "$HOST" "cd ${REMOTE_DIR} && docker compose up -d tr-engine"
fi

if $WEB; then
  echo "==> Uploading web files..."
  scp web/*.html web/*.js "${HOST}:${REMOTE_DIR}/web/" 2>/dev/null || \
    scp web/*.html "${HOST}:${REMOTE_DIR}/web/"
fi

if $BINARY; then
  echo "==> Waiting for healthy..."
  sleep 3
  ssh "$HOST" "curl -sf http://localhost:8080/api/v1/health | python3 -m json.tool"
fi

echo "==> Done."
