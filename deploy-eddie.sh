#!/usr/bin/env bash
# Deploy tr-engine to eddie (tr-engine-test)
# Copies binary, schema, and web files, then restarts the service.
set -euo pipefail

HOST="eddie"
REMOTE_DIR="~/tr-engine-test"
LOCAL_DIR="$(cd "$(dirname "$0")" && pwd)"

echo "=== Deploying tr-engine to ${HOST}:${REMOTE_DIR} ==="

# 0. Build Linux binary
echo "Building Linux binary..."
VERSION="${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
COMMIT="$(git rev-parse --short HEAD 2>/dev/null || echo unknown)"
BUILD_TIME="$(date -u '+%Y-%m-%dT%H:%M:%SZ')"
LDFLAGS="-X main.version=${VERSION} -X main.commit=${COMMIT} -X main.buildTime=${BUILD_TIME}"
GOOS=linux GOARCH=amd64 go build -ldflags "${LDFLAGS}" -o "${LOCAL_DIR}/tr-engine-linux" ./cmd/tr-engine
echo "Built ${VERSION} (${COMMIT})"

# 1. Stop existing process if running
echo "Stopping existing tr-engine..."
# Use pidfile approach to avoid pkill matching the SSH session itself
ssh "$HOST" 'pid=$(pgrep -x tr-engine 2>/dev/null) && kill "$pid" && echo "killed pid $pid" || echo "not running"'
sleep 1

# 2. Copy binary
echo "Uploading binary..."
scp "${LOCAL_DIR}/tr-engine-linux" "${HOST}:${REMOTE_DIR}/tr-engine.new"
ssh "$HOST" "cd ${REMOTE_DIR} && mv tr-engine tr-engine.bak 2>/dev/null || true && mv tr-engine.new tr-engine && chmod +x tr-engine"

# 3. Copy schema
echo "Uploading schema..."
scp "${LOCAL_DIR}/schema.sql" "${HOST}:${REMOTE_DIR}/schema.sql"

# 4. Copy web files
echo "Uploading web files..."
scp ${LOCAL_DIR}/web/*.html ${LOCAL_DIR}/web/*.js "${HOST}:${REMOTE_DIR}/web/" 2>/dev/null || true

# 5. Ensure PostgreSQL is running
echo "Ensuring PostgreSQL is up..."
ssh "$HOST" "cd ${REMOTE_DIR} && docker compose -f docker-compose-pg.yml up -d"
sleep 2

# 6. Start tr-engine (config loaded from .env in REMOTE_DIR)
echo "Starting tr-engine..."
ssh "$HOST" "cd ${REMOTE_DIR} && nohup ./tr-engine > tr-engine.log 2>&1 &"

sleep 2

# 7. Verify it's running
echo "Verifying..."
if ssh "$HOST" 'pgrep -x tr-engine > /dev/null 2>&1'; then
  echo "=== tr-engine is running ==="
  ssh "$HOST" "curl -s http://localhost:8090/api/v1/health | python3 -m json.tool 2>/dev/null || echo '(health check pending, may need a moment)'"
else
  echo "=== FAILED: tr-engine is not running ==="
  ssh "$HOST" "tail -20 ${REMOTE_DIR}/tr-engine.log"
  exit 1
fi
