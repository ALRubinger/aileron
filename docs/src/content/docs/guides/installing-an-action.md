---
title: "Installing an Action"
description: "Install action templates from a published source: single-action add, suite installs, trust prompts, OAuth binding, and what lands on disk."
---

This guide is for users adding actions to their local Aileron install. It walks through `aileron action add` for a single action, `aileron action add-suite` for a curated bundle, the trust prompt that fires on first install from a new publisher, and the auto-bind step that hands off to OAuth or API-key setup. By the end you will have one or more action templates installed under `~/.aileron/actions/` and credentials bound for any connector they need.

If you have not read it yet, start with [Actions](/concepts/actions/) for the model and [Connectors](/concepts/connectors/) for the surface actions invoke. The companion authoring guides are [Authoring an Action](/guides/authoring-an-action/) and [Authoring an Action Suite](/guides/authoring-an-action-suite/).

## What gets installed

An action is a single Markdown file with TOML frontmatter. `aileron action add` resolves the action's FQN, fetches its tarball, verifies the signature against your local keyring, walks any connector dependencies the action declares, and writes the body to `~/.aileron/actions/<name>.md`. The user owns the file from that point — edit the trigger phrases, retitle the action, swap connector pins, all without anyone's permission.

A suite is a TOML manifest naming several actions to install together. `aileron action add-suite` runs the same flow once per entry, sharing trust state and connector installs across the set so prompts fire at most once per publisher.

## Installing a single action

```sh
aileron action add github://ALRubinger/aileron-connector-google/actions/list-recent-emails@0.0.6
```

The version is required. Either suffix the FQN with `@<version>` or pass `--version=<v>` separately; supplying both is an error if they conflict.

On a fresh machine the CLI runs three things in order before installing:

1. **Trust prompt** for the action's authority (the `<scheme>://<owner>/<repo>` part of its FQN). Aileron fetches `keys/publisher.pub` from the publisher's default branch, prints the fingerprint, and asks for confirmation. Subsequent installs from the same publisher skip this prompt because the key is already in `~/.aileron/keyring.json`.
2. **Preview** of the action's metadata: name, version, source, hash, intent, and the connector dependencies it declares, split into "already installed" vs "will be installed alongside this action". Signature failure or parse errors abort here, before any consent prompt.
3. **Trust prompt** for any *new* connector dependency whose authority differs from the action's. Same fetch + register flow as step 1.

After all trust is established and the preview renders, the CLI prompts `Install? [y/N]:`. On `y`, the server runs the install pipeline (action + every missing connector, atomically) and prints the resulting path:

```
Added: list-recent-emails
  source: github://ALRubinger/aileron-connector-google/actions/list-recent-emails@0.0.6
  path:   /Users/you/.aileron/actions/list-recent-emails.md
```

Connector binaries land under `~/.aileron/store/connectors/sha256/<hash>/`, content-addressed by the same hash the action's frontmatter pins. Two actions that pin the same connector hash share the same on-disk binary.

### Re-installing the same version

If the action and connector hashes the server resolves match what's already installed, the CLI short-circuits with `Already installed: <name>` and exits zero. No prompt, no rewrite. Pass `--force` to overwrite an existing action of the same name with a different hash.

## Installing a suite

A suite manifest is a single TOML file at the root of a connector repo (or anywhere reachable). It names several actions to install together:

```sh
aileron action add-suite github://ALRubinger/aileron-connector-google/suite.toml@latest
```

The CLI resolves the ref, fetches the manifest from `raw.githubusercontent.com`, parses it, and walks each entry through the same install flow as `action add`. Trust state is shared across the run, so the publisher prompt fires at most once even when ten actions reference the same authority. Per-action failures don't abort the run; the CLI prints a final summary and exits non-zero if any failed.

```
Suite: aileron-connector-google
  Read and draft Gmail; read and create calendar events; send mail and create events with per-call approval.
  6 action(s) to install

── [1/6] github://ALRubinger/aileron-connector-google/actions/list-recent-emails@0.0.6
…

Suite install summary:
  ✓ github://ALRubinger/aileron-connector-google/actions/list-recent-emails@0.0.6
  ✓ github://ALRubinger/aileron-connector-google/actions/get-email@0.0.6
  …

All 6 action(s) installed.
```

### Ref forms

Three ref forms cover the common patterns. The `@<ref>` suffix is required — there is no implicit "default branch HEAD."

| Form | What happens |
|------|--------------|
| `@latest` | The CLI calls `api.github.com/repos/<owner>/<repo>/releases/latest` and uses the returned `tag_name`. Set `GITHUB_TOKEN` to raise the anonymous 60 req/hour rate limit. |
| `@<tag>` (e.g. `@v0.0.6`) | The CLI uses the tag verbatim. |
| `@<sha>` (40 hex chars) | The CLI uses the commit SHA verbatim. Path-form entries inside the suite can't inherit a SHA as their install version (actions install by SemVer release tag), so a `@<sha>` source must use FQN-form entries throughout. |

