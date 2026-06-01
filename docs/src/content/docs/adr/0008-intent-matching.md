---
title: "ADR-0008: Action Exposure to Agents"
description: "MCP is the canonical host-launch action surface; v4 sandbox launch supersedes this with generated HTTPS shims and the data-plane direction."
order: 8
---


<div class="meta">
<table>
  <tr><th>Status</th><td>Accepted</td></tr>
  <tr><th>Date</th><td>2026-05-11</td></tr>
  <tr><th>Tracking</th><td><a href="https://github.com/ALRubinger/aileron/issues/343">#343</a></td></tr>
</table>
</div>

## Context

> **Revision note, 2026-06-01:** This ADR remains accurate for the current host-launch integration path that registers `aileron-mcp` with agents. It is no longer the v4 sandbox-runtime architecture. For containerized launch, see [ADR-0018](/adr/0018-v4-single-binary-runtime), [ADR-0019](/adr/0019-v4-https-data-plane), and [ADR-0020](/adr/0020-v4-connector-specs-and-shims). The sandbox path uses generated HTTPS shims and the Aileron data plane rather than reviving `aileron-mcp` as an in-container runtime model.

[ADR-0003](/adr/0003-action-model) establishes that actions are atomic, declaratively-defined units that the agent invokes. It defers a load-bearing question: *how does an agent see and call an installed action?*

Two integration surfaces are theoretically available:

1. **Model Context Protocol (MCP).** Aileron ships an MCP server (`aileron-mcp`). The agent host (Claude Code, Codex, OpenCode, Goose, Pi, others) registers it as one of its MCP servers. Installed actions appear in the agent's tool catalog as MCP tools. When the LLM picks one, the agent host dispatches the call back to `aileron-mcp` over the MCP transport, which forwards it to the daemon's `POST /v1/actions/{name}/run` endpoint. The daemon executes the action server-side, honors the manifest's `[approval]` block, and returns the result.

2. **LLM-gateway in-band tool injection.** Aileron sits between the agent and its LLM provider. For every chat-completion / messages request, the runtime inspects the body, derives a function definition for each installed action, appends it to the request's `tools` array, and forwards upstream. When the LLM emits a tool call matching one of the injected actions, the runtime intercepts it in the response stream, runs the action locally, and synthesizes a tool-result message back into the conversation.

Surface (1) needs the agent host to support MCP. Every agent in Aileron's launch matrix does — `aileron launch claude` registers `aileron-mcp` via `--mcp-config`; `aileron launch codex` writes the same registration into `~/.codex/config.toml`; the Goose / OpenCode / Pi launchers do the equivalent in their respective config formats. Surface (2) needs the agent's API base URL to be redirectable. Some launch agents do this (claude → `ANTHROPIC_BASE_URL`, codex → `OPENAI_BASE_URL`); others don't (goose, opencode, pi resolve their LLM endpoint per-provider in their own config).

For host launch, this ADR ratifies that MCP is the canonical action-exposure surface and that the LLM gateway is a transparent reverse proxy with no in-band tool catalog augmentation or call interception.

## Decision

### In host launch, MCP is the only path actions reach the agent

Aileron exposes actions to agents via the Model Context Protocol, served by `aileron-mcp`. There is exactly one execution path for every action invocation, regardless of which agent is launched:

```
agent host → aileron-mcp (MCP transport) → daemon HTTP POST /v1/actions/{name}/run
   → action.Executor.Execute → connector op → audit + result
```

The daemon's `RunAction` handler is the chokepoint. When the manifest declares `[approval] required = true`, the handler registers an approval request, returns `202 Accepted` with a per-approval review URL, and waits for the user's decision before invoking the executor. When the manifest is unapproved, the handler executes synchronously and returns `200`. Both branches mint an audit ID. The ADR-0009 user channel and ADR-0010 failure semantics ride on this single path.

### The LLM gateway is a transparent reverse proxy

