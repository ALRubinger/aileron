---
title: "Publishing a Connector"
description: "How to author, sign, and release a connector + action templates that Aileron users can install via aileron connector install / aileron action add."
---

This guide walks through publishing a connector and its action templates so Aileron users can install them via `aileron connector install <FQN>` and `aileron action add <FQN>`. It documents the conventions Aileron's install pipeline expects: where the binary lives, how it's signed, how the user trusts your publisher key, and how a single tag push produces every artifact in a release cohort.

The reference implementation is [`github.com/ALRubinger/aileron-connector-google`](https://github.com/ALRubinger/aileron-connector-google) — a per-service connector exposing twenty-one ops across Gmail, Calendar, Contacts, Drive, and Docs, with action templates living alongside. Snippets here mirror its layout; clone the repo when you want to see the full working setup.

## The one-repo-per-service model

Per [ADR-0002](/adr/0002-connector-model), connectors are sandboxed binaries with their own publication identity and lifecycle. **A connector lives in its own repository, separate from Aileron itself.** A security fix to the Google connector should not require an Aileron release; Aileron's tag space should not be polluted with connector tags.

Convention: one repo per external service, named `aileron-connector-<service>` (e.g. `aileron-connector-google`, `aileron-connector-slack`). One connector binary per repo, exposing all of that service's ops. Action templates that exercise the connector live in the same repo at `actions/<name>/`. This matches the Terraform-provider pattern: one provider per service, dozens of resources inside.

## Repository layout

```
aileron-connector-google/
├── connector/
│   ├── main.go              # //go:build wasip1 — WASM entry, host-import calls
│   ├── helpers.go           # untagged — host-testable pure helpers
│   ├── helpers_test.go
│   ├── go.mod
│   └── manifest.toml        # capability declarations + OAuth provider config
├── actions/
│   ├── list-recent-emails/action.md
│   ├── get-email/action.md
│   ├── list-upcoming-events/action.md
│   ├── draft-email/action.md
│   ├── send-email/action.md
│   └── create-calendar-event/action.md
├── keys/
│   ├── publisher.pub        # committed (PEM)
│   └── README.md            # how consumers extract the raw key for their keyring
├── Taskfile.yml             # build, pack, hash, test
├── .github/workflows/
│   ├── ci.yml
│   └── release.yml          # one tag push → connector + every action tarball
└── README.md
```

## Connector manifest

```toml
[connector]
name = "github://<owner>/aileron-connector-<service>"
version = "0.0.0-dev"     # CI substitutes the real version at release time
publisher = "Your Name"

[capabilities.network]
hosts = ["api.example.com:443"]

[capabilities.credential]
kind = "oauth2"
scope = "Read messages and create events"   # human-readable; surfaced in CLI prompts

[capabilities.runtime]
imports = ["log", "http_request", "http_response_size", "http_response_status", "http_response_read"]
```

`version = "0.0.0-dev"` is a deliberate placeholder. The release workflow substitutes the real version (extracted from the pushed tag) into a build copy of the manifest before signing — see [Releasing](#releasing) below. The committed source stays a template, so the publisher pushes a tag and never edits version fields by hand.

### OAuth2 connectors

A connector that needs OAuth2 access declares the OAuth provider config inside `[capabilities.credential.oauth2]`:

```toml
[capabilities.credential]
kind = "oauth2"
scope = "Read your Gmail messages and Calendar events; create drafts and calendar events"

[capabilities.credential.oauth2]
authorize_url = "https://accounts.google.com/o/oauth2/v2/auth"
token_url     = "https://oauth2.googleapis.com/token"
client_id     = "298373247853-i63n2n3ghapvbsjn3ocb2jr6u9vkcmih.apps.googleusercontent.com"
client_secret = "bound-at-release"
scopes = [
  "https://www.googleapis.com/auth/gmail.readonly",
  "https://www.googleapis.com/auth/gmail.compose",
  "https://www.googleapis.com/auth/calendar.readonly",
  "https://www.googleapis.com/auth/calendar.events",
]
```

**Trust model: publishers register their own OAuth apps.** Each connector publisher creates an OAuth app on the target service (Google Cloud Console, Slack API dashboard, Notion integrations panel, etc.) and pastes the resulting `client_id` into the manifest. The user's consent screen names the publisher — *your* app name appears, not Aileron's. This is the load-bearing trust property of ADR-0002: the OAuth client is whoever wrote the code that will use the access.

**`client_secret` is sometimes required, and is bound at release time.** Aileron's OAuth dance uses PKCE (S256) per [ADR-0006](/adr/0006-capability-binding-ux/), but several providers — Google's "Desktop app" OAuth client type is the canonical example — still require `client_secret` on token-exchange and refresh requests. This is a provider quirk, not a spec requirement; PKCE is doing the cryptographic work, but the API rejects the call without the value present.

For providers that demand it, the value ships in the connector binary the same way `gcloud` and `gh` ship their bundled secrets. **Never commit it to the repo** — GitHub's secret scanner forwards Google client secrets to Google, which auto-rotates them on detection. The committed source manifest carries `client_secret = "bound-at-release"` as a placeholder; the release workflow substitutes it from a repo secret (e.g. `GOOGLE_OAUTH_CLIENT_SECRET`) before signing and packing. The bound value lives only in the signed connector tarball.

For providers that *don't* require `client_secret` (most non-Google OAuth servers), omit the field. The manifest validator allows omission.

**Loopback redirect required.** When you register your OAuth app with the provider, register `http://localhost` (and/or `http://127.0.0.1`) as a valid redirect URI. The Aileron runtime allocates a free loopback port at bind time and serves the callback locally. Niche providers without loopback support are post-MVP.

**Validation.** Aileron's manifest validator requires every OAuth field at install time: `authorize_url`, `token_url`, `client_id`, and at least one entry in `scopes`. URLs must be `https://` (or `http://localhost` / `http://127.0.0.1` for tests).

#### Registering an OAuth app — Google quickstart

For reference, here's the rough Google flow (other providers are similar):

1. Visit [console.cloud.google.com](https://console.cloud.google.com), create a project named after your connector.
2. Enable the APIs your connector calls (Gmail API, Calendar API, etc.).
3. Configure the OAuth consent screen — application name, user-facing logo, scopes you'll request. This is what users see.
4. Create an **OAuth 2.0 Client ID** of type "Desktop app". Add `http://localhost` and `http://127.0.0.1` to the authorized redirect URIs.
5. Copy the resulting Client ID into your connector manifest's `[capabilities.credential.oauth2].client_id` field. Ignore the "client secret" — PKCE replaces it for installed-app flows.
6. Submit your consent screen for verification when you're ready to ship to users (Google requires verification for production OAuth apps requesting sensitive scopes).

#### Token storage and refresh

When the user runs `aileron binding setup <connector-FQN>` (or `aileron action add` triggers the auto-prompt), the runtime drives the OAuth dance, exchanges the code at your `token_url`, and stores the resulting `{access_token, refresh_token, expires_at}` envelope in the user's vault. Aileron-runtime refreshes the access token transparently when it nears expiry — your connector never sees expired tokens, never sees the refresh, and never holds the bytes. The host injects `Authorization: Bearer <fresh-token>` into your connector's outbound HTTP request before it leaves the sandbox.

Every field is enforced by the install pipeline:

- `name` must match the FQN used at install time (no spoofing).
- `version` is strict SemVer.
- `[capabilities.network].hosts` is a closed list of `host:port` pairs (no wildcards). The runtime denies any outbound request not on the list.
- `[capabilities.credential].kind` must equal the kind on the user's binding. v1 supports `api_key` and `oauth2`.

## WASM build

The connector is a `wasip1` Go program — see [Authoring a Connector](/guides/authoring-a-connector/) for the full host-import ABI and source structure. Build via Taskfile from the repo root:

```sh
task build
```

…or directly:

```sh
cd connector
GOOS=wasip1 GOARCH=wasm go build -trimpath -ldflags='-s -w' -o ../connector.wasm .
```

`-trimpath` and `-ldflags='-s -w'` keep the binary reproducible and small (the resulting `.wasm` ends up content-addressed in the user's store, so smaller is faster to fetch).

## Signing

Aileron verifies every install with an ed25519 signature over `binary || manifest`. Publishers generate one keypair per repo (or per publisher identity), keep the private key out of the repo, and commit only the public key.

### Generate the keypair (one-time)

```sh
# 1. Generate. Private key in /tmp briefly; public goes in the repo.
openssl genpkey -algorithm ed25519 -out /tmp/publisher.key
openssl pkey -in /tmp/publisher.key -pubout -out keys/publisher.pub

# 2. Encode the private key for the GitHub Actions secret.
#    macOS:
base64 -i /tmp/publisher.key | pbcopy
#    Linux:
# base64 < /tmp/publisher.key | xclip -selection clipboard

# 3. In GitHub: repo Settings → Secrets and variables → Actions →
#    New repository secret. Name: AILERON_SIGNING_KEY. Value: paste.

# 4. Save the PRIVATE key to a password manager, then delete the local copy:
rm /tmp/publisher.key

# 5. Commit the public key:
git add keys/publisher.pub
git commit -m "feat(keys): commit publisher signing public key"
git push
```

### Sign at release time

The CI release workflow does this for you (see [Releasing](#releasing)). For reference, the signing step is:

```sh
# Hash input is the canonical concatenation: binary || manifest.
cat connector.wasm connector/manifest.toml > /tmp/payload.bin

# Sign with the ed25519 private key (rawin = sign the bytes, not a hash of them).
openssl pkeyutl -sign -rawin -inkey /tmp/publisher.key \
  -in /tmp/payload.bin -out signature.sig

# Pack the flat archive Aileron's install pipeline expects.
mkdir -p /tmp/pack
cp connector.wasm           /tmp/pack/
cp connector/manifest.toml  /tmp/pack/
cp signature.sig            /tmp/pack/
tar czf aileron.tar.gz -C /tmp/pack \
  connector.wasm manifest.toml signature.sig
```

The tarball must be flat: exactly three files at the root (`connector.wasm`, `manifest.toml`, `signature.sig`), no `connector/` directory prefix. The Aileron install pipeline (`internal/cstore/tarball.go`) requires that exact shape.

## Releasing

**One tag, one workflow run, all artifacts.** The publisher pushes a `vX.Y.Z` tag; CI does the rest. There are no manifest edits before tagging — the source manifests are templates with placeholder versions and a placeholder connector hash, and CI substitutes the real values into build copies before signing and packing.

```sh
git tag vX.Y.Z
git push origin vX.Y.Z
# Wait ~2 minutes. Done — connector + every action tarball is published
# at the per-FQN tag the install pipeline expects.
```

The source manifests carry placeholders that CI binds at release time:

- `version = "0.0.0-dev"` in `connector/manifest.toml` and every `actions/*/action.md` (also in each action's `source` URL and `[[requires.connectors]]` block). CI replaces `0.0.0-dev` with the version from the pushed tag.
- `hash = "sha256:bound-at-release"` in every action manifest's `[[requires.connectors]]` block. CI replaces this with the connector tarball's real content hash, computed after the connector is built in the same workflow run.
- `client_secret = "bound-at-release"` in the connector manifest, for OAuth providers that require it. CI replaces this from a repository secret.

What CI does on each `vX.Y.Z` push (see [`release.yml`](https://github.com/ALRubinger/aileron-connector-google/blob/main/.github/workflows/release.yml) in the reference repo):

1. Substitutes `0.0.0-dev` with the tag's version across every manifest in the working tree.
2. Substitutes `client_secret = "bound-at-release"` with the repo secret value (OAuth providers that need it).
3. Builds `connector.wasm` (wasip1).
4. Computes `sha256(connector.wasm || manifest.toml)` — the canonical-hash input from ADR-0004.
5. Signs the connector payload, packs `aileron.tar.gz`, publishes at tag `vX.Y.Z`.
6. For each `actions/*/action.md`: substitutes `sha256:bound-at-release` with the real connector hash, signs the substituted manifest, packs `aileron.tar.gz`, publishes at tag `actions/<name>/vX.Y.Z`.

All artifacts in a version cohort share provenance — every per-action release is created at the same commit the connector tag points at, with all action tarballs pinned to the same connector content hash.

### Release tag conventions

Aileron's resolver per [ADR-0004](/adr/0004-dependency-resolution) maps FQNs to GitHub release URLs:

- **Connector** (one connector at the root of the repo): tag `v<version>`.
  - `github://ALRubinger/aileron-connector-google@0.0.1` → tag `v0.0.1`.
- **Action** (action templates under the connector repo): tag `actions/<name>/v<version>`.
  - `github://ALRubinger/aileron-connector-google/actions/list-recent-emails@0.0.1` → tag `actions/list-recent-emails/v0.0.1`.

The release asset must be named `aileron.tar.gz` for both connectors and actions.

Action tarballs contain `action.md` and `signature.sig` (signed over the substituted action.md bytes alone — the version and hash placeholders are replaced *before* signing).

### What you see on the releases page

Each `vX.Y.Z` push produces one connector release plus one per-action release, all from the same commit. The connector release (tagged `vX.Y.Z`) carries the **Latest** badge; the per-action releases (tagged `actions/<name>/vX.Y.Z`) are marked **Pre-release** so the page anchors visually on the connector. Per-action tags are how Aileron's resolver locates artifacts — they aren't subordinate releases despite their tag prefix; they're sibling artifacts in the same cohort, all pinned to the same connector content hash.

## How users trust your publisher

Aileron ships with **no trusted publishers by default** — the keyring is fail-closed (`internal/cstore/keyring_config.go`). Users opt in to your publisher by running:

```sh
aileron keyring trust github://<owner>/<repo>
```

The CLI fetches `keys/publisher.pub` from the default branch of your repo (the convention path ratified by [ADR-0002](/adr/0002-connector-model)), parses the key, and registers it under the authority `github://<owner>/<repo>`. **Committing `keys/publisher.pub` to your default branch is what makes that one-liner work** — the publisher key MUST be there, with that exact path, for users to trust you without manual ceremony.

The keyring is a list per authority (to support rotation — register the new key alongside the old, switch signing, drop the old). Re-running `aileron keyring trust <authority>` after the publisher commits a new `keys/publisher.pub` adds the new key alongside the existing one; an already-trusted key is detected and reported as a no-op.

Without an entry in the keyring for your authority, `aileron connector install` fails closed with `signature_failure` — unsigned or unverified binaries never reach disk.

## Action templates

An action template is a Markdown file with TOML frontmatter that references your connector by FQN+version+hash. The full guide is [Authoring an Action](/guides/authoring-an-action/); from a publishing perspective the key point is the **placeholder convention**:

```markdown
+++
name = "list-recent-emails"
version = "0.0.0-dev"
source = "github://ALRubinger/aileron-connector-google/actions/list-recent-emails@0.0.0-dev"

[[requires.connectors]]
name = "github://ALRubinger/aileron-connector-google"
version = "0.0.0-dev"
hash = "sha256:bound-at-release"
capabilities = ["list_recent_emails"]

[match]
intent = "list my recent Gmail messages"

[[execute]]
id = "list"
connector = "github://ALRubinger/aileron-connector-google"
op = "list_recent_emails"
idempotent = true

[[inputs]]
name = "query"
type = "string"
description = "Optional Gmail search query, e.g. \"is:unread\"."
required = false
+++

# List Recent Gmail Messages

Fetches a list of recent Gmail messages for the authenticated user.
```

`0.0.0-dev` and `sha256:bound-at-release` stay in the committed source. The release workflow substitutes both into a build copy before signing, so every action in a cohort automatically pins to the same connector hash with no per-release commits. The Markdown body is the LLM-facing function description — write tight prose, the LLM reads it to decide when to invoke.

## End-to-end install flow

Once published, a user runs:

```sh
# Trust the publisher (one-time per user — fetches keys/publisher.pub
# from the default branch of your repo).
aileron keyring trust github://ALRubinger/aileron-connector-google

# Install the connector at a specific tag
aileron connector install github://ALRubinger/aileron-connector-google@0.0.1

# Install one of its action templates
aileron action add github://ALRubinger/aileron-connector-google/actions/list-recent-emails@0.0.1

# Bind the credential — for OAuth2, drives the consent dance in the browser
aileron binding setup github://ALRubinger/aileron-connector-google

# Action is now available to the agent.
aileron launch claude
```

## Versioning

- Connector and action versions are independent. A new connector op is a connector minor version bump (`0.1.0` → `0.2.0`); a new action template is an action version bump but not a connector bump.
- Action templates pin a specific connector version + hash. Updating an action to use a newer connector requires republishing the action with the new pin.
- Pre-MVP convention: stay at `0.x.y` until your service surface is stable.

## Testing spawn-primitive connectors

Connectors that wrap a local CLI via the spawn primitive (per [ADR-0002](/adr/0002-connector-model) and [ADR-0014](/adr/0014-spawn-sandbox-technology)) need an analog of `httptest.Server` that records subprocess invocations without forking a real binary on CI runners. The runtime ships two helpers under `internal/sandbox/sandboxtest/`:

- **`sandboxtest.RecordingExecutor`**. Implements `sandbox.SpawnExecutor`. Every `aileron_host.spawn` call goes through it; tests inspect the recorded envelopes the way they would on an `httptest.Server`'s recorded requests. Substitute via `sandbox.WithSpawnExecutor`.
- **`sandboxtest.FakeBinary`**. Writes a scripted shell stub to a tempdir. The stub emits configured stdout, stderr, and exit code, and optionally records its argv, env, and cwd to a JSON file. Use this when the test should exercise the real `os/exec` path through the platform sandbox.

Sketch of a connector-side spawn test:

```go
import (
    "github.com/ALRubinger/aileron/internal/sandbox"
    "github.com/ALRubinger/aileron/internal/sandbox/sandboxtest"
)

func TestSpawnConnector_LogSubcommandHonorsArgvPattern(t *testing.T) {
    rec := &sandboxtest.RecordingExecutor{
        Result: sandbox.SpawnResult{ExitCode: 0, Stdout: []byte("commit-list\n")},
    }
    rt, _ := sandbox.NewWazeroRuntime(ctx, sandbox.WithSpawnExecutor(rec))
    // ... compile your connector, invoke its `log` op ...

    if got := rec.LastCall().Argv[1]; got != "log" {
        t.Errorf("argv[1] = %q, want log", got)
    }
}
```

Connector repos publish their own integration tests against real binaries; this in-process harness covers the gate, the host-function plumbing, and the manifest-shape contracts without paying the per-test process-fork cost.

## See also

- [ADR-0002: Connector Model](/adr/0002-connector-model)
- [ADR-0003: Action Model](/adr/0003-action-model)
- [ADR-0004: Dependency Resolution](/adr/0004-dependency-resolution)
- [ADR-0006: Capability Binding UX](/adr/0006-capability-binding-ux)
- [ADR-0007: Install Consent](/adr/0007-install-consent)
- [ADR-0014: Spawn Sandbox Technology](/adr/0014-spawn-sandbox-technology)
