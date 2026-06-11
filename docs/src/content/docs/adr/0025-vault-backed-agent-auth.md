---
title: "ADR-0025: Vault-backed Agent Authentication Injection"
description: "The Agent interface gains a bidirectional AuthSpec descriptor. The launcher renders vault entries into the sandbox container at launch, captures in-container rotations back to vault on clean exit, and seeds the vault via the agent's own in-container login on first launch."
order: 25
---

<div class="meta">
<table>
  <tr><th>Status</th><td>Accepted</td></tr>
  <tr><th>Date</th><td>2026-06-10</td></tr>
  <tr><th>Tracking</th><td><a href="https://github.com/ALRubinger/aileron/issues/969">#969</a>, <a href="https://github.com/ALRubinger/aileron/issues/747">#747</a></td></tr>
</table>
</div>

## Context

Sandbox launches today land in a fresh `/home/agent/.claude/` and `/home/agent/.codex/` every time. Each agent's first-launch wizard fires on every invocation. Subscription auth (the dominant auth mode for both Claude Code and Codex CLI) has no path through the launcher.

The vault SPI at `internal/vault/spi.go` already enumerates `oauth_refresh_token` and `api_key` as `Metadata.Type` values. Nothing in `internal/launch/` consults it. The brainstorm at [`docs/brainstorms/2026-06-10-vault-backed-agent-auth-injection-requirements.md`](https://github.com/ALRubinger/aileron/blob/main/docs/brainstorms/2026-06-10-vault-backed-agent-auth-injection-requirements.md) settled the user-facing shape: the vault is durable, the container is ephemeral, and a writable bind-mount is the conduit between them.

The decision below pins the implementation contract: a new `AuthSpec` descriptor on the `Agent` interface, a daemon-brokered vault credentials API, per-agent specs for Claude and Codex, and a single seeding path that runs entirely inside the sandbox container.

## Decision

The `Agent` interface gains `AuthSpec() AuthSpec`, returning a static declarative description of the agent's vault-backed credential bindings:

```text
AuthSpec {
  EnvBindings:  [ { VaultPath, Required, Render(Secret) -> map[string]string } ]
  FileBindings: [ { VaultPath, ContainerPath, Mode, Required, MountAsFile,
                    Render(Secret) -> []byte, Capture([]byte) -> Secret,
                    PreLaunchRefresh?(Secret, RefreshDeps) -> Secret } ]
  StaticFiles:  [ { ContainerPath, Mode, Content []byte } ]
}
```

The launcher consumes the spec around the sandbox lifecycle in four phases:

1. **Render before container start.** For each binding, GET the vault entry at the binding's `VaultPath` through a new daemon HTTP endpoint (`/v1/vault/agents/{name}/credentials`). Run the binding's `Render` function. EnvBindings produce env vars that merge into the agent process's environment. FileBindings produce bytes written into a host-side transient directory chmod 0700, then bind-mounted into the container at the binding's parent directory (the default) or as an individual file at `ContainerPath` (when `MountAsFile = true`). StaticFiles land in the same transient directory regardless of vault state.
2. **Optional pre-launch refresh.** When a FileBinding declares `PreLaunchRefresh`, the hook runs after the GET and before Render. The hook exchanges the refresh token for a new access token against the vendor's auth server, persists the rotated bundle through the daemon's `PUT /v1/vault/agents/{name}/credentials` BEFORE returning successfully, and hands Render the new Secret. A failed persist aborts the launch. Codex uses this hook to refresh against `auth.openai.com`; Claude self-refreshes inside the container.
3. **Run.** The launcher invokes `sandboxcontainer.Builder.Run` with the merged env and mounts. The in-container agent finds its credentials at the documented paths and starts silently.
4. **Capture on clean exit.** When `Builder.Run` returns nil, the launcher reads each FileBinding's host-side file, runs the binding's `Capture` function (which validates the envelope schema and rejects malformed bytes), and PUTs the result back through the daemon. Forcible termination skips Capture so the prior vault entry is retained.

The daemon's new endpoints are namespace-scoped at the routing layer: `{name}` translates internally to `agents/<name>/oauth`, so other vault paths are unreachable through this surface. The endpoint returns named error codes in the response body so the launcher discriminates `vault_not_found` (drives the in-container-login bootstrap path) from `vault_locked` (drives the unlock prompt) by code rather than HTTP status alone.

**Seeding is exclusively in-container in v1.** First launch with an empty vault prints `[launcher] no credentials in vault for <agent>. agent will prompt for login` to stderr and starts the container with the writable bind-mount empty. The in-container agent performs its normal interactive login (paste-the-code OAuth fallback for Claude, device-auth for Codex). Capture on clean exit seeds the vault. Every subsequent launch renders silently. Host-side credential import is deferred to a follow-up; the brainstorm closes that scope in v1.

**Sandbox-only in v1.** Host-launch parity for `AuthSpec` is architecturally clean to extend later. It is not part of this decision.

## Consequences

### Positive

- Subscription auth survives across launches. The user logs in once per agent, inside the sandbox. Every later launch renders silently.
- Rotation persistence is free. Claude's mid-session OAuth rotation lands in the bind-mounted file; Capture snapshots it back so the next launch picks up the new access token.
- The launcher never opens the vault file. The daemon is the single trust boundary for unlocked credentials, matching [ADR-0011](/adr/0011-local-credential-vault) and [ADR-0012](/adr/0012-local-daemon-architecture).
- The contract is independent of seeding. Render and Capture never know whether bytes arrived via in-container login, via `aileron vault put`, or via any future host-import path.
- Agents that have no vault-backed credentials return the zero-value `AuthSpec{}` and pay no overhead. Goose, Pi, and OpenCode ship that shape in v1.

### Negative

- The launcher gains a Render-and-Capture lifecycle around `sandboxcontainer.Builder.Run`. The added surface is bounded (one new file under `internal/launch/`) and gated entirely behind sandbox mode.
- One new HTTP endpoint pair on the daemon's surface. Same auth posture as existing vault routes; namespace-scoped at routing so a stolen bearer token cannot reach arbitrary vault paths through this endpoint.
- Two well-known vault paths land: `agents/claude/oauth` and `agents/codex/oauth`. The `agents/<name>/<purpose>` scheme is documented here for future agents to extend cleanly.
- The macOS Keychain and Linux secret-tool keyring topologies that Codex CLI supports natively are unreachable in the sandbox. Codex's sandbox `config.toml` emits `cli_auth_credentials_store = "file"` so the in-container CLI reads `auth.json` instead. The host CLI is unchanged.
- v1 uses last-writer-wins for the Capture-side vault write. Concurrent launches against the same agent are not in the v1 ICP. Refresh tokens survive the race because both writers exchanged the same upstream token. A freshness-comparison hook on FileBinding is a clean follow-up when real users surface the concern.

### Trust-model deltas vs host launch

- The vault entry holds a usable OAuth credential. The daemon already protects this surface per [ADR-0011](/adr/0011-local-credential-vault); the new endpoints inherit the same posture without widening it.
- The writable host-side transient directory is chmod 0700, so OAuth bytes do not leak through a shared host's default umask. The launcher removes it on Launch exit.

## Alternatives Considered

### Render-only contract (no Capture)

Ship the descriptor with Render bindings only. Bootstrap is `aileron vault put` manually. Rotation persistence is the user's problem.

Rejected. Rotation persistence is the load-bearing simplification of the in-container snapshot model. Without Capture, every rotation drops on container exit and the user re-imports every few hours. Render-only would technically work but discards most of the value.

### Direct vault file open from the launcher

Have the launcher call `OpenLocalVault` itself, avoiding the new daemon HTTP API.

Rejected. The daemon is the trust boundary for the unlocked vault per [ADR-0011](/adr/0011-local-credential-vault) and [ADR-0012](/adr/0012-local-daemon-architecture). A second vault opener fragments that boundary and complicates concurrent-unlock semantics.

### Per-agent capture hook on `Agent`

Instead of a descriptor, give `Agent` a `Capture(ctx, hostPath) error` method and let each agent implement its own capture lifecycle.

Rejected. The descriptor approach keeps the contract declarative and the per-agent code small. The lifecycle is fixed (read file at known path, vault put) and should not be pluggable per agent.

### Aileron-initiated OAuth dance

Aileron runs the OAuth dance with the vendor's auth server, exchanges the authorization code, persists the bundle to vault, and avoids the in-container login entirely.

Parked indefinitely. Vendor client-ID policy is unresolved for both Claude and Codex. The in-container-login path covers bootstrap sufficiently and does not require vendor cooperation.

### `aileron auth <agent> --import-from-host` CLI

Reach into the host's existing Claude or Codex CLI install, extract the credential bytes, and PUT them to vault. Per-platform code paths for Linux files, macOS Keychain, and Windows files.

Deferred. The in-container login path covers bootstrap without the host-OS extraction matrix. Macos and Windows users with an existing host CLI install pay one paste-the-code dance per agent per machine. That cost is small and lands on a surface (the in-container terminal) where consent is explicit by construction. The host-import surface is a candidate follow-up when real users surface the ask.

### Future composition with v4 HTTPS data plane ([#896](https://github.com/ALRubinger/aileron/issues/896))

The current decision stores refresh tokens in the vault and hands them to the agent via the AuthSpec FileBinding. When the v4 HTTPS data plane lands and the daemon proxies credentialed network calls at the proxy boundary, the AuthSpec contract stays unchanged. The EnvBinding Render adapts to return vault-binding references rather than raw access tokens, and the daemon-side proxy bears the credential. The current shape is a transitional posture that #896 narrows, not a competing topology.

## References

- [Issue #969](https://github.com/ALRubinger/aileron/issues/969). Vault-backed agent auth injection (this ADR's tracking issue)
- [Issue #747](https://github.com/ALRubinger/aileron/issues/747). Milestone v4 umbrella
- [ADR-0011](/adr/0011-local-credential-vault). Vault is the daemon's trust boundary
- [ADR-0012](/adr/0012-local-daemon-architecture). Daemon owns the unlocked vault
- [ADR-0023](/adr/0023-v4-vault-centric-encryption). v4 vault-centric encryption schema
- [ADR-0024](/adr/0024-sandbox-mcp-parity). Sandbox MCP parity (this ADR builds on the same writable-bind-mount lifecycle)
- [`docs/development/sandbox-agent-auth`](/development/sandbox-agent-auth). Operator-facing walkthrough for vault-backed sandbox auth
