#!/usr/bin/env bash
# `make chaos`: the LIVE-09 campaign through a bad network (300 ms latency, lost requests, lost answers, duplicated requests) while a node is killed,
# Redis stops and PostgreSQL restarts, two nodes of the current release (TESTPLAN CHAOS-02..04, LIVE-09). Docker required.
set -euo pipefail
cd "$(dirname "$0")/.."
export PATH="$PATH:/usr/local/go/bin"
P="tl-chaos-${RUN_ID:-$$}"
FILE="$PWD/deploy/compose/docker-compose.upgrade.yml"
export UPGRADE_OLD_IMAGE="tasklane:chaos" UPGRADE_NEW_IMAGE="tasklane:chaos" UPGRADE_REDIS_URL=redis://redis:6379/0
C="docker compose -p $P -f $FILE"
LOGS="${CHAOS_LOGS:-chaos-logs}"
rc=1
cleanup() {
  if [ "$rc" != 0 ]; then mkdir -p "$LOGS"; $C --profile tools logs --no-color > "$LOGS/compose.log" 2>&1 || true; echo "chaos test failed: logs in $LOGS/compose.log"; fi
  $C --profile tools down -v --remove-orphans >/dev/null 2>&1 || true
}
trap cleanup EXIT
start=$(date +%s)
docker build -q -f scripts/upgrade/Dockerfile --build-arg VERSION=0.1.0 -t "$UPGRADE_NEW_IMAGE" . >/dev/null
export UPGRADE_COMPOSE="$C --profile tools"
go test -tags chaos ./internal/upgrade -run TestCHAOS03 -count=1 -timeout 20m "$@"
rc=0
echo "chaos test passed in $(( $(date +%s) - start )) s"
