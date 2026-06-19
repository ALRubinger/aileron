---
title: "Binding Descriptors"
description: "A binding descriptor is a small YAML file that seals an arbitrary CLI's credential at the network boundary without writing any per-CLI code. This page covers the descriptor schema, the two-layer loading order, and the shipped Linear and GitHub built-ins."
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
    emit_mechanism: inject
    header: Authorization
    template: "{token}"
```

Each binding is a quad plus scheme-specific fields.

- `host` is the upstream host matched at the proxy boundary. It is an exact host (`api.linear.app`) or a single leading-wildcard form (`*.example.com`). Ports are not part of the pattern.
- `credential_ref` is a vault credential reference the daemon resolves at injection time. It is a connector-style binding name (`<kind>/<service>/<identity>`) or a user-level reference (`user/<service>`), the namespace `aileron auth <service>` writes. It is never the credential bytes. The descriptor names where the credential lives, never its value.
- `scheme` is one of the closed injection-scheme set: `bearer`, `basic`, `header-template`, `query-param`, `sigv4-resign`. An unknown scheme is a load-time error. `sigv4-resign` is enumerated but not yet implemented.
- `emit_mechanism` declares how the credential reaches egress. `inject` injects the credential unconditionally at the proxy. `sentinel-swap` plants a non-secret sentinel the proxy swaps for the real credential. The field is optional and defaults to `inject`. A value outside the closed set (`inject`, `sentinel-swap`) is a load-time error. A `sentinel-swap` binding must declare a `sentinel` block. An `inject` binding must declare none.

Scheme-specific fields:

- `username` is required for the `basic` scheme. It is the non-secret HTTP basic-auth username (e.g. `x-access-token` for git-over-HTTPS). The token always rides in the password field.
- `header` and `template` are required for the `header-template` scheme. `header` is the header name to set. `template` is the verbatim header value with a `{token}` placeholder the daemon substitutes with the credential at injection time.
- `query_param` is required for the `query-param` scheme. It is the query-parameter name the credential is set on.

`sentinel-swap` fields:

- `sentinel` is a nested block required for the `sentinel-swap` emit mechanism and forbidden for `inject`. It has two fields, both non-secret.
- `sentinel.value` is the format-mimicking placeholder the launcher plants inside the container and the proxy recognizes at egress. The value is non-secret and safe to commit. Presenting it upstream authenticates nothing. The proxy swaps it for the real credential before the request leaves the boundary.
- `sentinel.env` is the environment-variable name the launcher sets to `sentinel.value` inside the container. This is what generalizes the plant target, so the launcher is not hardcoded to one CLI's variable.

The plant target is an environment-variable name only. A CLI that reads its token from a config file rather than an environment variable is not yet expressible. `sentinel.env` covers `gh` and the common token-in-environment case.

Decoding is strict. An unknown YAML key is an error, not a silently ignored field, so a typo fails fast instead of shipping a binding that does nothing. A wrong or missing `version` is an error so the format can evolve without a silent misparse.

## Two-layer loading

Descriptors load from two layers, in increasing precedence.

1. **Built-in defaults.** Trusted community profiles Aileron ships, embedded at build time. The Linear descriptor above is one. GitHub is a second one (see below). This is the trusted layer, distinct from the user layer below.
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

## Worked example: a sentinel-swap binding

Some CLIs refuse to issue a request when they hold no token. They validate locally and short-circuit, so the proxy never sees an outbound request to seal. `gh` is the canonical case. The `sentinel-swap` mechanism closes this gap. The launcher plants a non-secret placeholder under an environment variable the CLI reads, the CLI's local check passes, and the CLI issues the request. The proxy recognizes the placeholder at egress and swaps in the real credential.

`sentinel-swap` works for any host that declares a `sentinel` block, not just GitHub. The recognizer is generic: a request is swapped only when its carrier matches that binding's own `sentinel.value`, so a foreign token on a `sentinel-swap` host is left untouched. GitHub is one instance of the pattern.

```yaml
version: v1
bindings:
  - host: api.github.com
    credential_ref: user/github
    scheme: bearer
    emit_mechanism: sentinel-swap
    sentinel:
      value: ghp_AILERONSENTINELAAAAAAAAAAAAAAAAAAAAA
      env: GH_TOKEN
```

This is the exact binding GitHub ships with as a trusted built-in (see below). The launcher plants `sentinel.value` under `GH_TOKEN`, `gh`'s local check passes, and the proxy swaps the placeholder for the real `user/github` credential at egress. The `value` is a placeholder with no authority, so it is safe to commit in a shipped descriptor.

## Shipped built-in: GitHub

GitHub is the second profile Aileron ships as a trusted built-in, after Linear. It used to be special-cased as Go code in the proxy. It now ships as a trusted default expressed purely as data, flowing the same generic loader every community profile uses. GitHub is no longer special-cased in Go.

GitHub needs two bindings because git-over-HTTPS and `gh` authenticate differently.

```yaml
version: v1
bindings:
  - host: github.com
    credential_ref: user/github
    scheme: basic
    emit_mechanism: inject
    username: x-access-token
  - host: api.github.com
    credential_ref: user/github
    scheme: bearer
    emit_mechanism: sentinel-swap
    sentinel:
      value: ghp_AILERONSENTINELAAAAAAAAAAAAAAAAAAAAA
      env: GH_TOKEN
```

`github.com` is `basic`, `inject`. git-over-HTTPS sends `Authorization: Basic base64(x-access-token:<token>)`, the documented convention for authenticating git with a GitHub token. git issues an unauthenticated request, so the proxy injects the credential unconditionally with no sentinel needed.

`api.github.com` is `bearer`, `sentinel-swap`. `gh` short-circuits locally without a token, so the launcher plants the sentinel as `GH_TOKEN`, `gh` issues the request, and the proxy swaps the sentinel for the real credential at egress. A foreign token the agent supplies itself is left untouched.

Both bindings resolve the same `user/github` reference. Store your token once:

```sh
aileron auth github
```

Only the two exact apexes are sealed. A request to any other `*.github.com` host (`raw.githubusercontent.com` is a different apex entirely) falls through to passthrough rather than being injected with a credential it was never scoped for.

Because GitHub ships in the built-in layer, it is a normal default in the built-in less-than user precedence. A user descriptor that redefines `github.com` or `api.github.com` overrides the shipped default for that host, exactly as it would for Linear. GitHub is a trusted default, not privileged code.

## Out of scope: stateful CLI caches

Some CLIs keep a local cache or index between runs (for example a SQLite or full-text-search store). Persisting that local state across ephemeral sandboxes is **not** handled by binding descriptors and is **not** implemented here. A descriptor seals a credential at the network boundary. It does nothing about on-disk state inside the sandbox. Cache persistence is tracked separately in [issue #1190](https://github.com/ALRubinger/aileron/issues/1190). Choosing a stateless-credential tool like Linear as the proving example keeps this guide cleanly within the credential-injection boundary.