`POST /v1/chat/completions` and `POST /v1/messages` forward to the configured upstream LLM provider unchanged: same path, same method, same body, same headers, streaming preserved. The gateway never inspects the request body, never appends to the `tools` array, never watches the response stream for tool calls, never executes anything. SSE chunks reach the agent the moment upstream emits them (the proxy uses `FlushInterval = -1`).

Aileron is in the gateway path because it gives the runtime a vault check before the LLM is reached and is the natural mounting point for future request-level mediation (rate limiting, prompt logging, redaction, model gating). None of those features inject tools or intercept calls in-band; if any are added later, they operate on the request as-is.

## Why MCP rather than in-band tool injection

### One execution path is the trust property

Two paths means two implementations of every cross-cutting concern: approval gating, audit emission, retry policy, capability binding, error envelopes. Keeping them in sync is the work; getting them out of sync is the bug. The original in-band intercept loop ratified by an earlier draft of this ADR shipped with the approval gate on the MCP path only. Under `aileron launch claude`, `send-email` actions installed with `[approval] required = true` ran without consent because the LLM picked the in-band tool, the intercept engine called `executor.Execute` directly, and the gate in the HTTP handler was never reached. The discovery of that gap is what motivates the rewrite of this ADR.

A single execution chokepoint makes the trust story enforceable by construction: any future caller — MCP, a CLI subcommand, a webapp action button, a future SDK — goes through the same handler that consults the manifest and the approval queue. No parallel implementation can drift.

### MCP is universal in the launch matrix

When the original draft of this ADR was written, MCP was newer and not all agents supported it. The argument for in-band injection was: if we only support MCP-aware agents, we leave most users behind. That argument has expired. Every agent in Aileron's launch matrix — claude, codex, opencode, goose, pi — now speaks MCP and is configured by `aileron launch` to register `aileron-mcp`. The "non-MCP agent" use case is not currently in the product.

If a future non-MCP agent appears that warrants gateway-side injection, the conversation can be reopened. Until then, supporting two paths to serve a hypothetical user is the wrong trade against the security gap two paths produce.

### Tool naming, parameter schema, and description ownership are MCP concerns

Under MCP, the agent host advertises the server's tools to the LLM directly. Aileron contributes:

- **Tool name**: snake_case mapping of the manifest's kebab-case `name` (e.g., `ship-update` → `ship_update`), matching the OpenAI / Anthropic function-calling convention the LLM was trained on.
- **Parameter schema**: derived from the action manifest's `[[inputs]]` block ([ADR-0003](/adr/0003-action-model)).
- **Description**: the action's Markdown body — the same prose [ADR-0001](/adr/0001-manifest-format) and [ADR-0003](/adr/0003-action-model) name as the LLM-facing function description. Authors who write good action documentation see the LLM make better choices; documentation IS prompt engineering at this layer.

The agent host renders these into whatever shape the LLM expects (OpenAI chat-completion `tools[]`, Anthropic messages `tools[]`, etc.). Aileron does not need to know the upstream shape; MCP brokers it.

### Tool name collisions

The agent host owns the agent's overall tool catalog. When two MCP servers (or an MCP server plus the agent's built-in tools) declare the same tool name, the agent host chooses how to disambiguate — typically by namespacing one or both. Aileron does not police naming across the host's other tool sources.

The `aileron-mcp` server's own tools are namespaced in the agent host's catalog under the server's MCP identifier (e.g., `mcp__aileron__send_email` in Claude Code's `--allowedTools` form), so the LLM and the host both see unambiguous tool identities even when an unrelated tool elsewhere shares a bare name.

## Alternatives Considered

### LLM-gateway in-band tool injection (rejected — was previously accepted, retired 2026-05-11)

Aileron sits at the LLM endpoint, augments the tool catalog the LLM sees, and intercepts matching tool calls in the response stream to execute actions locally.

