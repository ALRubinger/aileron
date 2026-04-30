---
title: "ADR-0001: Manifest Format Conventions"
description: "Markdown body + TOML frontmatter for actions; pure TOML for connectors and project config; JSON for runtime IPC"
order: 1
---


<div class="meta">
<table>
  <tr><th>Status</th><td>Accepted</td></tr>
  <tr><th>Date</th><td>2026-04-29</td></tr>
  <tr><th>Tracking</th><td><a href="https://github.com/ALRubinger/aileron/issues/343">#343</a></td></tr>
</table>
</div>

## Context

Aileron's runtime executes against four distinct kinds of declarative artifact:

| Artifact | Read by | Lifetime |
|---|---|---|
| Action files | Runtime (frontmatter), users (prose), Hub (renderer), LLM (function description) | Live in `~/.aileron/actions/`; user-owned and editable |
| Connector manifests | Runtime (capability validation, install verification) | Shipped with the connector binary; immutable per version |
| User config (`~/.aileron/config.toml`) | Runtime, CLI | Per-user; surface preferences, runtime listen address, hub registry |
| Runtime IPC and internal state | Aileron processes (Runtime ↔ connector sandbox; Runtime ↔ CLI) | Process-local; never authored by hand |

These artifacts have very different audiences and very different security postures. Action files mix structured contract with human prose and LLM-facing description. Connector manifests are a security boundary: they declare what a sandboxed binary is allowed to do. User config is editable infrastructure. Runtime IPC is a wire format between trusted processes.

A single format choice across all four would be the wrong answer. Each kind of artifact has its own reader profile and its own failure modes. This ADR fixes a separate format choice for each — and ratifies the trade-offs.

## Decision

### Action files: Markdown body + TOML frontmatter (`+++` delimited)

Action files use a Hugo/Zola-style envelope: TOML frontmatter delimited by `+++`, followed by a Markdown body. The frontmatter is the contract Aileron executes; the body is human-facing documentation that doubles as the LLM's function description when the action is surfaced as a tool.

```markdown
+++
name = "ship-update"
version = "1.0.0"

[[requires.connectors]]
name = "slack"
version = "1.2.0"
hash = "sha256:abc123..."
capabilities = ["chat:write", "channels:read"]

[match]
intent = "tell team I shipped"
+++

# Ship Update

Posts a "shipped" announcement to a Slack channel with the merged PR link.
```

**The body is the LLM-facing description.** When the Runtime augments the agent's tool catalog with installed actions, each function's `description` field is drawn from the action file's Markdown body — typically the first paragraph or a designated section. The author writes one piece of prose and four readers consume it.

**Why Markdown.** Action files have three concurrent audiences and we will not maintain three parallel artifacts. The "structured frontmatter + prose" convention is the established pattern for this shape (Hugo, Jekyll, Zola, Anthropic Skills, AGENTS.md, GitHub issues). It is widely understood, has mature tooling, and renders correctly in every reasonable surface (the Hub, IDE preview, GitHub).

