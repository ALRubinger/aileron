---
title: "Aileron Control"
description: "Governance that comes for free because Runtime sits in the request path by construction"
order: 3
---

> **Section:** part of the Pivot strategy. See [Overview](/) for the two-layer summary and [Aileron Runtime](/runtime) for what sits in the request path. The architectural commitments behind each Control surface live in [Architecture](/architecture/).

Because [Aileron Runtime](/runtime) is in the request path by construction, governance attaches without integration:

- **Identity and credential vault.** Keys, tokens, secrets — held by Aileron, never reach the LLM, scoped per action.
- **Policy enforcement.** Deterministic rules about what agents can do, with whom, under what conditions, applied before any action or LLM call executes.
- **Tamper-resistant approvals.** When an action requires human confirmation, Aileron prompts via system notification, OS biometric prompt, dedicated TUI panel, or web UI — never through the agent UI. The user's consent travels on a channel the agent cannot reach. (No competitor offers approval flows that are structurally protected from agent mediation.) See [The User Channel](/architecture/user-channel) for the architecture.
- **Audit and execution.** Every action execution is deterministic and reproducible. Every LLM call is logged with routing decisions. Every approval is recorded with the surface it came from.
