#!/usr/bin/env bash
# The supported-version matrix of the interoperability smoke (TESTPLAN 3.3): the same scenarios against each Redis or Valkey and OpenSearch
# combination, so the lowest and highest supported minors of each are exercised. `make interop` runs the first line only (the pull request run);
# `make interop-matrix` runs every line (nightly).
set -uo pipefail
cd "$(dirname "$0")/.."
# redis image | expected in doctor | opensearch image | expected in doctor
COMBOS=(
  "redis:7@sha256:4fa24486b8bcca8eec45ee0eb166edc674795e53a2b53d1a9ef263eecebaac85|Redis 7|opensearchproject/opensearch:2.17.1@sha256:1193b7c29c5d63028523728243cc4da047ac49f697a8f8105e5aeee2f89bcc4c|OpenSearch 2.17"
  "valkey/valkey:8@sha256:640c5e62cea04b6d6f2084232651d0cc70362d31f4f805e7be94dbed6855e8f2|Valkey 8|opensearchproject/opensearch:2.11.1@sha256:cbca8e35fb333af938289ac0f370abdcbde46dbe7629acc1af0cd4219da85b62|OpenSearch 2.11"
  "valkey/valkey:7.2@sha256:ef0f9fb533b1f06fb7aba6478d758ca1c077d49e2bcb2b4365a4ea7f50b6d42e|7.2|opensearchproject/opensearch:3.0.0@sha256:00565f63fa1aa1643d5a2cf61cb5f09b4aeddee2f12703f01c534f051bff1a99|OpenSearch 3.0"
)
[ "${1:-}" = "--first" ] && COMBOS=("${COMBOS[0]}")
fail=0
for c in "${COMBOS[@]}"; do
  IFS='|' read -r ri re oi oe <<<"$c"
  echo "=== $ri + $oi"
  if INTEROP_REDIS_IMAGE="$ri" INTEROP_REDIS_EXPECT="$re" INTEROP_OS_IMAGE="$oi" INTEROP_OS_EXPECT="$oe" INTEROP_LOGS="interop-logs/$(echo "$ri$oi" | tr -c 'a-z0-9' _ | cut -c1-40)" ./scripts/interop.sh; then
    echo "PASS $ri + $oi"
  else
    echo "FAIL $ri + $oi"; fail=1
  fi
done
exit $fail
