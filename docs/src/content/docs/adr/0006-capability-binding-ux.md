---
title: "ADR-0006: Capability Binding UX"
description: "How users bind abstract capability types declared by connectors to concrete vault entries; install-time transparency, first-use binding, opt-in pre-binding, visible lifecycle"
order: 6
---


<div class="meta">
<table>
  <tr><th>Status</th><td>Accepted</td></tr>
  <tr><th>Date</th><td>2026-04-29</td></tr>
  <tr><th>Tracking</th><td><a href="https://github.com/ALRubinger/aileron/issues/343">#343</a></td></tr>
</table>
</div>

## Context

[ADR-0002](/adr/0002-connector-model) commits to a structural property: connectors declare *abstract capability types* — "OAuth2 with this scope," "API key," "request-signing key" — and users bind concrete credentials to those types at install or first use. The connector never names a vault path; the runtime never trusts a connector with a credential the user did not explicitly grant.

[ADR-0005](/adr/0005-sandbox-choice) commits to a related property: the runtime *mediates* credential use so that connectors never hold raw credential bytes. When a connector executes, the runtime resolves the connector's declared capability type to a bound credential and intercepts outgoing calls (HTTP requests, request signing, capability handles) to inject the credential.

Both ADRs explicitly defer the user-facing question:

- *How does a user actually bind an abstract capability to a concrete credential?*
- *When does the prompt happen — at install, at first use, ahead of time?*
- *What does the binding look like as a managed artifact: how does the user list, inspect, rotate, or revoke?*
- *What happens when an action invokes a connector that needs a binding the user has not yet provided?*
- *How do multiple credentials of the same kind coexist? (User has a "work" Slack and a "personal" Slack — both want `oauth2(chat:write)`.)*

These are user-facing decisions, not architectural ones. They do not change *what* Aileron is. They do shape every interaction a user has with a connector for the first time, and every time a credential rotates, and every time the user needs to know "does this connector still have access to my Stripe account?"

This ADR ratifies the binding UX.

## Decision

### A binding is always an explicit user act; never inferred, never automatic

The runtime does not pick credentials for the user. There is no "matching" heuristic that says "this OAuth2 token has the right scope, so I'll use it." Every binding is created by a deliberate user action, logged in the audit trail, and reversible.

This is the load-bearing rule. A "best-match" heuristic would mean a user who has both a personal and a work Slack credential could find the wrong one used silently. Worse, a malicious connector that requests `oauth2(scope=stripe.charge)` could end up bound to a Stripe credential the user intended for a different connector. Explicit binding eliminates the entire class of "I didn't know it was using that account" surprises.

Concretely: if no binding exists for a required capability, the runtime does not run the action. It triggers the first-use binding flow (see below) or, if running headless, fails with a structured error pointing at the unbound capability.

### Capability *types* surface at install; capability *bindings* surface at first use (or earlier on opt-in)

The flow has two distinct moments:

**At install** — the user sees what capability types the connector will need, but does not bind them yet. This is purely transparency: the install consent flow shows the manifest's declared capabilities so the user can decide whether the connector is safe to install at all. No credentials are touched. A user who installs a Slack connector knows it will eventually need a Slack OAuth2 token, but has not been asked to provide one.

