---
title: "Installing a Connector"
description: "Install a connector binary directly: keyring trust, the install consent prompt, capability preview, content-addressed storage, and credential binding."
---

This guide is for users installing a connector binary on its own, separate from any action that depends on it. It walks through `aileron keyring trust`, `aileron connector install`, the preview-and-consent flow, where the binary lands on disk, and the binding step that follows. By the end you will have a verified, sandboxed connector under `~/.aileron/store/connectors/sha256/<hash>/` and a credential bound (or queued for binding) against it.

If you have not read it yet, start with [Connectors](/concepts/connectors/) for the model. The companion authoring guide is [Authoring a Connector](/guides/authoring-a-connector/), and [Publishing a Connector](/guides/publishing-a-connector/) covers the publisher side of the contract this guide consumes.

## When to install a connector standalone

Most users don't run this command directly. `aileron action add <action-FQN>` walks the action's connector dependencies and installs every missing connector transitively, in the same atomic flow. The standalone command exists for three cases:

1. **You want the connector but no actions yet.** You're building your own actions against it, or you want to drive the binding setup before any action template lands.
2. **You're pinning a specific connector hash.** `--hash=sha256:<hex>` aborts the install if the computed hash doesn't match, useful for reproducible deployments.
3. **You're scripting against a connector check.** `aileron connector check` reports newer versions; `aileron connector install` is how you upgrade.

If you're starting from an action template, [Installing an Action](/guides/installing-an-action/) is the more direct path.

## Trust the publisher first

Connector install verifies the binary's signature against keys registered for the FQN's authority. Without an entry, the install fails closed with `signature_failure` before anything reaches disk.

```sh
aileron keyring trust github://ALRubinger/aileron-connector-google
```

The CLI fetches `keys/publisher.pub` from the publisher's default branch on `raw.githubusercontent.com`, parses the ed25519 public key, and adds it under the authority `github://ALRubinger/aileron-connector-google` in `~/.aileron/keyring.json`. Output looks like:

```
✓ Trusted publisher github://ALRubinger/aileron-connector-google
  Fingerprint: sha256:nXKt8AfDQH5jL2pM1RvB9w
  Keyring: /Users/you/.aileron/keyring.json
```

Re-running the command after the publisher rotates their key adds the new key alongside the old (rotation support, per [ADR-0002](/adr/0002-connector-model/)). Re-running with no rotation reports the existing fingerprint and exits zero.

The keyring is fail-closed by default. There are no pre-trusted publishers shipped with Aileron; every publisher you install from has to be explicitly added.

> **Note on auto-trust:** `aileron action add` auto-prompts to trust an action's publisher (and any new connector dep publishers) before the install. `aileron connector install` also auto-prompts when the FQN is published to the [Connector Hub](/adr/0013-connector-hub-and-trust-distribution/) (ADR-0013): the daemon renders a y/N/d=details decision with the publisher's key fingerprint, trust state, and risk indicators, and on confirm writes the publisher key to the keyring before installing. For an FQN not listed in the Hub (private repos), run `aileron keyring trust` first.

## Preview and install

```sh
aileron connector install github://ALRubinger/aileron-connector-google@0.0.6
```

The version is required. Either suffix the FQN with `@<version>` or pass `--version=<v>` separately.

The CLI runs a two-phase flow per [ADR-0007](/adr/0007-install-consent/):

1. **Preview.** The server fetches the tarball, verifies the signature against your keyring, parses the manifest, and returns the metadata. Signature failure or hash mismatch aborts here, before any prompt.
2. **Render the preview** for you to review:

   ```
   Connector install preview

     FQN:        github://ALRubinger/aileron-connector-google
     Version:    0.0.6
     Hash:       sha256:a1b2c3d4e5f6…
     Publisher:  Andrew Lee Rubinger
     Signature:  verified

     Network access:
       - gmail.googleapis.com:443
       - www.googleapis.com:443

     Credential required:
       kind:  oauth2
       scope: Read your Gmail messages and Calendar events; create drafts and calendar events
   ```

   The capabilities section is the closed list of hosts the connector can reach plus the credential kind it needs. Anything not on the list is denied at the sandbox boundary at runtime.

3. **Consent prompt** (`Install? [y/N]:`). On `y`, the server runs the install pipeline a second time and writes the verified tarball to disk:

   ```
   Installed: github://ALRubinger/aileron-connector-google@0.0.6
     hash: sha256:a1b2c3d4e5f6…
     path: /Users/you/.aileron/store/connectors/sha256/a1b2c3d4e5f6…
   ```

The store is content-addressed. Two installs that resolve to the same `binary || manifest` hash share the same on-disk directory, regardless of FQN or version label.

### Already-installed short-circuit

