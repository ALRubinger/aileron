---
title: "Discovering Connectors"
description: "Browse and install community-published connectors via the Aileron Connector Hub. Covers aileron hub search/show, the webapp Hub page, and the install-decision trust prompt."
---

This guide is for users who want to find and install community-published connectors. The Aileron Connector Hub is a public discovery index: a list of connectors that publishers have opted into making findable. Discovery is decoupled from trust. Appearing in the Hub is not an Aileron endorsement. You still confirm the publisher's signing key fingerprint before installing.

The Hub itself is the GitHub repo [`aileron-connectors-hub`](https://github.com/ALRubinger/aileron-connectors-hub). Each entry is one YAML file pointing at a connector's canonical `github://owner/repo` FQN. The Hub holds no binaries. Install artifacts still come from the publisher's source repository.

For the install pipeline that runs under all this, see [Installing a Connector](/guides/installing-a-connector/). The Hub layers discovery and a per-FQN trust prompt on top of that pipeline.

## Quick tour

```sh
# List every connector in the Hub.
aileron hub list

# Search by keyword (matches FQN and description).
aileron hub search calendar

# Show a single entry's metadata.
aileron hub show github://ALRubinger/aileron-connector-google
```

The same data is browseable in the webapp: under `aileron launch`, visit `/hub` for a searchable list with one-click install.

## How the daemon talks to the Hub

The daemon shallow-clones `aileron-connectors-hub` on every query and parses the entries on the fly. There is no persisted cache and no GitHub API call (per [ADR-0013](/adr/0013-connector-hub-and-trust-distribution/) and the resolution of issue #486). Each `aileron hub` invocation pays one shallow git clone of a small repo. The CLI is the same shape as any other Aileron subcommand: it talks to the local daemon over `/v1/hub/*`, the daemon does the work.

The webapp's `/hub` page reads the same endpoints. CLI and webapp render the same data in different shapes.

## `aileron hub` CLI

### `list`

Prints every Hub entry as a fixed-width table:

```
FQN                                                 PUBLISHER             DESCRIPTION
github://ALRubinger/aileron-connector-google        ALRubinger            Google Workspace connector (Calendar, Drive, Gmail)
github://alice/aileron-connector-slack              alice                 Slack messaging connector
```

Pass `--json` for NDJSON output (one JSON-encoded entry per line) when scripting.

### `search <query>`

Case-insensitive substring match on FQN and description. Server-side filter, so the daemon handles the matching rules.

```sh
aileron hub search slack
```

### `show <fqn>`

Prints the full record for one entry:

```
FQN:             github://ALRubinger/aileron-connector-google
Description:     Google Workspace connector (Calendar, Drive, Gmail)
Publisher:       ALRubinger
Key URL:         https://raw.githubusercontent.com/ALRubinger/aileron-connector-google/main/keys/publisher.pub
Release pattern: v*
```

The `Key URL` is where the daemon will fetch the publisher's signing key during install. The `Release pattern` is a glob the publisher commits to (e.g. `v*` means versioned releases at tags starting with `v`).

## The install-decision prompt

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
| `already trusted` | green | The publisher's current key is already on your keyring at this FQN. The install proceeds without trust-write work. |
| `unknown (first install)` | yellow | The keyring has no trusted key for this FQN. Confirming will register the publisher's current key. |
| `conflict — key differs from a trusted sibling repo` | red | A different connector by the same publisher (per `publisher_github`) carries a key you trust, and the key at this FQN's `key_url` doesn't match. Possible explanations are key rotation, MITM, or impersonation. Worth a closer look before confirming. |

The `conflict` state is rare and intentional. v0.x trust is strictly per-repo (per FQN), so one publisher with two connectors can carry two different keys without anything being wrong. The conflict surface flags the case so you notice. Run `aileron keyring list` to see what's currently trusted, compare fingerprints with the publisher's release notes or commits to `keys/publisher.pub` in their repo, then make a call.

## The webapp's Hub page

Under `aileron launch`, the daemon serves a static webapp at `http://127.0.0.1:<port>/`. The `/hub` page renders the same entries the CLI lists, with a debounced search box and a per-card **Install** button.

Clicking **Install** opens a modal showing the same install-decision payload the CLI renders: FQN, publisher, fingerprint, trust-state badge, risk indicators, the publisher's other Hub entries. A version input (required) and an optional expected-hash input let you pin the install. On confirm, the modal POSTs to the same `/v1/connectors/install` endpoint the CLI uses, with the same `confirmed_fingerprint` payload. Daemon-side behavior is identical to the CLI flow.

The modal renders the publisher footprint as informational context. There is no "Trust this publisher" button. v0.x trust is per-repo, and the design is deliberately careful not to invite users into a wider trust grant.

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
