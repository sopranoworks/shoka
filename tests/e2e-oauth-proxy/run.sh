#!/usr/bin/env bash
# Run the B-61 / B-63 Docker multi-host OAuth e2e harness.
#
#   ./run.sh           build + run BOTH registration modes; exit non-zero if either fails
#   ./run.sh logs      same, but also print the shoka container log (the B-59 dump)
#                      for each mode on completion — read exactly what crossed the proxy
#
# B-63 §2.3: the harness runs the full proxied OAuth + MCP flow ONCE PER registration
# posture — cimd then dcr — because the two postures CANNOT be advertised together
# (while the CIMD signal is present Claude skips DCR, §0.1). Each run configures shoka
# in that mode (REGISTRATION_MODE), asserts the AS metadata advertises exactly that
# mode's posture, and drives ONLY that mode's client path. Both must PASS.
#
# CONTAINER SCOPE (B-63 §3, NON-NEGOTIABLE): every compose command is scoped to the
# dedicated project `shoka-b63-e2e` via `-p`. This harness NEVER runs an unscoped
# `docker compose down`, `docker stop $(docker ps -q)`, `docker system prune`, or any
# broad name glob — it tears down ONLY its own project-scoped stack, so the operator's
# unrelated containers on the same host are never touched.
set -euo pipefail
cd "$(dirname "$0")"

SHOW_LOGS="${1:-}"

PROJECT="shoka-b63-e2e"
COMPOSE=(docker compose -p "$PROJECT" -f docker-compose.yml)

echo "==> generating local test certs (idempotent)"
./certs/gen-certs.sh

# teardown removes ONLY this project's stack (scoped by -p $PROJECT). Never broad.
teardown() {
  "${COMPOSE[@]}" down -v >/dev/null 2>&1 || true
}
trap teardown EXIT

# run_mode builds + runs the stack for one registration posture and returns the
# client's exit code (0 = PASS). It is fully scoped to $PROJECT.
run_mode() {
  local mode="$1"
  echo
  echo "================================================================"
  echo "==> registration mode: ${mode}  (project ${PROJECT})"
  echo "================================================================"
  # Reset any prior run's stack (scoped) so the second mode starts clean.
  "${COMPOSE[@]}" down -v >/dev/null 2>&1 || true

  set +e
  REGISTRATION_MODE="$mode" "${COMPOSE[@]}" up --build --abort-on-container-exit --exit-code-from client
  local code=$?
  set -e

  if [[ "$SHOW_LOGS" == "logs" ]]; then
    echo "==> [${mode}] shoka container log (B-59 verbatim dump of what crossed the proxy):"
    "${COMPOSE[@]}" logs shoka || true
  fi
  echo "==> [${mode}] client exit code: $code"
  return $code
}

overall=0
for mode in cimd dcr; do
  if ! run_mode "$mode"; then
    echo "==> B-63 E2E [${mode}]: FAIL — see the [FAIL] line above; re-run './run.sh logs' for the dump"
    overall=1
    break
  fi
  echo "==> B-63 E2E [${mode}]: PASS"
done

echo
if [[ $overall -eq 0 ]]; then
  echo "==> B-63 E2E: PASS — the full proxied OAuth + MCP connect works end-to-end in BOTH cimd and dcr modes"
else
  echo "==> B-63 E2E: FAIL"
fi
exit $overall