This was the design previously ratified by this ADR. It is rejected now because it produces two parallel execution paths (MCP and intercept), only one of which honored the manifest's `[approval]` block. The result was an ungated execution path under `aileron launch` for every action installed with approval required. The fix-in-place option (also gating the intercept path) was rejected in favor of removing the second path entirely:

- Every cross-cutting concern (approval, audit, retry, capability binding) had to be implemented twice and kept in sync. The synchronization is the whole maintenance cost, and getting it wrong is the security failure.
- Every agent currently in Aileron's launch matrix speaks MCP. The user the in-band path was meant to serve does not exist in the product.
- The supporting code (~4,000 LOC across `internal/intercept` and `internal/augment`, plus the gateway dispatcher branches) added meaningful surface area to an already non-trivial daemon for capability that MCP delivers natively.

If a future product surface appears that needs in-band injection — a deliberately non-MCP integration where redirecting the API URL is the only seam — the design can be revisited with the approval gate as a first-class concern. Until then, the simpler one-path architecture is a strict win.

### Replace the agent's tools entirely (rejected)

Aileron strips the agent's declared tools and replaces them with its own. The LLM only sees Aileron's actions.

Rejected because it breaks every agent that already works. An agent host has tools for a reason — codebase search, file reading, terminal execution, project-specific integrations — and stripping those would render the agent useless. Aileron's value is *additive*: it adds capability the agent didn't have. Replacing what the agent already provides is destructive in a way the use case doesn't justify. Under MCP this is a non-issue: the agent host merges Aileron's MCP tools with its own; nothing is replaced.

### Side-channel intent matching only (no tool exposure to the LLM) (rejected)

Aileron does not expose actions as tools at all. Instead, it inspects every chat completion request, matches the user's intent against installed actions via patterns, and either executes a matched action server-side or passes the request through unchanged.

Rejected because it severely limits the surface where Aileron can act. The vast majority of useful agent intents are not expressible as exact-phrase patterns. "Tell the team I shipped the auth migration to staging by EOD" doesn't match a regex; it matches the LLM's understanding of "tell-the-team-something" semantics. Without function-calling exposure, Aileron only catches the simplest cases and misses the conversational ones — exactly the cases where deterministic execution is most valuable.

### Match agent intent through a single dispatcher tool (rejected)

Aileron registers a single MCP tool named `aileron.match` that the LLM calls with `{ intent: "..." }`. Aileron then dispatches to the right action server-side.

Rejected because it inverts the LLM's role. The LLM is good at picking among many well-described functions; it is not as good at synthesizing a freeform intent string and trusting an opaque server-side dispatcher to do the right thing. Surfacing each action as its own MCP tool lets the LLM see exactly which action is being chosen and which arguments are being passed; the `aileron.match` approach hides those details inside Aileron's dispatcher and obscures the trust decision the LLM is making.

## Consequences

### For agent hosts

- The agent host must support MCP. Every agent currently in Aileron's launch matrix does. Hosts wanting to use Aileron without MCP support are out of scope; the cost-benefit no longer favors building a second exposure path.
- The agent host receives Aileron's actions through its existing MCP integration. No special handling required — Aileron's tools look identical to any other MCP server's tools.
- Tools the host already declares (its built-ins, other MCP servers) are unaffected. Aileron is additive only.

### For the LLM

- Aileron's actions appear in the agent host's tool catalog alongside everything else the host advertises. The LLM picks among them on description fit, just like any other tool.
- Function descriptions are the action's Markdown body. Good action documentation produces good tool selection.

### For action authors

- The Markdown body of the action file is the LLM's prompt for whether to invoke the action. Write descriptions that match how users actually phrase intents — that prose is the only signal the LLM uses to pick the action.

### For the runtime

