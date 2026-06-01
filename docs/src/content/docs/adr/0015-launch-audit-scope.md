---
title: "ADR-0015: Launch Audit Scope — Aileron Audits Aileron, Not the Agent"
description: "aileron launch sets up the daemon connection, registers aileron-mcp, routes the LLM through the gateway. It does not intercept the shell. Audit covers actions Aileron executes, not commands the agent runs locally."
order: 15
---


<div class="meta">
<table>
  <tr><th>Status</th><td>Accepted</td></tr>
  <tr><th>Date</th><td>2026-05-11</td></tr>
  <tr><th>Tracking</th><td><a href="https://github.com/ALRubinger/aileron/issues/624">#624</a></td></tr>
</table>
</div>

## Context

> **Revision note, 2026-06-01:** This ADR bounds host launch. It removed fragile host shell interception and remains correct for `--sandbox=off`. The v4 container runtime changes the enforcement boundary because Aileron owns the image and can define `/bin/bash` inside it. Container-only shell mediation is tracked in [#801](https://github.com/ALRubinger/aileron/issues/801) and [ADR-0021](/adr/0021-v4-shell-layer-mediation); it does not revive the old host shell-shim model.

`aileron launch <agent>` was originally specified in [issue #63](https://github.com/ALRubinger/aileron/issues/63) as "the connected coding session." Its pitch had two halves:

1. **Aileron is the trust surface for actions the agent performs through us** — sending a Slack message, calling an external API with a credential, writing to a vault entry. This is the surface [ADR-0009](/adr/0009-user-channel) and [ADR-0010](/adr/0010-failure-handling) ratified: agent-defined intent, Aileron-defined enforcement, decision recorded on an immutable audit store.
2. **Aileron is also the trust surface for every shell command the agent runs locally** — `git push`, `rm -rf`, `gh pr merge`. This was the `aileron-sh` shell-shim layer: a `$SHELL` replacement that evaluated each command against an `aileron.yaml` policy with `allow` / `deny` / `ask` buckets, persisted decisions to a JSONL log, and prompted the user via the webapp's `/approvals` page when policy said "ask."

Half (1) is the current product. Half (2) is the original product. They have drifted apart, and the drift has consequences.

**What still works in half (1).** Action installs, action calls, gateway-routed LLM traffic, binding lifecycle, vault unlocks, sent messages — every one of these flows through code Aileron controls (the daemon, the gateway, the MCP server, the action engine) and lands an event on `audit.EventStore` (the [ADR-0010](/adr/0010-failure-handling) audit store, surfaced as `/v1/audit`). The user gets one place to look — the webapp `/approvals` queue and the audit history — and one decision surface — Aileron's binding/approval prompts. The trust surface is intact.

**What never quite worked in half (2).** The shell-shim assumed every coding agent honoured `$SHELL` (and was a stand-in for "use the system shell to exec commands"). Some did. Many do not:

- **Claude Code** honoured a configurable shell via `CLAUDE_CODE_SHELL`, with a hard-coded constraint that the binary name contain `bash`. We worked around this by installing a wrapper at `~/.aileron/bash`. This worked.
- **Pi.dev** ignored `$SHELL` entirely and resolved its shell from `~/.pi/settings.json`. We worked around this by writing the shim path into that JSON. This worked.
- **OpenAI Codex CLI** ignores `$SHELL`, reads the user's shell from `getpwuid_r`, and validates the binary name against a fixed allowlist (`bash` / `zsh` / `sh` / `pwsh` / `powershell` / `cmd`) — anything else falls back to `/bin/sh`. There is no override path. Shell interception is impossible without an upstream patch to Codex.
- **Goose** and **OpenCode** sit somewhere in between. OpenCode has a first-class `shell` config key; Goose runs commands through a "developer extension" whose shell-binary handling is not documented as stable.

Per-agent workarounds were tenable when there were two agents. The third and fourth agents — Codex, Goose, OpenCode — break the pattern. Codex specifically cannot be supported under the shim model at all. Either the shim becomes "the integration story we have for the agents we already support, and a different story for the rest," or it goes.

**What we learned from the actual audit log.** On the maintainer's machine, with sessions running daily: `~/.aileron/audit/audit-*.jsonl` contains only `action.installed` / `binding.created` events (the `audit.EventStore` events). Zero `ShellEntry` records. The shim wrapper script at `~/.aileron/bash` is not present. Whatever Claude Code is doing for shell execution on this machine, it is not routing through `aileron-sh`. The shim's audit value, on the path users actually run, is zero.

**What the shell-shim was trying to enforce vs. what Aileron's MCP + gateway already enforce.** The original #63 list of "what shell commands the agent runs" — `git`, `gh`, `task`, `go test`, `rm`, `curl` — is not where Aileron earns its trust claim. The trust claim is: when the agent decides to send a Slack message, post a tweet, write to the vault, hit an external API with a credential — Aileron is on the wire and the user is on the approval path. That happens through the MCP server (the agent invokes `mcp__aileron__*` tools) and through the gateway (the LLM calls themselves, including tool-call payloads). Neither of those depends on the shell.

Plain local commands the agent runs against the user's filesystem — `git status`, `go test`, `ls` — are not Aileron's domain. They never were. The shell-shim was an attempt to extend Aileron's reach to that domain, and the attempt has aged badly: it is fragile across agents, the policy schema (allow/deny/ask) is the kind of thing OS-level sandboxing exists for, and on the machine where it was supposed to be running, it is not running.

## Decision

`aileron launch <agent>` is the daemon-connection and tool-registration step. It does three things:

1. **Spawn (if needed) and resolve the daemon.** Per [ADR-0012](/adr/0012-local-daemon-architecture).
2. **Register the session and route the agent's LLM traffic through the gateway.** Set the agent's LLM-endpoint env (`ANTHROPIC_BASE_URL` for Claude Code, `OPENAI_BASE_URL` for Codex) to the daemon's URL. Agents that resolve the LLM endpoint from a config file rather than env (Pi, Goose, OpenCode in some shapes) are not gateway-routed by `launch` and run against the user's configured provider directly.
3. **Register `aileron-mcp` with the agent.** Aileron's tools (vault, comms, action invocation, approval queue) become visible as `mcp__aileron__*` from inside the agent.

The launcher does not:

- Replace `$SHELL` with a shim.
- Install a wrapper script in the user's home directory.
- Set `AILERON_REAL_SHELL`, `AILERON_AUDIT_DIR`, `AILERON_AGENT`, `AILERON_APPROVAL_URL`, or `AILERON_SESSION_ID` in the agent's environment for the shim's benefit. (`AILERON_URL` and the MCP-related env vars stay — the MCP server reads them.)
- Write per-project `aileron.yaml` policy files or read them.
- Audit shell commands the agent runs locally.
- Approve or deny shell commands the agent runs locally.

The audit boundary is: **every action Aileron executes is audited.** Action installs, action invocations, binding decisions, gateway-routed LLM requests, vault unlocks. The `audit.EventStore` ([ADR-0010](/adr/0010-failure-handling)) is the canonical record. `/v1/audit` is the read surface.

Commands the agent runs on the user's machine through the agent's own exec tool — `git`, `go test`, `rm` — are outside Aileron's audit boundary. If the user wants those mediated, the agent's own approval/sandbox layer is where that work lives (e.g. Claude Code's `--allowedTools` ruleset, Codex CLI's sandbox-mode + approval-policy, OS-level sandboxing like macOS App Sandbox). Aileron does not duplicate that machinery.

## Consequences

### Removed

- The `cmd/aileron-sh/` binary, its tests, and its build targets.
- `internal/launch/wrapper.go` (the `~/.aileron/bash` installer).
- `internal/launch/hook.go` (the Claude Code PreToolUse hook handler).
- `internal/launch/eval.go` (policy evaluation against `aileron.yaml`).
- `internal/launch/agents/normalize.go` (agent-specific command-string unwrap, used only by the shim).
- The `internal/policy/launch/` package (schema, loader, writer, defaults, builtin — the entire `aileron.yaml` policy machinery).
- `internal/app/handlers_shell_approval.go` and the `POST /v1/sessions/{session_id}/approvals/shell` endpoint in `internal/api/openapi.yaml`.
- The `audit.ShellEntry` record type and the daily-rotated JSONL log it wrote to, used only by the shim. (`audit.MessageEntry` and the `audit.EventStore` SPI stay.)
- The `Agent.ConfigureShell` and `Agent.NormalizeCommand` interface methods. Agents that wrote configuration files for the shim no longer have a hook for that — there is nothing to configure.
- The `aileron init` CLI command, the `aileron policy test` CLI command, and the policy-related sections of `aileron status` output.
- Any webapp UI that rendered the shell-approval card or surfaced the per-shell-command audit summary.

### Kept

- `audit.EventStore` and the entire `audit/` SPI ([ADR-0010](/adr/0010-failure-handling)).
- The `/v1/audit` read endpoint.
- The webapp's action-approval queue at `/approvals` — the routes that back it are `/v1/bindings/*` and the action-invocation flow, not the shell endpoint.
- `aileron-mcp` and the MCP-server registration on launch.
- The daemon's gateway and the LLM-endpoint env wiring.
- `aileron sessions list` (session registration was never tied to the shim; the daemon stamps `StartedAt` / `EndedAt` regardless).
- `audit.MessageEntry` and the comms-side message audit ([ADR-0009](/adr/0009-user-channel)).

### Migrations and footprint

- **Users with a project-local `aileron.yaml`.** The file is harmless to leave in place; the launcher and the daemon ignore it. `aileron status` no longer mentions it. Documentation removes it from the supported configuration surface.
- **Users who relied on the shell-shim audit log.** None at MVP — the audit log was empty in the maintainer's environment, and the project's MVP audience is the maintainer plus a small early-access group. If a future user needs per-command audit of their agent's local exec, the agent's own audit/approval layer (or OS-level audit like `auditd` / Endpoint Security Framework) is the right tool. Aileron does not re-enter this space.
- **The Agent interface change is a breaking refactor inside `internal/`.** No external `Agent` implementations exist; the only implementations live in `internal/launch/agents/`. No semver / API compatibility concern.
- **OpenAPI regeneration.** Removing the shell-approval endpoint regenerates `internal/api/gen/server.gen.go` per the source-of-truth rule in `CLAUDE.md`.

### What this enables

- **Codex CLI support.** `aileron launch codex` is now implementable: register `aileron-mcp` in `~/.codex/config.toml`'s `[mcp_servers]` block, set `OPENAI_BASE_URL` to the daemon, exec `codex`. No shim, no per-agent shell workaround, no incompatibility with Codex's `getpwuid_r`-based shell resolution.
- **Goose support.** `aileron launch goose` registers `aileron-mcp` in `~/.config/goose/config.yaml`'s `extensions:` block. Goose's `GOOSE_MODE=auto` (its default autonomous mode) is the only env we set; we do not need to intercept its developer-extension shell tool.
- **OpenCode support.** `aileron launch opencode` registers `aileron-mcp` in `opencode.json`'s `mcp` block. We do not set `shell` or `permission.bash`; OpenCode's own permission system stays in charge of its local exec.
- **A consistent integration story.** Every agent supported by `launch` is supported the same way: daemon up, session registered, gateway routed (where the agent has an env-controllable endpoint), `aileron-mcp` registered. New agents are a single small file in `internal/launch/agents/`, not a per-agent shim integration.

### Risk this accepts

- **The pitch narrows.** "Every command your agent runs is policy-checked" is no longer true. Aileron does not gate `rm -rf /`. The pitch becomes: "every action your agent takes through Aileron — credentials, external APIs, messages, vault — is mediated and audited." That is the pitch the code already supports.
- **A motivated agent can do harm Aileron does not see.** This was already true: a deliberately adversarial agent can exec commands directly without going through any shell, can write to files outside any sandbox, can call hardcoded URLs. The shell-shim never protected against a determined adversary; it protected against a careless one. The careless-adversary protection moves to the agent's own approval/sandbox layer and to the OS.
- **No `aileron.yaml` policy as code.** The "policy as code, reviewable in PRs" framing from #63 is gone. If the team-level shared-policy use case re-emerges, it lands as a new ADR with a different mechanism (likely tied to per-action policy, not per-command).

## Out of scope

- A replacement for the shell-shim. There is no plan to add one.
- Per-action policy code (e.g. a manifest field that says "this action requires extra approval on weekends"). Possible future work; not this ADR.
- OS-level sandboxing of `launch`-spawned agent processes. Out of scope for v1; a deliberate decision under [ADR-0009](/adr/0009-user-channel)'s "decisions live out of band" framing.

## References

- [Issue #63](https://github.com/ALRubinger/aileron/issues/63) — original `aileron launch` design
- [Issue #98](https://github.com/ALRubinger/aileron/issues/98) — Pi.dev support
- [Issue #622](https://github.com/ALRubinger/aileron/issues/622) — `aileron launch opencode`
- [Issue #623](https://github.com/ALRubinger/aileron/issues/623) — `aileron launch goose`
- [Issue #624](https://github.com/ALRubinger/aileron/issues/624) — `aileron launch codex` (the issue that surfaced the incompatibility)
- [Issue #419](https://github.com/ALRubinger/aileron/issues/419) — pty terminal wrap removal (predecessor reduction of launch's footprint)
- [ADR-0009](/adr/0009-user-channel) — User Channel and OOB Approval Surfaces
- [ADR-0010](/adr/0010-failure-handling) — Failure-Handling Policy (audit store SPI)
- [ADR-0012](/adr/0012-local-daemon-architecture) — Local Daemon Architecture
