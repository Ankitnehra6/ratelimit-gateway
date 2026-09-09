#!/usr/bin/env bash
#
# Runs the k6 load test against a running gateway.
#
# Uses the k6 Docker image so nothing has to be installed locally. Results are
# written to results/ as JSON summaries alongside the console output.
#
#   make up          # start the stack first
#   make loadtest
#
# Environment:
#   RATE      requests per second for the throughput scenario (default 2000)
#   DURATION  throughput scenario duration (default 30s)

set -euo pipefail

RATE="${RATE:-2000}"
DURATION="${DURATION:-30s}"
OUT_DIR="${OUT_DIR:-results}"

cd "$(dirname "$0")/.."
mkdir -p "${OUT_DIR}"

# Inside the k6 container the gateway is on the host, not on localhost.
# host.docker.internal is built in on Docker Desktop and mapped explicitly on
# Linux; passing --add-host on both keeps this script portable.
GATEWAY_IN_CONTAINER="${GATEWAY:-http://host.docker.internal:8080}"
GATEWAY_LOCAL="${GATEWAY_LOCAL:-http://localhost:8080}"

echo "==> Verifying the gateway is reachable at ${GATEWAY_LOCAL}"
if ! curl -fsS "${GATEWAY_LOCAL}/healthz" >/dev/null 2>&1; then
  echo "gateway is not answering; run 'make up' first" >&2
  exit 1
fi

STAMP="$(date +%Y%m%d-%H%M%S)"
SUMMARY="${OUT_DIR}/summary-${STAMP}.json"

echo "==> Running k6: ${RATE} rps for ${DURATION}"
docker run --rm -i \
  --add-host=host.docker.internal:host-gateway \
  -v "$(pwd)/scripts:/scripts:ro" \
  -v "$(pwd)/${OUT_DIR}:/results" \
  -e "GATEWAY=${GATEWAY_IN_CONTAINER}" \
  -e "RATE=${RATE}" \
  -e "DURATION=${DURATION}" \
  grafana/k6:latest run \
    --summary-export="/results/$(basename "${SUMMARY}")" \
    /scripts/loadtest.js

echo
echo "==> Summary written to ${SUMMARY}"
