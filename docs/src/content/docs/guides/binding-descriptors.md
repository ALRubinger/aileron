---
title: "Binding Descriptors"
description: "A binding descriptor is a small YAML file that seals an arbitrary CLI's credential at the network boundary without writing any per-CLI code. This page covers the descriptor schema, the two-layer loading order, the shipped Linear built-in, and how gh's sealing now arrives as the sealing plane of its CLI-capability unit."
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

- `host` is the upstream host matched at the proxy boundary. It is an exact host (`api.linear.app`) or a single leading-wildcard form (`*.example.com`). Ports are not part of the pattern. `host` is optional when the entry declares a complete credential identity (`kind` + `identity_label`): such an identity binding is selected at egress by its `(kind, identity_label)` pair rather than by host. An entry with neither a host nor a complete identity is a load-time error.
- `kind` and `identity_label` are the non-secret credential-identity pair. `kind` is the credential kind (e.g. `aws-sigv4`) and `identity_label` names which credential of that kind to inject. Declare both or neither. When both are set the entry is a host-less identity binding the proxy selects by identity at egress.
- `credential_ref` is a vault credential reference the daemon resolves at injection time. It is a connector-style binding name (`<kind>/<service>/<identity>`) or a user-level reference (`user/<service>`), the namespace `aileron auth <service>` writes. It is never the credential bytes. The descriptor names where the credential lives, never its value.
- `scheme` is one of the closed injection-scheme set: `bearer`, `basic`, `header-template`, `query-param`, `sigv4-resign`. An unknown scheme is a load-time error.
- `emit_mechanism` declares how the credential reaches egress. `inject` injects the credential unconditionally at the proxy. `sentinel-swap` plants a non-secret sentinel the proxy swaps for the real credential. The field is optional and defaults to `inject`. A value outside the closed set (`inject`, `sentinel-swap`) is a load-time error. A `sentinel-swap` binding must declare a `sentinel` block. An `inject` binding must declare none.

Scheme-specific fields:

- `username` is required for the `basic` scheme. It is the non-secret HTTP basic-auth username (e.g. `x-access-token` for git-over-HTTPS). The token always rides in the password field.
- `header` and `template` are required for the `header-template` scheme. `header` is the header name to set. `template` is the verbatim header value with a `{token}` placeholder the daemon substitutes with the credential at injection time.
- `query_param` is required for the `query-param` scheme. It is the query-parameter name the credential is set on.
- `access_key_id` is required for the `sigv4-resign` scheme. It is the non-secret AWS access key id that appears verbatim in the signed `Credential=` field. The secret access key rides in the resolved credential value, never here. A `sigv4-resign` entry is identity-only: it declares `kind` + `identity_label` and carries no `host`. The daemon derives the signing region and service from the resolved upstream host at egress, so the binding carries no region or service and no second copy of the region can drift from the host being signed for.

`sentinel-swap` fields:

- `sentinel` is a nested block required for the `sentinel-swap` emit mechanism and forbidden for `inject`. It has two fields, both non-secret.
- `sentinel.value` is the format-mimicking placeholder the launcher plants inside the container and the proxy recognizes at egress. The value is non-secret and safe to commit. Presenting it upstream authenticates nothing. The proxy swaps it for the real credential before the request leaves the boundary.
- `sentinel.env` is the environment-variable name the launcher sets to `sentinel.value` inside the container. This is what generalizes the plant target, so the launcher is not hardcoded to one CLI's variable.

The plant target is an environment-variable name only. A CLI that reads its token from a config file rather than an environment variable is not yet expressible. `sentinel.env` covers `gh` and the common token-in-environment case.

Decoding is strict. An unknown YAML key is an error, not a silently ignored field, so a typo fails fast instead of shipping a binding that does nothing. A wrong or missing `version` is an error so the format can evolve without a silent misparse.

## Two-layer loading

Descriptors load from two layers, in increasing precedence.

1. **Built-in defaults.** Trusted community profiles Aileron ships, embedded at build time. The Linear descriptor above is one. This is the trusted layer, distinct from the user layer below.
2. **User layer.** `~/.aileron/binding-descriptors.yaml`.

A `gh` request is sealed too, but `gh`'s bindings no longer arrive as an embedded built-in default. They arrive through a third source: the sealing plane of `gh`'s CLI-capability unit, carried on its devcontainer Feature and projected into the binding table from the sandbox image's `devcontainer.metadata` label (see [the gh sealing plane](#shipped-via-the-gh-cli-capability-unit-sealing) below). Linear remains the embedded built-in worked example.

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

`sentinel-swap` works for any host that declares a `sentinel` block. The recognizer is generic: a request is swapped only when its carrier matches that binding's own `sentinel.value`, so a foreign token on a `sentinel-swap` host is left untouched. GitHub is one instance of the pattern.

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

This is the `gh` unit's sealing entry for `api.github.com` (see [the gh sealing plane](#shipped-via-the-gh-cli-capability-unit-sealing) below). The launcher plants `sentinel.value` under `GH_TOKEN`, `gh`'s local check passes, and the proxy swaps the placeholder for the real `user/github` credential at egress. The `value` is a placeholder with no authority, so it is safe to commit in a shipped descriptor.

## Shipped via the gh CLI-capability unit: sealing

`gh`'s sealing used to be special-cased as Go code in the proxy, then for a time it shipped as a central built-in descriptor named `github.yaml`. It is neither now. `gh`'s sealing is the sealing plane of `gh`'s CLI-capability unit, carried on `gh`'s devcontainer Feature under `customizations.aileron.cli`. The host reads that unit from the resolved sandbox image's `devcontainer.metadata` label and projects its sealing entries into the same binding table the built-in and user layers feed. See [ADR-0026](/adr/0026-cli-capability-units/) for the unit model and the [Add a CLI Capability](/development/sandbox-composition/#add-a-cli-capability) section of the composition guide for how to author one. The central `github.yaml` built-in no longer exists.

`gh` needs two sealing entries because git-over-HTTPS and `gh` authenticate differently. The unit declares the credential once under its `key: user/github`, and each entry's `credential_ref` is derived from that key rather than re-declared. The projected bindings are:

```yaml
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

Both entries resolve the same `user/github` reference. Store your token once:

```sh
aileron auth github
```

Only the two exact apexes are sealed. A request to any other `*.github.com` host (`raw.githubusercontent.com` is a different apex entirely) falls through to passthrough rather than being injected with a credential it was never scoped for.

The projection sits in the binding table between the built-in defaults and the user layer, so a user descriptor that redefines `github.com` or `api.github.com` still overrides `gh`'s shipped sealing for that host, exactly as it would for Linear. `gh`'s sealing is trusted data carried by its Feature, not privileged code.

## Out of scope: stateful CLI caches

Some CLIs keep a local cache or index between runs (for example a SQLite or full-text-search store). Persisting that local state across ephemeral sandboxes is **not** handled by binding descriptors and is **not** implemented here. A descriptor seals a credential at the network boundary. It does nothing about on-disk state inside the sandbox. Cache persistence is tracked separately in [issue #1190](https://github.com/ALRubinger/aileron/issues/1190). Choosing a stateless-credential tool like Linear as the proving example keeps this guide cleanly within the credential-injection boundary.
