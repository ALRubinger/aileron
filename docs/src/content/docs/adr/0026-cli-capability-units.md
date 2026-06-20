---
title: "ADR-0026: CLI-Capability Units"
description: "One CLI tool's complete credential story ships as one CLI-capability unit under customizations.aileron.cli on that tool's devcontainer Feature. The host reads the unit from the resolved image's devcontainer.metadata OCI label and projects it into the existing capture and proxybinding schemas."
order: 26
---

<div class="meta">
<table>
  <tr><th>Status</th><td>Accepted</td></tr>
  <tr><th>Date</th><td>2026-06-20</td></tr>
  <tr><th>Tracking</th><td><a href="https://github.com/ALRubinger/aileron/issues/1319">#1319</a>, <a href="https://github.com/ALRubinger/aileron/issues/1324">#1324</a></td></tr>
</table>
</div>

## Context

Before this decision, the credential story for `gh` lived in two separate central files owned by two separate packages. Its acquisition (how `gh auth login` runs in a container to mint a token) was a trusted YAML default, `gh.yaml`, embedded and loaded by the auth domain at `internal/auth/capture`. Its sealing (which outbound host gets the credential, and how it is injected at the proxy) was a separate trusted default, `github.yaml`, embedded and loaded by `internal/proxybinding`. Adding a CLI meant editing two central locations in two packages, and a tool's identity was declared more than once across those files.

[ADR-0025](/adr/0025-vault-backed-agent-auth) established the two descriptors this unit composes. A capture descriptor governs the acquisition side, the one-time login that mints a credential the vault did not yet hold. A proxybinding entry governs the sealing side, rendering a stored credential into an outbound request at the proxy boundary. [ADR-0019](/adr/0019-v4-https-data-plane) is the data plane those sealing entries inject at. [ADR-0017](/adr/0017-sandbox-composition) established that sandbox images are composed from devcontainer Features carrying `customizations.aileron` blocks. [ADR-0024](/adr/0024-sandbox-mcp-parity) draws the line this decision honors. A CLI stays a binary the agent runs, and only its credential is injected. A CLI is not re-represented as an MCP tool.

The gap this decision closes is the missing single home for one CLI's complete credential story. The acquisition lived in one package and the sealing lived in another, neither tied to the Feature that installs the tool.

## Decision

One CLI tool's complete credential story ships as one CLI-capability unit under `customizations.aileron.cli` on that tool's devcontainer Feature. The unit declares the tool's name, its single vault key, how its credential is acquired, and how each outbound host seals it. The unit is an envelope, not a new credential format.

### Four planes

A unit has four planes, three of which carry behavior this umbrella and one of which is forward-declared.

- **Presence** is accepted and reserved. A unit may declare `presence.builtin` naming the built-in image tier the tool is present in (e.g. `base`). The field is validated for well-formedness, and no behavior branches on it this umbrella.
- **Acquisition** is shipped for the device-flow mode. A device-flow unit drives an interactive container login and reads the token back out. The `direct-token` mode is reserved and rejected. A unit naming it fails validation with an explicit not-yet-implemented error rather than a silent best-effort decode.
- **Sealing** is shipped. Each sealing entry seals one outbound host's credential at the proxy boundary, using the same scheme set and sentinel rules `internal/proxybinding` already owns.
- **State** is reserved. A unit's `state` block is forward-declared for cross-sandbox cache persistence. An absent or empty `state` block is valid, and a non-empty `state` block fails validation with an explicit not-yet-implemented error.

### The carrier

The unit lives under `customizations.aileron.cli` on a devcontainer Feature. The devcontainer CLI stamps a built image with a `devcontainer.metadata` OCI label whose value is a JSON array of per-Feature metadata objects. The host reads that label from the resolved sandbox image through `internal/cli/unitloader`, parses each present `customizations.aileron.cli` block through the canonical `internal/cli` parser, and projects the resulting units into the two existing consumers. An image whose label is absent or carries no `customizations.aileron.cli` is a clean no-op that ships the embedded defaults alone. An image whose label is present but malformed is a loud error, because a present-but-broken unit must not silently ship nothing.

### The key single-source derivation

A unit declares its credential identity once, through a single `key` field in `<kind>/<service>` form (e.g. `user/github`). Every per-store vault reference derives from that one field. The acquisition `store_at` is the `key`. Every sealing entry's `credential_ref` is the `key`. The credential `kind` is the first path segment of the `key` (e.g. `user/github` derives kind `user`). The unit must not re-declare `store_at`, `credential_ref`, or `kind`. Re-declaring any derived field, even to a value equal to the derived one, is a load error so the `key` stays the one source of truth.

### Envelope over capture and proxybinding

The unit is a thin envelope over two existing formats, and it adds no new credential decoder. The acquisition block mirrors `capture.CaptureDescriptor` field for field, and converting a unit to a capture descriptor is a field copy. Each sealing entry mirrors `proxybinding.Entry`, minus the `credential_ref` the unit derives from `key`, and converting a sealing entry to a proxybinding entry is a field copy. Validation is delegated to the canonical `capture.(*CaptureDescriptor).Validate` and `proxybinding.(*Entry).Validate`. The scheme set, the sentinel rules, the host-pattern legality, and the arg-vector semantics live in the source packages, and this envelope re-implements none of them. The two consumer loaders remain independent, and a user descriptor can still override either by name. The projection is byte-identical to the former central defaults, pinned by `internal/app`'s drift guard.

