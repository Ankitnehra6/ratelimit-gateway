#!/usr/bin/env bash
#
# Fires enough traffic at the gateway to trip the anonymous quota, then prints
# what came back. A quick end-to-end check that limiting, headers, and proxying
# all work together.

set -euo pipefail

GATEWAY="${GATEWAY:-http://localhost:8080}"
REQUESTS="${REQUESTS:-30}"

echo "==> Checking the gateway is up"
if ! curl -fsS "${GATEWAY}/healthz" >/dev/null 2>&1; then
  echo "gateway is not answering at ${GATEWAY}; run 'make up' first" >&2
  exit 1
fi
echo "    ok"
echo

echo "==> Sending ${REQUESTS} anonymous requests (tier limit: 20/s)"
allowed=0
throttled=0
for _ in $(seq 1 "${REQUESTS}"); do
  status=$(curl -s -o /dev/null -w '%{http_code}' "${GATEWAY}/get")
  case "${status}" in
    200) allowed=$((allowed + 1)) ;;
    429) throttled=$((throttled + 1)) ;;
    *)   echo "    unexpected status ${status}" ;;
  esac
done
echo "    allowed:   ${allowed}"
echo "    throttled: ${throttled}"
echo

echo "==> Rate limit headers on an anonymous request"
curl -sS -D - -o /dev/null "${GATEWAY}/get" | grep -i -E '^(HTTP/|x-ratelimit|retry-after)' || true
echo

echo "==> The same burst with a pro-tier key (limit: 5000/s) should not throttle"
allowed=0
throttled=0
for _ in $(seq 1 "${REQUESTS}"); do
  status=$(curl -s -o /dev/null -w '%{http_code}' \
    -H 'X-API-Key: demo-pro-key' "${GATEWAY}/get")
  case "${status}" in
    200) allowed=$((allowed + 1)) ;;
    429) throttled=$((throttled + 1)) ;;
  esac
done
echo "    allowed:   ${allowed}"
echo "    throttled: ${throttled}"
echo

echo "==> Quota is enforced per tenant, not globally: anonymous is still limited"
curl -s -o /dev/null -w '    anonymous status: %{http_code}\n' "${GATEWAY}/get"
