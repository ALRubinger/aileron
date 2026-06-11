---
title: "Sandbox Agent Auth"
description: "Vault-backed credential injection for `aileron launch <agent> --sandbox=docker`. Covers the vault path scheme, per-agent envelope schemas, the in-container login flow, manual seeding, and recovery."
order: 10
---

`aileron launch <agent> --sandbox=docker` runs the agent inside an ephemeral container. The vault is durable. The launcher's `AuthSpec` lifecycle ([ADR-0025](/adr/0025-vault-backed-agent-auth)) is the bridge: it renders vault entries into the container at start and snapshots in-container rotations back on clean exit.

This page is for operators and contributors. It documents the per-agent envelope schemas, the seeding paths, and the recovery commands.

## Vault path scheme

Per-agent credentials live under the namespace `agents/<name>/<purpose>`:

| Path                  | Owner | Envelope schema                                                                                |
|-----------------------|-------|------------------------------------------------------------------------------------------------|
| `agents/claude/oauth` | Claude Code  | `{"claudeAiOauth":{"accessToken":...,"refreshToken":...,"expiresAt":...,"scopes":[...]}}` |
| `agents/codex/oauth`  | OpenAI Codex | `{"auth_mode":"chatgpt","tokens":{"access_token":...,"refresh_token":...,"id_token":...,"account_id":...},"last_refresh":"..."}` |

Other agents (Goose, OpenCode, Pi) ship a zero-value `AuthSpec{}` in v1: they continue to prompt for login on every sandbox launch. Per-agent specs for those three are tracked as follow-up issues.

The daemon's HTTP surface scopes the path at the routing layer. `GET/PUT /v1/vault/agents/{name}/credentials` translates `{name}` internally to `agents/<name>/oauth`. Other vault paths are unreachable through this endpoint.

## How a launch resolves credentials

The launcher's sandbox path performs four steps around `sandboxcontainer.Builder.Run`:

1. **Render.** The launcher GETs each binding's vault entry, runs the binding's Render function, and writes the result into a chmod-0700 transient directory on the host.
2. **Bind-mount.** The transient directory is mounted into the container at the binding's parent path (default) or as an individual file at the binding's `ContainerPath` (when `MountAsFile = true`, which Codex uses for `auth.json`). Static files like Claude's onboarding stub land in the same transient directory.
3. **Run.** The container starts. The in-container agent reads its credentials from the documented paths and starts silently.
4. **Capture.** On a clean exit (`Builder.Run` returns nil), the launcher reads each FileBinding's host-side file, runs the binding's Capture function (which validates the envelope), and PUTs the result back to the vault. Forcible termination (SIGKILL, runtime crash) skips Capture; the prior vault entry is retained.

Codex's binding also declares a `PreLaunchRefresh` hook that runs between the GET and Render. The hook exchanges the refresh token for a new access token against `auth.openai.com`, persists the rotated bundle through the daemon, and hands Render the new Secret. A failed persist aborts the launch; the rotated bundle must be in vault before container start.

## First launch: in-container login seeds the vault

When the vault has no entry for an agent, the launcher prints

```
[launcher] no credentials in vault for claude; agent will prompt for login
```

then starts the container with the bind-mount empty. The in-container agent performs its normal interactive login:

- **Claude Code:** paste-the-code OAuth fallback in the terminal.
- **Codex CLI:** device-auth flow against `auth.openai.com`.

When the container exits cleanly, Capture reads the file the agent wrote and PUTs the bytes to the vault. Every later launch renders silently.

## Manual seeding

Operators with an exported credential envelope can populate the vault directly:

```sh
aileron vault put agents/claude/oauth --from-file ~/.claude/.credentials.json
aileron vault put agents/codex/oauth  --from-file ~/.codex/auth.json
```

The bytes must match the envelope schema in the table above. Render validates on the way in; a malformed envelope fails the launch with a clear error before the container starts.

The `aileron auth <agent> --import-from-host` subcommand is intentionally not in v1. The host-import surface (Linux file read, macOS Keychain shell-out, Windows file read) is deferred to a follow-up. The in-container login path covers bootstrap.

## Recovery

To re-login from scratch (vault entry stale, refresh token revoked, or just starting over):

```sh
aileron vault delete agents/claude/oauth
aileron launch claude --sandbox=docker
```

The next launch starts with an empty bind-mount, the in-container agent prompts for login, and Capture seeds the vault again on exit.

If the Codex pre-launch refresh fails because the refresh token was revoked upstream, the launcher exits with a message that ends in this exact recovery command.

## Concurrency and freshness

v1 uses last-writer-wins for the Capture-side PUT. Two simultaneous `aileron launch codex --sandbox=docker` invocations against the same agent can race; refresh tokens survive the race because both writers exchanged the same upstream token. Concurrent launches against the same agent are not in the v1 ICP. A freshness-comparison hook on FileBinding is a clean follow-up if the concern surfaces.

Capture stays non-fatal. A vault-write failure or schema-validation failure surfaces as a one-line stderr warning that names the file path and the recovery option, and skips that binding's PUT. The session completes normally; the prior vault entry is retained.

## Adding a per-agent spec

The Agent SPI carries the spec as a static method:

```go
func (c Claude) AuthSpec() launch.AuthSpec {
    return launch.AuthSpec{
        FileBindings: []launch.FileBinding{{
            VaultPath:     "agents/claude/oauth",
            ContainerPath: "/home/agent/.claude/.credentials.json",
            Mode:          0o600,
            Required:      false,
            Render:        claudeRender,
            Capture:       claudeCapture,
        }},
        StaticFiles: []launch.StaticFile{{
            ContainerPath: "/home/agent/.claude.json",
            Mode:          0o644,
            Content:       claudeOnboardingStub,
        }},
    }
}
```

The descriptor types live in `internal/launch/authspec.go`. The per-agent implementations live alongside the agent in `internal/launch/agents/`. The launcher consumes the spec at runtime through `prepareAuthSpec` in `internal/launch/authspec_runtime.go`.

Set `MountAsFile = true` on a FileBinding when the agent does not rotate the credential in-container and the binding's parent directory must coexist with a mount installed by `ConfigureMCP` (Codex uses this for `auth.json` so the read-only `config.toml` mount stays unmasked).

Set `Required = false` (the typical case) so an empty vault triggers the in-container-login bootstrap path. Set `Required = true` only when the binding has no in-container login path and the launch should hard-fail on an empty vault.

## See also

- [ADR-0025: Vault-backed Agent Authentication Injection](/adr/0025-vault-backed-agent-auth)
- [ADR-0024: Sandbox MCP Parity (Path B1)](/adr/0024-sandbox-mcp-parity)
- [Adding an Agent](/development/adding-an-agent/)
- [Sandbox Composition](/development/sandbox-composition/)
