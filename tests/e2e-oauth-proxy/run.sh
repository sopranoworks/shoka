#!/usr/bin/env bash
# Run the B-61 / B-63 Docker multi-host OAuth e2e harness.
#
#   ./run.sh           build + run the full flow; exit code is the client's verdict
#   ./run.sh logs      same, but also print the shoka container log (the B-59 dump)
#                      on completion — read exactly what crossed the proxy
#
# The client container drives the COMPLETE proxied OAuth + MCP flow against the
# public TLS proxy URL on BOTH the CIMD and the DCR (RFC 7591, B-63) registration
# paths, and exits 0 (PASS) / non-zero (FAIL). --exit-code-from client surfaces
# that as this script's exit code, so CI / a make target can gate on it. Without
# the /register endpoint the DCR path cannot proceed, so the harness fails.
set -euo pipefail
cd "$(dirname "$0")"

SHOW_LOGS="${1:-}"

echo "==> generating local test certs (idempotent)"
./certs/gen-certs.sh

COMPOSE="docker compose -f docker-compose.yml"

cleanup() {
  if [[ "$SHOW_LOGS" == "logs" ]]; then
    echo "==> shoka container log (B-59 verbatim dump of what crossed the proxy):"
    $COMPOSE logs shoka || true
  fi
  echo "==> tearing down"
  $COMPOSE down -v --remove-orphans >/dev/null 2>&1 || true
}
trap cleanup EXIT

echo "==> building + running (proxy + shoka + client) on 192.0.2.0/24"
set +e
$COMPOSE up --build --abort-on-container-exit --exit-code-from client
code=$?
set -e

echo "==> client exit code: $code"
if [[ $code -eq 0 ]]; then
  echo "==> B-63 E2E: PASS — the full proxied OAuth + MCP connect works end-to-end on BOTH the CIMD and DCR paths"
else
  echo "==> B-63 E2E: FAIL — see the [FAIL] line above and re-run with './run.sh logs' for the verbatim dump"
fi
exit $code
