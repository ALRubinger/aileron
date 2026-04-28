---
title: "Overview"
description: "One-sentence pitch, two-layer architecture summary, and the architectural insight behind the Aileron pivot"
order: 0
---

**Aileron is the deterministic execution layer for AI agents. Whether the agent invokes an action via standard tool-calling or Aileron matches unambiguous intent before the LLM runs, the execution is the same: capability-isolated, credentials sealed in a vault the LLM never touches, with tamper-resistant approval for consequential actions. When the LLM is genuinely required, Aileron routes to the cheapest model that meets quality on the user's hardware.**

It is two layers:

- **[Aileron Runtime](/pivot/runtime)** — a transparent OpenAI-compatible endpoint that intercepts every agent request. Aileron augments the agent's tool catalog with installed actions; the LLM selects what to call; Aileron executes the deterministic ones in capability-isolated sandboxes. For unambiguous intents, Aileron can bypass the LLM entirely. When no action applies, Runtime orchestrates inference engines per machine and routes to the cheapest model that meets the developer's quality bar.
- **[Aileron Control](/pivot/control)** — governance, vault, policy, approvals, and audit applied throughout, in the request path by construction.

These layers are unified by architecture, not marketing. The LLM endpoint is the only seam in the agent stack where you can guarantee, simultaneously: zero SDK integration, credentials never leaked to the model, deterministic action execution regardless of how the LLM coordinated the call, tamper-resistant user consent for consequential actions, and a complete audit trail. Every other interception point has a structural compromise.

> **Companion documents:** for concrete hero use cases and the developer experience, see [What Your Agent Can Now Do](/pivot/what-your-agent-can-do). For an at-a-glance contrast with adjacent approaches, see [Aileron vs Tool Calling vs MCP](/pivot/tool-calling-mcp-comparison) and the deeper [Competitive Landscape](/pivot/competitive-landscape).

---

## The Architectural Insight

Every agent in production speaks `chat/completions` (or its successor). That endpoint is the most concentrated decision point in the stack: it is where intent is expressed, where a model is asked to act, where credentials would be exposed if anyone were careless enough to put them there, and where probabilistic execution begins.

Compare the available interception points:

| Seam | What it can do | Compromise |
|---|---|---|
| Pre-tool-use hooks (Claude Code, Cursor) | Rewrite, block, or approve tool calls | LLM has already emitted the call; cost paid, intent shaped |
| MCP server | Expose tools to the LLM | LLM still orchestrates; LLM in the loop |
| Agent SDK middleware | Wrap framework operations | Requires SDK cooperation; framework-specific |
| Sandboxed agent process | Intercept system calls | Cannot read LLM-shaped intent |
| Post-LLM gateway | Filter, route, observe | LLM has run; cost and latency already paid |
| **LLM endpoint substitution** | **Run a deterministic action in place of the LLM call, or execute the LLM's tool calls under capability isolation** | **None — invisible to the agent at the boundary that matters** |

This is the seam where Aileron makes deterministic execution possible. The LLM may still select what to call (via standard function calling); Aileron ensures the *execution* is deterministic, sandboxed, audited, and tamper-resistant. For unambiguous intents, Aileron can also bypass the LLM entirely. Either way, the agent's tool calls land in a structurally safer place than what any current architecture provides.

---

## The One-Sentence Pitch

**Aileron is the deterministic execution layer for AI agents: it executes the tool calls agents make with capability isolation, sealed credentials, tamper-resistant approval, and a complete audit trail — and routes to the cheapest model that meets quality only when the LLM is genuinely required. The agent's tool calls have properties it can't perceive: deterministic execution and trustworthy consent.**

---

## Where to go next

- **[The Problem](/pivot/the-problem)** — the six pains in the current agent stack that motivate the pivot.
- **[Aileron Runtime](/pivot/runtime)** — interception, substitution, and routing in one process.
- **[Aileron Control](/pivot/control)** — governance, vault, policy, approvals, and audit in the request path.
- **[Architecture](/pivot/architecture/)** — the load-bearing decisions: connector model, action model, capability binding, sandboxing, user channel, and more.
- **[Why This Is Structurally Defensible](/pivot/why-defensible)** — what surrounds the seam Aileron occupies and why none of it can take it.
- **[The Customer](/pivot/customer)** — who Aileron is for in v1.
- **[The Business](/pivot/business)** — revenue surfaces atop the open-source distribution layer.
- **[What Aileron Is Not](/pivot/not)** — explicit non-goals.
