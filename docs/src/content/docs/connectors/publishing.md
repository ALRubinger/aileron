---
title: "Publishing a connector"
description: "How to author, sign, and release a connector + action templates that Aileron users can install via aileron connector install / aileron action add."
---

This guide walks through publishing a connector and its action templates so Aileron users can install them via `aileron connector install <FQN>` and `aileron action add <FQN>`. It documents the conventions Aileron's install pipeline expects: where the binary lives, how it's signed, how the user trusts your publisher key.

The reference implementation is `github.com/ALRubinger/aileron-connector-github` — a per-service connector exposing GitHub ops with action templates living alongside.

## The two-repo model

Per [ADR-0002](/adr/0002-connector-model), connectors are sandboxed binaries with their own publication identity and lifecycle. **A connector lives in its own repository, separate from Aileron itself.** A security fix to the GitHub connector should not require an Aileron release; Aileron's tag space should not be polluted with connector tags.

Convention: one repo per external service, named `aileron-connector-<service>` (e.g. `aileron-connector-github`, `aileron-connector-slack`). One connector binary per repo, exposing all of that service's ops. Action templates that exercise the connector live in the same repo at `actions/<name>/`. This matches the Terraform-provider pattern: one provider per service, dozens of resources inside.

## Repository layout

```
aileron-connector-github/
├── connector/
│   ├── main.go              # wasip1 Go source
│   ├── manifest.toml        # capability declarations
│   └── README.md
├── actions/
│   ├── list-recent-prs/
│   │   └── action.md
│   └── file-bug/
│       └── action.md
├── Taskfile.yml             # build, sign, package
├── keys/                    # public keys committed; private keys NEVER committed
│   └── publisher.pub
└── README.md
```

## Connector manifest

```toml
[connector]
name = "github://<owner>/aileron-connector-github"
version = "0.1.0"
publisher = "Your Name"

[capabilities.network]
hosts = ["api.github.com:443"]

[capabilities.credential]
kind = "api_key"
scope = "repo:read"

[capabilities.runtime]
imports = ["log", "http_request", "http_response_size", "http_response_status", "http_response_read"]
```

### OAuth2 connectors

A connector that needs OAuth2 access declares the OAuth provider config inside `[capabilities.credential.oauth2]`:

```toml
[capabilities.credential]
kind = "oauth2"
scope = "Read your email and send messages"   # human-readable; surfaced in CLI prompts

[capabilities.credential.oauth2]
authorize_url = "https://accounts.google.com/o/oauth2/v2/auth"
token_url     = "https://oauth2.googleapis.com/token"
client_id     = "1234567890-xxxxxxx.apps.googleusercontent.com"
scopes = [
  "https://www.googleapis.com/auth/gmail.send",
  "https://www.googleapis.com/auth/calendar.readonly",
]
```

**Trust model: publishers register their own OAuth apps.** Each connector publisher creates an OAuth app on the target service (Google Cloud Console, Slack API dashboard, Notion integrations panel, etc.) and pastes the resulting `client_id` into the manifest. The user's consent screen names the publisher — *your* app name appears, not Aileron's. This is the load-bearing trust property of ADR-0002: the OAuth client is whoever wrote the code that will use the access.

**No `client_secret` field.** v1 requires PKCE (S256) for installed-app flows. The "secret" most providers list for installed apps isn't actually secret per their own guidance — PKCE is the cryptographic protection. The manifest carries no secrets.

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
- `[capabilities.credential].kind` must equal the kind on the user's binding (`api_key` for v1; `oauth2` lands with [#388](https://github.com/ALRubinger/aileron/issues/388)).

## WASM build

The connector is a `wasip1` Go program that imports four host functions:

```go
//go:wasmimport aileron_host log
func hostLog(...)

//go:wasmimport aileron_host http_request
func hostHTTPRequest(...) int32

//go:wasmimport aileron_host http_response_size
func hostHTTPResponseSize() int32

//go:wasmimport aileron_host http_response_status
func hostHTTPResponseStatus() int32

//go:wasmimport aileron_host http_response_read
func hostHTTPResponseRead(...) int32
```

The `internal/sandbox/testdata/echo/main.go` fixture in the Aileron repo is the canonical reference for the I/O envelope shape (JSON in on stdin, JSON out on stdout).

Build with:

```sh
cd connector
GOOS=wasip1 GOARCH=wasm go build -o ../build/connector.wasm .
```

## Signing

Aileron verifies every install with an ed25519 signature over `binary || manifest`. Publishers generate one keypair per repo (or per publisher identity), keep the private key out of the repo, and commit only the public key.