**Why TOML for the frontmatter.** See [TOML over YAML](#toml-over-yaml-for-structured-content) below — the rationale applies identically to the frontmatter portion.

### Connector manifests: pure TOML

Connector manifests are pure TOML. No prose body, no Markdown envelope.

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
```

A connector manifest is *not* a documentation surface. Its sole reader is the Runtime, and its sole job is to declare what the sandboxed binary is allowed to do. Mixing prose into this file would invite the same field to be parsed two ways and would obscure the security review surface. The Hub renders connector documentation from a separate README/markdown file shipped alongside the binary; the manifest stays minimal and machine-focused.

### User config: pure TOML (`~/.aileron/config.toml`)

User-level configuration uses pure TOML. The file is light and optional. When present, it carries user-scoped settings (telemetry opt-out, default Hub registry, runtime listen address, surface preferences for the user channel) that don't belong in any single action file.

```toml
[hub]
registry = "https://hub.aileron.dev"

[runtime]
listen = "127.0.0.1:8721"

[telemetry]
enabled = false
```

There is no lock file. Action files are self-describing — they pin connector versions and content hashes inline. Reproducibility comes from copying the action files; nothing else has to be regenerated.

Aileron does not write configuration files into the user's projects. There is no `<project>/aileron.toml`. Aileron is a personal capability layer; configuration is per-user, in the user's home directory.

### Runtime IPC and internal state: JSON

Wire formats between trusted Aileron processes (Runtime ↔ connector sandbox; Runtime ↔ CLI; Runtime ↔ approval surfaces) use JSON. Internal state files (the content-addressed connector store's catalog, audit log entries, cached binding metadata) also use JSON.

JSON is the right choice here for reasons that are nearly the inverse of the action-file rationale:

- **It is a wire format, not a documentation surface.** Humans rarely read it. Authoring ergonomics do not matter.
- **Every language Aileron will ever interoperate with has a battle-tested, fast JSON implementation in its standard library.** TOML support is patchier.
- **Streaming and partial parsing are routine.** The LLM gateway endpoints (OpenAI-compatible Chat Completions and Anthropic-compatible Messages) stream JSON deltas; the connector sandbox protocol streams JSON-RPC; audit logs are JSONL. None of these have natural TOML idioms.

### TOML over YAML for structured content

This is the load-bearing rationale shared by the action frontmatter, connector manifests, and project config decisions above.

YAML's well-known footguns are not stylistic — they are security-relevant for content that drives execution:

| Footgun | Concrete failure mode |
|---|---|
| Whitespace sensitivity | A misindented capability list silently changes what the connector is granted. |
| Type coercion (the "Norway problem") | `country: NO` parses as boolean `false`. `version: 1.10` parses as `1.1`. |
| Parser-implementation differences | The same YAML file parses differently in different ecosystems. Cross-tool reproducibility fails. |
| Deserialization to arbitrary objects | The vulnerability class behind `!!python/object` and Java's SnakeYAML CVE chain. Repeated CVE history. |

A misparsed action file is not a broken build. It is the *wrong action running with the wrong arguments*, against real systems, with real credentials. The cost of a parsing ambiguity is structurally higher in this domain than it is in a typical CI config file.

TOML's strict, unambiguous syntax eliminates these footguns. Its parser attack surface is dramatically smaller — there is no equivalent of the deserialization-to-arbitrary-objects vulnerability class. Spec-compliant parsers across language ecosystems agree on what a given file means.

### Reversibility

The TOML choice is reversible. Action frontmatter and connector manifests can be cleanly converted in either direction; tooling could later accept YAML frontmatter as an alternative input format if community feedback shows TOML is a real adoption barrier. We are starting with the safer format because the cost of being wrong about format strictness is borne by users at execution time, while the cost of accommodating a second input format later is borne by us at tooling time. That asymmetry favors strict-by-default.

## Alternatives Considered

### YAML for everything (rejected)

YAML is the dominant config format in the developer-tools ecosystem (Kubernetes, GitHub Actions, Anthropic Skills, most CI systems). Authors arriving from those tools will reach for YAML by default.

Rejected because the security-relevant footguns above outweigh the familiarity benefit. We are not in the same threat model as a CI config file. A misparsed action file ships the wrong API call to a real system with real credentials. Treating action files as a safety-critical declarative surface — not as a config file — is the correct framing.

### JSON for everything including authored files (rejected)

JSON is unambiguous and universally implemented. Using it everywhere would eliminate the question of which format goes where.

Rejected because JSON is hostile to authoring at scale: no comments, mandatory quoting, no multi-line strings, trailing-comma intolerance. Action files in particular need a Markdown body for the LLM-facing description; JSON cannot host prose without escape-hell. The authoring-ergonomics gap matters once authors are writing more than a handful of actions.

### A custom Aileron DSL (rejected)

A DSL purpose-built for actions could fit the domain more tightly than TOML.

Rejected on principle. Every DSL Aileron does not invent is one less parser, one less editor plugin, one less IDE integration, one less syntax-highlighting theme, one less validator we have to ship and maintain. The marginal expressiveness over TOML is small; the ecosystem cost is large. We adopt established formats and accept the modest impedance.

### YAML frontmatter, Markdown body (rejected — for now)

This is the Anthropic Skills convention and the dominant pattern in static-site generators. Authors coming from those tools would feel at home.

Rejected for the same reason as YAML-everywhere: action frontmatter declares capabilities and connector versions, which are precisely the fields where YAML's footguns are most expensive. The decision is reversible; we can add YAML frontmatter as an accepted alternative later if adoption friction is real.

## Consequences

### For action and connector authors

- Authors learn TOML if they don't already know it. The learning curve is shallow — TOML is intentionally simpler than YAML — but it is non-zero.
- Tools that generate action files (the Hub, `aileron action add`, scaffolding commands) emit TOML frontmatter. Templates ship as TOML.
- IDE tooling (syntax highlighting, schema validation, hover docs) needs TOML support. Mainstream editors have it; some niche tooling may not.

### For Aileron implementation

- The action-file parser must split TOML frontmatter from the Markdown body. The `+++` delimiter is a single, unambiguous boundary — no balanced-delimiter ambiguity, no escape-sequence quirks.
- Connector manifests and project config use a single TOML library across the codebase. We standardize on one Go TOML implementation to keep parser surface area minimal.
- Runtime IPC and audit log paths use JSON via `encoding/json`. No additional dependency is introduced for the wire format.
- Schema validation for TOML manifests is a first-class concern — install-time validation of connector manifests and action files, with structured error messages. Schema definitions live alongside the parser and are referenced from both the runtime and CLI.

### For the Hub and distribution

- The Hub renders action-file Markdown bodies as documentation pages. Frontmatter is parsed for metadata (name, version, declared connectors).
- The Hub validates action and connector manifests at publish time against the schema. Invalid manifests do not enter the catalog.
- Connector binaries ship with their manifest as a separate file; the manifest is content-hashed alongside the binary so manifest-tampering is caught at install verification.

### For interoperability and ecosystem

- Action files using `+++`-delimited TOML frontmatter render correctly on GitHub (which renders the Markdown body and shows the frontmatter as a code block) and in any standards-compliant Markdown viewer.
- Authors can write action files in any text editor. No proprietary tooling required.
- Existing Hugo/Zola/Anthropic-Skills authors recognize the envelope structure immediately. The pattern is established.

## Examples

### Action file (`actions/ship-update.md`)

````markdown
+++
name = "ship-update"
version = "1.0.0"
source = "hub://aileron/ship-update@1.0.0"

[[requires.connectors]]
name = "slack"
version = "1.2.0"
hash = "sha256:abc123..."
capabilities = ["chat:write", "channels:read"]

[[requires.connectors]]
name = "git"
version = "2.1.0"
hash = "sha256:def456..."
capabilities = ["read"]

[match]
intent = "tell team I shipped"

[[execute]]
id = "recent_merge"
connector = "git"
op = "read_recent_merge"

[[execute]]
id = "post"
connector = "slack"
op = "post_message"

[execute.inputs]
channel = "${args.channel}"
message = "${recent_merge.summary} → ${recent_merge.pr_url}"
+++

# Ship Update

Posts a "shipped" announcement to a Slack channel with the merged PR link.

## When it fires

Triggered when the user tells their agent things like:

- "tell team I shipped the migration"
- "post a ship update to #engineering"
- "let the team know I merged the PR"

## What it does

1. Reads the most recent merge commit from local git.
2. Extracts the PR URL from the commit body.
3. Formats a message and posts it to the specified Slack channel.
````

### Connector manifest (`gmail.toml`, shipped with the connector binary)

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

### User config (`~/.aileron/config.toml`)

```toml
[hub]
registry = "https://hub.aileron.dev"

[runtime]
listen = "127.0.0.1:8721"

[telemetry]
enabled = false
```

### Runtime IPC (illustrative; never authored by hand)

```json
{
  "jsonrpc": "2.0",
  "id": 42,
  "method": "connector.execute",
  "params": {
    "connector": "slack@1.2.0",
    "op": "post_message",
    "inputs": { "channel": "#engineering", "message": "shipped" }
  }
}
```

