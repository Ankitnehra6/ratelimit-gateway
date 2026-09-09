# ADR-0003: A circuit breaker guards the Redis call

**Status:** Accepted
**Date:** 2026-09-08

## Context

[ADR-0002](0002-fail-open-by-default.md) decided what to do when a limiter call fails.
It did not address what happens when calls keep failing.

Without a breaker, an unreachable Redis means every single request pays the full
`GATEWAY_LIMITER_TIMEOUT` before the fail-open policy applies. At a 50 ms timeout, a
gateway serving 2,000 rps adds 50 ms to every request and needs 100 concurrent in-flight
limiter calls just to stand still. The limiter's own latency becomes the outage, even
though the policy was supposed to make Redis optional.

It is also actively harmful to the recovery. A Redis that is struggling — not down,
overloaded — gets hammered by every replica retrying at full request rate, which is
precisely what prevents it from recovering.

## Decision

A three-state circuit breaker sits in front of every limiter call.

- **Closed** — calls pass through. Consecutive failures are counted; a success resets the
  count, because the signal of interest is a sustained failure, not a lone timeout.
- **Open** — after `GATEWAY_BREAKER_THRESHOLD` (default 5) consecutive failures, calls
  are skipped entirely and the failure policy applies immediately, at zero latency cost.
- **Half-open** — after `GATEWAY_BREAKER_COOLDOWN` (default 5s), a single probe is
  admitted. Success closes the breaker; failure re-opens it immediately, without waiting
  for the threshold again, since the dependency has just demonstrated it is still
  unhealthy.

The breaker is deliberately small and hand-written rather than pulled from a library: it
is roughly 100 lines, the semantics are the thing being demonstrated, and a dependency
would obscure them.

## Consequences

**Good.** A dead Redis costs nothing per request instead of a timeout per request.
`TestBreakerStopsCallingAFailedLimiter` drives 20 requests through a failing limiter and
measures exactly 3 Redis calls. Recovery is tested with one probe rather than a
thundering herd.

**Bad.** Between the breaker opening and the first successful probe, quota is not
enforced at all under fail-open — the breaker widens the unenforced window in exchange
for keeping latency flat. `gateway_breaker_state` is exported as a gauge so this is
visible on a dashboard rather than inferred.

**Trade-off accepted.** The breaker is per-replica, not shared. Ten replicas will each
discover a dead Redis independently, costing up to `10 × threshold` failed calls rather
than 5. Sharing breaker state would require a coordination store — which is the thing
that just failed.
