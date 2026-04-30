# Aileron

![GitHub License](https://img.shields.io/github/license/ALRubinger/aileron?style=for-the-badge)
![GitHub Actions Workflow Status](https://img.shields.io/github/actions/workflow/status/ALRubinger/aileron/ci.yml?style=for-the-badge&logo=github)
![Codecov](https://img.shields.io/codecov/c/github/ALRubinger/aileron?style=for-the-badge)

Aileron is a local LLM gateway that gives your AI agent the ability to take consequential action on your behalf — post the deploy update, file the ticket, send the email — without ever holding your credentials in the agent's process.

The agent points its API base URL at Aileron instead of OpenAI or Anthropic. Aileron speaks both Chat Completions and Messages, augments the agent's tool catalog with installed actions, and intercepts those actions for deterministic execution: sandboxed connectors, sealed credentials, capability-bounded calls, audited everywhere.

**[Read the full story at docs.withaileron.ai](https://docs.withaileron.ai)**

## Documentation

Everything lives at **[docs.withaileron.ai](https://docs.withaileron.ai)**:

- [Overview](https://docs.withaileron.ai/) — what Aileron is and the pitch
- [Concepts](https://docs.withaileron.ai/concepts/deterministic-agentic-execution/) — the architecture, layered: deterministic execution, the LLM gateway, actions, connectors, the vault, proof of control
- [Architecture Decisions](https://docs.withaileron.ai/adr/) — the eleven ADRs that ratify the post-Pivot architecture
- [API Reference](https://docs.withaileron.ai/api) — full OpenAPI specification

## Development

```sh
task build           # build everything
task test:go         # unit tests
task test:integration # integration tests (requires running server)
```

## License

See [LICENSE](LICENSE).
