#!/usr/bin/env bash
# `make upgrade`: the rolling upgrade under load (TESTPLAN UPG-02). Builds the previous release (UPGRADE_FROM, a commit or tag; default
# the one in scripts/upgrade-from) from a throwaway checkout and the current tree, and runs internal/upgrade against the compose profile.
set -euo pipefail
cd "$(dirname "$0")/.."
export PATH="$PATH:/usr/local/go/bin"
# --bus: the previous release is the first one that has the cluster event bus, both releases share a Redis, the crowd is 50 people and the second old node is killed
if [ "${1:-}" = "--bus" ]; then shift; export UPGRADE_BUS=1 UPGRADE_KILL=1 UPGRADE_USERS=50 UPGRADE_REDIS_URL=redis://redis:6379/0; UPGRADE_FROM="${UPGRADE_FROM:-91a5aec}"; fi
FROM="${UPGRADE_FROM:-$(cat scripts/upgrade-from)}"
P="tl-upgrade-${RUN_ID:-$$}"
FILE="$PWD/deploy/compose/docker-compose.upgrade.yml"
WT="$(mktemp -d)/old"
export UPGRADE_OLD_IMAGE="tasklane:upgrade-old" UPGRADE_NEW_IMAGE="tasklane:upgrade-new"
C="docker compose -p $P -f $FILE"
LOGS="${UPGRADE_LOGS:-upgrade-logs}"
rc=1
cleanup() {
  if [ "$rc" != 0 ]; then mkdir -p "$LOGS"; $C --profile tools logs --no-color > "$LOGS/compose.log" 2>&1 || true; echo "upgrade test failed: logs in $LOGS/compose.log"; fi
  $C --profile tools down -v --remove-orphans >/dev/null 2>&1 || true
  git worktree remove --force "$WT" >/dev/null 2>&1 || true
}
trap cleanup EXIT
start=$(date +%s)
git worktree add --detach "$WT" "$FROM" >/dev/null
echo "old release: $(git log -1 --format='%h %s' "$FROM" | cut -c1-80)"
docker build -q -f scripts/upgrade/Dockerfile --build-arg VERSION=0.0.9 -t "$UPGRADE_OLD_IMAGE" "$WT" >/dev/null
docker build -q -f scripts/upgrade/Dockerfile --build-arg VERSION=0.1.0 -t "$UPGRADE_NEW_IMAGE" . >/dev/null
echo "images built in $(( $(date +%s) - start )) s"
export UPGRADE_COMPOSE="$C --profile tools"
go test -tags upgrade ./internal/upgrade -count=1 -timeout 20m "$@"
rc=0
echo "upgrade test passed in $(( $(date +%s) - start )) s"
