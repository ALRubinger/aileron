# ADR-0009: Pivot from MCP Gateway to Deterministic Execution Plane

**Status:** Accepted
**Date:** 2026-04-05
**Supersedes:** ADR-0001, ADR-0002, ADR-0003, ADR-0008

## Context

Aileron was positioned as an MCP gateway — a proxy that sits between agent hosts and downstream MCP servers, intercepting tool calls for policy evaluation, approval routing, credential injection, and audit logging (ADR-0001). The Marketplace (ADR-0002, ADR-0003) allowed users to browse and install MCP servers from the official registry, and per-user/enterprise scoping (ADR-0008) governed server visibility.

The competitive landscape has shifted. Multiple vendors (Fiddler AI, Galileo Agent Control, Unleash) now offer agent control planes that monitor, intercept, and block agent actions. These solutions sit in the same architectural position as an MCP gateway — they are middleware that relies on the agent or downstream service to hold credentials and execute actions.

This creates two problems:

1. **Thin middleware is commoditizable.** Any MCP-aware proxy can intercept tool calls. The gateway model does not create durable differentiation.
2. **Agents still hold the keys.** When credentials live in the agent's environment (even if injected by a vault), prompt injection, context compression, or model errors can bypass safety rules because the enforcement layer is advisory, not structural.

## Decision

Aileron pivots from an MCP gateway to a **deterministic execution plane** for irreversible actions:

1. **Agents decide; Aileron acts.** Agents handle planning, research, and user interaction. All irreversible actions (sending email, scheduling meetings, making payments) are submitted as structured intents to Aileron. Aileron owns the credentials, evaluates deterministic policy, and executes the action itself.

2. **Aileron owns identity.** Users connect their external accounts (Gmail, Google Calendar, Outlook, payment rails) to Aileron via OAuth. Refresh tokens are stored in Aileron's vault. Agents never receive credentials — they receive only structured results.

3. **Intent tools replace proxied tools.** The MCP gateway no longer discovers and proxies tools from downstream servers. Instead, Aileron exposes a curated set of intent tools (`list_inbox_briefs`, `send_email_intent`, `request_purchase`, etc.) that are defined in code and versioned with the codebase.

4. **Two-phase flow for irreversible actions.** Read-only and reversible operations (list inbox, create draft) flow freely. Irreversible operations (send email, create calendar invite, issue payment) trigger policy evaluation and optional human approval. This matches how humans reason about risk.

5. **Protected Actions catalog.** The Marketplace is repurposed from an MCP server registry browser to a curated catalog of protected actions that Aileron owns and executes. Each card represents a capability (Email, Calendar, Payments) with its own account connection flow and policy defaults.

6. **Connectors execute directly.** The existing `connector.Connector` SPI (Execute with injected credentials) becomes the primary execution path. Connectors call external APIs (Gmail, Google Calendar, Stripe Issuing) directly rather than forwarding to MCP subprocess servers.

## Consequences

- The downstream MCP server concept (ADR-0001) is retired. `core/mcpclient/`, `core/mcpremote/`, and `DownstreamServer` configuration are deprecated.
- The MCP Registry proxy (ADR-0002) is removed. The Marketplace UI (ADR-0003) is repurposed as a Protected Actions catalog.
- Per-user/enterprise MCP server scoping (ADR-0008) is replaced by connected accounts scoping (per-user account connections with enterprise-level policy governance).
- A new `ConnectedAccount` model and `core/account/` package manage OAuth flows for external services, separate from the Aileron login flow in `core/auth/`.
- The `ActionIntent`, `EmailAction`, `CalendarAction`, `PaymentAction` types in `core/model/` are already designed for this model and require only minor additions.
- The policy engine, approval orchestrator, audit store, and vault are reused without structural changes.
- The MCP transport protocol (JSON-RPC 2.0 over stdio) is preserved — agent hosts still connect to Aileron as an MCP server. Only the tool definitions change.
- Long-term defensibility comes from the execution & identity graph (audit trail), financial infrastructure partnerships (virtual card issuance), credential vault stickiness, and developer network effects — not from the gateway position.
