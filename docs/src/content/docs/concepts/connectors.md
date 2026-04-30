---
title: "Connectors"
description: "Connectors are sandboxed binaries that know how to talk to a specific external service. Aileron core knows nothing about Slack, GitHub, or Stripe — connectors do."
order: 3
---

A **connector** is a sandboxed binary that knows how to talk to a specific external service. The Slack connector knows the Slack HTTP API. The Linear connector knows Linear's GraphQL endpoint. The Stripe connector knows Stripe's idempotency conventions. Aileron core knows none of this — connectors do.

If [actions](/concepts/actions/) are *what* the agent can do, connectors are *how* it gets done.

## What a connector is

A connector is a standalone artifact: a WASM binary plus a manifest. The manifest declares exactly what the connector needs from the runtime — which network destinations it dials, which credential kind it expects, which host functions it imports. The runtime grants nothing not declared.

```toml
[connector]
name = "github://aileron/slack"
version = "1.2.3"
provenance_hash = "sha256:abc123..."

[capabilities.network]
hosts = ["slack.com:443"]

[capabilities.credential]
kind = "oauth2"
scope = "chat:write,channels:read"

[capabilities.runtime]
imports = ["wasi:http/outgoing-handler"]

[provides]
intents = ["post_message", "list_channels"]
```

The manifest is a *request*. If the connector tries to dial `evil.example.com:443` at runtime, the WASM sandbox denies the syscall — there's no entry for that host in the manifest. If it tries to read a credential it didn't declare a need for, it can't even name a vault path that would work.

## Connectors are sandboxed; Aileron core doesn't trust them

Connectors are third-party code. The Slack connector might be written by Slack, by Aileron, by a community contributor, or by you. The runtime treats all of them the same: untrusted, sandboxed, and bounded by what their manifest declared.

Three properties make this safe:

- **Memory isolation.** WASM components have separate linear memory. One connector cannot read another connector's memory, the runtime's memory, or the vault.
- **Capability bounds.** Network access, credential access, host functions — all are gated by the manifest. The sandbox refuses anything not declared.
- **Sealed credentials.** When a connector needs a credential, the runtime mediates. The connector emits an outgoing HTTP request with placeholder authorization; the runtime intercepts host-side, attaches the credential, and forwards the request. The connector never sees the credential's bytes.

The result: even a fully malicious connector binary cannot exfiltrate credentials, dial unauthorized hosts, or affect other connectors. The compromise scope is bounded by the manifest, not by trust in the publisher.

## Aileron core ships only primitive capability types

The runtime knows about three primitive capability types:

- **Network access** — outbound HTTP/TCP to declared `host:port` pairs.
- **Credential access** — a vault credential of a declared kind (OAuth2, API key, basic auth, signing key) with declared scopes.
- **Host functions** — narrow runtime services (audit emit, structured logging, time, RNG).

That's it. The runtime has no built-in knowledge of Gmail, Slack, Stripe, GitHub, Linear, or any other service. Service-specific knowledge — which endpoints to hit, what request shape to send, how OAuth refresh works — lives entirely inside the connector binary.

This keeps the runtime small and vendor-neutral, and makes the ecosystem coherent: every connector composes with the runtime through the same primitive grant types. A connector for an obscure service composes the same way a Slack connector does.

## Connector identity: fully-qualified URIs

Connectors are named with **fully-qualified URIs** that encode where they came from:

| FQN | Meaning |
|---|---|
| `github://aileron/slack` | Published from the `aileron` GitHub organization, repo `slack` |
| `github://acme/stripe` | Published from the `acme` GitHub organization, repo `stripe` |
| `gitlab://team/linear` | Published from the `team` GitLab namespace, repo `linear` |
| `hub://aileron/slack` | Published to the Aileron Hub under `aileron`'s namespace |

This is decentralized — no central naming authority, no registry to register with. Two organizations can both publish a connector named `slack` without colliding because `github://acme/slack` and `github://other/slack` are distinct identities.

Forks are first-class: if `acme` forks `aileron/slack`, the fork lives at `github://acme/slack` and is a *different* connector with its own version line and content hash. Action files reference one or the other explicitly; there's no "I forked it but kept the name" ambiguity.

## How actions use connectors

An action declares the connectors it needs, including the exact version, the content hash, and the capability subset:

```toml
[[requires.connectors]]
name = "github://aileron/slack"
version = "1.2.0"
hash = "sha256:abc123..."
capabilities = ["chat:write", "channels:read"]
```

The runtime enforces this at two boundaries — the connector cannot exceed its own manifest, *and* the action cannot exceed the subset it declared. Defense in depth: even if an action's execution path tries to invoke a capability the connector permits, if the action didn't declare it, the call is denied.

A single connector typically supports many actions. The Slack connector might be referenced by `ship-update`, `daily-standup`, `oncall-handoff`, and a dozen others. Connectors are reusable; actions are specific.

## How you get connectors

Most users never install connectors directly — they install [actions](/concepts/actions/), and the actions' connector dependencies install transparently as part of the action add flow. Each new connector dependency triggers its own install consent prompt, so you see exactly what each connector will be allowed to do before it lands on your machine.

If you do want to install a connector directly:

```sh
$ aileron connector install github://aileron/slack@1.2.0
```

The runtime fetches the binary, verifies the signature against keys associated with the source's authority, computes the content hash, checks it matches what was declared, and writes the binary to a content-addressed store at `~/.aileron/store/`. The same connector is shared across every action that uses it — no duplicate downloads.

Once a connector is in the store, action invocation is fully offline. No phone-home, no runtime registry lookup. Updates are explicit (`aileron connector check` shows what's available; `aileron action update <name>` shows the diff).

## What connectors don't do

A few non-obvious limits:

- **Connectors don't hold credentials.** The runtime mediates every credential use; the connector's only reach into credential land is through the runtime's host-side injection. There's no `vault.read("credential-x")` API and there will not be.
- **Connectors don't talk to each other.** Each connector instance is isolated. If two connectors need to coordinate, the action that uses both is the coordination point.
- **Connectors aren't long-running.** A connector instance starts when an action invokes it and terminates when the action finishes. State that needs to persist across invocations lives in the user's vault or in external systems, not in connector memory.
