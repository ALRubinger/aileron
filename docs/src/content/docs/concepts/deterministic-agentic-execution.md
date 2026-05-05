---
title: "Deterministic Agentic Execution"
description: "Three concerns are in play whenever an agent takes action: proposing what to do, exposing what's available, and executing the chosen one. Tool calling and MCP cover the first two. Aileron is positioned as an answer to the third."
order: 1
---

Tool calling and MCP are established conventions that let LLMs propose actions and let services expose tools. Both are widely used. While there are reasonable critiques of the approaches of each, both MCP and tools indisputably form the backbone of modern agent capability.

**Aileron is new**. It's positioned alongside both, providing a deterministic execution path for the actions the LLM proposes — credential-sealed, capability-bounded, and audited.

Aileron's value shows up specifically when the agent needs to take consequential action in the real world.

## Three concerns

Three concerns are in play whenever an agent takes action: **proposing** what to do, **exposing** what's available, and **executing** the chosen one. Tool calling answers the first as an LLM-level protocol; MCP answers the second as a standard for tool exposure; the third — execution — has historically been treated as an implementation detail of whatever host happens to be running the call. Aileron is positioned as a deliberate answer to that third concern. The sections below cover each in turn.

|  | What it answers | What it doesn't |
|---|---|---|
| **Tool calling** *(established)* | Lets the LLM propose actions in a structured way | Doesn't decide what gets executed, doesn't hold credentials, doesn't enforce policy |
| **MCP** *(established)* | Standardizes how tools are exposed and discovered across hosts | Doesn't change who holds credentials or how execution is enforced |
| **Aileron** *(new)* | Makes the execution itself deterministic, credential-sealed, and audited | Doesn't replace tool calling or MCP; it composes with both |

Each concern can be addressed independently. Combined, they give you an agent that can post the deploy update, file the bug, and charge the customer — without ever holding your Slack token, your Linear API key, or your Stripe credentials.

## Proposing — Tool calling

Tool calling is an **LLM capability**. The agent's host code declares a set of functions in the chat completion request; the LLM picks one and emits a structured tool call; the host runs the function and feeds the result back. This is how every modern agent — Claude Code, Cursor, Continue, Aider, Copilot, custom apps — talks to the outside world.

Tool calling is the *protocol* by which the LLM proposes actions. It's flexible: any callable function can become a tool. It's universal: every major model supports it.

**What it's good at:** rapid prototyping, exploration, fitting LLM intent to function APIs.

**What it doesn't do:** it doesn't say anything about *who runs the function* or *what credentials they hold*. The host code that registered the tool is fully in charge of execution. If the host is your IDE plugin, your Slack credential lives in the IDE plugin's process. If the host is a script, the credential lives in the script.

## Exposing — MCP (Model Context Protocol)

