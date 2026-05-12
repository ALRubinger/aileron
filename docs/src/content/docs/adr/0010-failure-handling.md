---
title: "ADR-0010: Failure-Handling Policy"
description: "Failures are visible and structured; idempotency is the default; bounded retry on retriable errors; v1 actions are linear with first-failure-terminates semantics. LLM fallback and conditional compensation are post-MVP enhancements."
order: 10
---


<div class="meta">
<table>
  <tr><th>Status</th><td>Accepted</td></tr>
  <tr><th>Date</th><td>2026-04-29</td></tr>
  <tr><th>Tracking</th><td><a href="https://github.com/ALRubinger/aileron/issues/343">#343</a></td></tr>
</table>
</div>

## Context

Several earlier ADRs deferred questions about what happens when things go wrong:

- [ADR-0003](/adr/0003-action-model) — *what happens when one of an action's connector calls fails partway through?*
- [ADR-0005](/adr/0005-sandbox-choice) — *what error categories does the sandbox boundary produce, and how do they map to retry policy?*
- [ADR-0009](/adr/0009-user-channel) — *what happens when an approval times out, is denied, or fails to render?*

Failure is not exceptional. In a system that talks to external APIs (Slack rate-limits, Stripe goes down, GitHub returns 5xx), runs sandboxed code (resource limits, connector bugs), gates calls on credentials (expired tokens, revoked scopes), and asks users for approval (denials, timeouts), things will fail constantly. The architectural question is what the runtime promises about *how* failures surface and *what* is retried.

Two failure modes are uniquely dangerous in this domain and shape the decision:

1. **Silent failure.** An action appears to succeed but didn't. The user thinks the email was sent; it wasn't. The user thinks the ticket was filed; it wasn't. Recovery is much harder when the failure is invisible than when it's surfaced loudly.

2. **Inappropriate retry.** An action fails mid-execution; the runtime retries automatically; the partial work from the first attempt and the work from the second attempt combine into a doubled side effect. The user gets two emails. The card is charged twice. The ticket appears twice in different states.

These two failure modes pull in opposite directions. Avoiding the first wants noisy, immediate failure surfacing. Avoiding the second wants careful, idempotent retry. The policy this ADR ratifies threads both: **failures are visible by default, retries are idempotent by default, and authors who need different behavior must opt in explicitly.**

## Decision

### Visible failure is the default

When any operation fails — sandbox capability denial, connector runtime error, network failure, external API error, anything — the failure surfaces immediately to the calling agent through a structured error. It is not silently retried, swallowed, fallback-substituted, or smoothed over.

