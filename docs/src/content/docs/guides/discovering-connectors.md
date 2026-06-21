---
title: "Discovering on the Hub"
description: "Browse and install community-published suites, actions, and connectors via the Aileron Hub. Covers aileron hub search/show, the webapp Hub page, the composite trust panel, and the install-decision prompt."
---

This guide is for users who want to find and install community-published suites, actions, and connectors. The Aileron Hub is a public discovery index — a catalog that publishers opt into so users can find them. Discovery is decoupled from trust. Appearing in the Hub is not an Aileron endorsement. You still confirm the publisher's signing key fingerprint before installing.

The Hub itself is the GitHub repo [`aileron-hub`](https://github.com/ALRubinger/aileron-hub). It catalogs three entry types, each one YAML pointer to a canonical FQN:

- `suites/*.yaml` — bundles of related actions (e.g. "Gmail + Calendar").
- `actions/*.yaml` — individual action templates (e.g. "draft a Gmail").
- `connectors/*.yaml` — the binaries those actions and suites depend on.

The Hub holds no binaries. Install artifacts still come from the publisher's source repository. The intent-first default browse leads with **Suites**, then **Actions**, then **Providers** (connectors) — see [ADR-0013](/adr/0013-connector-hub-and-trust-distribution/).

For the install pipeline that runs under all this, see [Installing a Connector](/guides/installing-a-connector/) or [Installing an Action](/guides/installing-an-action/). The Hub layers discovery and a per-FQN trust prompt on top of those pipelines.

## Quick tour

```sh
# Default browse: all three catalogs grouped by type, Suites first.
aileron hub list

# Per-catalog browse.
aileron hub list suites
aileron hub list actions
aileron hub list connectors

# Cross-catalog search (matches FQN and description in all three).
aileron hub search calendar

# Restrict search to one catalog.
aileron hub search calendar --type suites

# Show a single entry (dispatches by entry type).
aileron hub show github://ALRubinger/aileron-connector-google/actions/draft-email
```

The same data is browseable in the webapp: under `aileron launch`, visit `/hub` for a tabbed shell (Suites / Actions / Providers) with per-card detail pages and a composite install modal.

## How the daemon talks to the Hub

The daemon shallow-clones `aileron-hub` on every query and parses the entries on the fly. There is no persisted cache and no GitHub API call (per [ADR-0013](/adr/0013-connector-hub-and-trust-distribution/) and the resolution of issue #486). Each `aileron hub` invocation pays one shallow git clone of a small repo. The CLI is the same shape as any other Aileron subcommand: it talks to the local daemon over `/v1/hub/*`, the daemon does the work.

The webapp's `/hub` page reads the same endpoints. CLI and webapp render the same data in different shapes.

## `aileron hub` CLI

### `list [<catalog>]`

With no argument, prints all three catalogs grouped by type:

```
── SUITES ──
FQN                                                 PUBLISHER             ACTIONS  DESCRIPTION
github://ALRubinger/aileron-connector-google/suite  ALRubinger            5        Read and draft Gmail; read and create calendar events

── ACTIONS ──
FQN                                                              PUBLISHER             DESCRIPTION
github://ALRubinger/aileron-connector-google/actions/draft-email ALRubinger            Draft a Gmail message with subject, recipients, and body
...

── PROVIDERS ──
FQN                                                 PUBLISHER             DESCRIPTION
github://ALRubinger/aileron-connector-google        ALRubinger            Google Workspace connector (Calendar, Drive, Gmail)
```

With an explicit catalog (`suites` / `actions` / `connectors`), prints just that one. Pass `--json` for NDJSON (single-catalog) or a single grouped JSON object (default mode) when scripting.

### `search <query> [--type <catalog>]`

Case-insensitive substring match on FQN and description across all three catalogs by default. `--type` narrows to one catalog when you already know what you want. Server-side filter, so the daemon handles the matching rules.

```sh
aileron hub search slack
aileron hub search draft --type actions
```

### `show <fqn> [--type <catalog>]`

Tries each catalog in order (connector → action → suite) and prints the first match. `--type` skips dispatch and queries the named catalog directly. The rendering shape depends on entry type — connector entries show key URL + release pattern, actions show the required connector + intents, suites show member actions + the closure of connectors they depend on.

```
Kind:           action
FQN:            github://ALRubinger/aileron-connector-google/actions/draft-email
Description:    Draft a Gmail message with subject, recipients, and body
Publisher:      ALRubinger
Connector:      github://ALRubinger/aileron-connector-google
Intents:        draft email, compose gmail
Category:       communication
```

## The install-decision prompt (connectors)

When you run `aileron connector install <FQN>` on a Hub-listed connector, the CLI does not jump straight to the preview flow. It first fetches an install-decision payload from the daemon, which:

1. Looks up the Hub entry.
2. Fetches the publisher's current public key from `key_url`.
3. Computes the key's fingerprint.
4. Compares against your local keyring to determine trust state.

You see something like this:

```
Hub install-decision

  FQN:         github://ALRubinger/aileron-connector-google
  Description: Google Workspace connector (Calendar, Drive, Gmail)
  Publisher:   ALRubinger
  Fingerprint: sha256:nXKt8AfDQH5jL2pM1RvB9w
  Trust:       unknown (first install)

  Risk indicators:
    • First connector by this publisher you've installed

Install? [y/N/d=details]:
```

Three answers are accepted:

- **`y`** confirms. The daemon writes the publisher key to your keyring under this FQN, then runs the install pipeline. Trust persists on disk even if the install pipeline later fails. Re-running the install does not re-prompt.
- **`N`** (or any unrecognized answer) cancels. Nothing is written. The daemon does not run the install pipeline.
- **`d`** shows full details: the complete list of risk indicators, every other connector the same publisher has listed in the Hub. The prompt then asks again with `y/N`.

### Trust states and colors

The `Trust:` line picks one of three labels, color-coded so the unusual states stand out:

| State | Color | Meaning |
|---|---|---|
| `already trusted` | green | An owner-level grant for this publisher is on your keyring and its key matches the key at this FQN. The install proceeds without trust-write work. |
| `unknown (first install)` | yellow | No owner-level grant for this publisher resolves a trusted key. Confirming will register the publisher's current key. |
| `conflict — key differs from the publisher's trusted key` | red | An owner-level grant for this publisher is on your keyring, and the key at this FQN's `key_url` doesn't match it. Possible explanations are key rotation, MITM, or impersonation. Worth a closer look before confirming. |

The `conflict` state is rare and intentional. Trust is evaluated at owner granularity in keyring v2, so a publisher you trust once is expected to sign every connector they ship with the same key. The conflict surface flags the case where a fetched key diverges from that owner-level grant, which the install-decision payload also reports as a non-blocking entry in its `risk_indicators` array. It is informational and never blocks the install on its own. Run `aileron keyring list` to see what's currently trusted, compare fingerprints with the publisher's release notes or commits to `keys/publisher.pub` in their repo, then make a call.

## The composite install-decision (actions and suites)

When you run `aileron action add <ACTION-FQN>` or `aileron action add-suite <SUITE-URL>` on a Hub-listed action or suite, the CLI fetches a *composite* install-decision payload. It walks the dependency closure and groups by unique connector authority, surfacing one trust panel per authority — even if a suite bundles five actions that all depend on the same connector, you see one trust gate, not five.

```
Hub install-decision (suite)

  Suite:     github://ALRubinger/aileron-connector-google/suite
  Summary:   Read and draft Gmail; read and create calendar events
  Publisher: ALRubinger
  Actions:   5
    - github://ALRubinger/aileron-connector-google/actions/list-recent-emails
    - github://ALRubinger/aileron-connector-google/actions/draft-email
    ...

  Trust gate(s): 1 connector authority

  ● github://ALRubinger/aileron-connector-google
      Publisher:   ALRubinger
      Fingerprint: sha256:i4l2kuD8q++d5b9v8/LLI1
      Trust:       unknown (first install)
      Risk:        • First connector by this publisher you've installed

Trust these publishers and continue? [y/N/d=details]:
```

A single `y` covers every authority surfaced in the panel. The daemon writes per-FQN trust to your keyring for each before the install pipeline runs. Per-authority y/N prompts that the legacy flow would have fired are suppressed — `trustState` records them as already accepted.

For a cross-publisher suite (one whose member actions depend on connectors from multiple publishers), the panel renders one section per unique authority and the same single confirm covers them all.

Fall-through: if the action or suite isn't Hub-listed (404), the composite endpoint returns nothing useful and the CLI falls through to the legacy per-authority trust prompts unchanged. Local-file suite installs (`aileron action add-suite ./my-suite.toml`) never carry a Hub FQN and always use the legacy flow.

## The webapp's Hub page

Under `aileron launch`, the daemon serves a static webapp at `http://127.0.0.1:<port>/`. The `/hub` page renders a tabbed shell with the three catalogs:

- **Suites** (default tab) — bundled action sets, grouped by intent.
- **Actions** — atomic action templates, with the required connector visible per card.
- **Providers** — connector entries; the same view as the pre-amendment page.

Each card has a per-entry **Install** button and a clickable FQN linking to a detail page at `/hub/suites/[fqn]` or `/hub/actions/[fqn]`. The detail page renders the full entry record (publisher, dependencies, member actions, intents, category) and its own Install button.

Clicking **Install** on a connector opens the connector-level install modal — same install-decision payload the CLI renders (FQN, publisher, fingerprint, trust-state badge, risk indicators, publisher footprint). A version input (required) and an optional expected-hash input let you pin the install. On confirm, the modal POSTs to `/v1/connectors/install` with the `confirmed_fingerprint` payload. Daemon-side behavior is identical to the CLI flow.

Clicking **Install** on an action or suite opens the composite install modal — the per-authority trust panel grouped exactly like the CLI's `Trust these publishers and continue?` prompt. The modal surfaces the CLI command (`aileron action add ...@latest` or `aileron action add-suite ...@latest`) for completing the install today; webapp-driven install execution for actions and suites is a follow-up (tracked in [#739](https://github.com/ALRubinger/aileron/issues/739)).

Neither modal exposes a one-click "Trust this publisher" button. Owner-level trust is a deliberate, explicit step rather than something the install modal invites you into. To trust a whole publisher, run `aileron keyring trust github://<owner>` from the CLI (see [Owner-level trust and revocation](#owner-level-trust-and-revocation) below); the modal keeps each install its own decision.

## Owner-level trust and revocation

Keyring v2 lets you trust a publisher once and cover every connector they ship. The keyring stores owner-level grants keyed by `<scheme>://<owner>` alongside the older per-repo grants keyed by `<scheme>://<owner>/<repo>`. An owner-level grant authorizes every connector under that owner.

```sh
# Trust a whole publisher. The key is fetched from the connector repo you name.
aileron keyring trust github://ALRubinger/aileron-connector-google

# Trust a whole publisher by owner alone. The key is resolved from the Hub entry.
aileron keyring trust github://ALRubinger

# Pin a single repo instead, the older per-repo grant.
aileron keyring trust github://ALRubinger/aileron-connector-slack
```

Trusting `github://ALRubinger/aileron-connector-google` writes an owner-level grant for `github://ALRubinger`, so a later install of `github://ALRubinger/aileron-connector-slack` verifies against the same publisher key without a second prompt. A bare owner argument resolves the key from the publisher's Hub catalog entry. When no Hub entry carries a usable key, the command errors and points you at the per-connector form.

`aileron keyring list` groups what you trust by owner. Revocation operates at the scope you name:

```sh
# Drop the owner-level grant for a publisher.
aileron keyring revoke github://ALRubinger

# Drop a single per-repo grant.
aileron keyring revoke github://ALRubinger/aileron-connector-slack

# Remove one key everywhere it appears, by fingerprint.
aileron keyring revoke --key sha256:i4l2kuD8q++d5b9v8/LLI1
```

The `--key` form retires a rotated or compromised key from every authority at once. Supplying both an authority and `--key`, or neither, is a usage error.

## What the Hub does not do

| What | Why not |
|---|---|
| Vet publishers | Anyone with a GitHub account can open a PR adding their connector to the Hub. Aileron does not review submissions for quality, security, or claims made in the description. The Hub is a discovery surface, not an editorial one. |
| Host binaries | Install artifacts live at `github://owner/repo`. The Hub entry is one YAML file pointing at the canonical FQN. If a publisher takes their repo down, the Hub entry breaks; Aileron stores nothing on the publisher's behalf. |
| Show popularity signals | No stars, no last-commit timestamps, no download counts in v0.x. Surfacing those would force every user's daemon to call `api.github.com` (60/hr unauthenticated ceiling) on every Hub page load. A server-side metadata service that amortizes those calls is tracked in [issue #614](https://github.com/ALRubinger/aileron/issues/614). |
| Phone home | No telemetry. The daemon does not report which connectors you browsed, which you installed, or which Hub entries you opened. The discovery surface and the install pipeline both run locally. |

## Common errors

- **`hub_unreachable`**: the daemon couldn't clone the Hub repo. Check that the daemon has network access to `github.com`. The same error wraps any non-404 git clone failure.
- **`not_found`** at install time: you supplied a `confirmed_fingerprint` (i.e. you went through the install-decision flow), but the FQN is not in the Hub. The daemon and the CLI are out of sync on what's installable, probably because the Hub PR for the entry hasn't merged yet. Workaround: install with pre-established trust via `aileron keyring trust <FQN>` followed by `aileron connector install <FQN>@<version>`, which uses the legacy flow.
- **`fingerprint_mismatch`**: between rendering the install-decision and confirming, the publisher's key at `key_url` changed. The daemon refuses to install rather than silently trusting a different key. Re-run the install command to see the new fingerprint, compare against the publisher's release notes, and decide.

## See also

- [Installing a Connector](/guides/installing-a-connector/): the install pipeline that runs after the install-decision prompt.
- [Publishing to the Hub](/guides/publishing-to-the-hub/): the publisher side of this surface.
- [ADR-0013: Connector Hub and Trust Distribution](/adr/0013-connector-hub-and-trust-distribution/): design rationale for the Hub.
- [ADR-0002: Connector Model](/adr/0002-connector-model/): the FQN authority and keyring trust model the Hub layers on top of.