[MCP](https://modelcontextprotocol.io) is a **standard for exposing tools** to LLM hosts. Instead of every host writing its own tool implementations, an MCP server provides tools that any MCP-aware host can consume. Run a Slack MCP server, and Claude Code, Cursor, and your custom agent can all use it without separate integration work.

MCP standardizes the surface for tool exposure across hosts. It turns "build N tool integrations into M agent hosts" (an N×M problem) into "build N tools and M consumers, separately."

**What it's good at:** ecosystem leverage, organizational scaling, separating tool implementation from host implementation.

**What it doesn't do:** it doesn't change the trust model. The MCP server still holds credentials. The host still has to trust the MCP server. The LLM still controls when and how the tool is invoked. MCP is a transport convention and an exposure mechanism, not an execution-safety story.

## Executing — Aileron

Aileron is a **deterministic execution path** for what the LLM proposes. Installed *actions* — declarative, version-pinned, capability-bounded units of work — are surfaced to the agent through Aileron's MCP server. Any agent that already speaks tool calling and consumes MCP servers can use them: no SDK changes, no framework integration, no plugin in the agent host.

When the LLM picks an Aileron action, the agent routes the tool call back to the Aileron runtime, which executes it through the connector sandbox, returns the result, and writes the audit record.

What this gets you:

- **Sealed credentials.** Connectors run in a WASM sandbox ([ADR-0005](/adr/0005-sandbox-choice)) and never see raw credential bytes. The runtime mediates outbound HTTP requests, injecting credentials host-side. Even a malicious connector binary can't exfiltrate the credential it was given.
- **Capability bounds.** Each connector declares exactly what it needs (network destinations, credential kinds, host functions) in a manifest ([ADR-0002](/adr/0002-connector-model)); each action declares the subset it actually uses ([ADR-0003](/adr/0003-action-model)). Out-of-bounds calls are denied at the sandbox boundary.
- **Out-of-band approval.** Anything consequential that needs your eyes ([ADR-0009](/adr/0009-user-channel)) prompts you on a surface the agent cannot draw over or fake — your terminal in v1; OS notifications, biometric prompts, and a hosted backend in Phase 2.
- **Visible failure.** Errors are structured, surfaced to the agent, and recorded in the audit log ([ADR-0010](/adr/0010-failure-handling)). No silent retries, no LLM-generated fallbacks pretending the action succeeded.
- **Local-first.** Actions live in your home directory. Connectors live in a content-addressed store ([ADR-0004](/adr/0004-dependency-resolution)). Once installed, action invocation is fully offline. Your credentials are encrypted at rest with a passphrase you control ([ADR-0011](/adr/0011-local-credential-vault)).

**What it's good at:** running consequential actions safely. Sending the email, filing the ticket, charging the customer, posting to Slack — operations where the cost of "the LLM did the wrong thing" is real.

**What it doesn't do:** it doesn't replace tool calling (it uses it). It doesn't replace MCP (an Aileron action and an MCP tool are different things; you can use both in the same agent simultaneously). It doesn't constrain what the LLM can think about; it constrains what the system will actually do on your behalf.

## How they compose

When an agent is configured to use all three, the picture looks like this:

1. **The agent host** (Claude Code, your custom app) declares its own tools using **tool calling**. Things like file reads, codebase search, terminal execution.
2. **MCP servers** the host has registered expose more tools. Maybe a documentation MCP, a database MCP, a Kubernetes MCP.
3. **Aileron** publishes your installed actions through its own MCP server: slack-post, linear-ticket, gmail-send. They appear in the agent's tool catalog like any other MCP-served tool.

The LLM sees one merged set of tools and picks among them based on which best fits the user's request.

When the LLM picks a host tool, the host runs it. When it picks an MCP tool, the MCP server runs it. When it picks an Aileron action, the Aileron runtime runs it — deterministically, with sealed credentials, against capability bounds, with audit.

The three don't compete. If you don't need Aileron, drop it and the rest still works.

## Where each is the right answer

| You want to... | Use this |
|---|---|
| Let an LLM call functions in your own code (read a file, run a query, search the codebase) | **Tool calling**. Any modern LLM. Done. |
| Expose your service's tools to many different agents without writing N×M integrations | **MCP**. Run an MCP server; any MCP-aware host consumes it. |
| Let an agent take consequential action on your behalf — post messages, file tickets, send emails, charge cards — without holding the credentials in the agent's process | **Aileron**. Install the relevant action; bind your credential; the agent uses the action through normal tool calling and the runtime executes it safely. |

The three are not mutually exclusive. An agent can use any combination — they compose without forcing the others.

## Shared principles, different scopes

All three agree on some things:

- **The LLM proposes; something else disposes.** Even tool calling separates the LLM's pick from the host's execution. The disagreement is about *what kind of guarantees* the disposer makes.
- **Structured interfaces beat ad-hoc strings.** Tool calling defines a function schema; MCP defines a tool registration; Aileron actions declare connector requirements and capability subsets. None of them lets the LLM emit raw shell commands and call it execution.
- **Composition over monoliths.** Tool calling is composable per-call; MCP is composable per-server; Aileron actions are composable atomic units. None of them tries to be a one-stop framework.

The *differences* are about trust and enforcement:

- Tool calling and MCP put trust in the host (or the MCP server). Whatever credential discipline they have is whatever the host implementer chose to build.
- Aileron puts trust in the *runtime sandbox*. Capability bounds, credential sealing, and audit are structural properties enforced by the runtime, not policies the connector author had to remember to implement.

The right framing isn't "which one wins." It's "what each guarantees, and what gaps you'd have without the others."

## When you don't need Aileron

If your agent only does informational work — reads files, searches docs, summarizes PRs, asks for clarification — you don't need Aileron. Tool calling (with or without MCP) is enough. The LLM proposing the wrong read query has a low cost; the LLM proposing the wrong send-email has a high one.

Aileron's value shows up the moment the agent starts doing things that are hard to undo. That's where deterministic execution, sealed credentials, and tamper-resistant approval start mattering. If your agent is mostly read-only, the rest of the stack is plenty.