### The worked `gh` example

`gh`'s complete credential story ships as one unit on its devcontainer Feature at `images/sandbox-features/gh/devcontainer-feature.json`. The `customizations.aileron.cli` block is the unit:

```yaml
name: gh
key: user/github
presence:
  builtin: base
acquisition:
  mode: device-flow
  container_name: aileron-auth-github
  login_cmd: [gh, auth, login, --hostname, github.com, --git-protocol, https, --web]
  token_cmd: [gh, auth, token, --hostname, github.com]
  browser_shim: echo
sealing:
  - host: github.com
    scheme: basic
    emit_mechanism: inject
    username: x-access-token
  - host: api.github.com
    scheme: bearer
    emit_mechanism: sentinel-swap
    sentinel:
      value: ghp_AILERONSENTINELAAAAAAAAAAAAAAAAAAAAA
      env: GH_TOKEN
```

The single `key: user/github` is the source of the acquisition `store_at`, the credential `kind` of `user`, and the `credential_ref` of both sealing entries. The `github.com` entry seals git-over-HTTPS with HTTP basic auth and injects unconditionally. The `api.github.com` entry seals `gh` with a bearer token, planting a non-secret sentinel the proxy swaps for the real credential at egress. The `sentinel.value` is a placeholder with no authority and is safe to commit.

### The cutover

Both central files were removed under [#1323](https://github.com/ALRubinger/aileron/issues/1323). The auth domain's `gh.yaml` and proxybinding's `github.yaml` no longer exist on `main`. `gh`'s unit on its devcontainer Feature is now the single source of truth, and the host projects it into the same two layers the central files used to feed.

## Consequences

### Positive

- One CLI's complete credential story lives in one place, on the Feature that installs the tool. Acquisition and sealing are no longer split across two packages.
- Adding or changing a CLI's credential story is one edit on that CLI's Feature, with no edit to a central file in core.
- The credential identity is declared once. The single `key` derives every per-store reference, so a tool's identity cannot drift between its acquisition and its sealing.
- The projection is byte-identical to the former central defaults, pinned by a drift guard, so the cutover changed the source of the data and not the data itself.
- The envelope re-implements no field semantics. Validation delegates to the canonical capture and proxybinding validators, so there is one scheme set and one name contract.

### Negative

- Only the device-flow acquisition mode, sealing, and base-tier presence are live. The `direct-token` acquisition mode is reserved and rejected.
- The `state` plane is reserved and rejected when non-empty, so cross-sandbox cache persistence is not yet expressible in a unit.
- Presence carries no behavior this umbrella. A non-base presence declaration is accepted as data but drives nothing.
- The sentinel plant target is an environment-variable name only. A CLI that reads its token from a config file rather than an environment variable is not yet expressible.

## Deferred follow-on

The following are explicitly forward-declared and not done in this umbrella.

- `aileron auth <cli>` capture-image convergence for a Feature-installed non-base CLI. The standalone `aileron auth github` command already resolves gh's acquisition from the same image unit layer the launcher reads, because gh installs in the base image that `aileron auth` inspects. A CLI installed by a non-base Feature is not yet reachable by `aileron auth`, so its acquisition cannot converge on its unit this way.
- The acquisition `direct-token` mode, for tools that supply a token directly rather than acquiring one through an interactive login (the `linear-pp-cli` case).
- The `state` and cross-sandbox cache plane, absorbing the cache-persistence ask tracked in [#1190](https://github.com/ALRubinger/aileron/issues/1190).
- Third-party untrusted units and the trust tiers that gate them, so a unit from an untrusted Feature is held to a different bar than a trusted in-house one.
- File-based sentinel plant, so a CLI that reads its token from a config file rather than an environment variable can be sealed.

## References

- [Issue #1319](https://github.com/ALRubinger/aileron/issues/1319). CLI-capability-unit umbrella
- [Issue #1324](https://github.com/ALRubinger/aileron/issues/1324). This ADR's tracking sub-issue
- [Issue #1190](https://github.com/ALRubinger/aileron/issues/1190). Stateful CLI cache persistence (the deferred state plane)
- `images/sandbox-features/gh/devcontainer-feature.json`. The `gh` unit's carrier Feature
- `internal/cli`. The unit type, parser, validator, and the two conversion adapters
- `internal/cli/unitloader`. The host-side bridge from the image `devcontainer.metadata` label to the two consumers
- `internal/auth/capture`. The acquisition consumer the unit projects into
- `internal/proxybinding`. The sealing consumer the unit projects into
- [ADR-0017](/adr/0017-sandbox-composition). Sandbox composition through devcontainer Features
- [ADR-0019](/adr/0019-v4-https-data-plane). The HTTPS data plane the sealing entries inject at
- [ADR-0024](/adr/0024-sandbox-mcp-parity). A CLI stays a binary; only its credential is injected
- [ADR-0025](/adr/0025-vault-backed-agent-auth). The acquisition and binding descriptors this unit composes
