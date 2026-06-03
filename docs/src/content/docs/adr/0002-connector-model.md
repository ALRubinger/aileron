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
- **Spawn** — bounded execution of a local CLI subprocess on the connector's behalf. Declared programs, argv shapes, environment, and filesystem scope; output captured and returned to the connector. See "Spawn primitive: bounded subprocess execution" below.

The runtime does *not* know about Gmail, Slack, Stripe, GitHub, or any other named service. Service-specific knowledge — endpoints, request shapes, OAuth flow particulars, retry semantics — lives entirely inside the connector binary. Adding support for a new external service is an entirely out-of-tree activity: write a connector, declare its needs, ship it.

This keeps the runtime small, vendor-neutral, and decoupled from the long tail of API churn. It is also what makes a connector marketplace coherent: every connector composes with the runtime through the same primitive grant types, so a third-party connector and a first-party connector are indistinguishable from the runtime's point of view.

### Connectors are content-addressed: name + exact version + content hash

A connector is identified by the triple `(name, version, hash)`. The name is a fully-qualified URI (see next section). The version is a semantic version. The hash is the content hash of the connector binary plus its manifest, taken together as a single byte stream.

Action files reference connectors by all three:

```toml
[[requires.connectors]]
name = "github://aileron/slack"
version = "1.2.0"
hash = "sha256:abc123..."
```

The hash is verified at install time *and* before every execution. The runtime refuses to execute a connector whose on-disk bytes do not match the declared hash. This catches:

- Tampering after install (a malicious process replaces the binary in the local store).
- Manifest desynchronization (the manifest was edited locally to grant more capability than the original publisher signed).
- Accidental corruption.

There are no version ranges, no `^1.2.x`-style ranges, no "latest" pseudo-version. What an action file declares is what runs. Updating a connector is an explicit, visible edit to the action files that reference it. (See "Versioning is strict SemVer" below for the full version model.)

### Connector identity is a fully-qualified URI

Connector names are not bare strings. They are fully-qualified URIs that encode the *publication channel*, the *publisher's identity within that channel*, and an optional *subpath* locating the connector within the publication source:

```
<scheme>://<owner>/<repo-or-namespace>/<...subpath>/<connector>
```

For git-host schemes (`github://`, `gitlab://`), the first two segments after the scheme are the repo (`<owner>/<repo>`) and any further segments locate the connector within the repo's tree.

Examples:

| FQN | Meaning |
|---|---|
| `github://aileron/slack` | Repo `aileron/slack` on GitHub; one connector at the repo root |
| `github://aileron/integrations/connectors/slack` | Repo `aileron/integrations` on GitHub; connector lives at `connectors/slack/` (monorepo) |
| `github://aileron/integrations/connectors/discord` | Same repo as above; sibling connector |
| `gitlab://team/linear` | Repo `team/linear` on GitLab; one connector at the repo root |

This is decentralized. The scheme plus full path uniquely identifies a publication source; there is no central naming authority. Two organizations both publishing a connector named `slack` cannot collide — `github://acme/slack` and `github://other/slack` are distinct identities and produce distinct content hashes.

**Monorepo support is first-class.** A single repo can house many connectors (and many actions — see [ADR-0003](/adr/0003-action-model)) at distinct subpaths. Each artifact has its own manifest at its own path within the repo. Publishers organize subpaths however they like; common conventions like `connectors/<name>/` or `integrations/<service>/` will emerge but nothing is enforced.

**Initial scheme set:**

- `github://` — published as a GitHub release artifact under the named owner/repo, optionally at a subpath within the repo.
- `gitlab://` — published as a GitLab release artifact under the named owner/repo, optionally at a subpath within the repo.

There is no `hub://` scheme. The Aileron Hub is a discovery and metadata layer over canonical `github://` / `gitlab://` FQNs, not a publication channel of its own. See [ADR-0013](/adr/0013-connector-hub-and-trust-distribution).

Forward-compatible: additional schemes (custom forges, OCI registries, signed HTTPS URLs) can be added as the ecosystem matures. Schemes are added by code in Aileron's resolver, not by user configuration; an unknown scheme is a hard error at install.

**FQN, hash, and signature relationships:**

- The FQN is *identification*. It says where the connector came from and who is responsible for it.
- The content hash is *the trust anchor for what runs*. The runtime executes only bytes whose hash matches.
- The signature is *the identity that vouches for the binary at publish time*. Install tooling verifies the signature against keys associated with the FQN's authority (e.g., signing keys configured in the source repo).

