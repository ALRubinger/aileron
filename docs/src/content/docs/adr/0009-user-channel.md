---
title: "ADR-0009: User Channel and OOB Approval Surfaces"
description: "Streaming chat completions carry the inline pause-for-approval; consent decisions happen on out-of-band surfaces the agent cannot reach. v1 MVP ships with CLI prompt only; richer surfaces and hosted backend integration land in Phase 2."
order: 9
---


<div class="meta">
<table>
  <tr><th>Status</th><td>Accepted</td></tr>
  <tr><th>Date</th><td>2026-04-29</td></tr>
  <tr><th>Tracking</th><td><a href="https://github.com/ALRubinger/aileron/issues/343">#343</a></td></tr>
</table>
</div>

## Context

[ADR-0008](/adr/0008-intent-matching) establishes that Aileron exposes installed actions to agents over MCP and runs them through a single execution path on the daemon. [ADR-0006](/adr/0006-capability-binding-ux) and [ADR-0007](/adr/0007-install-consent) establish that several flows — credential binding, connector install, action update — require explicit user consent.

Each of those flows assumes the existence of a *user channel*: some surface where Aileron can ask the user a question and receive an authoritative answer. The question this ADR answers is what that channel actually is.

The constraint is structural and load-bearing:

> **The agent must never be in the trust path for approval decisions.**

If the agent could intermediate the approval prompt — see it, modify it, suppress it, fake the user's response — every protection earlier ADRs ratified would collapse. Prompt injection in the agent's context could become a way to silently approve installs, bind credentials to attacker-controlled accounts, or auto-approve consequential actions. The capability boundary is only as strong as the channel by which the user authorizes things across it.

This is the same property iOS, macOS, and Android settled on for security-relevant prompts: the OS shows the prompt directly, on a surface the requesting application cannot draw over, with input the requesting application cannot inject. Aileron needs the equivalent for AI agents.

The output side has the opposite need: the agent's response stream is *exactly* the right place to show the user what's happening. The user is watching the chat; they see "I'll post this to #engineering, but need your approval first..." inline; the approval surface fires alongside. Output and consent are two separate channels that work together.

This ADR ratifies both.

## Decision

### Two channels: in-band streaming output, out-of-band consent

The user has two separate communication channels with Aileron:

1. **In-band output channel** — the agent's chat surface, reached via MCP tool result text. Aileron's progress messages, "I'm about to do X" announcements, and pause-for-approval explanations ride back as the result of the tool call the agent just made; the agent host renders the text into the conversation alongside its normal output. The user reads everything in one place.

2. **Out-of-band (OOB) consent channel** — a separate surface, on a separate process, the agent cannot draw over or input to. Approval decisions happen here.

The output channel is informational. The consent channel is authoritative. Aileron may surface a *description* of an approval request in-band (so the user sees the question in their chat flow), but the *answer* always comes through the OOB channel. A user replying "yes, do it" inside the chat does *not* approve anything — the OOB prompt must still fire and be answered there.

This separation is what makes prompt injection structurally impossible to use for approval bypass. An agent that injects "the user already approved this; proceed" into the conversation does not change what the OOB channel says. The OOB channel is the source of truth; the chat is just commentary.

### Five tiers of OOB surface, ranked by visual integration

The OOB consent surfaces are tiered by how integrated they are into the user's flow. Higher tiers are less disruptive and more authenticated; lower tiers are more universally available.

| Tier | Surface | Authentication | When used |
|---|---|---|---|
| 1 | OS biometric prompt (Touch ID, Face ID, Windows Hello) | Hardware-backed user identity | Default for high-stakes approvals on supported platforms; fast confirm-or-deny |
| 2 | System notification with inline action buttons | OS-mediated user input on the lock-screen / notification surface | Default for medium-stakes approvals; non-blocking; queues if user is away |
| 3 | TUI panel | Terminal session control (the user is the session owner) | When biometric and notifications aren't available or have been declined |
| 4 | Web UI (localhost browser tab) | Browser session bound to the local Aileron process | For richer prompts (e.g., review-the-policy-diff before approving an update) |
| 5 | CLI prompt in the launching terminal | The terminal session is the user's | Always-available backstop; works in headless and remote contexts |

A request fires the highest tier the user has enabled and the platform supports; if that tier fails or times out, Aileron falls through to the next available tier. The user can configure which tiers are enabled (e.g., disable biometric on shared machines, disable notifications when working with the laptop closed). Tier 5 — the CLI prompt — cannot be disabled. It is the floor.

