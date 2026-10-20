# Gonka Proxy

**No more 429s. No more 502s.** Gonka Proxy sits in front of your LLM providers and gives you one stable, OpenAI-compatible endpoint to point your app at. If one provider is rate-limited or down, it silently hands the request to the next one — your app never sees the error.

Tiny and cheap: it runs in under **10 MB of RAM** even under load, so you can run it next to anything.

## What it does

- Exposes **one local OpenAI-compatible endpoint** (`/v1/chat/completions`) — your app keeps talking to one URL, no matter what's happening upstream.
- Maintains a **priority-ordered pool of providers** (e.g. primary, then backups).
- On a **402, 429, 404 (optionally) or 5xx** (or a timeout/network error), it **fails over** to the next available provider in order.
- A failed provider goes into a short **cooldown**, then comes back automatically. If every provider is down, it waits and retries until one responds or you cancel.
- Maps your **virtual model names** to each provider's real models — one proxy can serve many models, and each request only goes to providers that serve its model.
- Enforces a **reasoning effort** on every upstream request — globally, per model, per provider, or (if you allow it) per request.
- Can be **reconfigured at runtime** through an authenticated admin API or `SIGHUP`, without dropping in-flight requests or forgetting cooldowns.
- Can give up after a **max routing time** instead of waiting forever, and can require a **client token**.

## Configuration

Copy the example and edit it:

```sh
cp config.example.yaml config.yaml
```

