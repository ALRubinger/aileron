# Aileron

![GitHub License](https://img.shields.io/github/license/ALRubinger/aileron?style=for-the-badge)
![GitHub Actions Workflow Status](https://img.shields.io/github/actions/workflow/status/ALRubinger/aileron/ci.yml?style=for-the-badge&logo=github)
![Codecov](https://img.shields.io/codecov/c/github/ALRubinger/aileron?style=for-the-badge)

Aileron is the runtime that lets your AI agent act in the real world. Send the email, post the Slack update, file the ticket. Your credentials stay in a local vault. Your approval gates the irreversible steps. Every action runs the same way every time, against the real API, inside a sandbox.

It complements Tool Calling and MCP rather than replacing them. The LLM still expresses intent through tool calls. MCP still exposes the catalog. Aileron is the layer that runs underneath, turning those calls into deterministic, auditable executions against your real systems.

**[Read the full story at docs.withaileron.ai](https://docs.withaileron.ai)**

## Documentation

Everything lives at **[docs.withaileron.ai](https://docs.withaileron.ai)**:

- [Overview](https://docs.withaileron.ai/) — what Aileron is and the pitch
- [Getting Started](https://docs.withaileron.ai/getting-started/) — install, equip Gmail and Calendar actions, launch Claude Code through Aileron
- [Concepts](https://docs.withaileron.ai/concepts/deterministic-agentic-execution/) — the architecture layered: deterministic execution, actions, connectors, the vault, proof of control
- [Architecture Decisions](https://docs.withaileron.ai/adr/) — the ADRs that ratify the post-Pivot architecture, including [ADR-0012: Local Daemon Architecture](https://docs.withaileron.ai/adr/0012-local-daemon-architecture/)
- [API Reference](https://docs.withaileron.ai/api) — full OpenAPI specification
- [Repository Layout](https://docs.withaileron.ai/development/repo-layout/) — how the source tree is organized
- [Building from Source](https://docs.withaileron.ai/development/building-from-source/) — prerequisites and Taskfile entry points for contributors

## License

See [LICENSE](LICENSE).
