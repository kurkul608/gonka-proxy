# ADR 0007: Max Routing Time

**Status:** Accepted
**Date:** 2026-10-19

## Context

The proxy never gives up: when every Provider is cooling it waits (Recovery Wait) and starts another Routing Pass until a Provider answers or the client cancels. That suits interactive coding tools that would rather wait. Backend callers (an API serving a user request, a job with its own retry policy) need a bounded answer so they can fall back or retry later. Some callers are real-time, others batch, so the bound must also be adjustable per request.

## Decision

1. **`max_routing_time`** (duration, optional). Absent or `0` keeps the endless behaviour. When set, a request that has not obtained a response within that time — across Failovers, Routing Passes and Recovery Waits — receives `503 {"error":{"type":"server_error","code":"no_provider_available",...}}`.

2. **Per-request header `X-Gonka-Max-Routing-Time`.** A Go duration (`10s`, `1500ms`) or a number of seconds (`2.5`). It may only shorten a configured limit (the effective limit is the smaller one); without a configured limit it sets one. An unparsable or non-positive value is `400 invalid_max_routing_time`. The header is not forwarded upstream.

3. **It bounds routing, not responses.** The deadline runs until a response is *committed*: an upstream 200 (headers received) or a Client Error passed through. After that it is disarmed, so long completions and streams are never cut. If it expires during an upstream attempt, that attempt is canceled and **no Cooldown is recorded** (the Provider did not fail; the caller ran out of patience).

4. **No Providers at all.** With zero enabled Providers (empty config, or all disabled), or none serving the requested model, the proxy answers `503 no_provider_available` immediately instead of entering a Recovery Wait that could never succeed — with or without a limit.

## Consequences

- Callers get a predictable worst-case latency and a distinct error code to branch on.
- A short limit can end a request while a slow Provider would still have answered; callers choose the trade-off per request.
- Default behaviour for existing configurations is unchanged.

## Alternatives Considered

- **Only a per-request header.** Rejected: operators want a safe default for every caller.
- **Header may extend the configured limit.** Rejected: the operator's limit protects the proxy's resources.
- **Bounding the full response time.** Rejected: would cut healthy long streams; clients can enforce their own total timeout.
