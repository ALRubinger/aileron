# ADR-0013: Pivot from Cloud Execution Plane to Local Policy-Enforced Shell

**Status:** Accepted
**Date:** 2026-04-08
**Supersedes:** [ADR-0009](0009-deterministic-execution-plane.md) (Pivot from MCP Gateway to Deterministic Execution Plane)

## Context

ADR-0009 positioned Aileron as a cloud-hosted deterministic execution plane. Agents submitted structured intents to a server, which owned credentials, evaluated policy, and executed irreversible actions (email, calendar, payments) on behalf of users. The model required a running server, connected OAuth accounts, and a Protected Actions catalog.

Two problems emerged:

1. **The execution plane solves the wrong problem first.** Developers using AI coding agents don't need a cloud service to send emails on their behalf. They need their agent to stop asking permission for `go test` while still blocking `git push origin main`. The immediate pain is the 50-prompt approval session, not enterprise action governance.

2. **The cloud-first model creates adoption friction.** A running server, database, OAuth configuration, and account connections are prerequisites before any value is delivered. Developers want to install a tool and see results in minutes, not hours.

The competitive landscape confirms this: coding agents (Claude Code, Codex, Goose, OpenCode) are where developers interact with AI daily. The control gap is at the shell level — what commands the agent can run, what credentials it can access, and what happens when a teammate asks a question on Slack while the agent is working.

## Decision

Aileron pivots from a cloud execution plane to a **local-first CLI tool** centered on `aileron launch`:

1. **One shim catches everything.** `aileron launch claude` spawns the agent with `SHELL=aileron-sh`. Every command the agent runs goes through the shim. No per-command wrappers, no agent-specific hooks, no server required. This works with any agent that respects `$SHELL`.

2. **Policy as code replaces server-side policy.** An `aileron.yaml` file in the repo defines three buckets: allow (auto-approve), deny (hard block), ask (prompt the developer). The policy is version-controlled, reviewable in PRs, and shared with the team. Community-maintained profiles (`lang/go`, `os/darwin`) provide sensible defaults.

3. **Terminal-native UX replaces web UI.** Aileron runs the agent inside a pty proxy, reserving terminal lines for a notification bar. Shell approval prompts use `/dev/tty` — the agent can't see or interact with them. A hotkey opens a full-screen notification overlay for Slack/Discord messages. No browser, no separate app.

4. **Local credential brokering replaces connected accounts.** Secrets are stored in a local encrypted vault (reusing the zero-knowledge architecture from ADR-0010). The agent uses a generic `http_request` MCP tool; Aileron matches the target URL to a configured secret, injects the credential, and returns the response. One tool covers any REST API — no per-service connectors needed for basic credential brokering.

5. **Local audit trail replaces centralized audit store.** Every decision is logged to an append-only JSONL file. `aileron log` provides filtering and session summaries. No database required.

6. **Agent-portable.** Policy, credentials, and audit trail are agent-agnostic. Switch from Claude Code to Codex to Goose and everything carries over. The `.editorconfig` of agent security.

## What stays

- **Zero-knowledge vault** (ADR-0010): The Argon2id + AES-256-GCM architecture is reused for local secret storage. The passphrase-derived KEK model is unchanged.
- **TEE support** (ADR-0011, ADR-0012): Parked for MVP but architecturally load-bearing for the post-MVP enterprise layer (see below). The provider SPI and Confidential Space implementation remain in the codebase.
- **Policy engine** (`core/policy/`): The `RuleEngine` and condition evaluation logic are reused. The `aileron.yaml` schema translates to the existing `PolicyRule` types.
- **Auth system** (ADR-0005, ADR-0007): The server-side auth, OAuth providers, and enterprise account model remain for the hosted control plane. They are not part of the `aileron launch` flow.
- **Server and API**: The HTTP server, OpenAPI spec, and generated handlers remain. They serve the hosted control plane and may be used for team-level policy management in the future.

## What changes

| Before (ADR-0009) | After |
|---|---|
| Cloud-hosted server required | Local CLI, zero infrastructure |
| Structured intent submission | Shell command interception via `$SHELL` |
| Connected OAuth accounts | Local encrypted vault + URL-based credential brokering |
| Protected Actions catalog (web UI) | `aileron.yaml` policy file (code) |
| Server-side policy evaluation | Local policy evaluation in `aileron-sh` |
| Web-based approval UI | Terminal-native `/dev/tty` prompts |
| Centralized audit store (PostgreSQL) | Local JSONL audit log |
| Single agent host (MCP) | Any agent that respects `$SHELL` |

## Consequences

- The primary developer experience is `aileron launch <agent>`, not a web dashboard.
- The `aileron-sh` shell shim and `aileron.yaml` schema become the core product surfaces.
- The MCP server (`aileron-mcp`) is repurposed from intent submission to credential brokering and communication tools (`http_request`, `send_message`, `read_messages`).
- Community policy profiles become a contribution surface — the `.gitignore` templates model.
- First-experience target: under 10 minutes from install to a working `aileron launch claude` session with policy enforcement.

## Post-MVP: remote execution and the enterprise layer

The cloud execution plane from ADR-0009 is deferred, not discarded. Local credential brokering works for API keys and bot tokens, but a class of actions is better served by remote execution:

- **OAuth-managed credentials** (Gmail, Google Calendar, Stripe) require refresh token flows that shouldn't live on a developer's laptop. Tokens should be housed in a secure enclave, not on disk.
- **Enterprise policy** may demand that certain credentials never touch developer machines — the TEE provides hardware-enforced isolation.
- **High-stakes irreversible actions** (send email on behalf of the org, issue a payment) benefit from organizational audit and approval workflows that survive a closed laptop.
- **Uptime** — remote execution decouples action completion from the developer's session.

The existing server, vault, TEE, connector SPI, and approval orchestrator are the foundation for this layer. The architecture is: `aileron launch` handles shell policy and local brokering (the developer UX); the hosted control plane handles remote execution and enterprise governance (the organization UX). The local tool is the on-ramp; the cloud layer is the upgrade path.

This is a future phase — the MVP focuses entirely on the local-first experience.

## Implementation

The work is tracked in four issues:

- [#63](https://github.com/ALRubinger/aileron/issues/63): `aileron launch` — launcher, shell shim, pty proxy, audit log
- [#64](https://github.com/ALRubinger/aileron/issues/64): Policy schema — `aileron.yaml` parser, profile loading, engine translation
- [#65](https://github.com/ALRubinger/aileron/issues/65): Terminal UX — bidirectional Slack/Discord communication, notification bar, hotkey overlay
- [#66](https://github.com/ALRubinger/aileron/issues/66): Product vision and positioning
