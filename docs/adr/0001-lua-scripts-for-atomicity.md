# ADR-0001: Rate limiting algorithms run as Lua inside Redis

**Status:** Accepted
**Date:** 2026-09-08

## Context

The gateway runs as several replicas behind a load balancer. Quota state is shared, so
every decision is a read-modify-write against a value other replicas are touching at the
same moment.

Done naively from the client — read the counter, decide, write it back — two replicas
interleave their reads before either writes, and both admit a request that only one
should have. Under real concurrency this is not a rare race; on a hot key it is the
common case.

Three options were considered.

### Client-side `WATCH`/`MULTI`

Redis's optimistic transaction: watch the key, build the change, and let the `EXEC` fail
if anything else touched it. Correct, but it needs a retry loop in the client, and the
retry rate climbs with contention — exactly when the limiter matters most. A hot tenant
key is the worst case for optimistic concurrency, and it costs at least two round trips
per successful decision.

### A Redis module

A native module would be fastest, but it must be compiled per platform and installed on
the server, which rules out managed Redis (ElastiCache, MemoryStore) entirely.

### A Lua script

Redis runs a script atomically: nothing else executes against the keyspace while it
runs. One round trip, no retry loop, no contention penalty, and it works on any stock
Redis including every managed offering.

## Decision

Each algorithm is a Lua script executed with `EVALSHA`, via go-redis's `redis.Script`,
which caches the SHA and falls back to `EVAL` when the script is not yet loaded.

Two constraints follow from this and are honoured in every script:

1. **The current time is passed in as an argument**, never read with Redis's `TIME`.
   Scripts must be deterministic to be safe under replication; reading the clock inside
   the script is not.
2. **Every key a script touches is passed in `KEYS`**, so the scripts remain compatible
   with Redis Cluster, where a script may only touch keys in one slot.

## Consequences

**Good.** One round trip per decision. Atomicity is guaranteed by Redis rather than by
application logic. `TestConcurrentCallsNeverOverspend` fires 500 concurrent requests at a
100-request quota and measures exactly 100 admitted, for all three algorithms.

**Bad.** The core logic is now in a second language, with no type checking and no
debugger. This is mitigated by keeping the scripts small and by testing them against a
real Redis rather than a mock — the tests exercise the actual Lua, not a Go reimagining
of it.

**Also bad.** A slow script blocks the whole Redis server, since Redis is single
threaded. All three scripts here are O(1) or O(log n) with no unbounded loops, but this
is a real constraint on any future algorithm added to this package.
