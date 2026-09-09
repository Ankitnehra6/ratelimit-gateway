# ADR-0004: Metrics are labelled by tier, never by tenant

**Status:** Accepted
**Date:** 2026-09-08

## Context

The obvious thing to do with a per-tenant rate limiter is to expose per-tenant metrics:

```
gateway_rate_limit_decisions_total{tenant="acme", decision="throttled"}
```

That answers the question everyone asks first — "which customer is getting throttled?" —
directly from a dashboard, with no log searching.

It is also the single most reliable way to take down a Prometheus server.

Prometheus stores one time series per unique combination of label values. A label whose
value comes from user-controlled or unbounded input creates an unbounded number of
series. Ten thousand tenants across the five metrics this gateway exports, each with a
handful of other label values, is comfortably hundreds of thousands of series from one
service — and each one costs memory in the scraping server for as long as it is retained.

The failure mode is worse than it sounds, because it is not triggered by the gateway
being unhealthy. It is triggered by the gateway being *successful*: onboarding customers
is what grows the cardinality. And the anonymous tier makes it sharper still, since those
tenant IDs are derived from client IP — an unbounded space that an attacker controls
directly. A trivial script rotating source addresses would mint a new time series per
request.

## Decision

Every metric label in this service comes from a **bounded, operator-defined set**.

- `tier` — defined in `tenants.json`, a handful of values
- `algorithm` — three values
- `decision` — `allowed`, `throttled`, `degraded`
- `cause` / `policy` — two values each
- `state` — three breaker states
- `status` — HTTP status codes

Tenant identity is deliberately absent. Where per-tenant detail matters, it goes to the
**structured logs** instead: the middleware logs `tenant` and `tier` on every limiter
failure. Logs are built for high-cardinality data; metrics are not.

## Consequences

**Good.** The series count this service can produce is fixed at deployment time and does
not grow with the customer base or with traffic. It cannot be inflated by an attacker.

**Bad.** "Which tenant is being throttled right now?" is no longer a dashboard query. It
needs a log search, which is slower and requires log aggregation to be set up. This is a
real ergonomic cost and it is the correct trade: the alternative degrades gradually and
then fails all at once, at the worst possible moment, and the fix at that point is an
emergency.

If per-tenant visibility becomes genuinely necessary, the ways to get it without
unbounded labels are to expose only the top-N tenants by throttle rate as an explicitly
capped set, or to compute per-tenant aggregates in the logging pipeline and emit those as
a separate, bounded metric. Both are additions on top of this decision, not reversals of
it.