The agent sees the failure. The user sees the failure (through the agent's chat output, since the failure rides back as the MCP tool result the agent host renders into the conversation per [ADR-0008](/adr/0008-intent-matching)). The audit log records the failure. There is no path through the runtime where an action "didn't quite work" but the agent thinks it did.

This is the load-bearing rule. Aileron's value proposition is *deterministic execution* — the user can trust that what the agent believes happened is what actually happened. A runtime that masks failures behind silent retries or LLM-generated fallbacks gives that trust away.

### Errors are structured

Every error returned to the calling action (and through it, to the agent) follows a fixed schema:

```json
{
  "error": {
    "class": "<failure-class-string>",
    "message": "<user-facing description>",
    "retriable": true | false,
    "boundary": "<which-layer-produced-the-error>",
    "audit_id": "audit-<id>",
    "details": { ...class-specific fields... }
  }
}
```

| Field | Meaning |
|---|---|
| `class` | One of the canonical failure classes (see below). Stable across versions. |
| `message` | Human-readable description; safe to show to the user. Does not contain credentials or sensitive payload. |
| `retriable` | Whether this kind of failure is safe to retry. False for terminal errors (capability denied, signature failure, hash mismatch); true for transient errors (network timeout, 5xx from upstream API). |
| `boundary` | Which layer produced the error: `sandbox`, `connector_manifest`, `action`, `runtime`, or `external` (the upstream API). The `user` boundary is reserved for post-MVP approval-related errors. |
| `audit_id` | Reference into the audit log for full context. |
| `details` | Class-specific additional fields (e.g., the host that was denied, the credential scope that didn't match). |

Agents and action authors can rely on these fields. The `class` taxonomy is closed — adding a new class requires an ADR or amendment to this one.

### The canonical failure classes

| Class | Boundary | Retriable | Meaning |
|---|---|---|---|
| `capability_denied` | sandbox / connector_manifest / action | false | A capability check refused the operation. Defense-in-depth check (see [ADR-0002](/adr/0002-connector-model), [ADR-0003](/adr/0003-action-model), [ADR-0005](/adr/0005-sandbox-choice)). |
| `binding_required` | runtime | false (until bound) | No credential is bound for a required capability ([ADR-0006](/adr/0006-capability-binding-ux)). The user must bind one and retry. |
| `binding_failed` | runtime | depends on cause | An existing binding refused: token expired, key revoked, scope changed. Sometimes recoverable through `aileron binding rebind`. |
| `resource_limit_exceeded` | sandbox | false | The connector instance hit a resource limit (memory, wall time, fuel) ([ADR-0005](/adr/0005-sandbox-choice)). |
| `connector_runtime_error` | sandbox | depends on cause | The connector code itself failed: panicked, returned an unexpected shape, threw an exception. |
| `network_error` | external | true | Outbound network call failed before reaching a server (DNS, connection refused, TLS error). |
| `external_api_error` | external | depends on status | The upstream service returned an error response. Includes the status code in `details`. Retriable for 5xx and 429; not retriable for most 4xx. |
| `hash_mismatch` | runtime | false | A connector or action's on-disk bytes don't match the declared hash ([ADR-0004](/adr/0004-dependency-resolution)). |
| `signature_failure` | runtime | false | A binary's signature doesn't verify ([ADR-0007](/adr/0007-install-consent)). |

Two additional classes — `approval_denied` and `approval_timeout` — are reserved for post-MVP per-invocation approval per [ADR-0009](/adr/0009-user-channel). They become live when that flow lands; until then, no path in the runtime produces them.

Every class has a default `retriable` value. The taxonomy is closed; adding a class requires an ADR amendment.

### Idempotency by default

Actions are assumed idempotent unless the manifest declares otherwise. An action that posts a Slack message, files a Linear ticket, or sends an email — these are actually *not* idempotent in the strict sense, but Aileron treats the *runtime's retry* as a same-side effect: if the runtime retries an action because of a transient failure, the connector is responsible for ensuring the retry doesn't double-send.

Connectors achieve this through standard idempotency-key patterns:

- For **HTTP requests**, the connector includes an idempotency key in the request (e.g., Stripe's `Idempotency-Key` header, Slack's `client_msg_id`); the upstream service deduplicates.
- For **operations without server-side idempotency**, the connector implements a local-side check (e.g., "did I already send this exact email in the last 60 seconds?") before retrying.
- For **operations that genuinely cannot be made idempotent** (the connector author's judgment), the connector author opts out:

```toml
[connector]
name = "github://acme/legacy-payment-rail"
version = "0.3.0"

[connector.idempotency]
default = "not_idempotent"
```

A non-idempotent connector tells the runtime: "do not retry me. If a call fails partway, surface the failure and let the calling action decide." Retry behavior at the action layer is then opt-in per call rather than the default.

Action manifests can also override per-call:

```toml
[[execute]]
id = "send"
connector = "github://acme/legacy-payment-rail"
op = "charge_card"
idempotent = false
```

Hub validation in v1 enforces only the manifest-side discipline: the `[connector.idempotency]` declaration is honored, and the runtime simply does not retry non-idempotent connectors. Per-call action overrides are recognized in the schema but are flagged for post-MVP retry-tuning work.

### The runtime retries `retriable` errors with bounded backoff

When a failure has `retriable: true`, the runtime may retry the operation. v1 defaults:

- **Maximum 3 retries** per call.
- **Exponential backoff**: 1s, 2s, 4s, with jitter.
- **Total wall-time cap**: matches the action's wall-time limit (per [ADR-0005](/adr/0005-sandbox-choice)).
- **Network errors and `external_api_error` with 5xx/429** are retried.
- **Other classes** are not retried even if `retriable: true`.

After max retries, the failure surfaces with the original class and a `retried: 3` field in `details` so the agent and audit log know what was tried.

These defaults apply uniformly in v1; per-call retry tuning (overriding max retries or backoff per `[[execute]]` step) is a post-MVP enhancement. The default policy covers the v1 use cases; tuning surfaces are speculative until concrete actions need them.

### Multi-step actions: atomic failure, no auto-compensation

[ADR-0003](/adr/0003-action-model) establishes that actions are atomic — there is no inter-action dependency graph. But a single action can have multiple `[[execute]]` steps, and one of those steps can fail.

The rule: **the action fails as a whole on the first step failure.** The action returns the failure of the failing step. Earlier steps that completed successfully are *not* automatically rolled back.

```toml
[[execute]]
id = "find_pr"
connector = "github://aileron/github"
op = "find_pr_by_branch"

[[execute]]
id = "post"
connector = "github://aileron/slack"
op = "post_message"

[[execute]]
id = "react"
connector = "github://aileron/slack"
op = "add_reaction"
```

If `post` fails, `find_pr` already happened (idempotent read; no side effect to undo) but `react` does not run. The action returns the `post` failure. There is no automatic compensation step that "undoes" the read.

Authors who want compensation write it as a separate atomic action that the agent composes. If `ship-update` fails, the agent can call a `file-followup-ticket` action next based on its conversational context — that's the right composition layer. Conditional execution within a single action (e.g., "run this step only if step X failed") is a post-MVP enhancement; v1 actions are linear `[[execute]]` chains with first-failure-terminates semantics.

**Why no auto-rollback.** The "saga pattern" of automatic compensation is appealing in distributed-systems theory but breaks down when the operations are external APIs the runtime doesn't fully understand. Aileron cannot know how to "undo" a Slack post (delete it? edit it to say "ignore"?) or a charge (refund? void?) without the connector author defining what that means. Defaulting to "no compensation; author writes their own" keeps the runtime honest about what it can guarantee.

### What failures look like to the agent

When an action fails, the daemon's response to `aileron-mcp` carries the failure envelope; `aileron-mcp` returns it to the agent host as the MCP tool result for the original tool call. The agent's chat surface renders it like any other tool result:

```json
{
  "tool_call_id": "...",
  "role": "tool",
  "content": "Action 'send-team-email' failed: external_api_error (Slack returned 401: invalid token).\nThe API key may have been revoked. Try `aileron binding rebind oauth2/slack/work` or notify the user.\naudit_id: audit-7f3e..."
}
```

The agent's LLM reads this and decides what to do next: retry the action, ask the user for guidance, or give up and explain the situation in plain language. Aileron does not synthesize agent-level recovery logic — that's the agent host's domain.

For per-invocation approval failures (post-MVP per [ADR-0009](/adr/0009-user-channel)), the same pattern applies: the structured error tells the agent the user denied or didn't respond, and the agent decides how to communicate.

## Alternatives Considered

### Silent retry by default (rejected)

The runtime retries failed operations transparently; the agent and user only see the final outcome (success or persistent failure).

Rejected because it conflates "the operation failed once" with "the operation worked." A connector that retries past a transient 429 silently is fine; a connector that retries past a 401 because the token was revoked produces hours of failed authorization attempts the user never sees. Visible failure surfacing is what makes recovery tractable. Bounded retry with the result (success or final failure) shown to the agent is the line — beyond bounded retry, the agent decides.

### Centralized error recovery with a global retry budget (rejected)

A runtime-wide retry budget governs all actions; the runtime makes intelligent retry decisions based on global state (rate limits, error rates, per-source health).

Rejected because it adds substantial coordination state (per-source health tracking, global budget allocation, distributed cache invalidation when running multiple agents) for marginal benefit. Per-action retry with per-call overrides handles the actual use cases. The fancy global-policy approach is what observability vendors build *on top of* the basic primitive — Aileron provides the primitive, leaves the global policy to operators if they want it.

### Auto-compensation / saga model (rejected)

The runtime tracks each `[[execute]]` step's compensation function (defined per step) and automatically runs the compensation chain on failure.

Rejected because compensation for external APIs isn't a function the runtime can know — it depends on the connector and the API. Aileron cannot know how to "undo" a Slack post or a charge without the connector author defining what that means. Authors who need compensation compose multiple atomic actions through the agent, where conversational context can drive the right follow-up. The saga pattern works in environments where the runtime owns the side effects (e.g., a database with rollback). Aileron does not own the side effects — Slack, Stripe, GitHub do.

### LLM fallback enabled by default (rejected)

When an action fails, the runtime automatically forwards the request to the upstream LLM as a fallback. Action authors opt out for connectors where fallback is dangerous.

Rejected because it inverts the safety story. The default would be "if the action fails, the LLM makes something up." For side-effecting actions, that's catastrophic — the LLM "fallback" for a failed `send_email` would claim the email was sent. Even for read-only actions, an unprompted LLM fallback masks the fact that the deterministic path didn't work. Opt-in keeps the burden of proof on the author who wants the relaxation.

### Best-effort partial success (no atomic-action rule) (rejected)

Multi-step actions return partial success on partial failure: "step 1 worked, step 2 failed." The agent decides what to do with the half-completed action.

Rejected because it gives the agent a contract that's hard to reason about. An action either ran to completion or it didn't. Partial success creates a state space the agent's LLM must understand and unwind, which is exactly the kind of thing LLMs are bad at. Atomic action semantics — the action as a whole succeeded or as a whole failed — keep the contract simple. Authors who genuinely want partial-progress semantics can split the operation into multiple atomic actions.

### Type-and-class-specific exception trees (rejected)

The error schema is a tree of typed exceptions (e.g., `NetworkError → ConnectionRefused → DNSFailure`); agents pattern-match on the type tree.

Rejected because it imposes a class-hierarchy ceremony that doesn't pay off in this domain. The agents that consume errors are LLMs; LLMs are good with flat string classifications and structured `details` fields, less good with type hierarchies. The flat closed-set `class` field with class-specific `details` covers everything an agent needs without the type-tree authoring overhead.

## Consequences

### For action and connector authors

- Idempotency is the default assumption. Authors implementing connectors that aren't naturally idempotent must use idempotency keys, server-side dedup, or local-side caching.
- Truly non-idempotent connectors declare `[connector.idempotency] default = "not_idempotent"` in the manifest. The runtime will not retry them by default.
- Compound flows that need conditional compensation are written as multiple atomic actions composed through the agent's conversation, not as single actions with branching logic. v1 actions are linear `[[execute]]` chains.

### For agents

- Every failure arrives as a structured error with a stable schema. The agent's LLM can reason about the failure class, retriability, and recommended action without parsing prose.
- The agent decides whether to retry beyond the runtime's bounded retries, ask the user, or give up. Aileron does not encode agent-level recovery policy.
- Failures are honest. If the action returned an error, it failed; there is no path through the runtime where an action silently fails but the agent thinks it succeeded.

### For Aileron runtime

- The runtime implements bounded retry with exponential backoff for `retriable: true` errors. v1 default: 3 retries; uniform across actions. Implementation lives in [`internal/retry`](https://github.com/ALRubinger/aileron/blob/main/internal/retry); the [`internal/clock`](https://github.com/ALRubinger/aileron/blob/main/internal/clock) abstraction makes retry tests deterministic.
- Idempotency is checked at the runtime/connector boundary; the runtime trusts the connector's `[connector.idempotency]` declaration but enforces the default-on-retry policy.
- The structured error envelope is constructed at the boundary that produced the error and passed through unchanged. Boundaries don't rewrap each other's errors. The closed taxonomy lives in [`internal/failure`](https://github.com/ALRubinger/aileron/blob/main/internal/failure); only the package's per-class constructors can produce a valid Failure value, so handlers cannot synthesize an arbitrary class string.
- Audit logging records every failure with full context: class, message, boundary, retried-count, actor identity, time, audit ID. Implementation lives in [`internal/audit`](https://github.com/ALRubinger/aileron/blob/main/internal/audit) on top of the existing `Store` SPI; v1 ships an in-memory implementation, with Postgres persistence post-MVP. Audit-log payload field names follow OTel span-attribute conventions (`aileron.failure.class`, `aileron.failure.boundary`, `aileron.failure.retriable`, `aileron.failure.message`, `aileron.failure.details`) — see [the audit-vs-wire split below](#audit-payload-vs-wire-envelope) for why the audit log diverges from the wire envelope shape.

### Scope: which envelope applies where

The ADR-0010 envelope applies to **errors returned to the calling action and through it to the agent** — the gateway endpoints (`/v1/chat/completions`, `/v1/messages`) and action / connector install responses.

Other API endpoints (intents, approvals, policies, accounts, auth) retain the existing `api.Error` envelope (`{error: {code, message, details, request_id}}`). Those errors are CRUD-shaped and don't fit ADR-0010's runtime taxonomy of `network_error` / `capability_denied` / etc. Forcing them in would either expand the closed taxonomy beyond its semantic anchor or drop fidelity. Consumers that want a single unified shape can layer one in their own client; the server emits two stable envelopes by design.

The `internal/auth` package was previously emitting a third, ad-hoc shape (`{"error": "<string>"}`). Stage 5 of #356 normalised those handlers to the standard `api.Error` shape so the v1 server emits exactly two envelope shapes — the gateway/action `FailureEnvelope` and the CRUD `api.Error`.

### Audit payload vs wire envelope

The wire failure envelope and the audit-log record describe the same underlying failure but live on two surfaces with two different conventions:

- **Wire envelope** (HTTP error response body): flat REST/JSON keys — `class`, `message`, `retriable`, `boundary`, `audit_id`, `details` — as documented above. Stable contract for agents and action authors.
- **Audit-log payload** (`audit.Event.Payload`): the same fields under OTel span-attribute names — `aileron.failure.class`, `aileron.failure.message`, `aileron.failure.retriable`, `aileron.failure.boundary`, `aileron.failure.details`. The OTel `aileron.` prefix scopes the attributes for vendor ownership when audit events are exported as spans (see [#390](https://github.com/ALRubinger/aileron/issues/390) Phase 6.5 schema alignment).

The recorder in [`internal/audit/record.go`](https://github.com/ALRubinger/aileron/blob/main/internal/audit/record.go) translates the wire envelope into the audit shape at write time. Other audit events (binding lifecycle, install consent, etc.) follow the same OTel-namespaced convention — `aileron.binding.name`, `aileron.connector.fqn`, `aileron.capability.kind`, `aileron.action.fqn`, `aileron.signature.status`, `aileron.consent.decision`, etc. The split is deliberate: the wire envelope is part of an HTTP API contract that uses standard REST conventions; the audit payload is a telemetry record whose field names need to survive co-existing in a span/trace alongside attributes from any other instrumented library, which is what OTel's prefix convention exists to handle.

Phase 7 (post-MVP) wires the actual OTLP exporter and `traceparent` propagation per [#390](https://github.com/ALRubinger/aileron/issues/390); the names are pre-aligned so that emission becomes a wiring change, not a schema break.

#### Reserved keys (declared, not yet emitted)

Two attribute keys are declared in the audit payload schema but not populated by any recorder today. They are reserved so future features can adopt them without forcing a downstream-visible rename once consumers depend on the format. Constants live in `internal/audit/keys.go`.

| Key | Reserved for | Issue |
|---|---|---|
| `aileron.signing.key_id` | The public-key fingerprint that signed an audit event under the Stage-1.5 hash-chained audit log. | [#398](https://github.com/ALRubinger/aileron/issues/398) |
| `aileron.policy.decision` | The verdict a pre-execution policy hook returned for the action that produced this event: `allow`, `deny`, `require_approval`, or an opaque decision-source reference. | [#396](https://github.com/ALRubinger/aileron/issues/396), [#399](https://github.com/ALRubinger/aileron/issues/399) |

Consumers reading the audit log MUST tolerate these keys appearing as empty / absent today and as populated values once the corresponding features ship. Reading code that names them by string literal should switch to the `audit.AttrSigningKeyID` and `audit.AttrPolicyDecision` constants so a future spelling refinement (which would itself require migration coordination) has a single point of change.

### For users

- Failures are surfaced. The user sees the agent's chat say "I tried to post the update but Slack returned an error — would you like me to try again with a different channel?" rather than the agent claiming success and the message never arriving.
- The audit log answers "why did this fail?" with structured detail. `aileron action audit --failed` is a useful debugging primitive.

### Open implementation questions (deferred)

- *How does the audit log structure the per-action failure history (one entry per attempt, summary entries, pruning policy)?* — implementation note rather than a fresh ADR.
- *Per-call retry tuning syntax (`retry = { max = N, backoff = ... }`) and per-action conditional execution (`when = "<step>.failed"`)* — post-MVP enhancements; will be ratified when concrete actions need them.
- *LLM fallback for read-only / informational actions (`[fallback] enabled = true`)* — post-MVP feature. The pattern is genuinely useful but specifying it before any action wants it is premature; ratify when a concrete read-only action surfaces the need.
- *Per-invocation approval failure classes (`approval_denied`, `approval_timeout`)* — paired with per-invocation approval per [ADR-0009](/adr/0009-user-channel); both light up post-MVP.
- *How does cross-machine audit replication work for users with multiple machines, or for teams running Aileron Control?* — paired with the hosted backend in [ADR-0009](/adr/0009-user-channel) Phase 2; post-MVP.

## Examples

### Network failure with bounded retry

Action attempts to post to Slack. Network is flaky; the first call fails with `network_error`. Runtime retries:

```
[t=0s]  Action 'ship-update' invoked.
[t=0s]  → step 'post': calling slack.post_message...
[t=0s]    network_error: connection refused. Retry 1/3 in 1s (with jitter).
[t=1.2s]  → retry 1: calling slack.post_message...
[t=1.4s]    success: { ok: true, ts: "1714512345.123" }
```

Action returns success. Audit log records the retry. The agent sees a normal tool result.

### Persistent external API error

Same action, but Slack's auth has been revoked:

```
[t=0s]  Action 'ship-update' invoked.
[t=0s]  → step 'post': calling slack.post_message...
[t=0.3s]   external_api_error: Slack returned 401 (invalid_auth).
           Retriable: false (4xx is not retried).
```

Action returns the error. Agent's LLM sees:

```json
{
  "error": {
    "class": "external_api_error",
    "message": "Slack returned 401 (invalid_auth). The token bound to oauth2/slack/work appears to be revoked.",
    "retriable": false,
    "boundary": "external",
    "audit_id": "audit-7f3e...",
    "details": {
      "status": 401,
      "binding": "oauth2/slack/work",
      "suggestion": "aileron binding rebind oauth2/slack/work"
    }
  }
}
```

The agent can communicate this to the user: "I tried to post the ship update, but the Slack token's expired. You can re-authenticate by running `aileron binding rebind oauth2/slack/work` and asking me to try again."

### Non-idempotent connector refusing retry

Action calls a payment-rail connector marked non-idempotent. Network blip on the call:

```
[t=0s]  Action 'charge-customer' invoked.
[t=0s]  → step 'charge': calling treasury.charge_card...
[t=0.1s]   network_error: TLS handshake timeout.
[t=0.1s]   Connector 'github://acme/treasury-rail' is non-idempotent.
           Retry suppressed. Action fails with the original error.
```

Agent receives:

```json
{
  "error": {
    "class": "network_error",
    "message": "TLS handshake timed out reaching treasury-api.acme.com:443. The charge connector is non-idempotent; retry was not attempted.",
    "retriable": false,
    "boundary": "external",
    "audit_id": "audit-c4f1..."
  }
}
```

The agent can ask the user how to proceed; an automatic retry could have double-charged the card.
