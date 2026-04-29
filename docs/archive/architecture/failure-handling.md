---
title: "Failure Handling"
description: "Visible by default; LLM fallback opt-in for informational actions only; structured errors; idempotency by default"
order: 10
---

> **Architecture:** part of the [Architecture](/architecture/) section of the Pivot strategy. See also [The Action Model](/architecture/action-model), [How Intent Matches to Actions](/architecture/intent-matching), and [The User Channel](/architecture/user-channel).

Four architectural commitments shape how Aileron behaves when an action fails. Specific retry budgets, manifest syntax for failure overrides, and detailed failure-class policies are deferred to the failure-handling ADR — they benefit from real implementation experience.

**1. Visible failure is the default; never silent LLM fallback.** When an action fails, the agent receives a structured error. Aileron does not fall back to the LLM and let it produce a probabilistic response that masquerades as the action's output. Silent fallback is the worst possible failure mode because the user thinks the action succeeded when it didn't — the LLM hallucinates a confirmation while real-world state diverges from belief. This is a security commitment, not a UX preference.

**2. LLM fallback is opt-in, gated to informational-class actions only.** Some actions are read-only or synthesis-style ("look up customer info," "summarize this thread") where degraded LLM answers beat no answers. For these, the action manifest can opt into LLM fallback explicitly, with the response clearly flagged as estimated. For any action with side effects — sends email, charges a card, modifies a database — Aileron refuses the fallback flag at install time. Side-effecting actions cannot fall back to probabilistic execution; the runtime structurally prevents it.

**3. Errors are structured so agents can reason about them.** When an action fails inside a function-calling flow, the tool result includes a structured error payload (failure class, retriability, user-facing message, audit ID). When an action fails in pre-LLM bypass mode, the streaming response includes a recognizable marker block. Agents that want to retry, inform the user, or try a different approach have the information to do so.

**4. Actions are designed for idempotency by default.** Idempotency keys derived from action inputs make retries safe. Authors writing genuinely non-idempotent actions opt out explicitly in the manifest; Aileron then disables auto-retry for those actions. This commitment shapes how the action manifest schema looks and how partial-failure recovery works — both of which become tractable when retries don't risk double-execution.

The detailed policy — failure-class taxonomy, default retry budgets, manifest syntax for overrides, recovery semantics for partial failures, OAuth refresh as a first-class flow rather than a failure mode — is deferred to the failure-handling ADR. These four commitments set the architectural posture; the ADR fills in the operational details with concrete implementation experience.