- The single execution path is `aileron-mcp → POST /v1/actions/{name}/run → action.Executor`. Every cross-cutting concern (approval gating per the manifest's `[approval]` block, audit emission, retry policy, capability binding, failure envelopes per [ADR-0010](/adr/0010-failure-handling)) is implemented exactly once.
- The LLM gateway (`POST /v1/chat/completions`, `POST /v1/messages`) is a transparent reverse proxy. It does not read the request body, does not consult the action store, and does not call the executor.
- The audit log records every action execution (full detail). Gateway requests are not audited at the request-body level — they're proxied opaquely.

### For users

- Adding Aileron is one `aileron launch <agent>` invocation. The launcher writes the agent's MCP config (registering `aileron-mcp`) and starts the agent.
- Actions installed via `aileron action add` are immediately available to the agent on the next tool-list refresh from MCP.

### Open implementation questions (deferred)

- *What surfaces does the agent see when an action requires user approval mid-execution (e.g., "send email" with a recipient list that needs confirmation)?* — [ADR-0009](/adr/0009-user-channel).
- *How does action invocation handle partial failure — connector call succeeds but commit fails, action declares retriability, etc.?* — [ADR-0010](/adr/0010-failure-handling).

## Examples

### Tool-call flow under `aileron launch claude`

User runs `aileron launch claude`. The launcher writes Claude Code's `--mcp-config` to register `aileron-mcp` as an MCP server, sets `ANTHROPIC_BASE_URL` to point at the daemon's gateway (so the gateway sees the LLM traffic for vault-locked checks and future request-level mediation), then execs `claude`.

User asks Claude: "Tell the team I shipped the auth migration."

1. Claude Code reads its tool catalog. From `aileron-mcp`, it sees `ship_update` (description: the Markdown body of the `ship-update.md` action). From its own built-ins, it sees `Bash`, `Read`, `Write`, etc.
2. Claude Code sends a `POST /v1/messages` to the daemon's gateway. The gateway forwards verbatim to `https://api.anthropic.com/v1/messages`. The Anthropic LLM response includes a `tool_use` block calling `ship_update` with `{channel: "#engineering"}`.
3. Claude Code receives the response stream, sees the `tool_use` for `ship_update`, dispatches it via the MCP transport to `aileron-mcp`.
4. `aileron-mcp` makes a `POST /v1/actions/ship-update/run` HTTP request to the daemon with the arguments.
5. The daemon's `RunAction` handler reads the manifest. `[approval]` not declared, so it executes synchronously: invokes the bound Slack connector, posts to `#engineering`, returns the result + audit ID.
6. `aileron-mcp` forwards the result back to Claude Code over MCP.
7. Claude Code sends the next turn's request (assistant content + tool result) back to the gateway, which forwards verbatim to Anthropic. Anthropic emits the final assistant message: "Posted to #engineering."

The LLM round-trips through Aileron's gateway. Tool execution happens through MCP, hits the daemon's HTTP API, and runs through the single execution path where the manifest's `[approval]` block (had it been set) would gate.

### Tool-call flow with an approval-gated action

Same setup. User asks Claude: "Email Alice the launch summary."

The flow is identical through step 4. At step 5, the manifest declares `[approval] required = true`. The daemon's `RunAction` handler:

- Registers a pending approval entry in the queue.
- Returns `202 Accepted` with `{approval_id, review_url, message}` — the message names the per-approval review URL and the `aileron open approval <id>` shell alternative.
- Spawns a background goroutine that waits on the queue's decision channel.

`aileron-mcp` surfaces the message verbatim to Claude as a tool result. Claude reads the message, surfaces it to the user. The user opens the URL (or runs the CLI alternative), reviews the to/subject/body that the agent constructed, approves or denies.

On approve, the daemon's background goroutine invokes the executor, dispatches the email through the bound Gmail connector, records the outcome against the approval ID. The agent polls `GET /v1/action-approvals/{id}/result` (via the `check_action_status` MCP tool) and observes `status: completed`.

On deny, the goroutine exits without invoking the executor. The agent's poll observes `status: denied`. No mail is sent, no quota is burned, the audit log records the deny with the user's reason.

There is exactly one place this gate is checked: `internal/app/handlers.go`'s `RunAction`. There is no parallel path the LLM stream could take that bypasses it.
