# ADR-0002: The gateway fails open when the limiter cannot decide

**Status:** Accepted
**Date:** 2026-09-08

## Context

Every request depends on a Redis call, and that call can fail: Redis restarts, a network
partition appears, the timeout fires under load. The gateway must then choose between two
policies with no third option.

**Fail closed** — reject the request. The quota is never exceeded, but Redis is now a
hard dependency of the entire API: a Redis blip becomes a full outage for every caller,
including well-behaved ones.

**Fail open** — serve the request unlimited. Traffic keeps flowing, but the upstream is
unprotected for the duration.

The deciding question is what the quota is actually protecting.

## Decision

**Fail open by default**, configurable with `GATEWAY_FAIL_OPEN`.

The reasoning is the relative cost of each failure. A rate limiter exists to protect an
upstream from excess load. The most common real incident is Redis being briefly
unavailable while traffic is entirely normal — and failing closed converts that small,
self-healing problem into a total API outage. Failing open converts it into a few seconds
of unenforced quota, which is usually harmless because most traffic at any moment is
under quota anyway.

Fail closed is right when the quota protects something that genuinely cannot absorb the
overage — a metered third-party API that charges per call, or a downstream with a hard
concurrency ceiling. Those deployments set `GATEWAY_FAIL_OPEN=false`.

## Consequences

Degradation is never silent. Every failed-open request carries
`X-RateLimit-Degraded: true`, and `gateway_limiter_errors_total{policy="fail_open"}`
increments, so a dashboard shows unenforced traffic rather than it passing unnoticed.

`/readyz` reports `degraded` with `X-Redis-Status: unreachable` but still returns 200
under fail-open, because the instance genuinely is still serving traffic correctly.
Returning 503 would make a load balancer pull a healthy instance out of rotation, turning
a Redis problem into a capacity problem. Under fail-closed the same condition returns
503, which is then accurate.

The startup Redis ping is a warning, not a fatal error, for the same reason: refusing to
boot without Redis would reintroduce the hard dependency this decision exists to avoid.