The file is read at startup. To change it while running, send the process `SIGHUP` (for example `docker kill -s HUP gonka-proxy`) or push a new config through the [admin API](#admin-api); only `server.listen_address` needs a restart.

```yaml
server:
  listen_address: 0.0.0.0:8080   # where the proxy listens (host:port)

# Optional tuning — these are the defaults if omitted:
cooldown: 120s            # how long a failed provider is benched before retry
recovery_wait: 30s        # wait before probing again when all providers are down
response_header_timeout: 30s  # max time to wait for a provider's response headers
failover_on_404: false    # set true to try another provider after any upstream 404
failover_on_auth_error: false  # set true to fail over (and bench the provider) on upstream 401/403
max_routing_time: 0s      # 0 = never give up; e.g. 60s returns 503 after a minute (see below)
log_level: WARN           # INFO, WARN (default), or ERROR

reasoning_effort: xhigh   # required; see "Reasoning effort" below

providers:                # one block per upstream, higher priority = preferred
  - name: primary
    base_url: https://provider.example/v1   # API root, not a /chat/completions URL
    api_key: your-key
    model_alias: provider-model-name        # real model name sent upstream
    priority: 100
  - name: backup
    base_url: https://backup.example/v1
    api_key: your-key
    model_alias: backup-model-name
    priority: 50          # tried only when primary is down or rate-limited
```

List your providers in any order; Gonka always tries the **highest `priority` number first**. Equal priorities keep YAML declaration order. Add as many blocks as you like.

Set `failover_on_404: true` to treat any provider HTTP 404 as a failover failure: the proxy tries the next available provider and places the one that returned 404 in cooldown. The default is `false`, which passes 404 responses back to the client unchanged.

Set `failover_on_auth_error: true` to do the same for provider HTTP 401 and 403. The client's own `Authorization` header is never sent upstream, so these always mean the provider rejected *its* API key; with the option on, that provider is benched (all its models) and the request moves on. The default `false` passes them back unchanged.

An empty `providers` list is valid: every chat request then gets `503 no_provider_available` until providers are pushed through the admin API.

## Multiple models

A provider either uses `model_alias` (one upstream model for **every** model name a client sends — the original behaviour) or a `models` map that lists the virtual models it serves:

```yaml
reasoning_effort: high

models:                   # optional per-model settings
  chat-fast:
    reasoning_effort: low
  chat-smart: {}          # declared, inherits everything

providers:
  - name: gate-a
    base_url: https://gate-a.example/v1
    api_key: key-a
    priority: 100
    models:               # virtual model -> upstream model
      chat-fast: deepseek-v4-flash
      chat-smart: deepseek-v4
  - name: gate-b
    base_url: https://gate-b.example/v1
    api_key: key-b
    priority: 50
    enabled: false        # kept in the config and in /admin/status, gets no traffic
    models:
      chat-fast: flash-model-b
  - name: catch-all
    base_url: https://legacy.example/v1
    api_key: key-c
    priority: 10
    model_alias: some-model   # legacy form: serves every virtual model
```

- A request for `chat-smart` goes to `gate-a`, then `catch-all`; `gate-b` never sees it.
- A model nobody serves gets `404` with `{"error":{"type":"invalid_request_error","code":"model_not_found",...}}`. A model that is only served by disabled providers gets `503 no_provider_available` right away.
- A provider must set exactly one of `model_alias` and `models`.
- `GET /v1/models` lists the virtual models (from the `models` maps and the `models:` section) — or, with only `model_alias` providers, each alias once.
- Responses carry `X-Gonka-Provider` (provider name) and `X-Gonka-Model` (the upstream model actually used).
- A chat request body over 32 MiB gets `413 request_too_large` and never reaches a provider.

**Cooldowns are per provider *and* model.** A 5xx, timeout, 404 or unsupported `reasoning_effort` benches only that model on that provider; its other models keep serving. Failures about the account or the host — 402, 429, network errors, and 401/403 with `failover_on_auth_error` — bench the whole provider. See [ADR 0004](docs/adr/0004-multi-model-routing-and-cooldown-scope.md).

## Routing time limit

By default the proxy never gives up: when every provider is cooling down it waits `recovery_wait` and tries again until one answers or the client disconnects. Set `max_routing_time` to bound that:

```yaml
max_routing_time: 60s
```

A request that has no response after that long — failovers and recovery waits included — gets `503` with `{"error":{"type":"server_error","code":"no_provider_available",...}}`. Clients can shorten the limit per request with a header (a Go duration or seconds); it can never exceed a configured `max_routing_time`:

```http
X-Gonka-Max-Routing-Time: 5s
```

The limit only covers finding a provider: once a provider starts answering, the response (or stream) is never cut. An attempt aborted by the limit does not put the provider in cooldown. See [ADR 0007](docs/adr/0007-max-routing-time.md).

`log_level` is a minimum severity. `INFO` includes detailed lifecycle diagnostics—Provider selection and successes, Cooldown and Recovery Wait transitions, and cancellation—while `WARN` (the default) suppresses those INFO-only events but retains Failover Failure and stream-abort events. `ERROR` is the strictest threshold and emits only ERROR-level events. At `INFO`, an error may include a bounded provider error message or stream tail; prompts, request bodies, authorization headers, and API keys are never logged, and response content is suppressed at `WARN` and `ERROR`.

## Reasoning effort

`reasoning_effort` hints how much reasoning the upstream model should do. The proxy writes the resolved value into every upstream request — by default whatever the client sends is overwritten. Allowed values: `none`, `low`, `medium`, `high`, `xhigh`, `max`, plus `null` (`~`) to strip the field entirely. The top-level key is **required**.

Not all providers accept the parameter, so each provider block can override it:

```yaml
providers:
  - name: primary
    # ...
    reasoning_effort: high   # optional: use a different value for this provider
  - name: legacy
    # ...
    reasoning_effort: ~      # strip the field entirely for this provider
```

- Omitted on a provider → it inherits the global value.
- A value → overrides the global value for that provider.
- `~` → removes `reasoning_effort` from requests to that provider.

The same rules apply after failover: each provider always gets its own resolved setting.

Per model, and per request if you allow it:

```yaml
reasoning_effort: high              # global default
allow_client_reasoning_effort: false
models:
  chat-fast:
    reasoning_effort: low           # default for this model (~ strips it)
  chat-live:
    allow_client_reasoning_effort: true   # clients may send their own value for this model
```

Each upstream request uses the first that applies:

1. the provider's `reasoning_effort` (a value, or `~` to strip);
2. the client's `reasoning_effort`, if allowed for the model and present (an invalid value is answered with `400 invalid_reasoning_effort`; `null` counts as absent);
3. the model's `reasoning_effort` (a value, or `~` to strip);
4. the global `reasoning_effort`.

See [ADR 0006](docs/adr/0006-reasoning-effort-precedence.md).

### Provider support

> ***The data is current as of 2026-09-02. If you would like to add new data or update existing information, please open an issue and I will make the changes.***

Values each provider currently accepts for DeepSeek V4 Flash 0731 model:


| Provider       | low | medium | high | xhigh | max |
| -------------- | --- | ------ | ---- | ----- | --- |
| proxy.gonka.gg | OK  | OK     | OK   | OK    | OK  |
| GonkaRouter    | OK  | OK     | OK   | OK    | OK  |
| GonkaGate      | OK  | OK     | OK   | OK    | OK  |
| Gonka-API      | OK  | OK     | OK   | OK    | OK  |


**Note**: it seems that sometimes `reasoning_effort: max` support depends on devshard/node and might be unstable. Consider adding `reasoning_effort: high` fallbacks to your config.

A provider that does not support the configured value answers with **HTTP 400** and an error like `reasoning_effort: unsupported value: ...`. The proxy detects that message and **fails over** to the next provider, so a single unsupported provider no longer breaks the request. Other 400 responses are still passed back to your app unchanged. To avoid repeated cooldowns, point those providers at a value they accept (or `~` to strip the field) with a per-provider `reasoning_effort`.

To check provider support against your own configuration, run:

```sh
./check-reasoning-effort.sh
```

The script tests every configured provider with all five values, shows progress and an aligned results table.

## Launch

**Docker (recommended):**

```sh
cp config.example.yaml config.yaml   # then edit your providers
docker compose up --build
```

This publishes the proxy on `127.0.0.1:58081` and mounts `config.yaml` read-only. Point your app at `http://127.0.0.1:58081/v1`.

**Multiple instances:** to run several proxies side by side (each with its own providers, cooldowns, and log level), copy `compose.multi.example.yaml` to `compose.yaml` and give each service its own config file and host port:

```yaml
services:
  proxy-a:
    build: .
    command: ["--config", "/etc/gonka-proxy/config.yaml"]
    ports:
      - "127.0.0.1:58081:8080"
    volumes:
      - ./config-a.yaml:/etc/gonka-proxy/config.yaml:ro
  proxy-b:
    build: .
    command: ["--config", "/etc/gonka-proxy/config.yaml"]
    ports:
      - "127.0.0.1:58082:8080"
    volumes:
      - ./config-b.yaml:/etc/gonka-proxy/config.yaml:ro
```

Every service mounts its config at the same in-container path (`/etc/gonka-proxy/config.yaml`) — only the host-side file differs. `config-*.yaml` and `compose.yaml` are gitignored, so per-instance credentials never get committed.

**Native (no Docker):**

```sh
go run ./cmd/gonka-proxy --config config.yaml
```

## Point your app at it

Set your OpenAI-compatible client's `base_url` to `http://127.0.0.1:58081/v1` (or your chosen address) and use one of your virtual model names (or, with `model_alias` providers, any name). The proxy handles the rest.

`GET /v1/models` lists the configured models, so clients that discover or test models (e.g. custom-provider forms) work against the proxy.

## Access tokens

Both tokens are read from the environment, never from the config file:

| Variable | Effect |
| --- | --- |
| `GONKA_PROXY_CLIENT_TOKEN` | When set, every `/v1/*` request needs `Authorization: Bearer <token>`; otherwise `401` with `{"error":{"type":"invalid_request_error","code":"invalid_api_key",...}}`. The client's header is never forwarded — providers always receive their own `api_key`. |
| `GONKA_PROXY_ADMIN_TOKEN` | Enables the admin API (at least 16 characters, or the proxy refuses to start). Unset: `/admin/*` answers `404`. |
| `GONKA_PROXY_ADMIN_LISTEN_ADDRESS` | Optional `host:port` for a separate admin listener; `/admin/*` is then served only there. Default: the proxy's own port. |
| `GONKA_PROXY_ADMIN_PERSIST` | `true` writes every accepted admin config back to the `--config` file (atomically, mode 0600). Needs the config **directory** mounted writable. Default `false`. |
| `GONKA_PROXY_STATS_WINDOW` | Sliding window of the [route stats](#route-stats) (windowed counters and p50/p95), a duration of at least `1s`. Default `15m`. |
| `GONKA_PROXY_STATS_EWMA_ALPHA` | Weight of the newest sample in the TTFB and tokens/s EWMAs, `0 < alpha ≤ 1`. Default `0.2`. |

Tokens are compared in constant time.

## Admin API

All endpoints need `Authorization: Bearer $GONKA_PROXY_ADMIN_TOKEN` (`401 invalid_admin_token` otherwise) and answer JSON. Errors use the OpenAI shape `{"error":{"message","type","param","code"}}`.

| Method and path | Body | Success | Errors |
| --- | --- | --- | --- |
| `GET /admin/config` | — | `200 {"version","loaded_at","config":{...}}` — the normalized config with every `api_key` shown as `****` + last 4 characters | — |
| `PUT /admin/config` | the full config, YAML (`application/yaml`, or no Content-Type) or JSON (`application/json`), same keys and rules as the file | `200 {"version","changed","persisted","loaded_at"}` | `400 invalid_config` (nothing changed), `413 request_too_large` (> 1 MiB), `415 unsupported_media_type`, `500 persist_failed` (nothing changed) |
| `GET /admin/status` | — | `200` routing status (below) | — |

```sh
curl -sS -X PUT http://gonka-proxy:8080/admin/config \
  -H "Authorization: Bearer $GONKA_PROXY_ADMIN_TOKEN" \
  -H 'Content-Type: application/yaml' --data-binary @config.yaml
# {"version":"5f0c…","changed":true,"persisted":false,"loaded_at":"2026-10-19T12:00:00Z"}
```

- **Atomic swap.** A valid config replaces the running one at once; requests already in flight finish on the config they started with. Cooldowns of providers with the same name, `base_url` and `api_key` are kept; removed providers and models are forgotten.
- **`version`** is the SHA-256 (hex) of the normalized config: the same config in YAML or JSON, in any key order, has the same version; any change — a rotated key included — changes it. Pushing an identical config answers `"changed": false`.
- **Not persisted by default**: after a restart the proxy reads its file again. Compare `version` and re-push if needed, or enable `GONKA_PROXY_ADMIN_PERSIST` (comments in the file are not kept).
- `server.listen_address` changes need a restart.

`GET /admin/status`:

```json
{
  "version": "5f0c…",
  "loaded_at": "2026-10-19T12:00:00Z",
  "providers": 2,
  "enabled_providers": 1,
  "provider_status": [
    {
      "name": "gate-a", "priority": 100, "enabled": true, "legacy": false,
      "available": true, "cooldown_until": null, "last_failure": null,
      "models": [
        {"virtual": "chat-fast", "upstream": "deepseek-v4-flash", "available": true, "cooldown_until": null, "last_failure": null},
        {"virtual": "chat-smart", "upstream": "deepseek-v4", "available": false,
         "cooldown_until": "2026-10-19T12:02:00Z",
         "last_failure": {"kind": "status", "category": "server-error", "status": 503, "scope": "model", "at": "2026-10-19T12:00:00Z"}}
      ]
    }
  ]
}
```

The provider-level `cooldown_until` / `last_failure` describe provider-wide cooldowns; the model entries combine both. `kind` is `status`, `timeout`, `network` or `stream`; `scope` is `provider` or `model`. A `model_alias` provider reports one model with `"virtual": "*"`. See [ADR 0005](docs/adr/0005-admin-api-and-hot-reload.md).

### Route stats

`GET /admin/status` also reports traffic per **(provider, upstream model)** pair. There is one entry for every configured pair, disabled providers included, in routing order: provider priority, then upstream model by name. Every upstream attempt counts, failovers included. A client request that failed over from `gate-a` to `gate-b` therefore adds a failure to `gate-a` and a success to `gate-b`. API keys and base URLs are never included.

```json
{
  "version": "5f0c…",
  "loaded_at": "2026-10-19T12:00:00Z",
  "providers": 1, "enabled_providers": 1, "provider_status": [ … ],
  "stats_window_seconds": 900,
  "stats_ewma_alpha": 0.2,
  "stats": [
    {
      "provider": "gate-a",
      "upstream": "deepseek-v4",
      "virtual_models": ["chat-smart"],
      "enabled": true,
      "since": "2026-10-19T12:00:00.000Z",
      "total": {
        "requests": 121, "successes": 112, "failures": 6,
        "failures_by_class": {"timeout": 2, "5xx": 3, "4xx": 1, "connection": 0},
        "canceled": 1, "client_errors": 1, "deadline": 1
      },
      "window": {
        "requests": 14, "successes": 13, "failures": 1,
        "failures_by_class": {"timeout": 0, "5xx": 1, "4xx": 0, "connection": 0},
        "canceled": 0, "client_errors": 0, "deadline": 0
      },
      "ttfb_ms": {
        "stream":     {"ewma": 412.5, "p50": 380.2, "p95": 910.7, "samples": 80, "window_samples": 9, "window_samples_truncated": false},
        "non_stream": {"ewma": 2210.4, "p50": 1980.0, "p95": 4105.3, "samples": 32, "window_samples": 4, "window_samples_truncated": false}
      },
      "tokens_per_second": {
        "stream":     {"ewma": 38.1, "p50": 36.4, "p95": 51.9, "samples": 74, "window_samples": 8, "window_samples_truncated": false},
        "non_stream": {"ewma": 21.7, "p50": 20.9, "p95": 27.5, "samples": 30, "window_samples": 4, "window_samples_truncated": false}
      },
      "last_success_at": "2026-10-19T12:14:58.120Z",
      "last_failure_at": "2026-10-19T12:09:03.554Z",
      "last_error_class": "5xx"
    },
    {
      "provider": "gate-a", "upstream": "deepseek-v4-flash", "virtual_models": ["chat-fast"], "enabled": true,
      "since": "2026-10-19T12:00:00.000Z",
      "total":  {"requests": 0, "successes": 0, "failures": 0, "failures_by_class": {"timeout": 0, "5xx": 0, "4xx": 0, "connection": 0}, "canceled": 0, "client_errors": 0, "deadline": 0},
      "window": {"requests": 0, "successes": 0, "failures": 0, "failures_by_class": {"timeout": 0, "5xx": 0, "4xx": 0, "connection": 0}, "canceled": 0, "client_errors": 0, "deadline": 0},
      "ttfb_ms": {
        "stream":     {"ewma": null, "p50": null, "p95": null, "samples": 0, "window_samples": 0, "window_samples_truncated": false},
        "non_stream": {"ewma": null, "p50": null, "p95": null, "samples": 0, "window_samples": 0, "window_samples_truncated": false}
      },
      "tokens_per_second": {
        "stream":     {"ewma": null, "p50": null, "p95": null, "samples": 0, "window_samples": 0, "window_samples_truncated": false},
        "non_stream": {"ewma": null, "p50": null, "p95": null, "samples": 0, "window_samples": 0, "window_samples_truncated": false}
      },
      "last_success_at": null, "last_failure_at": null, "last_error_class": null
    }
  ]
}
```

| Field | Meaning |
| --- | --- |
| `provider`, `upstream` | The provider name and upstream model, which are the values of the `X-Gonka-Provider` and `X-Gonka-Model` response headers. Together they identify the entry. |
| `virtual_models` | The virtual models this provider routes to `upstream`, sorted (`["*"]` for a `model_alias` provider). Several virtual models can share one upstream model, and so one entry. |
| `enabled` | `false` for a disabled provider. It keeps its stats but gets no traffic. |
| `since` | When counting started: process start, or the reload that added the pair. Timestamps are RFC 3339 UTC with milliseconds. |
| `total` / `window` | Counters since `since` / over the last `stats_window_seconds` (resolution: 1/60 of the window). `requests = successes + failures + canceled + client_errors + deadline`. |
| `failures` | Failures of the provider, split in `failures_by_class`. **`timeout`**: the response headers did not arrive within `response_header_timeout` or the operator's `max_routing_time`, or a body read timed out. **`5xx`**: a 5xx answer. **`4xx`**: 402 or 429, the 400 for an unsupported `reasoning_effort`, and 401, 403 and 404. A 401, 403 or 404 means a rejected key or a missing upstream model, so it counts even when it is passed back to the client because the failover switches are off. **`connection`**: a connect error, a broken or reset body, or a stream that ended without `[DONE]`. |
| `canceled` | The client went away (closed the connection or stopped reading). **Not** a failure. |
| `client_errors` | A client-caused 4xx (400, 422, …) passed back to the client unchanged. It is neither a failure nor a success. |
| `deadline` | The request's routing deadline cut the attempt. Either the client's shorter `X-Gonka-Max-Routing-Time` ran out while the provider had not answered, or the provider answered but the deadline (from either source) expired first. **Not** a failure. When the operator's `max_routing_time` runs out before the provider answers, it counts as a `timeout` failure instead. |
| `ttfb_ms.{stream,non_stream}` | Time from sending the upstream request to the first byte of the response body (the first chunk for a stream), in ms. Measured only for successful attempts. |
| `tokens_per_second.{stream,non_stream}` | Output rate of successful attempts: `usage.completion_tokens` from the upstream response divided by the generation time. For streams the time runs from the first chunk to the end, for non-streaming responses over the whole attempt. Attempts without usage add no sample. For streams, the proxy reads the last `usage` chunk, which upstreams send only when the client asks with `"stream_options": {"include_usage": true}`. The proxy does not add that option itself: clients that want stream samples must send it. |
| `ewma` | Exponentially weighted moving average over all samples since `since`. `stats_ewma_alpha` is the weight of the newest sample. |
| `p50`, `p95` | Nearest-rank percentiles of the samples inside the window. |
| `samples`, `window_samples` | Samples since `since` / inside the window. `ewma`, `p50` and `p95` are `null` without samples. |
| `window_samples_truncated` | `true` when the window held more samples than the 1024 kept per metric and mode. The percentiles then cover only the newest 1024. |
| `last_success_at`, `last_failure_at`, `last_error_class` | `null` until the first success or failure. Cancels, client errors and deadlines change none of them. |

Stats live in memory and restart from zero with the process. On a hot reload, pairs whose provider name, `base_url` and upstream model are unchanged keep their stats, even if the `api_key` or priority changed. Removed pairs are dropped, and a pair that is added again starts from zero. A request still running on the old configuration when its pair is removed is not counted. See [ADR 0008](docs/adr/0008-route-stats.md).