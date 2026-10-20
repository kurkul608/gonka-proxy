# ADR 0008: Route Stats in the Status Endpoint

**Status:** Accepted
**Date:** 2026-10-20

## Context

The product-broker platform picks Providers and Virtual Models for its users and wants to rank them by how they behave in practice: how often each (Provider, Upstream Model) pair fails and why, how long it takes to start answering, and how fast it generates. `GET /admin/status` only reported cooldowns. The proxy has no metrics endpoint, and adding a Prometheus stack only for this is out of scope.

## Decision

1. **Per (Provider, Upstream Model) stats in `GET /admin/status`.** A new `stats` array, plus `stats_window_seconds` and `stats_ewma_alpha`, is added next to the existing fields, which are unchanged. Each entry names the Provider, the Upstream Model and the Virtual Models routed to it. API keys and base URLs are never reported. The schema is in the README ("Route stats").

2. **What is counted.** Every upstream attempt counts, failovers included. An attempt ends in one of five outcomes:
   - a success;
   - a failure (`timeout`, `5xx`, `4xx` or `connection`);
   - a cancel (the client went away);
   - a client error (a client-caused 4xx such as 400 or 422, passed back unchanged);
   - a deadline (the request's routing deadline cut the attempt).

   Only failures count against a Provider, so a client that hangs up, sends a bad request or picks a short routing time never lowers its success rate.
   - **401, 403 and 404 are always `4xx` failures**, even when they are passed back to the client because the failover switches are off. They mean the proxy's key was rejected or the Upstream Model is missing, which is the Provider's configuration and not the client's request.
   - **Deadline.** When the deadline wins the race with an answer that has already arrived (the response is in, but `commit()` loses), the attempt is a deadline, whatever set the limit. When the limit expires before the Provider answers, the source decides. The operator's `max_routing_time` makes it a Provider `timeout`: the operator considers this long too slow. A shorter, client-chosen `X-Gonka-Max-Routing-Time` makes it a deadline.
   - **TTFB** is the time to the first response body byte.
   - **Tokens/s** uses `usage.completion_tokens` found in the last 16 KiB of the body. Attempts without usage add no sample.
   - **Stream and non-stream are separate.** Streams and non-streaming responses differ by an order of magnitude in TTFB, and their tokens/s is measured over different spans. Both metrics are therefore reported separately per mode (`stream`, `non_stream`), while the counters are shared.
   - **Stream usage is the client's choice.** Stream usage arrives only when the client sends `stream_options.include_usage`. The proxy does not inject it, because that changes the response the client receives. Callers that want stream tokens/s (the product-broker api) send it.

3. **Aggregation.** Each metric and mode keeps an EWMA over all samples (alpha from `GONKA_PROXY_STATS_EWMA_ALPHA`, default 0.2) and a ring of the newest 1024 timestamped samples. p50/p95 over the sliding window (`GONKA_PROXY_STATS_WINDOW`, default 15 minutes) are computed from the ring when the status is read: the ring is copied under the lock, then filtered and sorted after it is released. Windowed counters use 60 time buckets per window. Memory per pair is fixed at about 70 KB whatever the traffic. The cost is that, above 1024 samples per window, the percentiles describe only the newest 1024 samples, and `window_samples_truncated` says so.

4. **Settings come from the environment.** Window and alpha are process-level settings, like the tokens. Changing them through a config push would have to reset the windows, and they would change the Config Version of every existing configuration.

5. **Cheap and lock-light.** When a snapshot is installed, each of its routes gets the `*routeStats` pointer of its pair, and requests record through that pointer with no map lookup. Recording takes one per-pair mutex for an in-memory update (about 80–170 ns, no allocation). The registry map and its mutex are used only when a snapshot is installed. No lock is held across I/O.

6. **Hot reload.** Stats are keyed by Provider name, base URL and Upstream Model.
   - A reload keeps the stats of pairs that are still configured (a rotated API key or a priority change keeps them) and drops the others.
   - New entries are created before the new snapshot is stored, and dropped ones are removed after it, so an installed snapshot never points at a missing entry.
   - An attempt that finishes on an old Snapshot after its pair was dropped records into the detached entry, which disappears with that snapshot. Removed pairs therefore do not reappear, and a pair added again starts from zero even while old requests are still running.
   - Unlike Cooldowns, the API key is not part of the key: a new key does not change how fast the Provider is.

7. **No Prometheus endpoint.** The repository has none. The JSON is the interface.
