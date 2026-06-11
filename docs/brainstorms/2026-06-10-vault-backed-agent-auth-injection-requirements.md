---
date: 2026-06-10
topic: vault-backed-agent-auth-injection
issue: 969
related_issues: [747, 964, 965, 966]
related_adrs: [0023, 0024]
---

# Vault-Backed Agent Auth Injection (Claude, Codex)

## Summary

Extend the `Agent` interface with a bidirectional `AuthSpec` contract so the launcher materializes vault entries as in-container env vars and credential files at launch, and snapshots the (possibly rotated) credential files back to vault at exit. The default seeding path is an in-container login dance on first launch; on developer machines with an existing host install, the launcher transparently auto-imports from the host's credential store. Sandbox launches of Claude Code and Codex CLI become silent on every launch after the first — no theme picker, no login picker, no re-paste — and the same contract is topology-agnostic enough to carry forward into cloud Aileron without re-shaping.

---

## Problem Frame

Every sandbox launch lands in a fresh `/home/agent/.claude/` (or `/home/agent/.codex/`), so the agent CLIs run their first-launch wizards (theme picker, login picker) on every invocation. `ANTHROPIC_API_KEY` env passthrough would cover the API-key path but leaves subscription users — the dominant auth mode for both Claude Code and Codex CLI — completely stuck. The host-launch path has the same gap in a different shape: `composeAgentEnv` (`internal/launch/launcher.go:452`) forwards only the LLM-endpoint override and Aileron's session vars; nothing of the user's own model-provider auth flows through.

The vault (`internal/vault/spi.go`) already carries the right primitive — `Metadata.Type` enumerates `"api_key"` and `"oauth_refresh_token"`, `Value []byte` is opaque — but nothing in the launch path consults it. The gap is a launcher-side contract that turns a vault entry into either an env var or a file inside the launched agent's environment, plus a bootstrap UX that gets the user's tokens into the vault in the first place.

Two agents are in scope for v1: Claude Code and Codex CLI. Both are in Aileron's launch registry today; both have well-understood, recently-verified OAuth surfaces (see Sources). The auth shapes are different enough — Claude exposes env-var refresh-token injection, Codex requires file-based `auth.json` rendering with proactive refresh — that designing for both at once is what forces the abstraction to be right.

---

## Key Decisions

- **In-container login + capture is the primary seeding path.** The launcher mounts a writable per-agent state directory into the container; if the vault has no entry, the user does the agent's normal interactive login once inside the container (paste-the-code OAuth fallback works regardless of vendor cooperation), and the launcher snapshots the resulting credential file to vault at exit. Subsequent launches render the vault entry into the bind-mount before the agent starts. The vault is the durable source of truth; the bind-mount is a transient conduit.

- **`AuthSpec` is bidirectional.** Render (vault → in-container env/file) and Capture (in-container file → vault) are both first-class. Capture is what makes bootstrap work without a per-platform extraction matrix, and it doubles as the rotation-persistence mechanism — mid-session token rotation is caught on exit instead of being lost.

- **`AuthSpec` is a separate descriptor on `Agent`, not another method.** Each binding is declarative data (vault path, target env var or container path, render/capture funcs, required flag). Treating it as a method body would mix two concerns on the interface; the descriptor shape keeps `AuthSpec` independently inspectable and testable.

- **`--import-from-host` ships in v1 for Linux and macOS, both agents.** Auto-tried by the launcher when the vault is empty and host credentials are present; available as an explicit `aileron auth <agent> --import-from-host` command for re-imports. Windows extraction deferred — Windows users fall back to the in-container login path, which works.

- **Refresh ownership is asymmetric and intentional.** Claude refreshes itself inside the container (`CLAUDE_CODE_OAUTH_REFRESH_TOKEN` is the documented headless path); Codex's refresh runs in the launcher pre-launch (no env-var refresh-injection equivalent exists). Both end up writing the renewed credential bundle back to vault via Capture.

- **Client-ID policy: defer Aileron-initiated OAuth.** Both Anthropic and OpenAI ship public-but-unlicensed OAuth client IDs in their CLIs. Aileron does not run the dance itself in v1. The in-container login path uses the agent's own client ID with the agent's vendor, which is the supported mode.

