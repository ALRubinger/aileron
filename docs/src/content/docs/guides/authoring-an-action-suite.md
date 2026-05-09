---
title: "Authoring an Action Suite"
description: "Publish a curated bundle of actions in a single TOML manifest so users can install your full surface with one command."
---

This guide walks you from an empty file to a published `suite.toml` that any user can install with one CLI command. By the end you will have a manifest in your connector repo that names every action you ship, plus a clear understanding of how the path-form vs. FQN-form rule works.

If you have not read it yet, start with [Actions](/concepts/actions/) for the model and [Authoring an Action](/guides/authoring-an-action/) for how individual action templates are built. A suite is a thin layer on top: the actions you wrote already work standalone; the suite manifest just gives users one command instead of N.

## What you are building

An action suite is a single TOML file. There is no compiled artifact, no schema migration, no daemon plumbing. The whole contract is a `name`, a `description`, and a flat list of action references the user wants installed together.

Once committed at the root of your connector repo, users install everything you ship in one shot:

```sh
aileron action add-suite github://you/your-connector/suite.toml@latest
```

Per-run, the trust prompt fires once for your publisher key. The connector tarball is fetched once and reused. OAuth consent (if your connector needs it) pops up once on the first action and the resulting binding is reused for the rest. Per-action failures don't abort the run — they surface in a final summary, the remaining actions still install, and the command exits non-zero if any failed.

## When to publish a suite

Publish a `suite.toml` when your connector ships more than one action that users typically install together. Examples:

- A Google connector with read-Gmail, draft-Gmail, read-Calendar, create-Calendar actions: shipping a suite means a user gets the whole "Gmail-and-Calendar" surface in one command instead of six.
- A Slack connector with read-channel, post-message, list-users actions: a suite frames the bundle as the user's intent ("I want my agent to do Slack") rather than its implementation detail.

Skip the suite if you ship a single action — the existing `aileron action add <FQN>@<version>` form is already one command.

## File location

The convention is one `suite.toml` at the **repo root**:

```
your-connector/
├── actions/
│   ├── action-one/
│   ├── action-two/
│   └── action-three/
├── connector.toml
├── keys/
│   └── publisher.pub
└── suite.toml          ← publish here
```

A user typing `aileron action add-suite github://you/your-connector/suite.toml@latest` resolves `@latest` via the GitHub releases API, fetches `suite.toml` at that release tag from `raw.githubusercontent.com`, and walks every entry.

If you ship multiple themed bundles (rare), put them under `suites/` and users name them explicitly:

```
your-connector/suites/gmail-only.toml
your-connector/suites/calendar-only.toml
```

```sh
aileron action add-suite github://you/your-connector/suites/gmail-only.toml@latest
```

The CLI doesn't care which path you choose; the URL just points at whatever file you committed.

## Schema

A `suite.toml` has three top-level keys. No `[suite]` table, no `[[actions]]` array of tables — just flat top-level keys.

```toml
name = "gmail-and-calendar"
description = "Read and draft Gmail; read and create calendar events"

actions = [
  "actions/list-recent-emails",
  "actions/get-email",
  "actions/list-upcoming-events",
  "actions/draft-email",
]
```

### `name` (required)

A short, human-readable identifier shown in the install summary. Pick something that names the user's intent (`"gmail-and-calendar"`), not the implementation (`"connector-google-action-bundle"`).

### `description` (required, non-empty)

A one-line summary of what the suite does. Strict-mode validation rejects an empty string so published manifests always carry useful metadata.

### `actions` (required, non-empty)

A flat array of strings. Each entry is one of two forms.

**Path form** (`"actions/<name>"`) is the common case. It is interpreted as a path relative to the suite manifest's repo. The CLI inherits the version from the ref the suite was fetched at — so if the user runs `add-suite ...@v0.0.6`, every path-form entry installs at version `0.0.6`. Leading `/` is normalized away (`"/actions/foo"` and `"actions/foo"` are equivalent).

**FQN form** (`"<scheme>://<owner>/<repo>/<subpath>@<version>"`) is for cross-publisher entries — actions that live in a different repo than the suite. The version is required and must be strict SemVer. Use this only when you actually need to bundle an action you didn't publish; same-repo entries are clearer as path-form.

```toml
actions = [
  "actions/list-recent-emails",                              # same repo, inherits ref
  "github://other-pub/other-repo/actions/foo@1.2.3",         # cross-publisher, explicit
]
```