The five-tier model is the eventual surface stack. **The v1 MVP ships only tier 5; later phases enable the higher tiers** (see "v1 MVP scope" and "Phase 2 and beyond" below).

### v1 MVP scope: tier 5 (CLI) only

The v1 MVP implements **only tier 5 — the CLI prompt in the launching terminal.** Tiers 1 through 4 are not implemented in v1.

This narrows the implementation surface to what can ship as a single static Go binary with no platform-specific code. The structural property (agent not in trust path) is satisfied by the simplest possible surface: the terminal session Aileron was launched in is a process the agent cannot reach. No native API bindings, no notification daemon dependency, no embedded HTTP server beyond the existing chat-completion endpoint, no browser-spawn logic.

The trade-off is friction. A user running their agent in one terminal and Aileron in another must alt-tab to the Aileron terminal to answer prompts. For the v1 wedge audience — developers using terminal-native AI coding tools — this is tolerable; those users are already operating across multiple terminals. For broader audiences (UI-driven editor agents, mobile, hosted), the friction is unacceptable, which is why richer surfaces are scheduled for Phase 2.

**MVP also defers per-invocation action approval entirely.** The `[approval] required = true` manifest field is recognized, but actions that set it are rejected at install time in MVP. Per-invocation prompts are the use case where CLI hurts the most (the user is mid-conversation in their agent UI; alt-tabbing to a different terminal mid-stream is genuinely disruptive). The two flows MVP *does* support — install consent and capability binding — both fire from user-initiated terminal commands, where the user is already at the right keyboard and the prompt arrives in the same flow as their command. Per-invocation approval lights up when tier 2 (notifications) lands.

### Phase 2 and beyond: web UI, HTTP server, hosted backend

The next phase expands the surface stack and connects to a hosted Aileron backend:

- **Tier 4: web UI (localhost browser tab).** Aileron grows a richer HTTP server (beyond the chat-completion endpoint) that hosts an approval-prompt page. When a prompt fires, Aileron opens or focuses a localhost browser tab; the user clicks Approve / Deny. This eliminates the alt-tab friction for users with browsers available, and unlocks per-invocation approval for the broader UI-driven audience.
- **Tier 2: system notifications.** Native notification APIs per platform (UserNotifications on macOS, WinRT toast on Windows, libnotify on Linux). The least-disruptive surface for medium-stakes prompts.
- **Tier 1: OS biometric.** Touch ID, Face ID, Windows Hello. Hardware-backed authentication for high-stakes approvals on supported platforms.
- **Tier 3: TUI panel.** A richer terminal UI (alternate-screen panel, bubbletea-style) for users who prefer staying in the terminal.

In parallel, Phase 2 introduces **hosted Aileron** as a backend. The local Aileron process pairs with a cloud-hosted Aileron Control plane that:

- Routes approval prompts to mobile or web clients when the user is away from their development machine.
- Provides cross-device consistency — bind on laptop, approve from phone.
- Persists audit trails server-side for team and enterprise visibility.
- Coordinates approvals across multiple machines for users running Aileron in more than one place.

The web UI surface and the hosted backend share an HTML rendering layer: the page that renders an approval prompt at `localhost:8721/approve/:id` is the same page that renders the prompt at `https://control.aileron.dev/approve/:id` when paired with the hosted plane. The local-only path (web UI tier 4) and the hosted-paired path (mobile push, cross-device) are two configurations of the same UI.

Phase 2 will get its own ADR amendments specifying the HTTP server architecture, the pairing protocol with the hosted backend, and the security properties of cross-device approval. None of that is ratified here. What is ratified here is the *direction*: Phase 2 expands the OOB surface stack and adds a hosted control plane as a peer surface.

### Tier 0 (Aileron-aware agent plugins) is explicitly out of v1 scope

A "tier 0" surface — an Aileron-aware extension running inside the agent host (a Claude Code plugin, a Cursor extension) that displays approval prompts directly in the agent's UI — would be the most visually integrated tier of all. It is also the most structurally compromised: a plugin in the agent's process address space can be observed, manipulated, or co-opted by the agent. Prompt injection that targets the plugin could subvert approval decisions even with the plugin's UI rendering the question correctly.

V1 keeps the structural property simple by not allowing tier 0. Once we ship and exercise the OOB tiers in real usage, we may design a tier 0 with sufficient isolation guarantees (e.g., a sandboxed extension that draws to a separate window the agent cannot reach). That work is post-MVP.

