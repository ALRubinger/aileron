---
title: "The Problem"
description: "Six structural pains in the current AI agent stack that the Aileron pivot resolves"
order: 1
---

> **Section:** part of the Pivot strategy. Start at [Overview](/pivot/overview) for the one-sentence pitch and architectural insight; see [Aileron Runtime](/pivot/runtime) and [Aileron Control](/pivot/control) for what Aileron does about it.

## Agents are probabilistic where they should be deterministic

When an agent says "send the invoice to acme@example.com for $4,200," there is one correct outcome. The current architecture runs that intent through a stochastic model that may hallucinate the recipient, the amount, or the action itself. The model is asked to make decisions a deterministic function should make. Production agents accumulate this debt every turn.

## LLMs touch credentials they should never see

To execute real-world actions, agents pass API keys, tokens, and secrets through the LLM context — sometimes inadvertently, often unavoidably. The model could leak them in a response, log them in a trace, or return them as part of a tool argument it composed. "LLM never sees the key" is now table stakes (1Password, HashiCorp Vault, NVIDIA OpenShell all ship it), but every solution requires the agent author to opt in.

## The agent itself is in the trust path for consent

Approvals, denials, and configuration changes all flow through the agent UI today. A buggy or compromised agent can rewrite a denial as an approval, substitute one action's ID for another, or fabricate user consent that never happened. Existing tool-execution layers — gateways, MCP servers, agent frameworks — accept this risk because they have no separate channel for user intent. Aileron does.

## Cost and latency are paid even when the work is deterministic

A 50-step agent run sends every step through inference, including the steps where the answer is computable, the format is fixed, and the procedure is settled. The trivial turns and the hard turns pay the same token cost and incur the same network latency. Typical tool-call-heavy agents see 3–5x cost overhead from over-provisioning; substitution-dominant workloads can see 10x or more. The categorical wins are bigger still — substituted actions skip inference entirely, cutting end-to-end latency from seconds to milliseconds and producing identical results across runs.

## Audit and compliance can't keep up with probabilism

Regulated industries — finance, healthcare, legal — cannot deploy agents whose actions are non-reproducible. The same input produces different outputs across runs. The same prompt produces different tool invocations. There is no audit trail because there is no determinism to audit.

## Governance products require integration the agent author doesn't perform

Cerbos, Permit.io, Lakera, Pangea, Galileo all require SDK integration. The agent author is asked to be the safety engineer. Most aren't, and most won't be. The governance layer needs to be invisible to the agent — sitting in the path the agent already uses — or it doesn't get installed at all.