Generate the keypair once:

```sh
go run github.com/ALRubinger/aileron/connectors/sign/keygen \
  > keys/publisher.priv  # KEEP THIS OUT OF THE REPO
git add keys/publisher.pub
```

Sign the tarball at release time:

```sh
go run github.com/ALRubinger/aileron/connectors/sign \
  --priv keys/publisher.priv \
  --binary build/connector.wasm \
  --manifest connector/manifest.toml \
  --out build/signature.sig
```

Build the tarball:

```sh
tar czf build/aileron.tar.gz \
  -C build connector.wasm signature.sig \
  -C ../connector manifest.toml
```

## Release tag conventions

Aileron's resolver per [ADR-0004](/adr/0004-dependency-resolution) maps FQNs to GitHub release URLs:

- **Single-artifact repo** (one connector at the root): tag `v<version>`.
  - `github://acme/aileron-connector-foo@0.1.0` → tag `v0.1.0`.
- **Monorepo subpath** (action templates living under the connector repo): tag `<subpath>/v<version>`.
  - `github://acme/aileron-connector-foo/actions/list-prs@0.1.0` → tag `actions/list-prs/v0.1.0`.

The release asset must be named `aileron.tar.gz` for both connectors and actions.

Action tarballs contain `action.md` and an optional `signature.sig` (signed over the action.md bytes alone). v1 keeps action signing optional; mandatory signing lands with the install consent flow ([#363](https://github.com/ALRubinger/aileron/issues/363)).

## How users trust your publisher

Aileron ships with **no trusted publishers by default** — the keyring is fail-closed. Users opt in to your publisher by adding the public key to `~/.aileron/keyring.json`:

```json
{
  "version": 1,
  "publishers": {
    "github://acme/aileron-connector-foo": [
      "BASE64_ENCODED_ED25519_PUBLIC_KEY"
    ]
  }
}
```

The authority key is the FQN base (`<scheme>://<owner>/<repo>`); the value is a list of public keys (a list to support key rotation — register the new key alongside the old, switch signing, drop the old).

Document the registration step in your connector's README so users know what to copy into their keyring.

## Action templates

An action template is a Markdown file with TOML frontmatter that references your connector by FQN+version+hash:

```markdown
+++
name = "list-recent-prs"
version = "0.1.0"
source = "github://acme/aileron-connector-foo/actions/list-recent-prs@0.1.0"

[[requires.connectors]]
name = "github://acme/aileron-connector-foo"
version = "0.1.0"
hash = "sha256:abc123..."          # paste the connector's canonical hash
capabilities = ["list_prs"]        # subset the action exercises

[match]
intent = "list recent merged PRs"

[[inputs]]
name = "owner"
type = "string"
description = "Repository owner."

[[inputs]]
name = "repo"
type = "string"
description = "Repository name."

[[execute]]
id = "list"
connector = "github://acme/aileron-connector-foo"
op = "list_prs"

[execute.inputs]
owner = "${args.owner}"
repo = "${args.repo}"
+++

# List recent PRs

Lists recent merged PRs in a GitHub repository.
```

The Markdown body is the LLM-facing function description. Write tight prose — the LLM reads it to decide when to invoke the action.

## End-to-end install flow

Once published, a user runs:

```sh
# Trust the publisher (one-time)
echo '{"version":1,"publishers":{"github://acme/aileron-connector-foo":["BASE64..."]}}' \
  > ~/.aileron/keyring.json

# Install the connector
aileron connector install github://acme/aileron-connector-foo@0.1.0

# Install the action template
aileron action add github://acme/aileron-connector-foo/actions/list-recent-prs@0.1.0

# Bind the credential
aileron binding setup github://acme/aileron-connector-foo
# (CLI prompts for the api_key value)

# Action is now available to the agent.
```

## Versioning

- Connector and action versions are independent. A new connector op is a connector minor version bump (`0.1.0` → `0.2.0`); a new action template is an action version bump but not a connector bump.
- Action templates pin a specific connector version + hash. Updating an action to use a newer connector requires republishing the action with the new pin.
- Pre-MVP convention: stay at `0.x.y` until your service surface is stable.

## See also

- [ADR-0002: Connector Model](/adr/0002-connector-model)
- [ADR-0003: Action Model](/adr/0003-action-model)
- [ADR-0004: Dependency Resolution](/adr/0004-dependency-resolution)
- [ADR-0006: Capability Binding UX](/adr/0006-capability-binding-ux)
- [ADR-0007: Install Consent](/adr/0007-install-consent)
