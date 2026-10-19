#!/usr/bin/env bash
# `make interop`: the real-service interoperability smoke of TESTPLAN 3.3. Builds the image, brings the compose profile up, runs the INTEROP
# scenarios against the two nodes, and always tears the stack down (keeping its logs when something failed).
set -euo pipefail
cd "$(dirname "$0")/.."
export PATH="$PATH:/usr/local/go/bin"
P="tl-interop-${RUN_ID:-$$}"
FILE="${INTEROP_FILE:-deploy/compose/docker-compose.interop.yml}"
C="docker compose -p $P -f $FILE"
LOGS="${INTEROP_LOGS:-interop-logs}"
rc=1
cleanup() {
  if [ "$rc" != 0 ]; then mkdir -p "$LOGS"; $C logs --no-color > "$LOGS/compose.log" 2>&1 || true; echo "interop failed: logs in $LOGS/compose.log"; fi
  $C down -v --remove-orphans >/dev/null 2>&1 || true
}
trap cleanup EXIT
start=$(date +%s)
$C build app1 >/dev/null
$C up -d --wait --wait-timeout 240 postgres redis opensearch
$C up -d app1 app2
port() { $C port "$1" "$2" | awk -F: '{print $NF}'; }
export INTEROP_COMPOSE="$C"
export INTEROP_APP1="http://127.0.0.1:$(port app1 8080)" INTEROP_APP2="http://127.0.0.1:$(port app2 8080)"
export INTEROP_OS="http://127.0.0.1:$(port opensearch 9200)" INTEROP_REDIS="127.0.0.1:$(port redis 6379)"
export INTEROP_DB="postgres://tl:tl@127.0.0.1:$(port postgres 5432)/tasklane?sslmode=disable"
for u in "$INTEROP_APP1" "$INTEROP_APP2"; do
  for i in $(seq 1 90); do curl -sf "$u/readyz" >/dev/null && break; sleep 1; [ "$i" = 90 ] && { echo "$u never became ready"; exit 1; }; done
done
echo "stack up in $(( $(date +%s) - start )) s"
go test -tags interop ./internal/interop -count=1 -timeout 15m "$@"
(cd e2e && npm ci --no-audit --no-fund >/dev/null && npx playwright test -c playwright.interop.config.js)
rc=0
echo "interop passed in $(( $(date +%s) - start )) s"
