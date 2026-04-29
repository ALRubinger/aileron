---
title: "ADR-0002: Connector Model"
description: "Connectors are sandboxed, content-addressed binaries; Aileron core ships only primitive capability types"
order: 2
---


<div class="meta">
<table>
  <tr><th>Status</th><td>Accepted</td></tr>
  <tr><th>Date</th><td>2026-04-29</td></tr>
  <tr><th>Tracking</th><td><a href="https://github.com/ALRubinger/aileron/issues/343">#343</a></td></tr>
</table>
</div>

## Context

Aileron's value proposition rests on running real-world actions on behalf of an agent: send an email, post to Slack, charge a card, file a Linear ticket. Each of these requires code that knows how to talk to a specific external service. The architectural question is: where does that code live, and under what trust boundary does it run?

Three options cover the design space:

| Option | What it is | Failure mode |
|---|---|---|
| **In-tree services in Aileron core** | Aileron ships built-in support for Gmail, Slack, Stripe, etc. | Core code grows unboundedly with the world's APIs. A bug in any service implementation lives inside the trust boundary that holds every credential. |
| **In-process plugins** | Third-party code loaded into the runtime as a library | Same trust boundary as core. A malicious or buggy plugin can read other plugins' memory, reach any credential, exfiltrate freely. |
| **Sandboxed binaries** | Third-party code shipped as standalone artifacts and run in isolation | Trust boundary is per-binary. Compromise of one connector cannot reach another or the runtime. |

The first two options collapse Aileron's security story. If an agent installs a Slack connector to "post to #engineering" and that connector can read every credential the user has bound to other services, the credential-sealing claim is unfounded. The third option — sandboxed binaries — is the only one that lets Aileron honestly claim that credentials issued to one connector are unreachable to others.

This ADR ratifies that model and the consequences that follow from it.

## Decision

### Connectors are sandboxed binaries shipped separately from Aileron core

A connector is a standalone artifact: a binary plus a manifest. It is not a Go package linked into the Aileron runtime. It is not a script the runtime evaluates. It is not an extension loaded into the runtime's address space. It is a binary that runs in an isolated sandbox process and communicates with the runtime over a defined IPC boundary.