All three checks must pass. The FQN alone grants nothing; the hash alone tells you content but not provenance; the signature alone tells you who signed but not what. Together they are sufficient.

**Forks are distinct connectors.** If `acme` forks `aileron/slack`, the fork's FQN is `github://acme/slack`. It is a separate connector from `github://aileron/slack` with its own version line, its own hash chain, and its own signing identity. Action files reference one or the other explicitly; there is no ambient "I forked it but kept the name" ambiguity.

**Repository moves are republications.** If a publisher migrates from GitHub to GitLab, the connector's FQN changes from `github://owner/x` to `gitlab://owner/x`. Existing action references continue to work against locally-cached binaries (the hash is unchanged); updating to the new location is an explicit edit to the action file, never an automatic redirect.

### Versioning is strict SemVer; hash is the immutable trust anchor

Every connector version is a strict [Semantic Versioning](https://semver.org) string (`MAJOR.MINOR.PATCH`, with optional pre-release like `2.0.0-rc.1` and optional build metadata like `1.2.0+sha.abc`). The `version` field in the connector manifest and in action `[[requires.connectors]]` references must be a valid SemVer.

Specifically excluded:
- No `latest` pseudo-version. There is no implicit "give me the newest" — a version is always pinned.
- No version ranges (`^1.2.x`, `~1.2`, `>=1.0`). Each reference resolves to one and only one version.
- No date-based versioning (`2026.04.29`), no branch names, no commit SHAs in the `version` field. Commit identity is captured by the hash, not by the version.

This is a deliberate constraint. Strict SemVer gives ecosystem tooling a known schema for "what's the next compatible release?" and "what would a major bump break?" Strict pinning ensures reproducibility from git: the action file declares exactly what runs, and what runs cannot change without a visible TOML edit.

**Version is for human reasoning. Hash is for machine verification.**

- The `version` field says *which release this claims to be*. It corresponds to a tag in the publication source.
- The `hash` field says *exactly which bytes will run*. It is the SHA-256 of the connector binary plus its manifest.

Tags are mutable on git hosts (a publisher can re-tag, force-push, delete and recreate a release). The hash is immutable. If the two ever disagree — for example, the publisher re-tagged `v1.2.0` after release with different bytes — the runtime detects the mismatch on next install or execution and fails loudly. Trust flows from the hash; the version is a label.

**Multiple versions of the same connector coexist by construction.** Two actions in the same project can reference `github://aileron/slack@1.2.0` and `github://aileron/slack@2.0.0` simultaneously. Different hashes mean different entries in the content-addressed store; the runtime loads the version each action declares. There is no project-wide version resolution step; there is no "this conflicts with that" check.

This drops out of the content-addressed model directly, but is worth stating explicitly because it makes monorepos and gradual migrations work without ceremony. Action A can pin an old, stable version while Action B adopts a new major; both run side by side.

**Compact reference form.** In commands and provenance fields, `@<version>` is the canonical compact form: `aileron connector install github://aileron/slack@1.2.0`, `source = "github://aileron/ship-update@1.0.0"`. In TOML manifests, the canonical form is `name` and `version` as separate fields so each is queryable, diffable, and updatable independently. Both forms are equivalent; the FQN identity does not include the version.

The mechanics of *how* a version maps to a concrete fetch (tag conventions, release-asset layout, update discovery, prerelease handling) are resolver concerns and are deferred to [ADR-0004](/adr/0004-dependency-resolution).

### Connectors declare their needs in a manifest

The manifest is a pure-TOML file shipped alongside the binary. Its only job is to declare what the connector requires from the runtime:

```toml
[connector]
name = "github://acme/gmail"
version = "1.2.3"
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

The `name` field must match the FQN under which the binary was published. Install tooling verifies this — a binary fetched from `github://acme/gmail` whose manifest declares `name = "github://other/gmail"` is rejected.

The manifest is a *request*. The runtime grants nothing that is not declared in it. Capability requests at execution time that exceed the manifest's grant are denied at the sandbox boundary; the connector's process is terminated and the action fails with a structured error.

Manifest fields:

- `[connector]` — identity. Fully-qualified URI name, semantic version, content hash of the binary.
- `[capabilities.network]` — outbound network grants. Pinned to specific `host:port` pairs; no wildcards.
- `[capabilities.credential]` — credential kinds and scopes the connector needs. The connector declares the *type*; the user binds a concrete vault entry at install or first use.
- `[capabilities.runtime]` — host-function imports the connector uses (audit emit, logging, time, RNG, etc.).
- `[capabilities.spawn]` — local subprocess grants. Declares allowed programs, argv patterns, environment passthrough, filesystem scope, and cwd policy. Optional; absent for connectors that do not spawn subprocesses.
- `[provides]` — what intents this connector implements. Used by actions when declaring `requires.connectors` and by the Hub for discovery.

Each capability section is enumerable and bounded. There is no `*` and no implicit grant. A connector that did not declare network access cannot dial out, period.

### OAuth credentials are publisher-owned

A connector that declares `kind = "oauth2"` MUST also declare its OAuth provider configuration in a `[capabilities.credential.oauth2]` table:

```toml
[capabilities.credential]
kind = "oauth2"
scope = "Read your email and send messages"   # human-readable; surfaced in CLI prompts

[capabilities.credential.oauth2]
authorize_url = "https://accounts.google.com/o/oauth2/v2/auth"
token_url     = "https://oauth2.googleapis.com/token"
client_id     = "1234567890-xxxxxxx.apps.googleusercontent.com"
scopes = [
  "https://www.googleapis.com/auth/gmail.send",
  "https://www.googleapis.com/auth/calendar.readonly",
]
```

The `client_id` belongs to the **connector publisher**, not Aileron. Each publisher registers their own OAuth app per service (with Google, Slack, Notion, GitHub, etc.) and the consent screen the user sees names that publisher. This is the load-bearing trust property: the OAuth consent screen is a contract between the user and the entity identified on it. If a third-party connector's runtime use is granted under "Aileron"'s OAuth client, the trust attribution is wrong — the user authorized one entity but a different entity exercises the grant.

Publisher-owned apps fix that. Aileron-the-runtime is just machinery (PKCE, callback listener, refresh); Aileron-the-business publishes its own connectors with its own OAuth apps under the exact same rules — no special privilege.

**v1 OAuth requirements**:

- **PKCE (S256) required.** PKCE is the binding security mechanism for the loopback installed-app flow.
- **`client_secret` is optional and treated as a release-time bound value, not a source-repo value.** Some providers (Google's "Desktop app" OAuth client type, notably) reject token-exchange requests that omit a registered `client_secret` even when PKCE is used. The publisher MAY declare `client_secret` in the connector manifest's `[capabilities.credential.oauth2]` block; the runtime forwards it on token exchange and refresh when present.

  The value belongs in the **released artifact** — bound into the manifest by the publisher's release pipeline from a CI secret, distributed inside the signed connector tarball. It MUST NOT be committed to the connector's source repository. Public source repos are scanned by automated systems (GitHub's secret scanning forwards Google OAuth client secrets to Google, which auto-rotates them) and a committed `client_secret` will be revoked the moment it's pushed. Source manifests should keep a placeholder (e.g. `client_secret = "bound-at-release"`) and use the same template-substitution pattern as the connector's content hash.

  Inside the released tarball the value is recoverable by anyone who installs — that's the same exposure that `gcloud`, `gh`, and every distributed installed-app client accepts; per the providers' own installed-app guidance the value is not cryptographically secret in this flow. PKCE remains what binds an authorization code to the session that started it; the `client_secret` (when required) is provider plumbing.

  Providers that genuinely treat `client_secret` as a server-only credential (web-app flows behind a hosted backend) MUST NOT have it set in a connector manifest at all.
- **Loopback redirect only.** The OAuth callback is served on the local loopback interface. Two callers serve it; both stay on loopback, they just differ in who binds the port:

  - **CLI** (default). The daemon picks an ephemeral free port on init and embeds `http://127.0.0.1:<random-port>/callback` in the authorize URL. The CLI binds that port in its own process and posts the captured code + state back to `/v1/bindings/setup/oauth2/finish`. This is the original v1 shape.
  - **Daemon** (added [#743](https://github.com/ALRubinger/aileron/issues/743) for browser-driven flows). The daemon's own listen address is the redirect target; the callback path is `/v1/bindings/setup/oauth2/callback`. The OAuth provider redirects directly to the daemon, which validates state, exchanges the code, persists the binding, and redirects the user's browser to a loopback-only `return_to` URL the caller supplied at init. The browser-based webapp can't `bind()` a TCP port, so the daemon — already on loopback — is the natural listener for that flow.

  In both modes the publisher registers `http://localhost` (any port; RFC 8252 §7.3 + Google's "Desktop app" rule) as a valid redirect URI on their OAuth app. Niche providers without loopback support are post-MVP.
- **Token storage.** The runtime persists the resulting `{access_token, refresh_token, expires_at, ...}` envelope in the user's vault. The connector never sees the token bytes — the runtime injects `Authorization: Bearer <access_token>` host-side per RFC 6750, refreshing transparently before injection when the token is near expiry. The OAuth2 wire format is RFC-fixed and not configurable per manifest. The API-key wire format *is* configurable per manifest via optional `[capabilities.credential].header` and `.format` fields (default `Authorization: Bearer <key>`; Linear's personal API key uses `format = "{key}"` for a raw header value; other APIs use `format = "Token {key}"` or `header = "X-API-Key"`). The `{key}` placeholder is substituted with the bound credential's value at injection time. See [#917](https://github.com/ALRubinger/aileron/issues/917) for the per-API-convention rationale.

### Spawn primitive: bounded subprocess execution

A connector frequently needs to invoke an existing local CLI rather than re-implement its protocol. Examples include `git`, `slackdump`, or an OS messaging client. The spawn primitive is the runtime's mechanism for permitting that on the connector's behalf under the same trust model as the other primitives. Declared in the manifest, gated host-side, audited.

The connector itself never calls `os/exec` directly. The connector is a sandboxed WASM module without OS-process privileges. Instead, the connector calls a host function (`aileron_host.spawn` or `aileron_host.spawn_op`) and the runtime is what owns the `os/exec.Command` call. The runtime enforces the manifest's `[capabilities.spawn]` declaration on every invocation, exactly as `HostPolicy.CheckURL` (at `internal/sandbox/network.go:43-84`) enforces `[capabilities.network]` on every outbound HTTP call. Same gate pattern, different syscall.

The platform-level enforcement mechanism (how the kernel actually confines the subprocess to its declared filesystem scope, blocks network access from the subprocess itself, and prevents privilege escalation) is the subject of [ADR-0014](/adr/0014-spawn-sandbox-technology). This ADR commits to the property: the subprocess executes under an enforcement boundary that the runtime owns. It does not commit to a specific mechanism.

#### Enforcement and audit

Every spawn call passes through `SpawnPolicy.CheckSpawn(envelope)` before the runtime exec's anything. The check is the gate. It validates:

1. `program` resolves to a declared entry in `[capabilities.spawn].programs` (path match, hash match when declared).
2. `argv` matches the operation's declared `argv` pattern after placeholder substitution.
3. Every key in `env` is in `env_passthrough` (no exfiltration of arbitrary host env into the subprocess).
4. `cwd` (when set) is within `fs_read`.
5. The action's declared capability subset permits spawn. The runtime enforces both the connector manifest and the action's narrower subset, the same two-boundary defense in depth the rest of the model uses.

Denials raise `capability_denied` (the same error class as network denials) and are audited identically. Each allowed or denied spawn emits a structured audit event with the connector identity, program, argv shape (placeholders preserved, not interpolated arguments), exit code, and content hashes of stdout and stderr.

#### Credential injection

Vault credentials never reach the subprocess as files or arguments. When a connector's spawn invocation references a credential, the runtime resolves the vault entry (via the existing `credentialResolver`), sets the corresponding env var on the subprocess only, and discards the value when the subprocess exits. The connector never holds the credential bytes either. The env-var channel matches the manifest's `env_passthrough` declaration and is gated by it.

This preserves the same sealed-credential property the runtime enforces for HTTP calls. The credential exists at three layers (vault, subprocess env, never connector), and the trust boundary between connector and credential remains intact.

#### Implementation details

The mechanics below are the deeper spec for runtime and tooling implementors; readers seeking only the model can skip this subsection.

##### Manifest schema

```toml
[connector]
name    = "github://alr/local-gitcrawl"
version = "0.0.1"

[capabilities.spawn]
programs = [{ path = "/usr/bin/git", hash = "sha256:bd6e..." }]
env_passthrough = ["GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL"]
fs_read  = ["~/code/", "~/.gitconfig"]
fs_write = ["~/.cache/aileron/gitcrawl/"]
cwd      = "~/code/"

[capabilities.spawn.operations.log]
argv        = "git log --since={since} --author={author} --format=%H%x09%s"
description = "List commits since a date by an author"

[capabilities.spawn.operations.status]
argv        = "git status --short"
description = "Show working-tree state"
```

- **`programs`** lists allowed binaries. Each entry pins the binary by exact filesystem path and optionally a content hash. The runtime resolves the path before exec and refuses to spawn if it does not match an entry. When `hash` is set, the runtime additionally verifies the binary's bytes; a mismatch fails the spawn loudly.
- **`operations`** is a TOML table keyed by operation name. Each entry's `argv` is a templated argv with `{name}` placeholders the runtime substitutes from the agent's args at call time. The runtime derives the gate's argv-pattern allow-list from the union of `operations[*].argv`. An incoming op whose name is not in this table is denied.
- **`env_passthrough`** is the closed set of environment keys the runtime is permitted to set on the subprocess. The subprocess receives only declared keys; nothing is forwarded by default. Credential injection rides this channel.
- **`fs_read` / `fs_write`** are filesystem scopes (absolute or `~/`-anchored). The sandbox restricts the subprocess to these scopes for the corresponding access mode. The enforcement mechanism is platform-specific (see [ADR-0014](/adr/0014-spawn-sandbox-technology)).
- **`cwd`** is the optional working-directory policy. When set, the runtime invokes the subprocess with this cwd. When unset, the runtime uses a per-invocation temporary directory.

Each rule is enumerable and bounded. There is no `*`, no shell evaluation, no glob in argv. A connector that did not declare a program cannot spawn it, period.

##### Host-function ABI

The runtime registers the following functions under the existing `aileron_host` module:

| Function | Shape | Purpose |
|---|---|---|
| `spawn` | `(envelope_ptr, envelope_len) -> handle` | Low-level. Submits a fully-formed spawn envelope: `{ program, argv, env, cwd?, stdin? }`. Used by connectors that need envelope control. |
| `spawn_op` | `(op_ptr, op_len, args_ptr, args_len) -> handle` | High-level. Names an operation declared in `[capabilities.spawn.operations]` and a JSON map of placeholder values. The runtime substitutes, builds the envelope, and dispatches through the same gate as `spawn`. |
| `spawn_status` | `(handle) -> exit_code` | Returns the subprocess exit code (or a negative sentinel for runtime-side denial). |
| `spawn_output_size` | `(handle, which) -> n_bytes` | Reports the captured size of stdout (`which=0`) or stderr (`which=1`). |
| `spawn_output_read` | `(handle, which, dst_ptr, dst_len) -> n_read` | Copies captured output bytes into the connector's linear memory. |

Output retrieval uses the same `_size` / `_read` pattern as `http_response_*`. The buffered output never leaves the runtime's address space until the connector pulls it; the subprocess itself writes to runtime-owned pipes.

Envelope schema (for `spawn`):

```json
{
  "program": "/usr/bin/git",
  "argv": ["git", "log", "--since=2026-04-01", "--author=alr", "--format=%H%x09%s"],
  "env":  { "GIT_AUTHOR_NAME": "...", "GIT_AUTHOR_EMAIL": "..." },
  "cwd":  "/Users/alr/code/aileron",
  "stdin": null
}
```

For `spawn_op`, the connector passes only the op name and a parameter map; the runtime constructs the equivalent envelope from the manifest:

```json
{ "op": "log", "args": { "since": "2026-04-01", "author": "alr" } }
```

A `{name}` placeholder with no value in `args` produces `capability_denied` with `boundary_detail: "envelope"`. A connector cannot exceed the manifest's declarations at runtime.

### Capabilities are abstract types, not concrete resources

A connector's manifest never names a concrete vault path or a specific account. It declares the *type* of credential it needs (e.g. "OAuth2 with this scope") and the user binds a specific account to that requirement at install or first use.

This is a structural property, not a UX preference. A malicious connector cannot name a credential it shouldn't reach because the manifest grammar does not let it. The Stripe connector cannot request `vault://gmail/work`; it can only request `oauth2(scope=stripe.charge)`. If the user had no Stripe credential bound, the action would fail visibly — not silently fall through to some other key with similar shape.

The mechanics of how the user binds an abstract capability to a concrete resource (the install-time UI, the first-use flow, the rebind command) are out of scope for this ADR.

### Publisher identity, signing, and provenance hash

The publisher is implicit in the FQN's authority — `github://acme/gmail` is published by the `acme` GitHub organization. Signing keys are associated with that authority through the publication channel's own conventions (e.g., signing keys configured in the source repo or via sigstore identities tied to the org).

**Convention: a connector repo MUST publish its current ed25519 public key at `keys/publisher.pub` on the source repo's default branch.** This is the path `aileron keyring trust <authority>` reads from when the user trusts a publisher. The file is the PEM form (`openssl pkey -pubout`) or raw 32-byte ed25519 public key in base64; the install tooling accepts either. Publishers who rotate keys commit a new `keys/publisher.pub` and consumers re-run `aileron keyring trust` to pick up the new key alongside the existing one.

Install tooling verifies the binary's signature against keys authorized for the FQN's authority. A connector named `github://acme/gmail` signed by a key not associated with `acme`'s GitHub org is rejected at install. The `provenance_hash` field in the manifest records the binary's content hash at publish time; the runtime checks this matches actual on-disk bytes before every execution.

The runtime treats signature *presence* as information, not as authorization. A signed connector and an unsigned connector both go through the same install consent path; signature status is shown to the user (and the Hub may use it to organize browse views), but a valid signature does not bypass any capability check or skip the consent prompt.

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

- Actions name connectors by FQN: `github://aileron/slack` at version `1.2.0` with hash. No abstract capability names to look up; no bare `slack` that could mean any of several publishers.
- The capability subset an action uses is declared in the action file (e.g. `capabilities = ["chat:write"]` is a subset of the connector's full grant). The runtime enforces this subset *in addition* to the connector's manifest. Defense in depth.
- Updating a connector is a visible edit to action files that reference it. There is no automatic "follow latest" behavior.

### For the Hub and distribution

The Hub model, trust granularity, and publisher identity are ratified separately in [ADR-0013](/adr/0013-connector-hub-and-trust-distribution). Summary of how this ADR's commitments interact with that one:

- The Hub is a discovery and metadata layer, not a registry. It points to connectors at their canonical `github://` / `gitlab://` FQNs and does not host artifacts.
- Aileron does not vet publishers or audit connector binaries. The Hub repo's CI validates entry schema only; trust decisions live with consumers (keyring trust ahead of time, or the install-time prompt).
- Discovery surfaces (browse, search) use the Hub entry's description and the connector's documentation. The `provides.intents` field remains the structured discovery signal at the connector level.

### For security and audit

- Every cross-boundary call is auditable: the connector identity, the capability used, the credential bound, the call result.
- Compromise scope is bounded by the connector's manifest. A compromised Slack connector cannot reach Gmail credentials; it cannot dial hosts outside its declared list.
- Manifest tampering is impossible without invalidating the content hash, which the runtime checks before every execution.

### Open implementation questions (deferred to subsequent ADRs)

- *How does a `version` map to a concrete tag and release artifact in each scheme? How does `aileron connector check` discover available updates? How are pre-releases handled?* — deferred to [ADR-0004](/adr/0004-dependency-resolution).
- *How is the connector store laid out on disk, and how does the integrity-check pipeline interact with concurrent installs?* — deferred to [ADR-0004](/adr/0004-dependency-resolution).
- *Which sandbox technology is the default, and what are the OS-process escalation criteria?* — deferred to [ADR-0005](/adr/0005-sandbox-choice).
- *What does the install consent flow show, and what does the user actually click?* — deferred to [ADR-0007](/adr/0007-install-consent).
- *How does a user bind an abstract capability to a concrete vault entry?* — deferred to [ADR-0006](/adr/0006-capability-binding-ux).

## Examples

### Connector manifest (`gmail.toml`, shipped with the binary)

```toml
[connector]
name = "github://acme/gmail"
version = "1.2.3"
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
name = "github://acme/gmail"
version = "1.2.3"
hash = "sha256:abc123..."
capabilities = ["oauth2"]
```

The action declares the connector it needs (with FQN, exact version, and hash) and the subset of the connector's capability grant it actually uses. The runtime enforces both: the connector cannot exceed its manifest, and the action cannot use capabilities the action did not declare. Two boundaries. Defense in depth.

### Capability denial at runtime

A connector whose manifest declares only `gmail.googleapis.com:443` attempts to dial `evil.example.com:443` mid-execution. The sandbox rejects the syscall. The connector process is terminated. The runtime returns a structured error to the calling action:

```json
{
  "error": {
    "class": "capability_denied",
    "connector": "github://acme/gmail@1.2.3",
    "requested": "network:evil.example.com:443",
    "granted": ["network:gmail.googleapis.com:443", "network:oauth2.googleapis.com:443"],
    "audit_id": "audit-7f3e..."
  }
}
```

The action fails fast and visibly. The audit record persists.
