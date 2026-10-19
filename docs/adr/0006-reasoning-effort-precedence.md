# ADR 0006: Reasoning Effort Precedence

**Status:** Accepted
**Date:** 2026-10-19
**Supersedes (in part):** [ADR 0001](0001-reasoning-effort.md) decision 3 and [ADR 0002](0002-per-provider-reasoning-effort.md) decision 2 — the rule that every client-supplied value is overwritten, and the two-level resolution order. The required global key, the enum ([ADR 0003](0003-reasoning-effort-enum-expansion.md)) and the overwrite/strip mechanics stay.

## Context

With several Virtual Models per proxy (ADR 0004), one global `reasoning_effort` (plus per-Provider overrides) is too coarse: a classification model wants `low`, a drafting model `high`. Some callers also know best per request (a real-time path may ask for `none`). Per-Provider overrides must still win, because they exist for backends that reject a value or the parameter.

## Decision

1. **Per-Virtual-Model default.** `models.<name>.reasoning_effort` accepts the same values as the global key, `~`/`null` to strip, or is omitted to inherit.

2. **Client values, opt-in.** `allow_client_reasoning_effort: true` (top level, default `false`), overridable per Virtual Model with `models.<name>.allow_client_reasoning_effort`, lets a client-supplied `reasoning_effort` be used. A JSON `null` or absent field counts as "not supplied". When allowed, a value outside the enum (or not a string) is rejected with `400 {"error":{"type":"invalid_request_error","code":"invalid_reasoning_effort","param":"reasoning_effort"}}` before any Provider is contacted. When not allowed, the client value is overwritten exactly as before, valid or not.

3. **Precedence**, evaluated per attempt (so each Provider still gets its own value after failover):
   1. the Provider's `reasoning_effort` (value, or `~` to strip);
   2. the client's value, when allowed and supplied;
   3. the Virtual Model's `reasoning_effort` (value, or `~` to strip);
   4. the global `reasoning_effort` (value, or `~` to strip).

4. **Unsupported-value failover unchanged.** An HTTP 400 naming `reasoning_effort` as an unsupported value is still a Failover Failure; its Cooldown is scoped to the (Provider, Upstream Model) pair (ADR 0004).

## Consequences

- Existing configs resolve exactly as before: no `models:` section and `allow_client_reasoning_effort` off reduce the order to Provider override > global.
- Config is no longer the only source of the value when an operator opts in; the opt-in is explicit and scoped per model.

## Alternatives Considered

- **Client value beats the Provider override.** Rejected: Provider overrides exist because a backend rejects a value; letting clients bypass them would bring the 400s back.
- **Silently ignore invalid client values when allowed.** Rejected: hides client bugs; an explicit 400 is clearer.
- **Per-(Provider, model) efforts.** Not needed yet; can be added later as an object form of the `models` map value.