**At first use** — the first action invocation that would actually exercise an unbound capability triggers the binding flow. The runtime pauses execution, surfaces the binding prompt through the configured user channel (see [ADR-0005](/adr/0005-sandbox-choice)'s credential-mediation model and [ADR-0009](/adr/0009-user-channel) for surface specifics), the user authenticates and selects (or creates) the credential, and the runtime resumes the action with the new binding in place.

Splitting transparency from commitment matters. A user installing six connectors during project setup should not be forced through six full OAuth flows immediately — most of those connectors will be used eventually, and some may never be used in the actual workflow. First-use binding lets the user explore the project before paying the per-credential cost. And a user who installs a connector and doesn't use it never authenticates against a service they don't care about.

### Pre-binding (`aileron binding setup`) for headless and CI workflows

First-use binding is interactive by definition. For workflows that cannot prompt — CI pipelines, autonomous agent runs, scripted setup — the user opts in to pre-binding:

```sh
aileron binding setup github://aileron/slack
```

This walks every capability type the connector's manifest declares and prompts the user to bind each, *now*. After the command completes, the connector can be invoked headlessly with no further prompts.

`aileron binding setup` is also the right primitive for `aileron sync` (per [ADR-0004](/adr/0004-dependency-resolution)) when called with `--bind-all`: walk the user's full set of installed connectors and bind each one's capabilities up front.

### Bindings have stable identities, scoped per-capability-type

Every binding has a name within the capability type's namespace:

```
oauth2/slack/work
oauth2/slack/personal
api_key/openai/personal
api_key/anthropic/work
x509/treasury/prod
```

The format is `<kind>/<identity>` where the identity is user-chosen and meaningful to the user. The user picks the identity at binding-creation time (with sensible defaults — `<service>/<account-handle>` is auto-suggested from the OAuth response when applicable).

Identities are how multiple credentials of the same kind coexist. A user who has work and personal Slack accounts has two distinct bindings (`oauth2/slack/work` and `oauth2/slack/personal`); when an action invokes a Slack connector, the binding flow asks "which Slack account?" — never silently picks one.

The binding identity is the *handle* through which the runtime maps a capability requirement to a concrete credential. Action files do not reference identities — they declare only the capability type they need. The binding identity is local state managed by the binding system; the action file declares only the capability type it needs.

### The binding flow is the same code path as re-auth and refresh

When an OAuth2 token expires, when an API key rotates, when a session ends — the recovery is the *same* binding flow the user saw at first use. There is no separate "your token expired, sign in again" UX with different ergonomics from the original binding. One code path, one mental model.

This means: the binding system tracks each binding's freshness (TTL, refresh-token availability, etc.) and proactively re-authenticates when possible (silent refresh for OAuth2 with a valid refresh token), or surfaces the binding flow when re-auth requires user interaction.

Failures during refresh (revoked refresh token, expired credential the user must regenerate) downgrade gracefully: the call that triggered the refresh fails with a structured error pointing at the binding name, and `aileron binding rebind <name>` walks the user through replacing the binding.

### Bindings are managed visibly through CLI primitives

Five commands cover the binding lifecycle:

| Command | Purpose |
|---|---|
| `aileron binding list` | List all bindings on this machine, grouped by capability kind and connector |
| `aileron binding inspect <name>` | Show the binding's metadata: capability type, scope, identity, last-used timestamp, refresh status |
| `aileron binding setup <connector-FQN>` | Pre-bind every capability the connector declares (used for headless or proactive setup) |
| `aileron binding rebind <name>` | Replace an existing binding with a fresh credential (after rotation, after revocation) |
| `aileron binding revoke <name>` | Remove a binding entirely; subsequent invocations of any connector that would have used it trigger first-use binding |

Notable absences: there is no `aileron binding bind` command for creating a single new binding interactively. The semantics are unclear — "bind to what?" — and the right entry points are first-use (driven by the action invocation) or `binding setup` (driven by a connector). A direct create command would be a third entry point with no clear advantage.

### Bindings live in the user's local vault

Bindings are stored in the user's local vault, ratified in [ADR-0011](/adr/0011-local-credential-vault). The vault is part of the user's `~/.aileron/` directory, alongside their actions and connector store, and is encrypted at rest with a passphrase-derived key.

Bindings are user-local by construction. There is no mechanism for sharing bindings across users (that would mean sharing credentials, which is exactly what the trust model is designed to prevent). Shared service-account credentials for teams are a post-MVP feature paired with the hosted backend introduced in [ADR-0009](/adr/0009-user-channel) Phase 2; v1 supports only personal bindings.

### When a required binding is missing, behavior depends on context

| Context | Behavior |
|---|---|
| Interactive (running attached to a terminal or with a UI surface) | Pause action; trigger first-use binding flow; resume on success |
| Headless without `--bind-all` | Fail action with `binding_required` error naming the unbound capability and connector |
| Headless with `--bind-all` already attempted | Hard fail (the user said "all bindings should already exist," and one didn't) |
| Refresh failure (existing binding turned stale) | Pause action; trigger refresh flow (silent if possible, otherwise the same binding flow); fail action if refresh fails |

The error class for a missing binding is distinct from a capability denial — `binding_required` is "the connector has the right declared capability, the action declared the right subset, but the user has not provided a concrete credential yet." It is recoverable (provide a binding and retry) where capability denial is not.

### Capability handle resolution at call time

[ADR-0005](/adr/0005-sandbox-choice) ratifies that the runtime mediates credential use through *capability handles* — opaque values issued to the sandbox that the runtime resolves to actual credentials at the host-side mediation point. This ADR specifies the lifecycle:

1. Connector instance starts. Runtime issues handles for each capability the action declared, resolving each handle to the user's bound credential.
2. Connector includes a handle in an outgoing call (HTTP request, signing request, etc.).
3. Runtime intercepts the call host-side. Resolves the handle. Injects the actual credential. Forwards the call.
4. Response returns. Connector sees response payload; never sees the credential.
5. Connector instance terminates. Handles invalidate; cannot be reused.

Handles are scoped to a single connector instance — a single action invocation. The same binding produces a different handle for each invocation, so handles cannot be cached across invocations or smuggled out of one connector to another.

## Alternatives Considered

### "Best-match" automatic credential selection (rejected)

The runtime inspects the user's bound credentials, finds one whose kind and scope match the connector's declared capability, and uses it without prompting.

Rejected because it makes silent mistakes possible. A user with both `oauth2/slack/work` and `oauth2/slack/personal` would never know which account a given action used unless they read the audit log. Worse, a connector requesting an OAuth2 scope a malicious actor crafted to overlap with an existing binding could be silently bound to the wrong credential. Explicit selection, every time, eliminates the entire class.

### Always pre-bind at install (no first-use flow) (rejected)

Every install runs the full binding flow before completing. By the time `aileron action add` returns, every connector dependency has its credentials bound.

Rejected because it front-loads work the user may not need to do. A new user installing several actions to evaluate their fit shouldn't be forced through OAuth flows for connectors they may decide not to use. First-use binding aligns the user's effort with their actual usage. The pre-bind option (`aileron binding setup`) is available for users who want install-time setup; it just is not the default.

### Bindings shared across users (rejected for v1)

Multiple users share a single binding — one OAuth token bound on Alex's machine is reachable from Brian's machine too — through some federation mechanism.

Rejected for v1 because Aileron is user-level (per [ADR-0003](/adr/0003-action-model)). There is no team or project layer to attach a shared binding to. Each user authenticates against the services they use with their own accounts.

Shared service-account bindings (e.g., a CI account, a team-level Stripe account) are a post-MVP feature paired with the hosted backend in [ADR-0009](/adr/0009-user-channel) Phase 2. Until then, teams that need a shared credential share it out-of-band (a shared password manager, an env var in CI) and each user binds their own copy.

### A binding identity is a freeform string (rejected)

Users name bindings whatever they want — `my-slack-1`, `the-good-one`, `prod` — with no enforced structure.

Rejected because freeform identities defeat half the point of explicit binding: at the moment of selection, the user needs to recognize *which credential is which*. A dropdown showing `the-good-one` next to `my-slack-1` doesn't help anyone choose. The `<kind>/<service>/<identity>` format keeps the kind and service legible while letting the user name the variant. We default to deriving the identity from the credential's metadata (OAuth account email, API key label) so the common case requires no naming work.

### One global binding per capability type (rejected)

Each capability type can have at most one binding. A user with two Slack accounts must pick one as the canonical binding; the other is unreachable through Aileron.

Rejected because users genuinely have multiple credentials of the same kind in production scenarios — work and personal Gmail, multiple AWS accounts, separate Stripe environments for staging and prod. Forcing a global single binding pushes users to install duplicate connectors with renamed manifests, which is a worse outcome than supporting multi-binding cleanly.

### Bind once, reuse forever (no rebind/revoke flow) (rejected)

A binding is created and lives until the user manually edits the vault file. There are no first-class commands for replacing or removing bindings.

Rejected because credentials rotate. OAuth tokens expire, API keys are reissued, employees change companies. Without first-class rebind and revoke commands, users would either edit vault files by hand (error-prone) or revoke bindings by deleting them and stumbling into the first-use flow (high friction). `rebind` and `revoke` make the lifecycle visible and safe.

## Consequences

### For users

- The first time you run an action that exercises a new connector, you see a binding prompt. The flow is the same one you'll see again when the credential expires.
- Multiple accounts of the same service are first-class. You name them, you choose which one each invocation uses (when the runtime asks).
- `aileron binding list` is your inventory. `aileron binding revoke` is your kill switch.
- Pre-binding via `aileron binding setup` is opt-in for users who want all-up-front setup.

### For action authors

- Action files name capability types and subsets, not binding identities. An action ships portable across teams; each user binds their own credentials.
- An action that needs multiple credentials of the same kind (e.g., source Slack and destination Slack for an inter-workspace bridge) declares them as separate capability requirements with distinct names; the binding flow asks the user to bind each. This is rare.

### For connector authors

- Manifest declares the capability type and scope; there is nothing else to specify about credential handling. The runtime issues capability handles; the connector uses them.
- A connector cannot ask for "any OAuth2 token" — it must specify scope. Vague capability requests would defeat the binding-as-deliberate-choice rule.

### For Aileron runtime

- The vault component holds all bindings, exposes lookup-by-name to the credential-mediation layer, and surfaces the binding flow when a lookup misses.
- Binding-flow surfaces (OS notification, TUI, web UI, CLI fallback) are coordinated through the user-channel mechanism — implemented in [ADR-0009](/adr/0009-user-channel), but driven by binding events from this layer.
- Capability handles are minted per connector instance and invalidated on instance termination. The handle table is in-memory, never persisted.

### For audit and security

- Every binding event (create, refresh, rebind, revoke) is logged with timestamp, user-channel surface, and outcome.
- Every credential-mediated call records: action, connector, binding identity, request shape (without credential), response status, audit ID. Reading the audit log answers "which Slack account did this action use?" precisely.
- Stale bindings (no successful refresh in a configurable window) are flagged in `aileron binding list` so users know to rebind before next use.

### Open implementation questions (deferred to subsequent ADRs)

- *What surfaces does the binding prompt use (OS notification, TUI panel, web UI, CLI fallback) and how is the choice tier-ordered?* — [ADR-0009](/adr/0009-user-channel).
- *What does the install consent flow look like before any binding decisions are made?* — [ADR-0007](/adr/0007-install-consent).
- *Will Aileron support shared service-account bindings (one credential reachable across multiple users) for team / CI workflows?* — paired with the hosted backend introduced in [ADR-0009](/adr/0009-user-channel); post-MVP. Until then, teams share credentials out-of-band and each user binds their own copy.

## Examples

### First-use binding flow

User runs an agent with the `ship-update` action installed. Agent invokes the action. Runtime sees the action requires `oauth2(scope=chat:write)` for `github://aileron/slack` and no binding exists.

```
$ # (Agent invocation pauses; binding prompt surfaces via OS notification)
[Aileron — Authorize Slack]
ship-update needs to post messages on your behalf.

Capability:  oauth2 (chat:write, channels:read)
Connector:   github://aileron/slack@1.2.0
Action:      ship-update

  ◯ Sign in to a Slack workspace
  ◯ Choose existing binding...

[Continue]  [Cancel]
```

User clicks "Sign in," completes the OAuth flow in their browser, returns to a confirmation screen:

```
Bound!
Account:   alr@workplace.com
Workspace: workplace-engineering
Identity:  oauth2/slack/work

The action is resuming.
```

Agent invocation continues; the post lands in `#engineering`.

### Pre-binding from a CI workflow

```sh
$ aileron binding setup github://aileron/slack github://aileron/git github://aileron/linear

Pre-binding 3 connectors...

github://aileron/slack
  oauth2 (chat:write, channels:read)        ✓ bound as oauth2/slack/work

github://aileron/git
  filesystem (read)                         ✓ no credential needed

github://aileron/linear
  api_key (issues:write, comments:write)    ✓ bound as api_key/linear/team

Done.

$ aileron binding list
NAME                            KIND      SCOPE                            LAST USED
oauth2/slack/work               oauth2    chat:write, channels:read        never
api_key/linear/team             api_key   issues:write, comments:write     never
```

### Rebinding after credential rotation

User rotates their Linear API key in the Linear admin console. Next action invocation that uses Linear fails:

```
ERROR: refresh failed for api_key/linear/team
  The credential is no longer valid (HTTP 401 from api.linear.app).

To replace this binding:
  $ aileron binding rebind api_key/linear/team
```

User runs the rebind:

```
$ aileron binding rebind api_key/linear/team
[Aileron — Replace api_key/linear/team]
This binding is for: github://aileron/linear (api_key, scope: issues:write, comments:write)

Paste your new Linear API key:
> lin_api_xxxxxxxxxxxxxxxx

✓ Rebound.

The binding is back in service. Re-run the action that triggered the rebind.
```

### Listing and inspecting bindings

```
$ aileron binding list
NAME                          KIND     SCOPE                          LAST USED       STATUS
oauth2/slack/work             oauth2   chat:write, channels:read      2 minutes ago   ✓
oauth2/slack/personal         oauth2   chat:write                     never           ✓
api_key/linear/team           api_key  issues:write, comments:write   1 hour ago      ✓
api_key/openai/personal       api_key  *                              5 minutes ago   ⚠ stale (47 days since refresh)
oauth2/gmail/work             oauth2   gmail.send                     yesterday       ✓

5 bindings. 1 stale; consider running `aileron binding rebind api_key/openai/personal`.

$ aileron binding inspect oauth2/slack/work
Binding:        oauth2/slack/work
Kind:           oauth2
Scope:          chat:write, channels:read
Bound to:       Slack workspace "workplace-engineering"
Account:        alr@workplace.com
Created:        2026-04-15 10:23:14
Last used:      2026-04-29 14:01:05
Last refresh:   2026-04-28 03:00:11 (silent, success)
Refresh token:  present (used for silent refresh)
Used by connectors:
  - github://aileron/slack@1.2.0
  - github://aileron/slack@1.3.0
```
