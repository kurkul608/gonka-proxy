# Gonka Proxy

Gonka Proxy routes OpenAI-compatible inference requests across prioritized providers while preserving the client-facing API contract.

## Language

**Provider**:
An OpenAI-compatible inference endpoint configured with a human-readable name, base URL, API key, priority, and either one Model Alias (legacy) or a map of Virtual Models to Upstream Models. A Provider can be disabled (`enabled: false`): it stays configured and reported but receives no traffic.
_Avoid_: gate (the product-broker admin UI calls Providers "Gonka gates"; in this repo the term is Provider)

**Virtual Model**:
The model name sent by the client. It identifies the proxy configuration rather than an upstream model and is not forwarded to providers. A request is routed only to Providers that serve its Virtual Model; legacy Providers serve every Virtual Model.

**Upstream Model**:
The provider-specific model name that replaces the Virtual Model in an upstream request: the Model Alias of a legacy Provider, or the `models` map entry of a multi-model Provider. Reported in the `X-Gonka-Model` response header.
_Avoid_: real model, target model

**Model Alias**:
The single Upstream Model of a legacy Provider (`model_alias`), used for every Virtual Model routed to it.
_Avoid_: Model, virtual model

**Model Settings**:
Optional per-Virtual-Model settings in the top-level `models:` section: a default Reasoning Effort and whether client-supplied Reasoning Effort is allowed.

**Failover Failure**:
A provider response or connection failure that permits retrying another provider and starts a Cooldown. This includes HTTP `402`, HTTP `429`, HTTP `5xx`, timeouts, network errors, an HTTP `400` whose error message names `reasoning_effort` as an unsupported value, HTTP `404` when 404 failover is enabled, and HTTP `401`/`403` when auth-error failover is enabled.
_Avoid_: Any non-200 response, provider error

**Client Error**:
An HTTP `4xx` response other than `402` or `429` that is returned to the client unchanged and does not affect provider availability. A `400` naming `reasoning_effort` as an unsupported value is a Failover Failure instead, as are `404` (when 404 failover is enabled) and `401`/`403` (when auth-error failover is enabled).
_Avoid_: Provider failure

**Cooldown**:
A configured period during which a route that produced a Failover Failure is excluded from routing. Its scope is either one (Provider, Upstream Model) pair or, for key and account failures (`402`, `429`, network errors, enabled `401`/`403`), the whole Provider.
_Avoid_: Ban, disablement

**Routing Pass**:
One attempt to obtain a successful response by trying every available Provider that serves the Virtual Model, in descending priority order.
_Avoid_: Retry

**Recovery Wait**:
A configured delay entered when no providers remain available. When it ends, all cooldowns are cleared and a new Routing Pass begins; this repeats until success, client cancellation, or the Max Routing Time.
_Avoid_: Retry limit, terminal failure

**Max Routing Time**:
The longest a request may spend obtaining a response across Failovers, Routing Passes and Recovery Waits (`max_routing_time`, shortened per request with `X-Gonka-Max-Routing-Time`). When exceeded the client gets `503 no_provider_available`. It does not limit a response once it has started.

**Reasoning Effort**:
An OpenAI-compatible request field (`reasoning_effort`) that hints how much reasoning the upstream model should do. For every upstream request the proxy resolves one value — Provider override, then (if allowed) the client's value, then the Virtual Model default, then the global value — and overwrites the client's field with it; a resolved `null` strips the field.
_Avoid_: reasoning level, thinking budget

**Snapshot**:
One immutable, validated routing configuration. A request uses the Snapshot that was current when it arrived; a Hot Reload replaces it for new requests only.

**Hot Reload**:
Replacing the configuration while running — `PUT /admin/config` or `SIGHUP` — with the same validation as startup. Cooldowns of Providers whose name, base URL and API key are unchanged are kept.
_Avoid_: restart

**Config Version**:
The hex SHA-256 of the normalized configuration. Equal configurations have equal versions whatever their formatting; any change, including a rotated API key, changes it.

**Admin Token**:
The bearer token (`GONKA_PROXY_ADMIN_TOKEN`) that enables and guards the `/admin/*` API.

**Client Token**:
The optional bearer token (`GONKA_PROXY_CLIENT_TOKEN`) clients must send to `/v1/*`. It is never forwarded upstream; Providers always receive their own API key.