The user-facing implication: integrating Aileron into a host like Claude Code makes the chat output rich (in-band channel works as expected) but approval prompts come through an OS notification, biometric prompt, or TUI panel — not through the host's own UI. This is by design.

### What requires approval

Three categories of operation use the user channel:

- **Install consent** ([ADR-0007](/adr/0007-install-consent)) — adding a new connector or action; updating to a new version with new capabilities.
- **Capability binding** ([ADR-0006](/adr/0006-capability-binding-ux)) — first use of an unbound credential, rebind after expiration, explicit `binding setup`.
- **Per-invocation action approval** — actions can declare in their manifest that each invocation requires user approval before execution proceeds. This is intended for consequential operations (sending money, posting to public channels, modifying production systems) where the action author wants the user's eyes on every individual call.

The first two are explicit user-initiated commands; the user is at the keyboard and the prompt arrives in the same flow as the command. The third is different: per-invocation approval fires *inside* an active agent conversation, asynchronously to anything the user typed.

### Per-invocation approval: action manifest opt-in

An action's manifest can declare:

```toml
[approval]
required = true
```

When the agent invokes such an action through `aileron-mcp`, the daemon's `POST /v1/actions/{name}/run` handler:

1. Validates the request and registers a pending approval entry in the action-approval queue.
2. Returns `202 Accepted` immediately with a structured pending-approval response: `{ approval_id, review_url, message }`. The `message` is the human-readable instruction the LLM is meant to surface verbatim — it names the action, the per-approval review URL, and the `aileron open approval <id>` shell command alternative.
3. Spawns a background goroutine that waits on the queue's decision channel — no timeout; the user can approve or deny whenever.

`aileron-mcp` translates the 202 into an MCP tool result whose text is the daemon's `message`. The agent's LLM reads the message and surfaces it to the user. The user then opens the per-approval review URL in the Aileron webapp (or runs `aileron open approval <id>` to launch the same surface from any terminal). The webapp shows the action name, the connector(s) it will exercise, the call-time arguments, and **Approve** / **Deny** controls. The agent retrieves the eventual outcome via `GET /v1/action-approvals/{id}/result`, exposed as the `check_action_status` MCP tool.

This is the in-band / OOB split:

- **In-band** is the MCP tool result text. It rides through the agent's normal tool-result path, gets rendered into the chat by the host, and instructs the user where to go. It does not contain decision controls.
- **OOB** is the Aileron webapp's `/approvals` surface, opened from a per-approval URL (or from `aileron open approval <id>`). It contains the actual Approve / Deny controls. Decisions outside this surface — including the user replying "yes, do it" in the chat — are not honored.

On approve, the daemon's goroutine invokes the action's executor, dispatches through the bound connector, and records the outcome against the approval ID. On deny, the goroutine exits without invoking the executor; no quota is burned; the audit log records the deny with the user's optional reason.

Connector authors and action authors should set `[approval] required = true` for any operation whose impact a user genuinely wants eyes on every time. The Hub may surface a "requires approval" badge on actions that opt in.

### Channel coordination: in-band describes, OOB decides

When an action requires per-invocation approval, both channels surface the same prompt:

- **In-band**: the MCP tool result text the LLM surfaces to the user. The text identifies what's being asked and names the OOB surface where the answer lives ("Approval needed for send-email on github://ALRubinger/aileron-connector-google. Visit https://&lt;webapp&gt;/approvals?focus=&lt;id&gt; to approve, or run 'aileron open approval &lt;id&gt;' from any terminal.").
- **OOB**: the Aileron webapp's per-approval review URL shows the action name, arguments, and Approve / Deny controls.

The in-band message points at the OOB surface so the user knows where to look. The OOB surface contains the actual decision controls.

If only the in-band suggestion appears to be answered ("yes, do it" in the chat) but the OOB surface is unanswered, the action does not proceed. The chat is not the source of truth.

### Channel selection on agent host integration

When a user runs an agent (Claude Code, Codex, others) under `aileron launch`, the integration points are:

- **In-band channel** — the MCP tool-result text the agent host renders. The host already speaks MCP and renders tool results into its chat surface; nothing Aileron-specific is needed in the host.
- **OOB channel** — the Aileron webapp's `/approvals` surface, plus future Phase 2 surfaces (system notifications, biometric prompts, TUI panel, CLI fallback). The host's UI is not used as a decision surface (per "tier 0 out of v1 scope").

