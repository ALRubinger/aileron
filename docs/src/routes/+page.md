# Aileron

**The deterministic execution plane for AI agents.**

Aileron lets your agents fly. Security, privacy, and accountability keep you in control so you can put on the afterburner.

## The Problem

AI agents are powerful — but giving them direct access to your credentials, APIs, and irreversible actions is dangerous. Today, every agent deployment faces the same question: *"How do we let agents act on our behalf without handing them the keys?"*

## The Solution

Aileron sits between your agents and the outside world. Agents propose **intents** — what they want to do. Aileron evaluates policy, enforces approvals, and **executes the action itself**. Agents never hold credentials. Every action is logged with a complete audit trail.

### Key Principles

- **Zero-knowledge credential custody** — Your secrets are encrypted with a key only you hold. Aileron architecturally *cannot* see them.
- **Policy-as-code** — Allow, deny, or ask rules defined in `aileron.yaml`, checked into your repo, reviewable in PRs.
- **Complete audit trail** — Every action flows through Aileron, so the audit log is complete by construction.
- **Agent-agnostic** — Works with any agent framework or LLM provider.

## Documentation

- [API Reference](/api) — Full OpenAPI specification with interactive explorer
- [Architecture Decision Records](/adr) — Design decisions and their rationale
