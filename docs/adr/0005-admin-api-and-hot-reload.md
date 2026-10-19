# ADR 0005: Admin API and Hot Reload

**Status:** Accepted
**Date:** 2026-10-19

## Context

The configuration was loaded once at startup; every change needed a restart, which drops in-flight requests and forgets Cooldowns. The product-broker platform manages Providers (keys, priorities, models) in its own admin UI and must push changes to the proxy at runtime. The proxy runs on an internal Docker network, but the configuration contains API keys, so changing or reading it needs authentication.

## Decision

1. **Immutable snapshots.** All routing state derived from the configuration (sorted Providers, model index, timings, flags, HTTP client) lives in one immutable snapshot behind an atomic pointer. A request loads the snapshot once and uses it until it finishes; a reload stores a new snapshot. In-flight requests (including Recovery Waits and streams) therefore finish on the configuration they started with, and new requests use the new one, without locks on the hot path.

2. **Cooldowns survive reloads.** Cooldown state is kept outside the snapshot, keyed by Provider identity *(name, base URL, API key)* and Upstream Model. After a swap, entries whose Provider identity no longer exists, or whose Upstream Model the Provider no longer serves, are dropped. A rotated key or a renamed Provider therefore starts clean; a priority or reasoning-effort change keeps its cooldowns. The HTTP transport is reused unless `response_header_timeout` changes (then the old one's idle connections are closed).

3. **One validation path.** `PUT /admin/config`, `SIGHUP` and startup all go through `config.Parse` + `Config.Validate`. Admin pushes accept YAML or JSON with the file's keys and rules (JSON is converted to YAML before the strict decoder, so unknown keys are rejected in both).

4. **Admin API, enabled by environment.** `GONKA_PROXY_ADMIN_TOKEN` (at least 16 characters; a shorter value is a startup error) enables `/admin/*`; without it the paths answer 404 as before. Requests need `Authorization: Bearer <token>`; tokens are compared as SHA-256 digests with `crypto/subtle`, so the comparison time does not depend on the presented value. Endpoints:
   - `GET /admin/config` — the active configuration as a normalized JSON document with every `api_key` shown as `****` + last four characters (or `****` for keys shorter than 12), plus `version` and `loaded_at`.
   - `PUT /admin/config` — replace the configuration. `400 invalid_config` (nothing changes) or `200 {version, changed, persisted, loaded_at}`.
   - `GET /admin/status` — per Provider and model: enabled, available, cooldown_until, last failure (kind, category, status, scope, time).

5. **Tokens and admin settings come from the environment, not the config.** A pushed config can therefore never disable or change admin access, and the config document stays the routing configuration only. `GONKA_PROXY_ADMIN_LISTEN_ADDRESS` optionally serves `/admin/*` on its own listener (and then not on the proxy port), for deployments that publish the proxy port wider than the admin port.

6. **Version.** `version` is the hex SHA-256 of the normalized configuration's canonical JSON (defaults filled in, values normalized, map keys sorted, API keys included). Equivalent YAML/JSON documents share a version; any change, including a key rotation, changes it. Re-pushing an identical config answers `changed: false` and keeps `loaded_at`.

7. **Persistence is opt-in.** With `GONKA_PROXY_ADMIN_PERSIST=true`, an accepted config is written to the `--config` path (temporary file in the same directory, `fsync`, rename, mode 0600) *before* the swap; a write failure answers `500 persist_failed` and keeps the running configuration. Default off, because configs are usually mounted read-only; persistence needs the config's directory mounted writable (a rename cannot replace a single-file bind mount). Comments in the file are not preserved.

8. **`SIGHUP`** reloads the `--config` file; an invalid file is logged and the running configuration stays.

9. **`server.listen_address`** is read at startup only; a reload that changes it logs a warning.

## Consequences

- The admin UI can push configurations at any time without restarts or lost cooldowns.
- Reading and writing the configuration requires the admin token; API keys never leave the proxy in clear text through the API.
- A config file and the running configuration can differ (pushes without persistence). The integrator owns the source of truth and should re-push after a proxy restart (compare `version`).

## Alternatives Considered

- **File watching (inotify).** Rejected: unreliable on bind mounts, and pushes would still need an API.
- **Admin settings in the config file.** Rejected: a bad push could lock the operator out.
- **Mutex-guarded mutable config.** Rejected: contention on the request path and hard-to-reason partial updates.
- **Resetting cooldowns on every reload.** Rejected: an admin edit would re-expose a broken Provider to traffic.
