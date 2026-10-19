# ADR 0004: Multi-Model Routing and Cooldown Scope

**Status:** Accepted
**Date:** 2026-10-19

## Context

Until now every Provider had one `model_alias`: it served whatever Virtual Model the client sent and rewrote it to that single upstream model. One proxy therefore exposed one model. Callers such as the product-broker platform need several models at once (a cheap fast model for classification, a stronger one for drafting, each with its own reasoning effort) behind one endpoint, and the same Provider account usually serves several of them.

Keying Cooldowns per Provider no longer fits either: one broken model on a Provider (a 5xx from an overloaded node, an unsupported `reasoning_effort` for that model) would bench the Provider's healthy models too.

## Decision

1. **Two Provider forms.** A Provider sets exactly one of:
   - `model_alias` (legacy): serves *every* Virtual Model and rewrites it to the alias. Unchanged behaviour.
   - `models`: a map Virtual Model → Upstream Model. The Provider serves only the listed Virtual Models.
   Setting both, or neither, is a validation error. An empty `models` map is an error.

2. **Routing.** A request is routed in priority order across the *enabled* Providers that serve its Virtual Model: map entries plus every legacy Provider. If no configured Provider (enabled or not) and no `models:` entry knows the Virtual Model — and there is no legacy Provider — the proxy answers `404` with `{"error":{"type":"invalid_request_error","code":"model_not_found",...}}` without contacting a Provider or touching any Cooldown. A missing or non-string `model` is treated as an unknown model. A known model whose Providers are all disabled answers `503 no_provider_available` immediately.

3. **Optional `models:` section.** A top-level map of per-Virtual-Model settings (`reasoning_effort`, `allow_client_reasoning_effort`, see ADR 0006). Declaring a model there also makes it known (listed by `GET /v1/models`), even before a Provider serves it.

4. **Cooldown scope.** Cooldowns are keyed by *(Provider, Upstream Model)*, with a second, Provider-wide key:

   | Failover Failure | Scope | Why |
   | --- | --- | --- |
   | HTTP 5xx | model | node or model overload is usually per model |
   | response-header timeout | model | slow models time out, fast ones do not |
   | HTTP 404 (with `failover_on_404`) | model | the Provider lacks that model |
   | HTTP 400 unsupported `reasoning_effort` | model | the effort is accepted per model |
   | stream cut / EOF without `[DONE]` | model | a model stream broke |
   | HTTP 402 | **Provider** | the account has no balance |
   | HTTP 429 | **Provider** | rate limits are per key/account on the Gonka brokers |
   | HTTP 401 / 403 (with `failover_on_auth_error`) | **Provider** | the key is rejected |
   | network error (connect, reset, DNS) | **Provider** | the host is unreachable for every model |

   A route is available when neither its Provider-wide nor its model key is cooling. A Recovery Wait clears both kinds, with the existing version check (only cooldowns not renewed during the wait are cleared).

5. **401/403 stay Client Errors by default.** Passing an upstream 401/403 to the client is the existing behaviour, so `failover_on_auth_error` (default `false`) opts in. Because the client's `Authorization` is never forwarded, an upstream 401/403 is always about the Provider's key; deployments where the proxy owns all keys (an admin-managed pool) should enable it.

6. **Legacy behaviour is identical.** A legacy Provider has exactly one Upstream Model, so "this Provider's model" and "this Provider" bench the same routes; logs keep the plain Provider name. Multi-model Providers log `name [upstream-model]` for model-scoped cooldowns.

7. **Headers.** `X-Gonka-Provider` is the serving Provider's name and `X-Gonka-Model` is the Upstream Model actually sent (not the Virtual Model).

8. **`GET /v1/models`** lists every known Virtual Model (from Providers' `models` maps and the `models:` section) sorted by name; when there are none (only legacy Providers), it lists each Model Alias once in priority order.

## Consequences

- One proxy serves many Virtual Models; a misbehaving model no longer takes a Provider's other models out of rotation.
- Account-level problems still bench the whole Provider, so the proxy does not hammer a Provider with an exhausted balance once per model.
- Cooldown state grows with Providers × models; entries are dropped when a reload removes the Provider or model (ADR 0005).

## Alternatives Considered

- **Per-model Provider entries (one Provider per model, as today).** Rejected: duplicates keys and URLs, and a 429 on one entry would not bench its siblings on the same account.
- **Always Provider-wide cooldowns.** Rejected: one model's 5xx would bench healthy models.
- **Always model-scoped cooldowns.** Rejected: a 402/429 would be retried once per model before the Provider is fully benched.
- **Making 401/403 failover by default.** Rejected for backward compatibility; see decision 5.
