---
title: "Sandbox Agent Auth"
description: "Vault-backed credential injection for `aileron launch <agent> --sandbox=docker`. Covers the vault path scheme, per-agent envelope schemas, the in-container login flow, manual seeding, and recovery."
order: 10
---

`aileron launch <agent> --sandbox=docker` runs the agent inside an ephemeral container. The vault is durable. The launcher's `AuthSpec` lifecycle ([ADR-0025](/adr/0025-vault-backed-agent-auth)) is the bridge: it renders vault entries into the container at start and snapshots in-container rotations back on clean exit.

This page is for operators and contributors. It documents the per-agent envelope schemas, the seeding paths, and the recovery commands.

## Host launch is not vault-backed in v1

Vault-backed credential injection applies only to `aileron launch <agent> --sandbox=docker` (and `--sandbox=podman`). A plain `aileron launch <agent>` host launch is not vault-backed in v1. Host launch lets the agent find its own credentials in the host user's home directory (`~/.claude/`, `~/.codex/`). `prepareAuthSpec` does not run on the host path, so the `AuthSpec` lifecycle is sandbox-only. Host-side vault-backed auth is a deliberately deferred follow-up that needs its own PR and explicit user testing. See [ADR-0025](/adr/0025-vault-backed-agent-auth) for the decision record.

## Vault path scheme

Per-agent credentials live under the namespace `agents/<name>/<purpose>`:

| Path                    | Owner | Binding | Envelope / secret shape                                                                                |
|-------------------------|-------|---------|--------------------------------------------------------------------------------------------------------|
| `agents/claude/oauth`   | Claude Code  | FileBinding | `{"claudeAiOauth":{"accessToken":...,"refreshToken":...,"expiresAt":...,"scopes":[...]}}` |
| `agents/codex/oauth`    | OpenAI Codex | FileBinding | `{"auth_mode":"chatgpt","tokens":{"access_token":...,"refresh_token":...,"id_token":...,"account_id":...},"last_refresh":"..."}` |
| `agents/goose/oauth`    | Goose        | EnvBinding  | Raw provider API key (opaque bytes), rendered into `ANTHROPIC_API_KEY` |
| `agents/opencode/oauth` | OpenCode     | FileBinding | `{"<provider>":{...credential...},...}` — `auth.json` keyed by provider name |
| `agents/pi/oauth`       | Pi           | FileBinding | `{"<provider>":{"type":"api_key","key":...},...}` — `auth.json` keyed by provider name |

Two binding shapes appear in the table. A **FileBinding** backs a rotatable on-disk credential file: the launcher renders the vault bytes into the file before launch and snapshots the (possibly rotated) file back via Capture on clean exit. An **EnvBinding** backs a static credential read from the environment: the launcher renders the stored secret into one or more env vars at launch and has nothing to capture, because an env-key agent does not rewrite a credential file in-container.

