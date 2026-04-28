---
title: "What Aileron Is Not"
description: "Explicit non-goals — the categories Aileron does not occupy"
order: 8
---

> **Section:** part of the Pivot strategy. See [Overview](/pivot/overview) for what Aileron *is*, and [Why This Is Structurally Defensible](/pivot/why-defensible) for the surrounding categories Aileron is *adjacent to but not*.

- Not another model.
- Not another agent framework.
- Not a wrapper around Ollama — Ollama is one engine Aileron orchestrates.
- Not a thin API gateway — the routing and action decisions are the value, not the proxy.
- Not "near-frontier results on a laptop" — Aileron promises the cheapest model that meets the developer's quality bar.
- Not a tool-calling framework — Aileron *executes* tool calls deterministically with capability isolation, rather than delegating to whatever code the LLM coordinates.
- Not a content firewall — Aileron does not filter LLM output; it executes deterministic actions with structural safety guarantees.
- Not a workflow orchestrator — agents do not author DAGs; they speak chat completions, and Aileron decides what happens behind that endpoint.