## A worked example

[`aileron-connector-google`](https://github.com/ALRubinger/aileron-connector-google) ships six actions. Its `suite.toml` is:

```toml
name = "aileron-connector-google"
description = "Read and draft Gmail; read and create calendar events; send mail and create events with per-call approval."

actions = [
  "actions/list-recent-emails",
  "actions/get-email",
  "actions/list-upcoming-events",
  "actions/draft-email",
  "actions/send-email",
  "actions/create-calendar-event",
]
```

Users type one command:

```sh
aileron action add-suite github://ALRubinger/aileron-connector-google/suite.toml@latest
```

…and end up with all six actions installed at the latest release version, OAuth bound, ready to invoke.

## How users invoke a suite

Three ref forms cover the common patterns:

| Form | When to use |
|------|-------------|
| `@latest` | The user wants whatever the connector's most recent release ships. The CLI hits `api.github.com/repos/<owner>/<repo>/releases/latest` and uses the returned `tag_name` as the suite's ref. Honors `GITHUB_TOKEN` for higher rate limits. |
| `@<tag>` (e.g. `@v0.0.6`) | The user wants a specific release. The CLI uses the tag verbatim. |
| `@<sha>` (40 hex chars) | The user wants a specific commit. The CLI uses the SHA verbatim. **Caveat:** path-form entries can't inherit a SHA as their install version (actions install by SemVer release tag, not by SHA). A `@<sha>` source must use FQN-form entries throughout. Path-form inheritance also requires a `v`-prefixed SemVer tag (`v0.0.6`); other tag shapes leave nothing for path-form entries to inherit and surface the same error. |

The `@<ref>` is required. There is no implicit "default branch HEAD" — that would silently shift what users install between days, breaking reproducibility.

The same flags `aileron action add` accepts apply to the suite form:

| Flag | Effect |
|------|--------|
| `--yes` | Auto-accept every trust and consent prompt across the run. Use in CI or scripted installs. Does not bypass server-side signature verification. |
| `--force` | Overwrite existing actions with the same name. Without it, an entry whose name + hash already match the installed version is reported as already-installed and skipped. |
| `--no-bind` | Skip the post-install auto-prompt to bind unbound credential capabilities. Useful when a script will run `aileron binding setup` separately. |

## Validation rules

The parser is strict. Authors who break these rules see an error before users do; users who hit a malformed manifest see the same error inline.

- Unknown top-level keys → error. A typo at the top level fails fast rather than silently installing the wrong thing.
- Missing or empty `name` / `description` → error.
- Empty `actions` array → error.
- An empty string in the array → error.
- A path-form entry in a *local* manifest (loaded via filesystem path rather than a remote URL) → error. There's no ref to inherit. Rewrite the entry as FQN-form with explicit `@<version>`.
- An FQN-form entry without `@<version>` → error. Strict SemVer is enforced — same rule the single-action `aileron action add <FQN>` uses.

## Releasing a suite update

A `suite.toml` is content like any other file in your repo. To roll out a new bundle composition:

1. Edit `suite.toml` on `main`.
2. Cut a release as you would for an action change. The release tag (`v<semver>`) becomes the ref users see at `@latest`.

Path-form entries automatically resolve to the same release tag, so you don't have to bump versions inside the manifest.

## For self-curated bundles

The same command accepts a local filesystem path:

```sh
aileron action add-suite ./my-bundle.toml
```

Local manifests can't use path-form entries (no ref to inherit), so every entry must be a full FQN with explicit `@<version>`:

```toml
name = "my-bundle"
description = "Personal bundle for this project"

actions = [
  "github://ALRubinger/aileron-connector-google/actions/list-recent-emails@0.0.6",
  "github://OtherPublisher/another-connector/actions/foo@1.0.0",
]
```

Useful for project-scoped bundles ("for this repo, install these five actions") that don't belong in any one connector's published suite.

## Companion reading

- [Concepts → Actions](/concepts/actions/) — the model behind what you're bundling.
- [Authoring an Action](/guides/authoring-an-action/) — how to write the action templates a suite references.
- [Authoring a Connector](/guides/authoring-a-connector/) — connectors and actions are designed in parallel.
- [Publishing a Connector](/guides/publishing-a-connector/) — signing, tagging, and the keyring trust model that the per-action verification rides on.