Goose authenticates with a provider API key (resolved from `<PROVIDER>_API_KEY` or its keyring), so its binding is an EnvBinding rendering the stored key into `ANTHROPIC_API_KEY` (Goose's default provider under launch; seed the matching key for a different provider). OpenCode and Pi each persist provider credentials in a standalone `auth.json` (OpenCode under `~/.local/share/opencode/`, Pi under `~/.pi/agent/`), so both use a byte-identity FileBinding. Pi's `auth.json` is the dedicated credential file, distinct from `settings.json`, so the binding never snapshots non-credential session state.

The vault path's third segment is the literal `oauth` for every agent even when the stored secret is an API key, not an OAuth bundle. The daemon's per-agent endpoint and `vaultPathConforms` (in `internal/launch/authspec.go`) require exactly `agents/<name>/oauth`; any other shape fails launch validation.

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
- **OpenCode:** `opencode auth login` writes `auth.json`.
- **Pi:** `/login` writes `auth.json`.

When the container exits cleanly, Capture reads the file the agent wrote and PUTs the bytes to the vault. Every later launch renders silently.

Goose's EnvBinding has no Capture (there is no credential file to snapshot), so first-launch seeding for Goose is by manual `aileron vault put agents/goose/oauth` rather than by an in-container login that Capture stores.

## Manual seeding

`aileron vault put` writes a per-agent credential envelope directly. It is a daemon-backed command, so the daemon must be running and the vault unlocked.

```
aileron vault put agents/claude/oauth --from-file ./claude-credentials.json
```

`--from-file` reads the file bytes verbatim. There is no trailing-newline munging and no hidden multi-line prompt, so a credential file copied from another machine lands byte-for-byte. This is easier than `aileron secret set`, whose hidden interactive prompt makes pasting a multi-line JSON envelope awkward.

The other vault writes are:

- `aileron secret set <name>` stores a value at any path via interactive prompt. The path scheme is yours; pass `agents/claude/oauth` to populate Claude's envelope.
- The launcher's Capture pass on clean container exit. This is the primary path.

The bytes must match the envelope schema in the table above when set manually. Render validates on the way in; a malformed envelope fails the launch with a clear error before the container starts.

`aileron vault list` shows which agents currently have an entry, metadata only. The credential value is never listed.

```
aileron vault list
```

`aileron auth <agent> --import-from-host` seeds the vault from an already-authenticated host install. It reads the host `claude` or `codex` credential state, validates it against the same schema the launcher's Capture pass enforces, and writes it through the daemon to `agents/<agent>/oauth`. Operators who already ran `claude` or `codex` login on the host skip the per-machine in-container login.

```
aileron auth claude --import-from-host
aileron auth codex --import-from-host
```

The command reads the credential from the location each agent's CLI writes per operating system.

- Linux. Claude reads `~/.claude/.credentials.json`. Codex reads `~/.codex/auth.json`. A Linux keyring install (libsecret) is not supported in v1; the command returns a clear error naming the file-mode and interactive-login recovery paths.
- macOS. Claude reads the Keychain item under the `Claude Code-credentials` service. Codex prefers `~/.codex/auth.json` when it exists and otherwise reads the `Codex Auth` Keychain service. The first real Keychain read shows a macOS access dialog; approve it once so the command can read the item.
- Windows. Claude reads `%USERPROFILE%\.claude\.credentials.json`. Codex reads `%USERPROFILE%\.codex\auth.json`.

Only `claude` and `codex` accept `--import-from-host`. Other agents return a clear unsupported error. The bytes are read verbatim and validated through the agent's Capture before the PUT, so a malformed or partial envelope fails with the same error the launcher reports rather than landing a broken credential in the vault.

When the host install is not authenticated (the file is absent or empty, or the Keychain item is missing), the command reports that no host credentials were found and names the recovery path: log in on the host first, or run the interactive in-container login with `aileron launch <agent> --sandbox=docker`.

## Recovery

To re-login from scratch (vault entry stale, refresh token revoked, or just starting over), the recovery path is whichever of these you can execute:

- Delete the agent's entry with `aileron vault delete agents/<agent>/oauth`. The next launch finds no entry, falls through to the in-container login bootstrap, and re-seeds a fresh credential on clean exit. This is a daemon-backed command, so the daemon must be running.

  ```
  aileron vault delete agents/claude/oauth
  ```

  The command confirms before deleting. Pass `--yes` to skip the prompt in scripts.
- Use the in-container agent's logout flow (`/logout` in Claude Code, the Codex CLI's equivalent), then exit cleanly. Capture overwrites the vault entry with the agent's logged-out state, and the next launch prompts for login again.
- Delete the local vault file at the path printed by `aileron vault init`, which clears all stored secrets. Use this only when no other vault entries matter to you.

If the Codex pre-launch refresh fails because the refresh token was revoked upstream, the launcher exits with a message naming the recovery options above.

## Concurrency and freshness

v1 uses last-writer-wins for the Capture-side PUT. Two simultaneous `aileron launch codex --sandbox=docker` invocations against the same agent can race; refresh tokens survive the race because both writers exchanged the same upstream token. Concurrent launches against the same agent are not in the v1 ICP. A freshness-comparison hook on FileBinding is a clean follow-up if the concern surfaces.

A second race lives between Capture and the operator: if you run `aileron vault delete agents/<agent>/oauth` while a sandbox session is still running, the session's clean exit will re-write the entry with whatever credential bytes the in-container agent left behind. Exit the running container first, or let the in-container agent log out before exit, so your delete is the last writer.

Capture stays non-fatal. A vault-write failure or schema-validation failure surfaces as a one-line stderr warning that names the file path and the recovery option, and skips that binding's PUT. The session completes normally; the prior vault entry is retained.

Capture also runs on graceful shutdown. A SIGINT or SIGTERM to `aileron launch` would otherwise propagate as a forcible container kill before the clean-exit Capture gate ran, losing any in-container credential rotation. The launcher names the container `aileron-sbx-<sessionID>`, installs a signal handler around the run, and on the first SIGINT/SIGTERM best-effort stops the named container with a bounded 10-second grace window then runs the same Capture. A `sync.Once` guards Capture so a signal that races a clean exit writes the rotation back exactly once. A failed stop is non-fatal and Capture still runs. SIGKILL stays uncatchable and runtime crashes still skip Capture; in those cases the prior vault entry is retained and the next launch self-heals via the agent's own refresh (Claude) or the pre-launch refresh hook (Codex).

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
