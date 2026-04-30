---
title: "The LLM Gateway"
description: "Aileron runs as an OpenAI- and Anthropic-compatible gateway between the agent and its upstream LLM. Two things happen at that seam: tool augmentation and interception."
order: 2
---

Aileron runs as a local **LLM gateway** that speaks both [OpenAI's Chat Completions API](https://developers.openai.com/api/reference/chat-completions/overview) (`/v1/chat/completions`) and [Anthropic's Messages API](https://platform.claude.com/docs/en/api/messages) (`/v1/messages`). The agent points its API base URL at `http://localhost:8721/v1` instead of `api.openai.com` or `api.anthropic.com`. Every request the agent makes — whichever shape it speaks — flows through Aileron on its way to the upstream model provider.

This position — between the agent and the LLM — is the seam where Aileron does its work.

## Why this seam

Every modern agent already speaks one of these two protocols. Claude Code, Cursor, Continue, Aider, Copilot, and custom apps all send a request in OpenAI's Chat Completions shape or Anthropic's Messages shape. By sitting at the endpoint and supporting both, Aileron works with any agent that speaks either protocol — no SDK changes, no framework integration, no plugin to install in the agent host.

The LLM endpoint is also the only seam where the load-bearing properties (sealed credentials, deterministic execution, audit, capability bounds) can be guaranteed at once. Anywhere else, at least one property has to be trusted to whoever's running the call.

## Two things at the seam

When a request arrives — whether it's an OpenAI Chat Completions call or an Anthropic Messages call — Aileron does two things:

**Proposing — tool augmentation.** Aileron reads the user's installed [actions](/concepts/actions/) and appends them to the agent's tool catalog (the `tools[]` array, in either protocol) before forwarding upstream. The LLM sees one merged catalog — the agent's own tools, MCP-served tools, and Aileron-installed actions — and picks among them on their merits. The agent never knew Aileron added anything.

**Executing — interception.** When the LLM emits a tool call for an Aileron-installed action, Aileron intercepts before the response leaves the gateway. It runs the action through the connector sandbox, gets the result, and injects it back into the streaming response as a synthetic tool result. The agent's host code sees a normal streaming response with a tool result it didn't have to execute.

That's the entire integration. Point your agent at Aileron's URL, and the action catalog you've installed is now part of every request, regardless of which provider's protocol the agent speaks.