If the preview reports the same FQN + version + hash as an existing entry, the CLI prints `Already installed: <FQN>@<version>` and exits zero without prompting. There is no work to do.

## Flags

| Flag | Effect |
|------|--------|
| `--version=<v>` | Strict SemVer version to install. Required if the FQN has no `@<version>` suffix. |
| `--hash=sha256:<hex>` | Expected content hash. The install aborts if the computed hash doesn't match. Use this when you want byte-level reproducibility, e.g. in deployment scripts. |
| `--yes` | Skip the consent prompt. Does not bypass signature verification (the server still checks the keyring). |

## Bind the credential

Most connectors need a credential to do anything useful. After the connector installs, drop into the binding setup:

```sh
aileron binding setup github://ALRubinger/aileron-connector-google
```

The CLI prompts for an identity (`work`, `personal`, etc. — a label so you can have multiple bindings against the same connector), then dispatches by the connector's declared credential kind:

- **OAuth2.** The CLI POSTs to `/v1/bindings/setup/oauth2/init`, opens your browser to the publisher's `authorize_url`, listens on a loopback port for the redirect, and exchanges the code for tokens. The publisher's app name appears on the consent screen, not Aileron's. The bound `{access_token, refresh_token}` envelope is encrypted at rest in your vault; tokens are refreshed transparently when they near expiry.
- **API key.** The CLI prompts for the key value inline, encrypts it, and stores it in the vault.

Until at least one binding exists for a connector's credential capability, actions that need it fail with `binding_required` at invocation time. `aileron binding list` shows the metadata for every binding (no secret bytes, ever).

## Checking for updates

```sh
aileron connector check
```

Walks every installed connector, queries the publisher's source for newer versions per [ADR-0004](/adr/0004-dependency-resolution/), and reports a per-connector status:

```
  github://ALRubinger/aileron-connector-google@0.0.5 → 0.0.6 (update available)
  github://OtherPub/another-connector@1.0.0 (up to date)

1 update(s) available; 2 connector(s) checked.
```

Pass `--include-prerelease` to surface non-stable releases. Pass `--json` for NDJSON output (one connector per line) for scripting.

To install the new version, run `aileron connector install <FQN>@<new-version>`. There is no automatic upgrade in v1; the install consent prompt fires for every upgrade so you see the new capability surface before agreeing to it.

## What lands on disk

```
~/.aileron/
├── store/
│   └── connectors/
│       └── sha256/
│           ├── a1b2c3d4e5f6…/                  # one connector tarball, content-addressed
│           │   ├── connector.wasm
│           │   ├── manifest.toml
│           │   └── signature.sig
│           └── …
├── keyring.json                                # trusted publisher keys
└── secrets.json                                # encrypted vault — bindings live here
```

The connector binary is sandboxed at runtime. Per the manifest's declared capabilities, it can reach only the hosts on its allow-list and request only the credential kind it declared at install time. The capability gates fire at every outbound call; nothing the binary does at runtime can widen the surface you reviewed at install.

## What you can't do yet

There are no `aileron connector list` / `show` / `remove` subcommands in v1. `aileron status` reports the installed-connector count, and `aileron connector check` enumerates each one. To remove a connector, delete its directory under `~/.aileron/store/connectors/sha256/` (only safe if no installed action pins that hash — verify with `aileron sync` first).

## Common errors

- **`signature_failure`** — the connector's authority isn't in your keyring, or the publisher rotated their key. Run `aileron keyring trust <authority>` to fetch the current key. Without a trusted key, no install proceeds.
- **`hash mismatch`** — `--hash=...` was supplied and the computed hash doesn't match. Either the publisher republished under the same version (which they shouldn't do), or you have the wrong expected hash.
- **`version is required`** — neither the FQN nor `--version` carried a SemVer. Add `@<version>` to the FQN or pass `--version=<v>`.
- **`binding_required` (at action runtime)** — the connector installed fine but no binding exists for its credential capability. Run `aileron binding setup <connector-FQN>`.

## Companion reading

- [Installing an Action](/guides/installing-an-action/) — the more common entry point; `action add` walks connector deps for you.
- [Authoring a Connector](/guides/authoring-a-connector/) — write the WASM binary this guide installs.
- [Publishing a Connector](/guides/publishing-a-connector/) — sign and release a connector users can install.
- [ADR-0002: Connector Model](/adr/0002-connector-model/) — sandboxing, signing, and the trust model.
- [ADR-0004: Dependency Resolution](/adr/0004-dependency-resolution/) — how FQNs map to release artifacts.
- [ADR-0006: Capability Binding UX](/adr/0006-capability-binding-ux/) — the design behind `aileron binding setup`.
- [ADR-0007: Install Consent](/adr/0007-install-consent/) — why the preview/prompt flow looks the way it does.