Aileron's vault is in the runtime's process. Connectors never see long-lived credentials. When a connector executes, the runtime issues a short-lived scoped token (or makes a privileged outbound call on the connector's behalf, depending on the credential kind) for that single invocation. The connector never holds a key it could exfiltrate after the call returns.

The choice of sandbox technology (WASM, OS process, etc.) is a separate decision and not the subject of this ADR. What this ADR commits to is the *property*: every connector runs under an isolation boundary that the runtime enforces, and the runtime never trusts a connector with anything beyond what its manifest declared.

### Aileron core ships only primitive capability types

The runtime knows about a small, fixed set of primitive capability types:

- **Network access** — outbound TCP/HTTP to declared `host:port` pairs.
- **Credential access** — a vault credential of a declared kind (`oauth2`, `api_key`, `basic`, etc.) with declared scopes.
- **Host functions** — narrow capabilities the runtime exposes (e.g. structured logging, audit-event emission, time, RNG).

The runtime does *not* know about Gmail, Slack, Stripe, GitHub, or any other named service. Service-specific knowledge — endpoints, request shapes, OAuth flow particulars, retry semantics — lives entirely inside the connector binary. Adding support for a new external service is an entirely out-of-tree activity: write a connector, declare its needs, ship it.

This keeps the runtime small, vendor-neutral, and decoupled from the long tail of API churn. It is also what makes a connector marketplace coherent: every connector composes with the runtime through the same primitive grant types, so a third-party connector and a first-party connector are indistinguishable from the runtime's point of view.

### Connectors are content-addressed: name + exact version + content hash

A connector is identified by the triple `(name, version, hash)`. The name is a human-readable handle. The version is a semantic version. The hash is the content hash of the connector binary plus its manifest, taken together as a single byte stream.

Action files reference connectors by all three:

```toml
[[requires.connectors]]
name = "slack"
version = "1.2.0"
hash = "sha256:abc123..."
```

The hash is verified at install time *and* before every execution. The runtime refuses to execute a connector whose on-disk bytes do not match the declared hash. This catches:

- Tampering after install (a malicious process replaces the binary in the local store).
- Manifest desynchronization (the manifest was edited locally to grant more capability than the original publisher signed).
- Accidental corruption.

There are no version ranges, no `^1.2.x`-style ranges, no "latest" pseudo-version. What an action file declares is what runs. Updating a connector is an explicit, visible edit to the action files that reference it.

### Connectors declare their needs in a manifest

The manifest is a pure-TOML file shipped alongside the binary. Its only job is to declare what the connector requires from the runtime:

```toml
[connector]
name = "gmail"
version = "1.2.3"
publisher = "acme.dev"
provenance_hash = "sha256:abc123..."

[capabilities.network]
hosts = ["gmail.googleapis.com:443", "oauth2.googleapis.com:443"]

[capabilities.credential]
kind = "oauth2"
scope = "https://www.googleapis.com/auth/gmail.send"

[capabilities.runtime]
imports = ["wasi:http/outgoing-handler", "wasi:cli/stdout"]

[provides]
intents = ["send_email", "draft_email"]
```

The manifest is a *request*. The runtime grants nothing that is not declared in it. Capability requests at execution time that exceed the manifest's grant are denied at the sandbox boundary; the connector's process is terminated and the action fails with a structured error.

Manifest fields:

- `[connector]` — identity. Name, version, publisher, provenance hash for the binary.
- `[capabilities.network]` — outbound network grants. Pinned to specific `host:port` pairs; no wildcards.
- `[capabilities.credential]` — credential kinds and scopes the connector needs. The connector declares the *type*; the user binds a concrete vault entry at install or first use.
- `[capabilities.runtime]` — host-function imports the connector uses (audit emit, logging, time, RNG, etc.).
- `[provides]` — what intents this connector implements. Used by actions when declaring `requires.connectors` and by the Hub for discovery.

Each capability section is enumerable and bounded. There is no `*` and no implicit grant. A connector that did not declare network access cannot dial out, period.

### Capabilities are abstract types, not concrete resources

A connector's manifest never names a concrete vault path or a specific account. It declares the *type* of credential it needs (e.g. "OAuth2 with this scope") and the user binds a specific account to that requirement at install or first use.

This is a structural property, not a UX preference. A malicious connector cannot name a credential it shouldn't reach because the manifest grammar does not let it. The Stripe connector cannot request `vault://gmail/work`; it can only request `oauth2(scope=stripe.charge)`. If the user had no Stripe credential bound, the action would fail visibly — not silently fall through to some other key with similar shape.

The mechanics of how the user binds an abstract capability to a concrete resource (the install-time UI, the first-use flow, the rebind command) are out of scope for this ADR.

### Publisher identity and provenance hash

Each manifest carries `publisher` and `provenance_hash` fields. The publisher is the cryptographic identity that signed the binary. The provenance hash records the build artifact's hash at publish time.

The runtime treats publisher identity as *information*, not as authorization. A connector signed by a known publisher and a connector signed by an unknown publisher both go through the same install consent path; the publisher field is shown to the user, and the Hub may use it to organize browse views, but signature presence does not bypass anything.

This keeps the trust model honest. Signing tells the user *who* is making a claim about a binary; it does not tell the runtime *whether* to grant the binary capabilities. Capability grants come exclusively from the user's install consent.

## Alternatives Considered

### Built-in connectors in Aileron core (rejected)

The runtime ships with first-party support for the most common services (Gmail, Slack, Stripe, GitHub) and exposes them through a built-in API. Third-party connectors are a later addition.

Rejected for three reasons. First, the trust boundary is wrong: in-tree code shares Aileron's process and can reach every credential the runtime holds. Second, this couples Aileron's release cadence to the world's API churn — every breaking change at Gmail or Stripe forces an Aileron release. Third, it creates a permanent two-tier ecosystem (first-party blessed, third-party second-class) which discourages community contribution and concentrates maintenance burden on us.

### In-process plugins (rejected)

Connectors are loaded into the runtime as dynamic libraries (Go plugins, native shared objects, etc.) and execute in the runtime's address space.

Rejected because in-process plugins do not provide a security boundary. A buggy plugin can corrupt the runtime's memory; a malicious plugin can read every credential, every audit log, every other plugin's state. The whole sealed-credential premise of Aileron collapses if connector code shares an address space with the vault.

### Connectors as MCP servers (rejected)

Connectors are MCP servers, communicating with the runtime over the MCP protocol's JSON-RPC transport.

Rejected because MCP is a tool-discovery protocol layered over JSON-RPC; it does not natively express capability grants in a form the runtime can enforce. We would end up either implementing capability enforcement *outside* the MCP protocol (in which case MCP buys us nothing) or extending MCP with proprietary fields (in which case our connectors are no longer portable MCP servers anyway). MCP is a different abstraction at a different layer; coupling our connector model to it would be a category error. The `provides.intents` field is intentionally compatible *enough* with tool-call shapes that an MCP-shaped frontend on Aileron Hub remains an option, but the connector model itself is independent.

### Connectors as cloud-hosted services (rejected)

Connectors are HTTP endpoints hosted by their publishers. Aileron makes API calls to those endpoints.

Rejected because it introduces a third-party trust dependency for every integration. Every connector becomes a network round-trip, an availability dependency, a potential exfiltration channel, and a privacy concern (the publisher sees every action invocation). Local execution under a sandbox is strictly stronger on every axis except the publisher's ability to push hot fixes — and content-addressed versioning gives us a clean path to fix-then-update.

### Capability abstraction layer (e.g. `messaging:post_to_channel`) (rejected)

Connectors declare and actions consume *abstract* capabilities like `messaging:post_to_channel`, with multiple connectors able to provide the same abstract capability. Actions are then portable across connector implementations.

Rejected as deliberate scope reduction. Capability abstraction would require: a registry of who defines abstract capability names, a parser layer to map connector grants to abstract capabilities, a second trust layer (trust the spec *and* trust the implementation), and UX disambiguation when multiple connectors provide the same abstract capability. The benefit — substitutability between, say, Slack and Discord — is marginal in practice; the implementations differ enough that an action written for one rarely "just works" against the other. We name connectors directly. Action authors who want substitutability edit the action file (which is theirs, post-install).

## Consequences

### For Aileron core

- The runtime is small and stable. New services do not require core changes.
- Core implements: a sandbox host (manifest parser, capability enforcement, IPC); a content-addressed connector store (lookup by hash, integrity verification); a vault that issues short-lived scoped tokens to sandboxed connectors; an audit pipeline that records every cross-boundary call.
- Core does not implement any service-specific code. Adding "Aileron supports Gmail" is a published connector, not a code change in this repo.

### For connector authors

- A connector is its own ship cycle: build, sign, publish, version. The author owns it; we don't.
- Manifests are pure TOML. The connector author writes one TOML file alongside their binary.
- A connector that wants new capability must publish a new version with an updated manifest. Reusing the same hash is impossible by construction.
- Publishers are responsible for signing their binaries. Aileron will document the signing flow but does not act as a CA.

### For action authors

- Actions name connectors directly: `slack@1.2.0` with hash. No abstract capability names to look up.
- The capability subset an action uses is declared in the action file (e.g. `capabilities = ["chat:write"]` is a subset of the connector's full grant). The runtime enforces this subset *in addition* to the connector's manifest. Defense in depth.
- Updating a connector is a visible edit to action files that reference it. There is no automatic "follow latest" behavior.

### For the Hub and distribution

- The Hub indexes connectors by `(name, version, publisher, hash)` and serves their binaries plus manifests.
- Discovery surfaces (browse, search) use the `provides.intents` field and the connector's documentation.
- The Hub validates manifests at publish time. Manifests with malformed or contradictory capability declarations do not enter the catalog.

### For security and audit

- Every cross-boundary call is auditable: the connector identity, the capability used, the credential bound, the call result.
- Compromise scope is bounded by the connector's manifest. A compromised Slack connector cannot reach Gmail credentials; it cannot dial hosts outside its declared list.
- Manifest tampering is impossible without invalidating the content hash, which the runtime checks before every execution.

### Open implementation questions (deferred to subsequent ADRs)

- *How is the connector store laid out on disk, and how does the integrity-check pipeline interact with concurrent installs?* — deferred to the dependency-resolution ADR.
- *Which sandbox technology is the default, and what are the OS-process escalation criteria?* — deferred to the sandbox-choice ADR.
- *What does the install consent flow show, and what does the user actually click?* — deferred to the install-consent ADR.
- *How does a user bind an abstract capability to a concrete vault entry?* — deferred to the capability-binding-UX ADR.

## Examples

### Connector manifest (`gmail.toml`, shipped with the binary)

```toml
[connector]
name = "gmail"
version = "1.2.3"
publisher = "acme.dev"
provenance_hash = "sha256:abc123..."

[capabilities.network]
hosts = ["gmail.googleapis.com:443", "oauth2.googleapis.com:443"]

[capabilities.credential]
kind = "oauth2"
scope = "https://www.googleapis.com/auth/gmail.send"

[capabilities.runtime]
imports = ["wasi:http/outgoing-handler", "wasi:cli/stdout"]

[provides]
intents = ["send_email", "draft_email"]
```

### Action declaring a connector dependency

```toml
[[requires.connectors]]
name = "gmail"
version = "1.2.3"
hash = "sha256:abc123..."
capabilities = ["oauth2"]
```

The action declares the connector it needs (with exact version and hash) and the subset of the connector's capability grant it actually uses. The runtime enforces both: the connector cannot exceed its manifest, and the action cannot use capabilities the action did not declare. Two boundaries. Defense in depth.

### Capability denial at runtime

A connector whose manifest declares only `gmail.googleapis.com:443` attempts to dial `evil.example.com:443` mid-execution. The sandbox rejects the syscall. The connector process is terminated. The runtime returns a structured error to the calling action:

```json
{
  "error": {
    "class": "capability_denied",
    "connector": "gmail@1.2.3",
    "requested": "network:evil.example.com:443",
    "granted": ["network:gmail.googleapis.com:443", "network:oauth2.googleapis.com:443"],
    "audit_id": "audit-7f3e..."
  }
}
```

The action fails fast and visibly. The audit record persists.