The host needs no Aileron-specific code beyond MCP support and (when applicable) pointing its API base URL at the Aileron gateway for vault-locked checks and future request-level mediation. The OOB surfaces are owned and rendered by the Aileron process directly.

## Alternatives Considered

### Single approval surface only (rejected)

Aileron uses one approval surface (e.g., always the OS notification) and that is the only consent channel.

Rejected because no single surface is universally available. OS notifications are blocked in some environments; biometric prompts don't exist on Linux without configuration; web UIs require a browser; TUIs require a terminal; CLI prompts require an attached terminal session. Defaulting to a single tier means a real fraction of users hit a configuration where Aileron's consent flow is broken — and a broken consent flow either soft-fails (skip the prompt, security collapses) or hard-fails (action blocked, user sees a confusing error). A tiered fallback gives every user *some* surface that works.

### Approval through the agent's UI (tier 0 in v1) (rejected)

The agent host (Claude Code, Cursor) renders Aileron's approval prompts directly. The user clicks Approve / Deny in the same window where the chat lives.

Rejected because the agent's process is exactly the surface we cannot trust. A plugin or extension running in-process with the agent can be observed (the agent reads UI state), influenced (prompt injection alters what the plugin shows), or subverted (the agent forges the click event). The structural integrity of "the agent is not in the trust path" requires the approval surface to live outside the agent's reach.

This is deferred, not rejected forever. A future tier 0 with adequate isolation guarantees (sandboxed extension, separate window, OS-mediated input) is plausible. V1 defers the design.

### In-band approval (chat-mediated) (rejected)

The user types "yes" or "no" into the chat to answer approval prompts. There is no separate OOB surface.

Rejected for the same reason as tier 0: prompt injection in the agent's context could place "yes, approve" into the conversation in a way that looks like the user's input. Even if Aileron tries to authenticate the source of the approval message (timestamp, formatting heuristics), it cannot reliably distinguish user input from agent-injected input on the same channel. The OOB requirement exists precisely to break this ambiguity.

### SMS or email-based approval (rejected)

Approvals fire via SMS or email; the user clicks a link or replies to confirm.

Rejected because SMS and email are too async to fit the per-invocation flow (where an agent is waiting on the chat stream for a result), and the authentication is too weak (SIM swaps, email account compromise) to gate operations the rest of the system treats as security-critical. SMS or email may have a role for asynchronous notifications about *completed* actions ("Aileron sent an email on your behalf") but not as primary approval surfaces.

### A single global approval timeout for all surfaces (rejected)

All approval prompts time out after the same duration; no per-action override.

Rejected because per-invocation approvals during agent runs need to feel snappy (seconds), while pre-binding setups for headless workflows can tolerate minutes. Action authors should be able to specify reasonable timeouts in the manifest; the runtime enforces a hard cap (e.g., 5 minutes) on top of that.

## Consequences

### For users

- Every consequential prompt fires on a surface that *cannot* be drawn-over or proxied by the agent. Prompt injection cannot approve installs or trigger sends.
- The default surfaces (biometric + notification) are familiar UI patterns from existing OS interactions. There is no Aileron-specific UI to learn for the common case.
- The CLI fallback ensures consent works in every environment — headless, remote, restricted — even if richer surfaces are unavailable.

### For agent hosts

- Registering `aileron-mcp` and (when applicable) pointing at the Aileron gateway URL is the entire integration. No Aileron-aware UI code is required.
- The host's chat surface renders Aileron's in-band messages naturally — they arrive as MCP tool result text, the same path any other MCP server's tool results take.
- The host does not see, render, or respond to OOB consent prompts. Those happen on system surfaces (the Aileron webapp, OS notifications, biometric prompts) beside the host's window.

### For action authors

- Action manifests can declare `[approval] required = true` to gate every invocation behind user approval. Use this for consequential operations.
- The Hub may surface a badge for actions that require per-invocation approval, helping users assess what's installed.
- Per-invocation approval prompts show the action's name and the relevant arguments (recipient, channel, etc.) — author-supplied templates can shape what the user sees, but the runtime composes the final prompt to ensure the security-relevant fields can't be misrepresented.

### For Aileron runtime

- The MVP surface (tier 5 CLI) is part of the core runtime, pure Go, universally available. No platform-specific code.
- Phase 2 surface implementations are platform-specific and will live in the runtime. macOS will use native notification + Touch ID APIs; Windows will use WinRT toast notifications + Windows Hello; Linux will use libnotify + (optionally) PAM-mediated biometric on supported distributions. The web UI tier reuses Aileron's HTTP server.
- Channel coordination — the in-band message and the OOB prompt fire together — is implemented in the runtime's request-handling pipeline. The chat stream pauses; the OOB prompt fires; the resolution unpauses the stream.

