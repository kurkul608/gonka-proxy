# ADR 0008: Route Stats in the Status Endpoint

**Status:** Accepted
**Date:** 2026-10-20

## Context

The product-broker platform picks Providers and Virtual Models for its users and wants to rank them by how they behave in practice: how often each (Provider, Upstream Model) pair fails and why, how long it takes to start answering, and how fast it generates. `GET /admin/status` only reported cooldowns. The proxy has no metrics endpoint, and adding a Prometheus stack only for this is out of scope.

## Decision

1. **Per (Provider, Upstream Model) stats in `GET /admin/status`.** A new `stats` array, plus `stats_window_seconds` and `stats_ewma_alpha`, is added next to the existing fields, which are unchanged. Each entry names the Provider, the Upstream Model and the Virtual Models routed to it. API keys and base URLs are never reported. The schema is in the README ("Route stats").

2. **What is counted.** Every upstream attempt counts, failovers included. An attempt is a success, a failure (`timeout`, `5xx`, `4xx`, `connection`), a cancel (the client went away), or a client error (a 4xx passed back unchanged). Cancels and client errors are not failures, so a client that hangs up or sends a bad request never lowers a Provider's success rate. A routing-time expiry during an attempt counts as a `timeout` of that Provider. TTFB is the time to the first response body byte. Tokens/s uses `usage.completion_tokens` found in the last 16 KiB of the body; attempts without usage add no sample.

3. **Aggregation.** Each metric keeps an EWMA over all samples (alpha from `GONKA_PROXY_STATS_EWMA_ALPHA`, default 0.2) and a ring of the newest 1024 timestamped samples, from which p50/p95 over the sliding window (`GONKA_PROXY_STATS_WINDOW`, default 15 minutes) are computed when the status is read. Windowed counters use 60 time buckets per window. Memory per pair is fixed at about 37 KB whatever the traffic. The cost is that, above 1024 samples per window, the percentiles describe the newest 1024 samples rather than the whole window.

4. **Settings come from the environment.** Window and alpha are process-level settings, like the tokens. Changing them through a config push would have to reset the windows, and they would change the Config Version of every existing configuration.

5. **Cheap and lock-light.** The registry map is copy-on-write behind an atomic pointer and changes only on (re)configuration. Recording an attempt takes one per-pair mutex for an in-memory update (about 60–180 ns, no allocation). No lock is held across I/O.

6. **Hot reload.** Stats are keyed by Provider name, base URL and Upstream Model. A reload keeps the stats of pairs that are still configured (a rotated API key or a priority change keeps them) and drops the others. Attempts finishing on an old Snapshot for a dropped pair are ignored, so removed pairs do not reappear. A pair added again starts from zero. Unlike Cooldowns, the API key is not part of the key: a new key does not change how fast the Provider is.

7. **No Prometheus endpoint.** The repository has none. The JSON is the interface.
