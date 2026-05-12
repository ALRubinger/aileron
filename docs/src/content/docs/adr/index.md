---
title: "Architecture Decisions"
description: "Architecture decision records for Aileron"
order: 0
---

This section holds the architecture decision records (ADRs) that define Aileron's architecture. Each ADR captures one decision: the context, the choice, the trade-offs, and the consequences.

## What's landed

- [ADR-0001: Manifest Format Conventions](/adr/0001-manifest-format) — Markdown body + TOML frontmatter for actions; pure TOML for connectors and project config; JSON for runtime IPC.
- [ADR-0002: Connector Model](/adr/0002-connector-model) — Connectors are sandboxed, content-addressed binaries; Aileron core ships only primitive capability types.
- [ADR-0003: Action Model](/adr/0003-action-model) — Atomic actions copied into the user's `~/.aileron/actions/` on install; depend on connectors with explicit version + hash + capability subsets; capability enforcement at the action boundary in addition to the connector boundary. v1 is user-level only.
- [ADR-0004: Dependency Resolution](/adr/0004-dependency-resolution) — Per-scheme FQN-to-fetch-URL mapping (Go modules tag conventions); content-addressed shared store; install/sync/check pipelines; offline-by-default after first install.
- [ADR-0005: Sandbox Choice](/adr/0005-sandbox-choice) — WASM Component Model is the connector sandbox; runtime mediates credential use so connectors never hold raw credentials. Non-WASM-capable connectors are out of scope for v1.
- [ADR-0006: Capability Binding UX](/adr/0006-capability-binding-ux) — Bindings are explicit user acts; install-time transparency, first-use binding, opt-in pre-binding; bindings are user-local, never project-committable; visible lifecycle commands (list/inspect/setup/rebind/revoke).
- [ADR-0007: Install Consent Flow](/adr/0007-install-consent) — Install is a single binary decision (install or cancel); no per-capability denial; signature failure is a hard fail; updates show capability diffs; headless mode requires explicit `--yes` per command.
- [ADR-0008: Action Exposure to Agents](/adr/0008-intent-matching) — MCP is the canonical surface for exposing Aileron actions to agents; the LLM gateway is a transparent reverse proxy. Single execution path through `POST /v1/actions/{name}/run` where the manifest's `[approval]` block is honored.
- [ADR-0009: User Channel and OOB Approval Surfaces](/adr/0009-user-channel) — In-band MCP tool result text describes; out-of-band surfaces decide. The agent is structurally never in the trust path for approvals. v1 MVP ships with the webapp `/approvals` surface plus CLI prompt fallback; system notifications, biometric, and a hosted Aileron backend are Phase 2.
- [ADR-0010: Failure-Handling Policy](/adr/0010-failure-handling) — Failures are visible and structured; closed-set failure-class taxonomy; idempotency is the default; bounded retry on retriable errors; v1 actions are linear with first-failure-terminates semantics. LLM fallback, conditional compensation, and per-call retry tuning are post-MVP enhancements.
- [ADR-0011: Local Credential Vault](/adr/0011-local-credential-vault) — Encrypted-at-rest local vault for user credentials. Argon2id KDF from a passphrase derives a key encryption key (KEK); AES-256-GCM envelope encryption protects bindings on disk. KEK held in memory for the runtime session, never persisted. TEE-backed and browser-enclave variants exist in code but are post-MVP.
- [ADR-0012: Local Daemon Architecture](/adr/0012-local-daemon-architecture) — Aileron runs as one long-lived user-scoped local daemon. CLI commands and `aileron launch` are thin clients that auto-spawn the daemon and connect over TCP-on-loopback at an ephemeral port advertised via `~/.aileron/daemon.json`. Vault unlock, sessions, and audit logs live in the daemon. Cloud deployments are a separate shape.
- [ADR-0013: Connector Hub and Trust Distribution](/adr/0013-connector-hub-and-trust-distribution) — The Hub points to connectors at their canonical `github://owner/repo` FQNs; it does not host artifacts and does not vet publishers. Trust is per-repo for v0.x with per-publisher (Microsoft model) as the v2 direction. Daemon owns Hub data; CLI and webapp are thin clients on `/v1/hub/*`. Sigstore deferred with three accepted limitations.
- [ADR-0014: Spawn Sandbox Technology](/adr/0014-spawn-sandbox-technology) — Per-platform mechanism for confining subprocesses spawned via [ADR-0002](/adr/0002-connector-model)'s spawn primitive. Linux uses pure-Go namespaces plus Landlock; macOS uses `sandbox-exec` with a generated SBPL profile; Windows uses job objects plus restricted tokens. Manifest is platform-neutral; runtime translates per OS. Unsupported platforms refuse to spawn with a structured error.
- [ADR-0015: Launch Audit Scope](/adr/0015-launch-audit-scope) — `aileron launch` is daemon-connection + MCP-registration + gateway-routing. The shell-shim (`aileron-sh`, `aileron.yaml` policy, Claude PreToolUse hook) is removed. Aileron audits actions Aileron executes — installs, bindings, gateway requests, action calls — via the [ADR-0010](/adr/0010-failure-handling) audit store. Commands the agent runs locally are outside Aileron's audit boundary.
- [ADR-0016: Approval-Time Preview Fetch](/adr/0016-approval-preview) — *Proposed.* An action manifest's `[approval]` block may declare a `preview` directive naming a read-only op the runtime calls before showing the approval prompt. The preview response is rendered into the prompt and never returned to the agent, so the user sees an authoritative summary of what they are approving instead of agent-supplied (and forgeable) hints. Motivated by non-reversible ops like `send-draft` whose inputs (an opaque `draft_id`) reveal nothing about the consequence of approving.

## The sequence

The eleven ADRs form a complete architectural ratification of Aileron's v1, tracked in [issue #343](https://github.com/ALRubinger/aileron/issues/343):

1. **Manifest Format Conventions** — *landed ([ADR-0001](/adr/0001-manifest-format))*
2. **Connector Model** — *landed ([ADR-0002](/adr/0002-connector-model))*
3. **Action Model** — *landed ([ADR-0003](/adr/0003-action-model))*
4. **Dependency Resolution** — *landed ([ADR-0004](/adr/0004-dependency-resolution))*
5. **Sandbox Choice** — *landed ([ADR-0005](/adr/0005-sandbox-choice))*
6. **Capability Binding UX** — *landed ([ADR-0006](/adr/0006-capability-binding-ux))*
7. **Install Consent Flow** — *landed ([ADR-0007](/adr/0007-install-consent))*
8. **Action Exposure to Agents** — *landed ([ADR-0008](/adr/0008-intent-matching))*
9. **User Channel and OOB Approval Surfaces** — *landed ([ADR-0009](/adr/0009-user-channel))*
10. **Failure-Handling Policy** — *landed ([ADR-0010](/adr/0010-failure-handling))*
11. **Local Credential Vault** — *landed ([ADR-0011](/adr/0011-local-credential-vault))*

Aileron is user-level by construction in v1 — actions live in `~/.aileron/actions/`, not in the project's repo. Project-level shared actions, team-coordination features, and the TEE / browser-enclave vault variants are post-MVP, deferred until the hosted backend ships.

## How to read these

- Each ADR is a *decision*, not a *design document*. Operational detail is included where it ratifies the decision; broader implementation detail belongs in code and PRs.
- **ADRs are editable until the first working MVP ships.** Real usage will surface assumptions worth revising; until MVP, an ADR is amended in place when its decision changes. Once MVP ships, this section will switch to the standard immutability convention: superseding decisions land as new ADRs that explicitly cite what they replace.