- **Sandbox only for v1.** Host-launch parity is architecturally clean but not load-bearing today (host launch already inherits the user's env and the agent finds its own state files). Plumbing `AuthSpec` through host launch is a smaller follow-up after sandbox ships.

---

## Actors

- A1. **User** — interactive developer running `aileron launch <agent> --sandbox=docker`. Owns vendor account; may have host CLI authed or not.
- A2. **Launcher** (`internal/launch`) — resolves the vault, materializes `AuthSpec` bindings into the container, captures changes at exit.
- A3. **Agent** (Claude Code or Codex CLI inside the container) — reads its own credential file or env vars; rotates tokens against the vendor's token endpoint; never knows about Aileron's vault.
- A4. **Vault** (`internal/vault`) — durable encrypted store at `agents/<name>/<purpose>`.
- A5. **Vendor auth server** (`claude.ai/oauth/*`, `auth.openai.com/oauth/*`) — issues access tokens and rotates refresh tokens.

---

## Requirements

### Contract

- R1. The `Agent` interface gains an `AuthSpec() AuthSpec` method returning a static descriptor of the agent's auth bindings.
- R2. `AuthSpec` carries two binding kinds: `EnvBindings` (resolve vault entry → in-container env vars) and `FileBindings` (resolve vault entry → in-container file).
- R3. Each binding declares a `VaultPath`, a `Required` flag, a `Render` function (vault `Secret` → bytes-or-env-map), and — for `FileBindings` — a `Capture` function (bytes → vault `Secret`).
- R4. `AuthSpec` may also carry `StaticFiles` for in-container state that is not vault-backed and is constant per agent (e.g. Claude's `~/.claude.json` onboarding stub).
- R5. The contract is independent of how the vault is seeded — Render and Capture do not know whether the bytes came from `--import-from-host`, an in-container login, or manual `vault put`.

### Launch lifecycle

- R6. On launch, the launcher resolves each `EnvBinding`'s vault path; on hit, runs `Render` and merges the result into the in-container env. On miss with `Required=true`, the launch fails with an error naming the missing vault path and how to seed it.
- R7. On launch, the launcher resolves each `FileBinding`'s vault path; on hit, runs `Render`, writes the resulting bytes to a host-side transient directory, and bind-mounts that directory at the binding's `ContainerPath`. On miss with `Required=true`, the launch fails with an error; on miss with `Required=false`, the bind-mount is empty (agent will perform interactive login).
- R8. The bind-mount is writable. The in-container agent may rewrite the credential file during the session (token rotation, re-login).
- R9. On exit (clean shutdown or container-runtime-reported normal termination), the launcher reads each `FileBinding`'s host-side file, runs `Capture`, and writes the result to the binding's vault path. Capture is best-effort: on read error, schema error, or vault write error, the launcher logs and continues.
- R10. The launcher deletes the host-side transient directory after capture so plaintext credentials do not outlive the launch.
- R11. Forcible termination (SIGKILL, `docker kill`, runtime crash) skips capture without error. The vault retains the prior credential; the next launch may need a rotation refresh or re-login but does not break.

### Per-agent specs

- R12. **Claude.** `AuthSpec` declares an `EnvBinding` at vault path `agents/claude/oauth` rendering `CLAUDE_CODE_OAUTH_REFRESH_TOKEN` and `CLAUDE_CODE_OAUTH_SCOPES`; a `FileBinding` at vault path `agents/claude/oauth` rendering `/home/agent/.claude/.credentials.json` (full credential envelope) with Capture; and a `StaticFile` at `/home/agent/.claude.json` (mode `0644`) containing `{"hasCompletedOnboarding": true, "installMethod": "native"}`.
- R13. **Codex.** `AuthSpec` declares a `FileBinding` at vault path `agents/codex/oauth` rendering `/home/agent/.codex/auth.json` (mode `0600`) with Capture, plus a pre-launch refresh hook that exchanges the vault's refresh token at `https://auth.openai.com/oauth/token` immediately before container start and re-stores the new bundle on rotation.
- R14. **Codex config.toml.** The launcher emits `cli_auth_credentials_store = "file"` into the generated container-side `config.toml` (ADR-0024 path) so the in-container Codex resolves auth from `auth.json` instead of attempting a non-existent keyring.

### Seeding paths (v1)

- R15. **In-container login + capture (default).** When `aileron launch <agent> --sandbox=docker` runs against an empty vault and no host credentials are detected, the launcher proceeds with empty bind-mounts; the agent performs its normal interactive login (paste-the-code OAuth fallback for Claude, device auth for Codex); capture on exit seeds the vault.
- R16. **`--import-from-host` (auto-tried).** When the vault is empty and host credentials are detected at the agent's documented path, the launcher imports them to the vault before starting the container. Successful import suppresses the interactive login prompt on first launch.
- R17. **`aileron auth <agent> --import-from-host` (explicit).** Idempotent CLI command that performs the same import as the auto-tried path. Required for re-imports after host-side rotation and for explicit user control.

### Host extraction support (v1)

- R18. **Linux.** Direct file read of the agent's documented credential path (`~/.claude/.credentials.json`, `~/.codex/auth.json`). When Codex is configured for keyring storage on Linux, the import fails with a message telling the user to set `cli_auth_credentials_store = "file"` and re-run.
- R19. **macOS.** Shell-out to `/usr/bin/security find-generic-password -w` against the agent-specific service name. User approves the Keychain dialog with "Always Allow" on first import per credential. The dialog appears only during explicit or auto-tried import, not during normal launches.
- R20. **Windows.** Deferred to v1.x. Windows users see the in-container login path on first launch; the launcher does not attempt host extraction.

### Bootstrap UX

- R21. The first launch with an empty vault prints a one-line status: either `[launcher] imported host credentials for <agent> to agents/<agent>/oauth` (auto-import succeeded) or `[launcher] no credentials in vault for <agent> — agent will prompt for login` (auto-import not attempted or failed).
- R22. The auto-import path is silent on success; failure (host file present but schema mismatch, Keychain denial, vault write error) downgrades to a one-line warning and proceeds to the in-container login path. Auto-import never blocks the launch.

### Documentation

- R23. `docs/development/sandbox-agent-auth.md` (new) documents the vault path scheme (`agents/<name>/<purpose>`), per-agent schemas, the auto-import-then-snapshot flow, the recovery path (`aileron vault delete agents/<agent>/oauth && relaunch`), and the deferred items.
- R24. A new ADR (or amendment to ADR-0024) records the `AuthSpec` contract, the in-container-login + capture lifecycle, and the deferral of Aileron-initiated OAuth pending vendor signal.

---

## Key Flows

- F1. **First launch, no host credentials.**
  - **Trigger:** `aileron launch claude --sandbox=docker`, vault has no `agents/claude/oauth`, no `~/.claude/.credentials.json` on host.
  - **Actors:** A1, A2, A3, A5
  - **Steps:** Launcher resolves `AuthSpec` → no vault hit → empty bind-mount at `/home/agent/.claude/`, plus the `~/.claude.json` static stub. Container starts. User runs `claude`, gets the paste-the-code OAuth prompt, authorizes on `claude.ai` in their host browser, pastes the code into the in-container terminal. Claude writes `/home/agent/.claude/.credentials.json`. Container exits. Launcher reads the bind-mount, runs Capture, writes to `agents/claude/oauth`. Transient bind-mount dir deleted.
  - **Outcome:** Vault seeded. Next launch is silent.
  - **Covers:** R6, R7, R8, R9, R10, R15, R21

- F2. **First launch, host credentials present (auto-import).**
  - **Trigger:** `aileron launch claude --sandbox=docker`, vault has no `agents/claude/oauth`, host has `~/.claude/.credentials.json` (Linux) or a Keychain item (macOS).
  - **Actors:** A1, A2, A4
  - **Steps:** Launcher detects host credentials, runs import (file read on Linux, `security` shellout on macOS), writes to `agents/claude/oauth`. Continues to F3 (steady-state launch) without restart. Container starts with credentials already rendered.
  - **Outcome:** Vault seeded. User sees a one-line import status; no login dance.
  - **Covers:** R16, R18, R19, R21, R22

- F3. **Steady-state launch.**
  - **Trigger:** `aileron launch claude --sandbox=docker`, vault has `agents/claude/oauth`.
  - **Actors:** A1, A2, A3
  - **Steps:** Launcher renders the vault entry to the bind-mount + env, bind-mounts into container, starts container. Claude reads `.credentials.json`, talks to vendor LLM endpoint via Aileron gateway (where applicable). Container exits cleanly. Launcher snapshots the (possibly rotated) credential file back to vault.
  - **Outcome:** Silent launch. Rotation persisted to vault.
  - **Covers:** R6, R7, R9

- F4. **Mid-session rotation, Claude.**
  - **Trigger:** Claude detects access-token expiry, exchanges refresh token against `claude.ai`, gets a new bundle.
  - **Actors:** A3, A5
  - **Steps:** Claude rewrites `/home/agent/.claude/.credentials.json` with the new `expiresAt` (and possibly new refresh token). Bind-mount surfaces the new bytes to the host side. Session continues. On exit, Capture catches the rotated bundle.
  - **Outcome:** Vault entry updated. Next launch uses the rotated tokens.
  - **Covers:** R8, R9

- F5. **Mid-session rotation, Codex.**
  - **Trigger:** Codex's `auth.json` reports `last_refresh` older than ~8 days, or the API returns 401.
  - **Actors:** A2 (pre-launch only), A3, A5
  - **Steps:** Pre-launch hook in launcher exchanges the vault's refresh token against `auth.openai.com/oauth/token` before container start; on rotation, the new bundle is written to vault before render. During the session, Codex's own client may also rotate and rewrite `auth.json`; Capture on exit picks it up.
  - **Outcome:** Vault entry stays fresh from either side.
  - **Covers:** R9, R13

- F6. **Forcible termination.**
  - **Trigger:** User runs `docker kill <container>` or the runtime crashes.
  - **Actors:** A2
  - **Steps:** Launcher cannot run Capture. The vault retains the prior credential. Transient bind-mount dir is cleaned up on next launcher invocation (or by an explicit cleanup pass).
  - **Outcome:** Worst-case, the next launch needs the agent to refresh against the vendor (Claude self-refreshes; Codex's pre-launch hook handles it). No data corruption.
  - **Covers:** R11

---

## Acceptance Examples

- AE1. **Covers R15, R21.** Given an empty vault and no host credentials, when the user runs `aileron launch claude --sandbox=docker`, then the launcher prints `[launcher] no credentials in vault for claude — agent will prompt for login`, the container starts with empty `/home/agent/.claude/`, the user completes the paste-the-code login inside the container, and on container exit `aileron vault list` shows `agents/claude/oauth`.
- AE2. **Covers R16, R22.** Given an empty vault and an authenticated host `claude` install (file `~/.claude/.credentials.json` exists on Linux), when the user runs `aileron launch claude --sandbox=docker`, then the launcher prints `[launcher] imported host credentials for claude to agents/claude/oauth`, the container starts with the credential rendered, and Claude does not prompt for login.
- AE3. **Covers R7, R12.** Given a vault entry at `agents/claude/oauth`, when the user runs `aileron launch claude --sandbox=docker`, then `/home/agent/.claude/.credentials.json` exists inside the container at startup with mode `0600` and contents matching the rendered vault entry.
- AE4. **Covers R4, R12.** Given any launch of Claude in sandbox mode, then `/home/agent/.claude.json` exists with `hasCompletedOnboarding: true` regardless of vault state; the agent does not display the theme picker.
- AE5. **Covers R8, R9.** Given a launch where Claude rotates its access token mid-session, when the container exits cleanly, then the vault entry at `agents/claude/oauth` reflects the new `expiresAt` and (if rotated) the new refresh token.
- AE6. **Covers R13, R14.** Given a vault entry at `agents/codex/oauth` whose refresh token is still valid but whose access token is expired, when the user runs `aileron launch codex --sandbox=docker`, then the launcher's pre-launch hook exchanges the refresh token and renders the fresh `auth.json` into the container; Codex starts and talks to OpenAI without an interactive prompt; the rotated bundle is in vault before container start.
- AE7. **Covers R11.** Given a launch that is killed with `docker kill`, then the next launch with an unchanged vault either succeeds (vault credential still valid) or triggers a refresh-then-launch (Claude self-refresh on startup, Codex pre-launch hook), without any user-visible state loss or vault corruption.
- AE8. **Covers R19.** Given a launch on macOS where auto-import attempts `security find-generic-password` and the user clicks "Always Allow" once for the Claude Keychain item, then subsequent imports succeed without re-prompting the user.

---

## Scope Boundaries

### In v1

- `AuthSpec` contract on `Agent` interface.
- Claude + Codex implementations of `AuthSpec`.
- Render + Capture launch lifecycle, sandbox mode only.
- In-container login + capture as the default seeding path.
- `--import-from-host` for Linux and macOS, both agents, auto-tried on empty vault and as an explicit CLI subcommand.
- Codex `cli_auth_credentials_store = "file"` emission into the ADR-0024 generated config.
- Documentation under `docs/development/sandbox-agent-auth.md`.
- New ADR (or ADR-0024 amendment) for the contract.

### Deferred for later

- `--import-from-host` Windows support — extraction code for Credential Manager. Tracked as v1.x.
- API-key vault bindings — additive to the same `AuthSpec` shape (`Type=api_key`, env binding only); ships when fleet / CI / cloud needs it.
- Host-launch parity for `AuthSpec` — clean to extend, but no fire to put out today.
- Cloud / remote-vault wiring — needs no contract changes; the `AuthSpec` shape is topology-agnostic. Reactivates when cloud Aileron itself reactivates.
- Codex Linux keyring extraction (`secret-tool` / libsecret) — users migrate to file mode by setting `cli_auth_credentials_store = "file"` on the host.

### Outside this contract's identity

- Aileron-initiated OAuth (Aileron runs the dance with the vendor's auth server) — vendor client-ID policy is unresolved for both agents; parked indefinitely. The in-container login path uses the agent's own client ID with the vendor, which is the supported mode and does not depend on policy movement.
- Aileron interposing on the agent's LLM traffic for subscription auth — Codex ChatGPT-mode sessions don't honor `OPENAI_BASE_URL`, so the gateway-routing trick that works for API-key auth doesn't apply. This is a separate concern from credential injection.

---

## Dependencies / Assumptions

- The sandbox runtime supports writable bind-mounts of host directories into the container. Verified for Docker and Podman on Linux and macOS today; SELinux contexts on RHEL-family hosts may require `:Z` on the volume — instrument and document if it surfaces.
- Vault encryption at rest (ADR-0023) is appropriate for refresh tokens with multi-month lifetimes. Open question #3 from issue #969 — likely fine, but worth a security review pass before v1 ships.
- The Claude Code and Codex CLI versions Aileron ships in Tier 1 images (#965) honor the documented credential file paths and env vars described in the v1 specs. Validate against the pinned versions; gate updates on regression tests.
- Vendor OAuth client IDs in Claude Code and Codex CLI continue to support the paste-the-code (Claude) and device-auth (Codex) fallback flows from environments that cannot bind a loopback redirect.
- Token rotation on the vendor side does not invalidate prior refresh tokens before the new one is persisted to vault. Assumed for both agents; not load-bearing on success because Capture is best-effort and the next launch can re-refresh, but worth instrumenting for visibility.

---

## Outstanding Questions

### Resolve before planning

- None blocking. The contract shape, seeding paths, and v1 scope are pinned.

### Deferred to planning / implementation

- Exact precedence when both an `EnvBinding` and a `FileBinding` exist for the same vault path (Claude's case). Render both? Prefer one based on agent declaration? Likely "render both — Claude's env-var wins per its precedence chain, file is a backup."
- Capture trigger policy: every clean exit, or only when the in-container file's mtime/hash differs from the rendered bytes? Every-clean-exit is simpler; change-detection avoids unnecessary vault writes. Default to every-clean-exit in v1; revisit if vault write volume becomes a concern.
- Concurrent-launch writeback race when two sandboxes for the same agent exit roughly simultaneously. v1 ships last-writer-wins with no distributed lock; refresh tokens survive the race because both writers exchanged the same upstream token. Document the race; revisit if observed.
- macOS Keychain dialog UX during auto-tried import — does Aileron prompt the user before invoking `security`, or run it silently and accept that the OS dialog is the user's first signal? Trade-off between explicitness and ergonomics.
- Per-agent secret-rotation telemetry — emit an event when Capture overwrites an existing vault entry, so users can see rotation activity. Not v1-blocking; deferred to telemetry pass.

---

## Sources / Research

Issue #969 carries the verified findings and citations in full. Key sources:

- **Claude Code auth surface:** `code.claude.com/docs/en/authentication`, `code.claude.com/docs/en/env-vars`, `support.claude.com/en/articles/12304248-manage-api-key-environment-variables-in-claude-code`. GitHub issues `anthropics/claude-code#11985`, `#34262`, `#4714` corroborate first-run state behavior and credential-file edge cases.
- **Codex CLI auth surface:** `developers.openai.com/codex/auth`, `developers.openai.com/codex/auth/ci-cd-auth`, `developers.openai.com/codex/config-reference`, `developers.openai.com/codex/environment-variables`. DeepWiki `openai/codex/4.5.5-authentication-modes-and-account-management`. GitHub issues `openai/codex#14704` (silent keyring fallback), `#5212` (`OPENAI_API_KEY` interactive-prompt bug), `#16728` (Keychain reliance), and unanswered community thread on client-ID reuse (`community.openai.com/t/best-practice-for-clientid-when-using-codex-oauth/1371778`).
- **Aileron code anchors:** `internal/launch/agent.go` (Agent interface, Mode enum, MCPMount), `internal/launch/launcher.go:452` (`composeAgentEnv`), `internal/launch/agents/claude.go`, `internal/launch/agents/codex.go`, `internal/vault/spi.go` (Vault SPI, Metadata.Type).
- **Related ADRs:** ADR-0023 (v4 vault-centric encryption — storage substrate), ADR-0024 (sandbox MCP parity — Codex `config.toml` generation path, needs the `cli_auth_credentials_store = "file"` patch from R14).
- **Related issues:** #747 (v4 milestone parent), #964 (`--agent` flag + ready-to-build scaffold — surfaced the in-container first-run UX), #965 (per-agent Tier 1 images — converges on zero-config first launch), #966 (cross-compile aileron-mcp for sandbox).
