---
title: "Publishing to the Hub"
description: "Submit your connector to the Aileron Connector Hub so users can discover it via aileron hub search and the webapp Hub page. Covers the YAML entry shape, the schema, the PR workflow, and what changes after merge."
---

This guide is for publishers who have already shipped a connector and want users to discover it through the Aileron Connector Hub. The Hub is a public GitHub repo, [`aileron-connectors-hub`](https://github.com/ALRubinger/aileron-connectors-hub), that maintains one YAML entry per community-published connector. Adding your entry makes you findable through `aileron hub search`, the webapp's `/hub` page, and the install-decision prompt that fires on `aileron connector install`.

This is a discovery decision, not a release decision. Releasing a new version of your connector is unchanged: you push a tag, CI publishes the tarball at `github://<owner>/<repo>`. The Hub entry is one YAML file pointing at that FQN. Versions are not part of the entry.

If you have not shipped your connector yet, start with [Publishing a Connector](/guides/publishing-a-connector/). That guide covers the binary, the signing key, the release workflow, and the trust contract Aileron's install pipeline expects. The Hub builds on top of that. Without a signed release at a `github://` FQN, there is nothing to discover.

## When to submit a Hub entry

The Hub is optional. Connectors install fine without an entry. The question is whether you want users to find your connector through Aileron's discovery surface.

| Situation | Submit? |
|---|---|
| You want users to discover your connector through `aileron hub search` or the webapp `/hub` page | Yes |
| The connector is private, internal to your team, or you distribute the FQN out-of-band | No |
| You're shipping a one-off CLI wrapper for personal use | No |
| You want the install-decision trust prompt to fire (yellow banner, fingerprint comparison, key persists on confirm) when users `aileron connector install` your FQN | Yes |

Inclusion in the Hub is not an endorsement. Aileron does not vet what you publish. Maintainers of the Hub repo confirm that the entry's schema is valid and that it points at a real `github://` FQN. They do not audit your binary or your `keys/publisher.pub`. That contract is between you and the consumers who confirm your fingerprint at install time.

## The entry shape

Each entry is one YAML file under `connectors/`. The recommended naming convention is `<scheme>_<owner>_<repo>.yaml`, lowercased, dots replaced with dashes, to keep the directory listing scannable.

A complete entry:

```yaml
fqn: github://ALRubinger/aileron-connector-google
description: Google Workspace connector (Calendar, Drive, Gmail)
publisher_github: ALRubinger
key_url: https://raw.githubusercontent.com/ALRubinger/aileron-connector-google/main/keys/publisher.pub
release_pattern: v*
```

All five fields are required. The JSON Schema at [`schema/connector-entry.schema.json`](https://github.com/ALRubinger/aileron-connectors-hub/blob/main/schema/connector-entry.schema.json) is the authoritative spec. The Hub repo's CI validates every PR against it.

### `fqn`

The canonical FQN for your connector. Must match `^github://[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?/[A-Za-z0-9._-]+$`. In v0.x this is always a GitHub URL of the form `github://<owner>/<repo>`. No subpaths, no scheme other than `github`. Aileron's resolver maps this FQN to release artifacts at `https://github.com/<owner>/<repo>/releases`.

### `description`

A one-line summary, 1 to 200 characters. Shown next to the FQN in `aileron hub list` and in the webapp's `/hub` cards. Write it for the user searching for "calendar" or "slack", not for the LLM. Action-template Markdown bodies carry the LLM-facing description.

### `publisher_github`

Your GitHub username or organization, exactly as it appears on github.com. Must match `^[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?$`. This is used to compute the install-decision flow's publisher footprint: when a user installs your connector, the daemon scans the Hub for other entries with the same `publisher_github` and surfaces them as context.

Two publishers can ship a connector at the same name (`github://alice/aileron-connector-slack` and `github://bob/aileron-connector-slack` coexist in the Hub). The FQN owner disambiguates. There is no namespace collision.

### `key_url`

Direct URL to the PEM-encoded ed25519 public key that signs your releases. Must start with `https://`. The conventional location, ratified by [ADR-0002](/adr/0002-connector-model/), is `keys/publisher.pub` on the default branch of your connector repo:

```
https://raw.githubusercontent.com/<owner>/<repo>/main/keys/publisher.pub
```

Use `main` (or your repo's actual default branch) rather than a tag. The daemon fetches this URL during the install-decision flow to compute the fingerprint the user sees. Pinning to a tag would mean that updating your entry to point at a new key requires a new Hub PR even when the key is committed in your repo.

The Hub does not host your key. It records the URL where your key lives, and your repo is the authoritative source.

### `release_pattern`

A glob describing your release tag convention. Must be at least one character. v0.x supports a small set of patterns:

- `v*`: versioned tags like `v1.2.3` (the convention this guide and Aileron's reference connectors use).
- `release-*`: tags like `release-2025.01`.
- `*`: any tag. Rare. Useful for connectors that don't use semantic versioning.

The pattern is informational in v0.x. The install pipeline locates artifacts via the [ADR-0004](/adr/0004-dependency-resolution/) resolver, not via the Hub entry. Future work, [issue #614](https://github.com/ALRubinger/aileron/issues/614), may use `release_pattern` to drive a server-side "latest stable version" surface.

## The PR workflow

The Hub is GitHub-native. There is no upload form, no review committee, no Aileron review.

1. Fork [`aileron-connectors-hub`](https://github.com/ALRubinger/aileron-connectors-hub).
2. Add one YAML file under `connectors/`. Filename does not have to match `fqn` (the daemon reads `fqn` from the file contents), but follow the `<scheme>_<owner>_<repo>.yaml` convention.
3. Open a PR. The Hub repo's CI runs JSON Schema validation against `schema/connector-entry.schema.json` (see [PR #1](https://github.com/ALRubinger/aileron-connectors-hub/pull/1)). A green check means the YAML is well-formed, the required fields are present, and the patterns hold.
4. A maintainer of the Hub repo merges. The merge does not require Aileron-org membership. See [`CONTRIBUTING.md`](https://github.com/ALRubinger/aileron-connectors-hub/blob/main/CONTRIBUTING.md) for the maintainer rotation and review SLA.

The maintainer review is light: that the FQN resolves to a real GitHub repo, that `key_url` returns a parseable PEM ed25519 public key, that the entry is not a duplicate of an existing one. Maintainers do not run your binary, audit your code, or grade your description.

A typical PR is one file added under `connectors/`. Touching `schema/`, `CONTRIBUTING.md`, or the workflow files needs a separate PR with a different review bar.

## What changes after merge

Once your PR merges to `main`, the Hub entry is live immediately. The daemon shallow-clones the Hub repo per query, so the next time any user runs `aileron hub list` or visits `/hub` in the webapp, your entry shows up. There is no cache to invalidate.

Three things start happening:

- **Discovery.** Users searching for keywords in your description find your connector. `aileron hub search calendar` matches every entry whose FQN or description contains "calendar" case-insensitively.
- **Install-decision prompt.** When a user runs `aileron connector install <your-FQN>`, the CLI fetches the install-decision payload before the install pipeline runs. They see your fingerprint, the trust state (almost always `unknown (first install)` for a new publisher), and a yellow risk indicator noting it's their first connector from you. On confirm, the daemon writes your key to their keyring under your FQN and runs the install. See [Discovering Connectors](/guides/discovering-connectors/) for the user-side flow.
- **Publisher footprint.** If you list a second connector in the Hub later, users installing either will see the other as informational context: "Publisher has 1 other connector you already trust" or similar.

The webapp `/hub` page shows your entry under the same discovery surface, with a one-click Install button that opens the install-decision modal.

## Updating your entry

Edit your entry by opening a PR that modifies the YAML file. The schema validator runs on every PR. The same maintainer review process applies.

| You want to | Steps |
|---|---|
| Change your description | One-line edit to `description`. Schema validates length 1 to 200. |
| Change your key URL (e.g. branch rename from `main` to `default`) | Edit `key_url`. Make sure the new URL returns the same PEM bytes; otherwise the install-decision flow's fingerprint surface will pick up the change. |
| Rotate your signing key | Commit the new `keys/publisher.pub` to your repo first. The `key_url` already points at the default branch, so no Hub edit is needed. Users who already trust the old key see `conflict` on their next install of the same FQN, with the new fingerprint shown for comparison. |
| Move your repo to a new owner | The FQN changes. Open a Hub PR adding a new entry at the new FQN. Keep the old entry in place for a release cycle so users on the old FQN keep finding the connector. Then open a second PR removing the old entry. |
| Withdraw your connector | Open a PR deleting the YAML file. The discovery surface drops it on next daemon clone. Already-installed connectors keep working. Their trust state in users' keyrings persists. |

There is no "version" of a Hub entry. The Hub does not track release-by-release; that's the job of your repo's tags and the install pipeline.

## What the Hub does not require

| Not required | Why |
|---|---|
| Aileron-org GitHub membership | Anyone can open a PR. Maintainership of the Hub is intentionally lighter-touch than maintainership of the Aileron daemon repo. |
| Sigstore signing or transparency log entries | Long-lived static keys remain in v0.x. The three known limitations (leaked keys retroactively compromise past versions, no transparency log means undetected backdated signatures, weak identity binding) are accepted per [ADR-0013](/adr/0013-connector-hub-and-trust-distribution/). Revisit when v1 stability work begins. |
| A "verified publisher" badge | The Hub does not have publisher tiers. Every entry is equal, every user makes their own trust decision at install time. |
| OAuth provider registration | The Hub entry is about discovery. If your connector uses OAuth2, that's declared in your connector's `manifest.toml` per [Publishing a Connector](/guides/publishing-a-connector/), not in the Hub entry. |

## See also

- [Publishing a Connector](/guides/publishing-a-connector/): the connector lifecycle the Hub entry layers on top of: signing keys, release workflow, capability manifests.
- [Discovering Connectors](/guides/discovering-connectors/): the user side of this surface.
- [ADR-0013: Connector Hub and Trust Distribution](/adr/0013-connector-hub-and-trust-distribution/): design rationale for the Hub, the Hub-as-pointer (vs. Hub-as-registry) decision, and what was deferred.
- [ADR-0002: Connector Model](/adr/0002-connector-model/): the FQN authority + keyring trust model.
- [`aileron-connectors-hub` on GitHub](https://github.com/ALRubinger/aileron-connectors-hub): the repo itself.
