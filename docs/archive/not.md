---
title: "What Aileron Is Not"
description: "Explicit non-goals — the categories Aileron does not occupy"
order: 8
---

> **Section:** part of the Pivot strategy. See [Overview](/) for what Aileron *is*, and [Why This Is Structurally Defensible](/why-defensible) for the surrounding categories Aileron is *adjacent to but not*.

- Not another model.
- Not another agent framework.
- Not an inference engine or model router — Aileron does not run inference and does not select models for you.
- Not a thin API gateway — the action decisions are the value, not the proxy.
- Not a tool-calling framework — Aileron *executes* tool calls deterministically with capability isolation, rather than delegating to whatever code the LLM coordinates.
- Not a content firewall — Aileron does not filter LLM output; it executes deterministic actions with structural safety guarantees.
- Not a workflow orchestrator — agents do not author DAGs; they speak chat completions or messages, and Aileron decides what happens behind that endpoint.
