---
title: "Aileron"
description: "Policy-enforced shell for AI coding agents"
order: 0
---

**Policy-enforced shell for AI coding agents.**

Aileron lets your agents fly. Security, privacy, and accountability keep you in control so you can put on the afterburner.

## The Problem

AI coding agents run shell commands on your behalf — but giving them unrestricted access to your terminal is dangerous. Every team faces the same question: *"How do we let agents run commands without rubber-stamping everything or missing the dangerous ones?"*

## The Solution

`aileron launch claude` wraps your agent in a policy-enforced shell. Every command the agent runs flows through `aileron-sh`, which evaluates it against your `aileron.yaml` rules before allowing execution. Safe commands auto-approve. Dangerous commands are blocked. Ambiguous commands prompt you once.

### Key Principles

- **Policy-as-code** — Allow, deny, or ask rules defined in `aileron.yaml`, checked into your repo, reviewable in PRs.
- **Single approval layer** — Aileron replaces the agent's native permission prompts. One policy, one prompt.
- **Credential isolation** — Locally, credential brokering keeps secrets out of the agent's context. With the cloud vault (coming), secrets are encrypted with a key only you hold — Aileron architecturally *cannot* see them.
- **Complete audit trail** — Every decision is logged. The audit trail is the ground truth for what happened.
- **Agent-aware** — Works with Claude Code today, extensible to other agents. Agent-specific quirks (command wrapping, shell validation) are handled transparently.

## Documentation

- [API Reference](/api) — Full OpenAPI specification with interactive explorer
- [Architecture Decision Records](/adr) — Design decisions and their rationale
