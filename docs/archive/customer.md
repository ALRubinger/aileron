---
title: "The Customer"
description: "Who Aileron is for in v1: four developer personas where Aileron lands in their week, plus an honest cut-list of who isn't a fit yet"
order: 6
---

> **Section:** part of the Pivot strategy. See [Overview](/) for the pitch, [The Problem](/the-problem) for the pains these developers live with, and [What Your Agent Can Now Do](/what-your-agent-can-do) for hero use cases that ground the wedge.

The wedge is the individual developer. Below: four concrete personas where Aileron lands in their week, an honest list of who Aileron isn't for in v1, and a pointer to the separate enterprise treatment.

## Productivity that secures itself

### The indie agent builder

Two weeks from launching her side-project agent SaaS. The hard work — model orchestration, prompt design, the user-facing flow — is mostly done. The remaining checklist is the part she didn't sign up for: Slack OAuth, audit logging, "what happens if a user's API key shows up in a trace," approval UX for the destructive actions her agent can take. Every hour on safety infra is an hour not on the product.

> **Where Aileron lands:** she points her agent at `localhost:8721/v1`, installs `slack` and `stripe` connectors from the Hub, drops in a `ship-update` action, and inherits approval flows, credential isolation, and a deterministic execution path she didn't have to write. The launch checklist gets shorter, not longer.

### The AI-coding-tool power user

Lives in Claude Code or Cursor. The agent is great at code; it stops at code. Posting a ship update to Slack, filing a follow-up ticket in Linear, blocking time on the calendar — those happen in five other tabs because the agent can't be trusted to act outside the editor. Productivity hits a wall the moment the work crosses an organizational boundary.

> **Where Aileron lands:** the agent's tool catalog gains `ship-update`, `slack.post`, `linear.create_issue`, `cal.block`. Each executes deterministically, asks for approval where it matters, and finishes the work the editor used to stop short of. The editor stays the same; the seam behind it changes.

### The local-first developer

Runs Ollama, llama.cpp, or MLX heavily. Picked a model. Picked a quantization. Did some benchmarks. Three months later a new chip shipped, the right quantization changed, and the model lineup turned over twice. Hardware is 30–50% underused on average because nobody other than them is doing this tuning work, and they don't have the time to redo it every quarter.

> **Where Aileron lands:** Runtime profiles the hardware, benchmarks engines, picks per-machine. Trivial turns run free locally on whatever engine measured fastest this week. Cloud overflow only when the local quality bar isn't met. They stop being their own ML ops team.

### The privacy-conscious power user

Automating personal workflows where the data must not leave the device — medical reminders, financial review, journal summaries, family calendar. Every available agent product wants to send the data to someone's cloud. They've built the equivalent in a pile of shell scripts because that's the only stack where they trust where the data lives.

> **Where Aileron lands:** actions execute locally with vault-held credentials, the LLM portion routes to local engines first, and "route this through cloud" is opt-in per request — visible, not silent. Their shell-script pile becomes a small, declarative set of action files they can read in five minutes.

The wedge is the individual developer. The expansion is structurally aligned: the same install pattern scales to every system the developer touches, and the same architecture that gives an individual developer their first hero moment also gives an enterprise its first compliance-grade deployment.

---

## Who Aileron isn't for in v1

Being honest about the cut-list is part of being honest about the wedge.

- **Enterprise governance buyers who need turnkey vendor support today.** Federated vault, audit retention beyond cloud limits, signed-connector supply chain at scale, SSO/RBAC, dedicated security review — these matter and the architecture supports them, but they aren't the v1 sell. See [Enterprise — addressed later](/enterprise-later).
- **No-code workflow authors.** Aileron is a developer tool. If the right answer is "drag boxes, connect them, click run," the right tools are Zapier, Make, or n8n — not Aileron.
- **Teams committed to a managed/cloud-only stack.** Aileron is a local-first runtime that runs alongside the agent. Teams that have decided every component must run as a managed cloud SaaS will find the architecture pulling against them; the fit improves once Aileron Control is in place, but the v1 wedge is local.
- **Agent framework authors.** Aileron is orthogonal to LangChain, CrewAI, Mastra, AutoGen, Pydantic-AI, and the rest. It sits behind the LLM endpoint, not in the framework layer. Framework authors are potential *users* of Aileron, not competitors with it — they're already solving a different problem.

If you're in one of these groups today, Aileron will likely be relevant later; it just isn't the right tool to reach for in v1.

---

## Enterprise — addressed in detail later

Compliance, vault federation, audit retention, signed connectors, attested execution — these matter to enterprises and the architecture supports them. They get a dedicated treatment at [Enterprise — addressed later](/enterprise-later) and in a separate enterprise document beyond that. This page is for the developer who needs to feel productive in five minutes.
