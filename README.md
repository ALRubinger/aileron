# Aileron

![GitHub License](https://img.shields.io/github/license/ALRubinger/aileron?style=for-the-badge)
![GitHub Actions Workflow Status](https://img.shields.io/github/actions/workflow/status/ALRubinger/aileron/ci.yml?style=for-the-badge&logo=github)
![Codecov](https://img.shields.io/codecov/c/github/ALRubinger/aileron?style=for-the-badge)

Time is our most precious asset. We pour it into purpose, into connection. Managing our modern life demands time, and we believe it should take far less.

Aileron learns how you already live and work. Your conversations, your decisions, your preferences, your corrections. It builds an understanding of your world that no one had to write down, and that understanding is what makes the difference between an answer that wastes your time and one you'd trust with your name on it.

**[Read the full story at docs.withaileron.ai](https://docs.withaileron.ai)**

## Quick start

```sh
aileron launch claude
```

Aileron wraps your agent in a policy-enforced shell. Safe commands auto-approve. Dangerous ones are blocked. Ambiguous ones ask you once. Zero config required.

## What Aileron does

- **Policy-enforced shell.** Every command flows through `aileron-sh` and your `aileron.yaml` rules. The policy lives in version control, is reviewable in PRs, and learns from use.
- **Credential brokering.** Secrets in a zero-knowledge encrypted vault. The agent uses them but never sees them.
- **Bidirectional messaging.** Slack and Discord messages arrive in your terminal. The agent drafts replies using codebase context. You approve with one keypress.
- **Audit trail.** Every decision is logged. What the agent did, what the policy decided, what you authorized.
- **Agent-portable.** Switch agents and your policy, credentials, and audit trail come with you.

## Install

Download the latest release from [GitHub Releases](https://github.com/ALRubinger/aileron/releases). Place `aileron` and `aileron-sh` in the same directory on your PATH.

## Documentation

Full documentation, guides, and architecture decisions are at **[docs.withaileron.ai](https://docs.withaileron.ai)**.

- [For Individuals](https://docs.withaileron.ai/for-individuals) - How Aileron gives you your time back
- [For Organizations](https://docs.withaileron.ai/for-organizations) - Institutional memory that builds itself
- [How Aileron Works](https://docs.withaileron.ai/how-aileron-works) - The building blocks that make it trustworthy
- [Getting Started](https://docs.withaileron.ai/getting-started/installation) - Installation, policy, integrations
- [Deployment](https://docs.withaileron.ai/deployment/cloud) - Cloud, Railway, and TEE enclave setup
- [API Reference](https://docs.withaileron.ai/api) - Full OpenAPI specification
- [Architecture Decision Records](https://docs.withaileron.ai/adr/0001-hybrid-mcp-execution-model) - Design decisions and rationale

## Development

```sh
task build           # build everything
task test:go         # unit tests
task test:integration # integration tests (requires running server)
task up              # run full stack locally
task down            # stop
```

See the [Development guide](https://docs.withaileron.ai/development/building-from-source) for prerequisites and project structure.

## License

See [LICENSE](LICENSE).