### Local suite manifests

The same command accepts a filesystem path:

```sh
aileron action add-suite ./my-bundle.toml
```

Local manifests can't use path-form entries because there is no ref to inherit from. Every entry must be a full FQN with explicit `@<version>`:

```toml
name = "my-bundle"
description = "Personal bundle for this project"

actions = [
  "github://ALRubinger/aileron-connector-google/actions/list-recent-emails@0.0.6",
  "github://OtherPublisher/another-connector/actions/foo@1.0.0",
]
```

Use this for project-scoped bundles that don't belong in any one connector's published suite. See [Authoring an Action Suite](/guides/authoring-an-action-suite/) for the full schema.

## The auto-bind step

If an action's connector declares a credential capability (OAuth2 or API key) and you don't already have a binding for it, the CLI prompts to set one up immediately after the install lands:

```
This action needs oauth2 — Read your Gmail messages and Calendar events; create drafts and calendar events
access for github://ALRubinger/aileron-connector-google. Set up now? [Y/n]:
```

On `y`, the CLI drives the same flow `aileron binding setup <connector-FQN>` runs:

- **OAuth2 connectors** open your browser to the publisher's authorize URL, capture the code via a local loopback callback, exchange it for tokens, and store the result in your encrypted vault. The publisher's app name appears on the consent screen, not Aileron's. Tokens are refreshed transparently when they near expiry; the connector never sees them.
- **API key connectors** prompt for the value inline and store it in the vault.

You can decline a per-action bind with `n` and run `aileron binding setup <connector-FQN>` later. Pass `--no-bind` to suppress the prompt entirely (useful in scripts).

## Flags

The same flags apply to both `action add` and `action add-suite`:

| Flag | Effect |
|------|--------|
| `--yes` | Auto-accept trust and consent prompts. Does not bypass server-side signature verification — an unsigned or unverifiable artifact still fails closed. |
| `--force` | Overwrite an existing action with the same name. |
| `--no-bind` | Skip the post-install auto-bind prompt. The action installs; unbound capabilities are listed in the output and you bind them later with `aileron binding setup`. |

`action add` also accepts `--version=<v>` for cases where the FQN has no `@<version>` suffix.

## What lands on disk

```
~/.aileron/
├── actions/                              # one .md per installed action
│   ├── list-recent-emails.md
│   └── …
├── store/
│   └── connectors/
│       └── sha256/
│           └── <hash>/                   # content-addressed connector binaries
│               ├── connector.wasm
│               ├── manifest.toml
│               └── signature.sig
├── keyring.json                          # trusted publisher keys
├── secrets.json                          # encrypted vault (Argon2id)
└── audit/                                # daily-rotated audit log
```

Actions are user-owned files. Frontmatter validation runs on every load, so an edit that breaks the schema surfaces immediately the next time the daemon walks the directory.

## What you can't do yet

There are no `aileron action list` / `show` / `remove` subcommands in v1. To remove an installed action, delete the file at `~/.aileron/actions/<name>.md`. To audit what's installed, list the directory or use `aileron sync` (which walks the actions and reports their connector dependencies).

## Common errors

- **`signature_failure`** — the action's authority isn't in your keyring, or the publisher rotated their key without re-running `aileron keyring trust`. Run `aileron keyring trust <authority>` to add the new key, or accept the auto-trust prompt on the next install.
- **`no published releases for <owner>/<repo> — pin with @<release-tag> or @<sha> instead of @latest`** — the repo has no GitHub release. Either the publisher hasn't cut one yet, or you have the wrong repo. Pin a specific tag or SHA.
- **`actions[N] "..." path-form entries require a remote suite manifest with @<release-tag>`** — a local suite manifest used path-form (`actions/<name>`) entries. Rewrite each as a full FQN with explicit `@<version>`.
- **`FQN already includes @<v>; --version=<v> conflicts`** — the FQN suffix and `--version` flag disagree. Use one or the other.

## Companion reading

- [Authoring an Action](/guides/authoring-an-action/) — write the templates this guide installs.
- [Authoring an Action Suite](/guides/authoring-an-action-suite/) — author the bundles `add-suite` consumes.
- [Installing a Connector](/guides/installing-a-connector/) — for connectors you want to install standalone, before any action references them.
- [ADR-0004: Dependency Resolution](/adr/0004-dependency-resolution/) — how FQNs map to GitHub release URLs.
- [ADR-0006: Capability Binding UX](/adr/0006-capability-binding-ux/) — the design behind the auto-bind step.
- [ADR-0007: Install Consent](/adr/0007-install-consent/) — why the preview/prompt flow looks the way it does.
