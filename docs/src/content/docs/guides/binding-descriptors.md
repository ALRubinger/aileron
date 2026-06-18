---
title: "Binding Descriptors"
description: "A binding descriptor is a small YAML file that seals an arbitrary CLI's credential at the network boundary without writing any per-CLI code. This page covers the descriptor schema, the two-layer loading order, and a worked Linear example."
---

A command-line tool that talks to a third-party API needs a credential. The usual answer is to hand that credential to the tool, which means the credential lives wherever the tool runs. Aileron's credential-sealing substrate takes a different path. The credential stays in your vault, the agent in the sandbox never holds it, and the daemon injects it at the TLS forward-proxy boundary on the way out. See [ADR-0019](/adr/0019-v4-https-data-plane/) for the data-plane design this builds on.

A **binding descriptor** is how you tell that substrate which host gets which credential, and how to inject it. It is config, not code. A CLI vendor or a community profile ships a descriptor, the descriptor flows through a generic loader into the binding table, and the tool is sealed. No branch anywhere in the proxy keys on the tool's name. Re-representing a CLI as an MCP tool is the wrong move here and is explicitly rejected by [ADR-0024](/adr/0024-sandbox-mcp-parity/). The CLI stays a binary. Only its credential is injected.

## The descriptor format

A descriptor is a versioned YAML document with a list of per-host bindings.

```yaml
version: v1
bindings:
  - host: api.linear.app
    credential_ref: user/linear
    scheme: header-template
    emit_mechanism: A
    header: Authorization
    template: "{token}"
```

Each binding is a quad plus scheme-specific fields.

- `host` is the upstream host matched at the proxy boundary. It is an exact host (`api.linear.app`) or a single leading-wildcard form (`*.example.com`). Ports are not part of the pattern.
- `credential_ref` is a vault credential reference the daemon resolves at injection time. It is a connector-style binding name (`<kind>/<service>/<identity>`) or a user-level reference (`user/<service>`), the namespace `aileron auth <service>` writes. It is never the credential bytes. The descriptor names where the credential lives, never its value.
- `scheme` is one of the closed injection-scheme set: `bearer`, `basic`, `header-template`, `query-param`, `sigv4-resign`. An unknown scheme is a load-time error. `sigv4-resign` is enumerated but not yet implemented.
- `emit_mechanism` declares how the credential reaches egress. `A` injects the credential unconditionally at the proxy. `B` is sentinel-swap, where the launcher plants a non-secret sentinel the proxy swaps for the real credential. The field is optional and defaults to `A`. Mechanism `B` is rejected at load time until the sentinel-swap egress path ([#1196](https://github.com/ALRubinger/aileron/issues/1196)) is wired, because a descriptor must never validate against a mechanism no proxy code can honor. Today only `A` is accepted; an unknown value is also a load-time error.

Scheme-specific fields:

- `username` is required for the `basic` scheme. It is the non-secret HTTP basic-auth username (e.g. `x-access-token` for git-over-HTTPS). The token always rides in the password field.
- `header` and `template` are required for the `header-template` scheme. `header` is the header name to set. `template` is the verbatim header value with a `{token}` placeholder the daemon substitutes with the credential at injection time.
- `query_param` is required for the `query-param` scheme. It is the query-parameter name the credential is set on.

Decoding is strict. An unknown YAML key is an error, not a silently ignored field, so a typo fails fast instead of shipping a binding that does nothing. A wrong or missing `version` is an error so the format can evolve without a silent misparse.

## Two-layer loading

Descriptors load from two layers, in increasing precedence.

1. **Built-in defaults.** Community profiles Aileron ships, embedded at build time. The Linear descriptor above is one.
2. **User layer.** `~/.aileron/binding-descriptors.yaml`.

A later layer overrides an earlier one per `host` key. A user descriptor can replace a shipped community profile for the same host without editing the shipped file. A new host in any layer is added on top of the others rather than replacing them. An absent user file is not an error. It simply contributes nothing.

One invalid layer fails the whole load with a clear error. A malformed descriptor never degrades to a partial or empty binding table, because a typo that silently disables sealing would be a fail-open we reject.

## Worked example: Linear

[Linear](https://linear.app/)'s API authenticates with a personal API key sent verbatim in the `Authorization` header with no `Bearer` prefix. The `header-template` scheme expresses exactly that. The `template` is the bare `{token}`, so the daemon emits `Authorization: <key>` with nothing prepended.

Store your key in the vault under the reference the descriptor names:

```sh
aileron auth linear
```

The built-in Linear descriptor (shown at the top of this page) then seals every request to `api.linear.app`. The Linear CLI inside the sandbox holds no key. The daemon resolves `user/linear` and injects it at egress. If the vault has no `user/linear` entry, the binding still matches but resolution fails closed: no header is added and no secret leaks. An unauthenticated request behaves exactly as it would with no binding configured.

This is the whole generalization proof. Linear is a tool nobody special-cased in the proxy. It is sealed entirely by a descriptor. Adding another vendor is a new descriptor, never new proxy code.

## Out of scope: stateful CLI caches

Some CLIs keep a local cache or index between runs (for example a SQLite or full-text-search store). Persisting that local state across ephemeral sandboxes is **not** handled by binding descriptors and is **not** implemented here. A descriptor seals a credential at the network boundary. It does nothing about on-disk state inside the sandbox. Cache persistence is tracked separately in [issue #1190](https://github.com/ALRubinger/aileron/issues/1190). Choosing a stateless-credential tool like Linear as the proving example keeps this guide cleanly within the credential-injection boundary.
