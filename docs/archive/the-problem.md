---
title: "The Problem"
description: "Six structural pains in the current AI agent stack — illustrated with the kind of moments developers actually live"
order: 1
---

> **Section:** part of the Pivot strategy. Start at [Overview](/) for the one-sentence pitch and architectural insight; see [Aileron Runtime](/runtime) and [Aileron Control](/control) for what Aileron does about it.

The six pains below are the ones the agent stack imposes on the people using it. Each starts with a moment a working developer might have lived, then names the structural cause, then the cost of letting it stand.

## You verified everything. You became its fact-checker.

The agent confidently says the bug is in `auth.go:42`. You open the file. There's no `auth.go:42`; the function it described doesn't exist. You scroll, search the codebase, find the actual call site three packages over, and patch it. Tomorrow the same agent will be just as confident about something else — and you'll re-verify, because you've learned you have to.

When an agent says "send the invoice to acme@example.com for $4,200," there is one correct outcome. The current architecture runs that intent through a stochastic model that may hallucinate the recipient, the amount, or the action itself. The model is asked to make decisions a deterministic function should make. Production agents accumulate this debt every turn — and developers absorb the verification cost as a tax they didn't sign up for.

## Your secrets are in places you didn't put them.

You paste your Stripe test key into `.env` and tell your agent to "use it for testing." A week later you're reading a prompt-trace log and the key is *in* a tool-call argument the LLM stitched together — not because anything broke, but because the model decided that string was relevant context. You rotate. You don't know who else saw the trace.

To execute real-world actions, agents pass API keys, tokens, and secrets through the LLM context — sometimes inadvertently, often unavoidably. The model could leak them in a response, log them in a trace, or return them as part of a tool argument it composed. "LLM never sees the key" is now table stakes (1Password, HashiCorp Vault, NVIDIA OpenShell all ship it), but every solution requires the agent author to opt in.

## You're not sure if you confirmed that, or the agent just said you did.

The agent's response says *"Confirmed: dropping table `users`. Done."* You re-read it. Did you actually click confirm? Or did the agent compose that sentence as a fluent continuation of an earlier turn, and the destructive call went out without you ever seeing the prompt? The audit trail is the agent's word about itself.

Approvals, denials, and configuration changes all flow through the agent UI today. A buggy or compromised agent can rewrite a denial as an approval, substitute one action's ID for another, or fabricate user consent that never happened. Existing tool-execution layers — gateways, MCP servers, agent frameworks — accept this risk because they have no separate channel for user intent. Aileron does.

## You paid for inference on work that didn't need a model.

You watched your agent burn 11k tokens to format a JSON blob into a markdown table. The first 47 turns of last night's run did the same lookup-render-format cycle on different inputs. The shape never changed; the cost did. Your monthly bill rose with your turn count, not with the share of turns that genuinely needed a frontier model.

A 50-step agent run sends every step through inference, including the steps where the answer is computable, the format is fixed, and the procedure is settled. The trivial turns and the hard turns pay the same token cost and incur the same network latency. Typical tool-call-heavy agents see 3–5x cost overhead from over-provisioning; substitution-dominant workloads can see 10x or more. The categorical wins are bigger still — substituted actions skip inference entirely, cutting end-to-end latency from seconds to milliseconds and producing identical results across runs.

## Compliance asked what your agent did. You can't tell them.

Compliance asks for a record of what your agent did to customer X's account on March 12. You have logs — prompts, completions, tool calls. Two of the calls have arguments the LLM constructed that don't appear anywhere in your code. Re-running the same prompt produces a different sequence. There is no reproduction; there is no answer.

Regulated industries — finance, healthcare, legal — cannot deploy agents whose actions are non-reproducible. The same input produces different outputs across runs. The same prompt produces different tool invocations. There is no audit trail because there is no determinism to audit.

## You evaluated the policy product. The integration assumed you'd done their job.

You evaluated Cerbos for agent policy. The integration doc starts with *"wrap your tool execution function in..."* — but you don't *have* a tool execution function. Your agent is Claude Code with a handful of MCP servers. The policy layer never sees what's about to run, because the thing that runs lives inside an editor extension you don't control.

Cerbos, Permit.io, Lakera, Pangea, Galileo all require SDK integration. The agent author is asked to be the safety engineer. Most aren't, and most won't be. The governance layer needs to be invisible to the agent — sitting in the path the agent already uses — or it doesn't get installed at all.
