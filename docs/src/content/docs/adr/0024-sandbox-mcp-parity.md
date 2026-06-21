---
title: "ADR-0024: Sandbox MCP Parity (Path B1)"
description: "Sandbox launch revives aileron-mcp as a stdio subprocess of the in-container agent, reached via a read-only host-mounted binary; the daemon is reached over HTTPS via host.docker.internal."
order: 24
---

<div class="meta">
<table>
  <tr><th>Status</th><td>Accepted</td></tr>
  <tr><th>Date</th><td>2026-06-08</td></tr>
  <tr><th>Tracking</th><td><a href="https://github.com/ALRubinger/aileron/issues/953">#953</a>, <a href="https://github.com/ALRubinger/aileron/issues/747">#747</a></td></tr>
</table>
</div>

> **Revision note, 2026-06-15:** This ADR originally left the shims-on-`PATH` + `tools.txt` surface in place as a "complementary non-MCP-native CLI path," which created the deliberate dual surface recorded under "Negative — surface duplication." [#959](https://github.com/ALRubinger/aileron/issues/959) resolves that open surface choice by retiring the shim surface entirely. `aileron-mcp` is now the sole in-container tool surface. The two reasons shims were once load-bearing, BYOCLI tool-catalog cost and shim-based credential mediation, are both gone, and all five launch agents are MCP-capable. Passages below that describe shims as a live complementary surface are superseded by this note.

## Context

[ADR-0008](/adr/0008-intent-matching) ratifies MCP as the canonical action-exposure surface under host launch. [ADR-0018](/adr/0018-v4-single-binary-runtime) codified that the v4 sandbox runtime would NOT revive `aileron-mcp` in-container, on the basis that container-side generated HTTPS shims plus the data-plane direction in [ADR-0019](/adr/0019-v4-https-data-plane) and [ADR-0020](/adr/0020-v4-connector-specs-and-shims) would carry the load.

That decision is reversed here. Operating the v4 sandbox without MCP parity left the in-container agent with a degraded tool surface compared to host launch: agents that consume MCP server registrations natively (Claude, Pi, Goose, OpenCode, Codex) see Aileron's action catalog as bash-callable shims, not as first-class function-call targets the LLM can pick by description. The cost is paid in prompt context, in tool-selection quality, and in user-visible asymmetry between `aileron launch <agent>` and `aileron launch --sandbox=docker <agent>`. This ADR originally kept the shims surface as a complementary non-MCP-native CLI path; [#959](https://github.com/ALRubinger/aileron/issues/959) later retired it, leaving `aileron-mcp` as the sole surface (see the revision note above).

The path forward considered two architectural candidates ([#953](https://github.com/ALRubinger/aileron/issues/953) body):

- **(a) Host-reached.** `aileron-mcp` runs on the host; the container reaches it via the Docker host alias `host.docker.internal:<port>`. Cheap only if `aileron-mcp` already speaks TCP, which it does not — it is strictly stdio MCP today. Option (a) requires a net-new stdio↔TCP bridge subprocess inside the container.
- **(b) In-container subprocess.** `aileron-mcp` is exec'd as a stdio subprocess of the agent process inside the container. It reaches the daemon over HTTPS via the already-rewritten `AILERON_URL` and authenticates with the already-injected `AILERON_TOKEN`.

The sandbox launcher already plumbs `AILERON_URL` and `AILERON_TOKEN` into the container env (the speculative wiring that landed for [#796](https://github.com/ALRubinger/aileron/issues/796)). The `sandboxDiscoveryMounts` pattern — which host-builds tooling and read-only-mounts it into the container under `/etc/aileron/` and `/usr/local/bin/` — is the established shape for bringing host-built artifacts into the container at launch time. Option (b) reuses both directly.

## Decision

Under `aileron launch --sandbox=docker <agent>`, the launcher revives `aileron-mcp` as an MCP server named `aileron`:

- The host-built `aileron-mcp` binary is resolved by the existing `resolveMCPBinary(selfPath)` (sibling-then-`PATH` lookup) and bind-mounted read-only into the container at `/usr/local/bin/aileron-mcp`. The `sandbox-base` image is not modified.
- The launcher calls `agent.ConfigureMCP("/usr/local/bin/aileron-mcp", mcpEnv, dir)`. The container-side binary path is what the in-container agent execs; the host-side path only needs to exist for the bind-mount.
- `mcpEnv` carries the daemon URLs rewritten for the container runtime (`AILERON_URL`, `AILERON_COMMS_URL`, `AILERON_APPROVAL_URL`) plus the launch-scoped `AILERON_SESSION_ID` and `AILERON_TOKEN`.
- The agent registers `aileron-mcp` per its native mechanism: Claude / Pi via `--mcp-config` JSON, Goose via `--with-extension`, OpenCode via `opencode.json` written into the launch directory (which is the workspace bind-mount), Codex via a temp `config.toml` bind-mounted into the container at `/home/agent/.codex/config.toml`. Codex's host `~/.codex/config.toml` is never touched in sandbox mode.
- The sandbox container's validate step asserts `aileron-mcp` is present on `PATH` and executes (`aileron-mcp --version` smoke-checks for cross-arch mismatch).
- The reserved-sandbox-command guard reserved `aileron-mcp` so no connector shim could collide with the MCP binary path. With the shim surface retired in [#959](https://github.com/ALRubinger/aileron/issues/959), that guard was removed; `aileron-mcp` is the only mount under `/usr/local/bin/` that launch emits.
- The user's own MCP servers (registered through their devcontainer.json or agent-side `mcp.json`) coexist independently. Aileron does NOT aggregate, route, or proxy them. The container MCP model is B1 (Aileron is one MCP server), not B2 (Aileron is an MCP gateway).

The trust contract from [ADR-0009](/adr/0009-user-channel) is preserved: the agent is never in the approval path. Approval surfaces (webapp `review_url`, CLI `aileron approval approve <id>`) reach the user on the host as they do under host launch. The daemon emits the same `execution.started` / `execution.succeeded` / `execution.failed` and `approval.requested` / `approval.approved` / `approval.denied` events for actions invoked via the MCP path, stamped with the launch session id.

v4 is Docker-only, so the container host alias is `host.docker.internal`. Podman is deferred to a later track, not rejected, and its `host.containers.internal` alias is the deferred re-add path for when Podman returns. The runtime abstraction seam is preserved, so re-adding Podman is re-enabling it in `resolveRuntime` and the support matrix. See [ADR-0014](/adr/0014-spawn-sandbox-technology) and umbrella issue [#1050](https://github.com/ALRubinger/aileron/issues/1050) for the descope rationale.

## Consequences

### Positive

- The in-container agent sees the same first-class MCP tool surface as the host-launched agent. Tool-selection quality, prompt context cost, and approval ergonomics match host launch.
- No new MCP transport surface. `aileron-mcp` keeps its stdio contract; the agent's MCP client expects a stdio child.
- No new daemon endpoint. The daemon-reachability problem is solved by the existing `AILERON_URL` rewrite.
- No `sandbox-base` image rebuild. Operators get the fix the first time they launch against a new `aileron` CLI release.
- The host-mount keeps `aileron-mcp` in version-lockstep with the host's `aileron` CLI, eliminating the version-skew failure mode where a baked image's `aileron-mcp` falls behind the host daemon's API surface.

### Negative — trust-model deltas vs host launch

Two surfaces widen, named here so the threat model is documented rather than rediscovered later.

- **Daemon network exposure.** Host launch binds loopback only. To be reachable from the container via `host.docker.internal`, the daemon binds either an explicit `host-gateway` interface (`--add-host=host.docker.internal:host-gateway` since Docker 20.10) or a non-loopback address. The credential the in-container `aileron-mcp` now carries (`AILERON_TOKEN`) is no longer the full-authority master token. It is a session-scoped HMAC-signed caveat token, minted at session establishment and restricted to the four capabilities `aileron-mcp` exercises (`actions:list`, `actions:run`, `approvals:poll`, `comms`) bound to its own session id (delivered in [#958](https://github.com/ALRubinger/aileron/issues/958)). A leaked container token costs "some actions ran," not "vault decrypted, policy rewritten." The full-authority master token stays host-side and is used only for the host CLI, the webapp, and the forward-proxy basic-auth credential. Any host process that holds the *master* token still gains full authority, so loopback binding of the master surface remains the protection there.
- **Binary-mount UID delta.** The host-mounted `aileron-mcp` runs under the container's `agent` UID, not the host user's UID. Read-only mount sidesteps the obvious file-mutation issue. The threat model is the same as for the existing `sandboxDiscoveryMounts` shims.

### Negative — operational

- Operators must have a `aileron-mcp` binary on the host (sibling of `aileron`, or on `PATH`). This matches host launch's existing hard error; for operators of the v4 sandbox today, the constraint is not new.
- Cross-arch hosts (e.g., arm64 host bind-mounting into amd64 container) fail at validate time, not at first MCP call. The validate step runs `aileron-mcp --version` for exactly this reason.
- Sealed customer-operated runtimes that do not ship the Aileron CLI on the host cannot host-mount. The image-bake path is the next iteration (see "Future considerations").

### Negative — surface duplication (resolved by #959)

As originally shipped, the shims surface (`/usr/local/bin/<shim>` + `tools.txt`) and the MCP catalog (`mcp__aileron__<tool>`) exposed the same action operations to MCP-capable agents. The agent saw both, so the "MCP catalog is O(1) in N connectors" framing was partially undone by the context-window cost of carrying two surfaces. [#959](https://github.com/ALRubinger/aileron/issues/959) resolves this by retiring the shim surface entirely rather than capability-gating it, leaving `aileron-mcp` as the sole surface. The reversal condition is documented under "Future considerations": re-introducing arbitrary-CLI wrapping (BYOCLI) would revive the case for a non-MCP-native CLI surface.

## Alternatives Considered

### Option (a): Host-reached `aileron-mcp` via stdio↔TCP bridge (rejected)

`aileron-mcp` runs on the host as a long-lived TCP listener. Inside the container, a bridge subprocess exposes a stdio MCP socket to the agent and forwards bytes to the host's TCP listener.

Rejected. It introduces a net-new MCP transport surface (stdio↔TCP) for marginal value. The daemon-reachability problem option (a) was meant to solve is already solved by the sandbox launcher's existing `AILERON_URL` rewrite, which option (b) consumes directly.

### Option (a) variant: Unix-socket forwarding via shared mount (rejected)

The host's `aileron-mcp` binds a Unix socket on a path that is bind-mounted into the container. A small bridge subprocess inside the container exposes a stdio MCP socket to the agent and forwards bytes to the shared Unix socket.

Rejected. The stdio↔socket bridge subprocess problem option (a) introduces remains; only the host-side transport changed (TCP → Unix socket). Same net-new surface, same maintenance.

### Option (a) variant: Sidecar container with shared network namespace (rejected)

`aileron-mcp` runs in a sidecar container that shares the agent container's network namespace. The agent container reaches it on a localhost port inside the shared namespace.

Rejected for v4's single-Docker-runtime today. Sidecar lifecycle and orchestration is a meaningfully larger packaging change than option (b)'s host-mount. The conversation can be reopened for v5 SaaS pod-design where multi-container pods are the default unit.

### Image-bake of `aileron-mcp` (deferred, not rejected)

`aileron-mcp` is baked into the `sandbox-base` image at `/usr/local/bin/aileron-mcp`. No host binary needed.

Deferred. The host-mount keeps the binary version in lockstep with the host's CLI release for v4's customer-operated default; for sealed customer-operated runtimes ([v4.x BYOC](https://github.com/ALRubinger/aileron/issues/747) and v5 SaaS) the image-bake path becomes the right shape. Trigger criteria for the flip:

1. The sandbox image needs to run without a host-side `aileron-mcp` available.
2. The `aileron-mcp` API surface stabilizes enough that version skew is a managed-release decision, not a per-launch coincidence.
3. v4.x BYOC ships and customer-built host images don't include the binary.

Shipped for the published image in [#957](https://github.com/ALRubinger/aileron/issues/957), triggered by criterion 1. The published `ghcr.io/alrubinger/aileron-sandbox-base` image bakes `aileron-mcp` and carries an `ai.aileron.mcp.version` label. The launcher reads the label to skip the host-mount when the image is baked, and falls back to the host-mount for unlabeled images. The local Tier 0 base build stays unbaked, so the v4 default topology keeps its host-mount lockstep. `aileron sandbox check` warns on version skew between a baked image and the host CLI.

### Codex `cli_auth_credentials_store` emission

The Codex sandbox `config.toml` generated by `ConfigureMCP` under `ModeSandbox` now also emits `cli_auth_credentials_store = "file"` as a top-level key. Without that line the in-container Codex CLI tries the macOS Keychain or Linux secret-tool keyring, neither of which exists inside the sandbox. The file-mode store reads `auth.json` from `/home/agent/.codex/auth.json`, which the launcher writes from the vault via the [ADR-0025](/adr/0025-vault-backed-agent-auth) `AuthSpec` FileBinding. This emission rides on the same mechanism as the `[mcp_servers.aileron]` block this ADR introduced; the host `~/.codex/config.toml` is still never touched under `ModeSandbox`.

## Future considerations

- **Token scoping (delivered).** The launch-scoped `AILERON_TOKEN` used to give the holder full daemon-action authority. Delivered in [#958](https://github.com/ALRubinger/aileron/issues/958): the daemon mints a per-daemon HMAC-signed caveat token at session establishment (`POST /v1/sessions` returns `caveat_token`) carrying `caps = [actions:list, actions:run, approvals:poll, comms]` bound to `session = <id>`, and the `/v1/*` middleware decodes it statelessly to enforce a route→capability map plus the session binding (a caveat is rejected for any session other than its bound id). The launcher injects this caveat token — not the master daemon token — into the container as `AILERON_TOKEN`; `aileron-mcp` is unchanged since it just reads the env var. The master daemon.json token keeps full unscoped `/v1/*` access for the host CLI and webapp. Explicitly out of scope and not delivered: caveat tokens for the static shims, and any general-purpose `/v1/*` authz redesign.
- **Image-bake for sealed runtimes.** Shipped for the published image in [#957](https://github.com/ALRubinger/aileron/issues/957); local Tier 0 stays unbaked. See "Alternatives Considered" → image-bake deferred.
- **Shim surface resolution (done).** This was filed as a follow-up to suppress shim emission for MCP-capable agents. [#959](https://github.com/ALRubinger/aileron/issues/959) resolved it more directly by retiring the shim surface outright, since every launch agent is MCP-capable and BYOCLI is gone. The reversal condition: re-introducing arbitrary-CLI wrapping would revive the case for a non-MCP-native CLI surface.
- **Codex multi-config-file workaround.** Whether Codex reads additional `~/.codex/*.toml` files beyond `config.toml` is unverified at the time of this ADR. If Codex supports a multi-file config, users wanting Codex+sandbox with extra MCP servers gain a clean merge path; if not, the manual recipe documents the limitation and recommends pre-merge of entries.

## References

- [Issue #953](https://github.com/ALRubinger/aileron/issues/953) — Sandbox MCP parity (this ADR's tracking issue)
- [Issue #957](https://github.com/ALRubinger/aileron/issues/957) — Image-bake of `aileron-mcp` into the published `sandbox-base` image (the deferred flip)
- [Issue #958](https://github.com/ALRubinger/aileron/issues/958) — Session-scoped caveat token for the in-container `aileron-mcp` (the "Token scoping" future consideration, delivered)
- [Issue #747](https://github.com/ALRubinger/aileron/issues/747) — Milestone v4 umbrella
- [Issue #796](https://github.com/ALRubinger/aileron/issues/796) — Shims + `tools.txt` surface; provides the speculative env wiring and mount pattern this ADR consumes
- [ADR-0008](/adr/0008-intent-matching) — MCP as canonical tooling under host launch; this ADR extends it to sandbox launch
- [ADR-0009](/adr/0009-user-channel) — Agent never in the trust path; preserved unchanged across host and sandbox
- [ADR-0017](/adr/0017-sandbox-composition) — Sandbox composition via devcontainer.json
- [ADR-0018](/adr/0018-v4-single-binary-runtime) — The "no `aileron-mcp` in sandbox" decision amended here
- [ADR-0020](/adr/0020-v4-connector-specs-and-shims) — Generated HTTPS shims; the rendered shim surface was retired in [#959](https://github.com/ALRubinger/aileron/issues/959), and the connector-spec loading it described is retained for data-plane operation validation
