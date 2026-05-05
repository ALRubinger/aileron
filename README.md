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

## Repository layout

- `cmd/` — Go binaries: `aileron` (CLI), `aileron-mcp` (MCP server), `aileron-sh` (policy-enforced shell shim), `aileron-enclave` (TEE worker), `aileron-connector-dev-run` (dev harness).
- `internal/` — Go packages: app handlers, action runtime, sandbox (Wazero/WASM), vault, connectors, approval orchestrator, notifier, audit.
- `webapp/` — **the local webapp** surfaced under `aileron launch` for action-approval review and decision. Built statically with SvelteKit + `@sveltejs/adapter-static`; embedded into the daemon binary via Go embed (`internal/app/webapp_dist/`) and served at `/` by the same gateway the agent talks to. See [`webapp/README.md`](./webapp/README.md) for the dev loop.
- `ui/` — **frozen** cloud-tier reference UI (SvelteKit + `@sveltejs/adapter-node`, multi-user auth, organization settings). Per the strategic pivot in [#335](https://github.com/ALRubinger/aileron/issues/335), the local experience comes first; the cloud-hosted Aileron will be rebuilt from first principles when its time comes. `ui/` is preserved as a pattern reference for that future work but is not actively maintained or built into the daemon. New webapp work belongs in `webapp/`.
- `docs/` — Astro documentation site that publishes to [docs.withaileron.ai](https://docs.withaileron.ai).
- `sdk/` — language SDKs.

## Development

```sh
task build              # build everything (binaries + webapp + docs)
task build:webapp       # just the local webapp; refreshes internal/app/webapp_dist
task dev:webapp         # webapp dev server (hot-reload) at localhost:5173
task test:go            # unit tests
task test:integration   # integration tests (requires running server)
```

## License

See [LICENSE](LICENSE).