### For audit and security

- Every approval event records: surface used, time-to-decision, outcome (approve/deny/timeout), invoking agent, action and arguments under review, audit ID.
- The audit log is the authoritative record of what was approved. A user reviewing the log can answer "what did I approve, and on which surface?" precisely.
- Surface fallback events are logged (Phase 2 and beyond, when multiple tiers exist): "biometric prompt unavailable, fell back to notification" — visible in the audit trail so users can spot configuration issues.

### Open implementation questions (deferred)

- *What happens when an approval times out, is denied, or fails to render — and how does that map to action retry or compensation?* — [ADR-0010](/adr/0010-failure-handling).
- *Where does the user configure surface preferences (biometric vs. notification, surface enablement)?* — user-level config in `~/.aileron/config.toml`. Surface preferences are personal, not shared.
- *What does a tier 0 (in-host) approval surface look like, and what isolation guarantees would it need?* — deferred to post-MVP. Not in v1.

## Examples

### MVP install consent (CLI prompt)

User runs an install command in the terminal where Aileron was launched:

```
$ aileron action add hub://aileron/ship-update@1.0.0
Resolving hub://aileron/ship-update@1.0.0...
  Fetching template...     ✓
  Verifying signature...   ✓

────────────────────────────────────────────────────────────────────
APPROVAL REQUIRED  (Aileron — install consent)
────────────────────────────────────────────────────────────────────

Add action: hub://aileron/ship-update@1.0.0

  Description: Posts a "shipped" announcement to a Slack channel
               with the merged PR link.

  Capabilities exercised:
    • github://aileron/slack@1.2.0  → chat:write, channels:read
    • github://aileron/git@2.1.0    → read

  File will be written to: actions/ship-update.md

────────────────────────────────────────────────────────────────────

[A]pprove   [D]eny   [V]iew full manifest

>
```

User types `A`, hits Enter. Aileron writes the action file and queues the connector installs (each with its own CLI prompt in the same terminal). No alt-tab needed because the user is already where the prompt fires.

### Per-invocation approval flow (MVP)

User chats with their agent (Claude Code, launched as `aileron launch claude`):

```
User: send the deploy summary email to the team
```

Claude reads its tool catalog, sees `send_email` from `aileron-mcp`, picks it. Claude Code dispatches the call over MCP. `aileron-mcp` makes a `POST /v1/actions/send-email/run` request to the daemon with `{ recipients: ["team@example.com"], subject: "Deploy summary 2026-04-29", body: "..." }`.

The action manifest declares `[approval] required = true`. The daemon's `RunAction` handler registers a pending approval entry and returns `202 Accepted` immediately:

```json
{
  "status": "pending_approval",
  "approval_id": "act-20260429T140322-3f9c1a",
  "review_url": "http://localhost:8721/approvals?focus=act-20260429T140322-3f9c1a",
  "message": "Approval needed for send-email on github://ALRubinger/aileron-connector-google. Visit http://localhost:8721/approvals?focus=act-20260429T140322-3f9c1a to approve, or run 'aileron open approval act-20260429T140322-3f9c1a' from any terminal."
}
```

`aileron-mcp` surfaces the `message` to Claude as the tool result text. Claude renders it for the user:

```
Assistant:
Approval needed for send-email. Visit
http://localhost:8721/approvals?focus=act-20260429T140322-3f9c1a
to approve, or run 'aileron open approval act-20260429T140322-3f9c1a'
from any terminal.
```

The user opens the URL (or runs the CLI alternative). The webapp's `/approvals` page shows the action name, the connector, the to/subject/body, and Approve / Deny controls. The user reviews, clicks **Approve**.

The daemon's background goroutine wakes, invokes the action's executor, dispatches the email through the bound Gmail connector, records the outcome against the approval ID. The agent (or the user) can poll `GET /v1/action-approvals/{id}/result` — surfaced as the `check_action_status` MCP tool — to learn the outcome:

```
status: completed
audit_id: audit-7c1f...
result: {"ok": true}
```

If the user denied:

```
status: denied
reason: wrong recipient
```

The action did not run; no quota burned; the audit log records the deny with the user's reason. Claude continues the conversation: "The email send was declined. (Audit ID: act-20260429T140322-3f9c1a, denied with reason: wrong recipient.)"