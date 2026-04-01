# ADR-0001: Hybrid MCP Execution Model

**Status:** Proposed
**Date:** 2026-04-01

## Context

Aileron is an MCP gateway/aggregator — the single MCP server an agent host connects to. It proxies downstream MCP servers, intercepting every `tools/call` for policy evaluation, approval gating, credential injection, and audit logging.

Today, the gateway spawns each downstream MCP server as a **local subprocess** communicating over stdio (newline-delimited JSON-RPC 2.0). The YAML configuration defines a `command` and optional `env` per server. This model works well for single-user, local-first operation.

For enterprise deployment, Aileron's control plane (policy engine, approval orchestration, vault, audit log) is cloud-hosted. This raises a fundamental question: **where do the downstream MCP servers run?**

Three deployment models were considered:

### Fully centralized

All downstream MCP servers run alongside the control plane in the cloud. Users connect their agent hosts to a remote Aileron gateway.

**Problems:**
- MCP servers that need local context (filesystem access, IDE integrations, local dev tools) cannot run remotely.
- Centralizing N users × M servers creates multi-tenant sandboxing, resource scaling, and noisy-neighbor challenges.
- Aileron becomes a managed execution platform — a fundamentally different product scope.

### Fully local

The gateway and all downstream MCP servers run on each user's machine. The control plane is remote, but tool execution is entirely local.

**Problems:**
- Every user must install and configure every MCP server locally, including dependencies (Node.js runtimes, Python environments, etc.).
- Credentials for API-backed services (GitHub tokens, Slack tokens) must be distributed to every machine, expanding the attack surface.
- IT loses visibility into which MCP server versions are running across the fleet.

### Hybrid (selected)

Some MCP servers run locally, others run centrally. The execution mode is determined per server based on whether the server needs local context or is purely API-backed.

## Decision

Adopt a **hybrid execution model** where each downstream MCP server is configured as either `local` or `remote`:

- **Local servers** are spawned as subprocesses on the user's machine (the current model). They communicate with the gateway over stdio. Use this for servers that require local filesystem access, IDE integration, or other machine-specific context.

- **Remote servers** are managed centrally by the Aileron control plane and accessed by the local gateway over HTTP (MCP Streamable HTTP transport). Use this for purely API-backed servers (GitHub, Slack, Jira, etc.) where no local context is needed.

The **policy interception layer is transport-agnostic** — it sits above the execution transport and enforces the same rules regardless of where a tool runs. From the agent's perspective, nothing changes: all tools appear as `servername__toolname` on a single MCP surface.

## Deployment Topology

```
┌─────────────────────────────────────────────────────────┐
│                  Aileron Control Plane                   │
│                     (cloud-hosted)                       │
│                                                         │
│  ┌──────────┐ ┌───────────┐ ┌───────┐ ┌─────────────┐  │
│  │  Policy   │ │ Approval  │ │ Vault │ │  Audit Log  │  │
│  │  Engine   │ │   Orch.   │ │       │ │             │  │
│  └──────────┘ └───────────┘ └───────┘ └─────────────┘  │
│                                                         │
│  ┌──────────────────────────────────────────────────┐   │
│  │         Remote MCP Server Pool                    │   │
│  │  ┌────────┐  ┌───────┐  ┌──────┐                │   │
│  │  │ GitHub │  │ Slack │  │ Jira │  ...            │   │
│  │  └────────┘  └───────┘  └──────┘                │   │
│  └──────────────────────────────────────────────────┘   │
└──────────────────────┬──────────────────────────────────┘
                       │ HTTPS
                       │
        ┌──────────────┴──────────────┐
        │   Aileron Gateway (local)   │
        │        per user             │
        │                             │
        │  ┌───────────────────────┐  │
        │  │  Policy Interceptor   │  │  ← transport-agnostic
        │  └───────┬───────┬───────┘  │
        │          │       │          │
        │     stdio│       │HTTP/SSE  │
        │          │       │          │
        │  ┌───────┴──┐ ┌─┴────────┐ │
        │  │ Local MCP │ │  Remote  │ │
        │  │ Servers   │ │  Proxy   │ │
        │  │(fs, IDE)  │ │          │ │
        │  └───────────┘ └──────────┘ │
        └──────────────┬──────────────┘
                       │ stdio (MCP)
                       │
                ┌──────┴──────┐
                │ Agent Host  │
                │ (Claude,    │
                │  Cursor,    │
                │  etc.)      │
                └─────────────┘
```

## Configuration Schema Direction

The `DownstreamServer` config struct gains a `mode` field:

```yaml
version: "1"
downstream_servers:
  # Local: spawned as subprocess (current behavior)
  - name: "filesystem"
    mode: local
    command: ["npx", "-y", "@modelcontextprotocol/server-filesystem", "/home/user/projects"]

  # Remote: managed by the control plane, accessed over HTTP
  - name: "github"
    mode: remote
    # No command — the control plane manages the server instance.
    # The gateway resolves the endpoint from the control plane at startup.
    env:
      GITHUB_TOKEN: "vault://secrets/github-token"
    policy_mapping:
      tool_prefix: "git"
```

When `mode` is omitted, it defaults to `local` for backward compatibility.

For remote servers, the gateway discovers the endpoint from the control plane during initialization rather than hardcoding URLs in the local config. This keeps the control plane as the single source of truth for remote server topology.

## Credential Flow

The credential model differs by execution mode:

**Local servers:** Credentials are resolved from the vault at gateway startup and injected as environment variables into the subprocess (the current `vault://` prefix mechanism in `config.ResolveVaultEnv`). The local gateway holds credentials in memory for the lifetime of the subprocess.

**Remote servers:** Credentials are managed entirely within the control plane. The remote MCP server instances receive credentials server-side. The local gateway never sees them. This is a security improvement — API tokens for GitHub, Slack, etc. never leave the central infrastructure.

## Transport Abstraction

The gateway currently depends directly on `mcpclient.Client` (a concrete stdio subprocess client). To support both execution modes, introduce a `ToolExecutor` interface:

```go
// ToolExecutor abstracts tool discovery and invocation across transports.
type ToolExecutor interface {
    Name() string
    Tools() []ToolDef
    CallTool(ctx context.Context, name string, arguments map[string]any) (*ToolResult, error)
    Close() error
}
```

The existing `mcpclient.Client` already satisfies this interface. A new `remoteclient.Client` would implement the same interface over HTTP/SSE. The gateway's `routeToolCall()` works against `ToolExecutor` — policy interception is identical regardless of transport.

## Consequences

The hybrid model must serve two distinct personas with different needs. The architecture succeeds only if both feel native — not like one is a degraded version of the other.

### Individual developer experience

The individual developer wants MCP tools that just work, with minimal setup and no organizational overhead. Aileron must be the easiest way to run MCP servers, not a tax on productivity.

**What this enables:**

- **Single binary, zero config to start**: A developer installs Aileron, points it at a YAML file listing the MCP servers they want, and every tool is available through one MCP connection. No per-server client configuration in their agent host. Aileron is the one MCP they configure; everything else is behind it.
- **Opt-in policy, not mandatory**: An individual developer running Aileron locally with no `AILERON_API_URL` gets the embedded control plane — policy defaults to allow-all, no approval gates, no external dependencies. They still get the aggregation benefit (one MCP surface for N servers) and audit logging (local, for their own review). Policy becomes a feature they adopt when they want guardrails, not a gate they must pass through.
- **Local-first with remote as convenience**: Developers can run everything locally (the current model) and optionally shift API-backed servers to Aileron Cloud (hosted by Aileron the company) to skip credential management and server maintenance. This is additive — they don't lose anything by connecting to the remote pool, and they can fall back to local at any time.
- **Personal audit trail**: Even without enterprise policy, every tool call is logged. A developer debugging "what did my agent just do?" can review the local audit log. This is a feature for power users who want visibility into agent behavior.
- **Credential convenience**: For local-only use, credentials are in the YAML or local vault. If they connect to Aileron Cloud, they can OAuth into GitHub/Slack/etc. once and never manage tokens again. The remote server pool handles credential lifecycle.

**Risks to individual developer experience:**

- **Latency sensitivity**: Developers will feel remote tool call latency. If `github__create_pull_request` is noticeably slower through the remote pool than running the GitHub MCP server locally, developers will prefer local. The remote pool must be fast enough that convenience outweighs the round-trip.
- **Offline operation**: Developers on planes or poor connections lose access to remote servers. The gateway should degrade gracefully — local servers continue working, remote servers return clear errors (not hangs), and the developer can switch a server from remote to local in their config if needed.
- **Configuration complexity creep**: The YAML must stay simple for the common case. A developer adding a new MCP server should be one stanza with `name` and `command`. The `mode`, `policy_mapping`, and remote-specific fields are opt-in. We must resist making the simple case verbose to support the enterprise case.

### Enterprise management experience

The enterprise wants centralized control over what agents can do, without blocking developer productivity. The organization (or Aileron the company, hosting on their behalf) manages policy, credentials, and audit.

**What this enables:**

- **Centralized policy authoring**: Security and platform teams define policies once in the control plane. Every developer's local gateway enforces them. "No agent may push to main" or "all Jira ticket creation requires manager approval" — these rules apply uniformly without requiring anything from individual developers.
- **Two hosting models for the control plane**: The enterprise can either (a) use Aileron Cloud, where Aileron the company hosts the control plane and remote server pool, or (b) self-host the Aileron control plane within their own infrastructure. The local gateway doesn't care — it talks to `AILERON_API_URL` either way. This matters for regulated industries that cannot send tool calls through a third party.
- **Credential centralization**: API tokens for GitHub, Slack, Jira, etc. are stored in the control plane's vault and injected server-side into remote MCP server instances. Individual developer machines never see these tokens. When a token is rotated, it's updated once — not on N developer laptops. When a developer leaves, revoking their Aileron access revokes their tool access; no token scavenger hunt across machines.
- **Fleet-wide audit**: Every tool call from every developer flows through the central audit log. Compliance teams can answer "who created that PR?" or "which agents accessed production credentials this week?" without collecting logs from individual machines. For local tool calls, the gateway reports the policy decision and tool metadata to the central audit endpoint (the tool result stays local).
- **Managed MCP server catalog**: IT publishes a catalog of approved MCP servers in the control plane. Developers see them automatically — no manual installation. IT controls versions, ensuring the fleet isn't running vulnerable or deprecated server versions. New servers can be rolled out incrementally (by team, by region).
- **Approval routing**: The control plane routes approval requests based on organizational structure — a tool call in the `production` workspace goes to the oncall engineer, a new cloud resource request goes to the cost-approval channel. This is centrally configured, not per-developer.
- **Organizational boundaries for multi-team**: Workspaces in the control plane map to teams or projects. Policy rules, server catalogs, and audit logs are scoped per workspace. A developer on the payments team sees different available servers and policies than one on the marketing team.

**Risks to enterprise management experience:**

- **Control plane availability**: If the control plane is down, remote servers are unreachable and policy evaluation for local servers may fail. The gateway needs a resilience strategy:
  - **Fail-closed** (default for enterprise): if policy can't be evaluated, deny the tool call. Safe but disruptive.
  - **Fail-open** (opt-in): allow tool calls but log them for retroactive review. Risky but unblocking.
  - **Policy cache**: the gateway caches the last-known policy rules and evaluates locally when the control plane is unreachable. Bounded staleness (e.g., cache valid for 1 hour). This is the likely sweet spot.
- **Tenant isolation in the remote pool**: If multiple organizations share an Aileron Cloud instance, remote MCP server instances must be fully isolated — separate processes, separate credentials, no shared state. Per-tenant server pools are simpler but more expensive; shared pools with request-scoped isolation are cheaper but harder to secure. This is a deployment decision for Aileron Cloud, not an architectural one, but the architecture must support both.
- **Shadow IT risk**: If Aileron policy is too restrictive, developers may bypass it by running MCP servers directly (not through the gateway). The structural enforcement only works if the agent host is configured to use Aileron as its sole MCP server. Enterprise MDM or agent host configuration management is needed to close this gap — Aileron can't enforce it alone.
- **Configuration distribution**: Enterprise config (which servers are available, workspace assignments) must flow from the control plane to local gateways. The gateway should fetch its server catalog from the control plane at startup, not rely on locally-authored YAML for remote servers. This means the config schema has two layers: local overrides (developer-authored) and organizational baseline (control-plane-managed), with a clear merge strategy.

### Shared consequences (both personas)

- **Transport abstraction maintenance**: Two client implementations (stdio and HTTP/SSE) with different failure modes. Process crashes vs. network errors, buffered stdio vs. streaming HTTP, synchronous subprocess lifecycle vs. connection pooling. Both must satisfy the same `ToolExecutor` interface, and the gateway's tests must cover both paths.
- **MCP protocol evolution**: As the MCP specification evolves (new transports, new capabilities like resources and prompts), both client implementations must track it. The abstraction boundary should be narrow enough that protocol changes don't require rewriting the policy layer.
- **Audit schema consistency**: Local and remote tool calls must produce structurally identical audit events so that downstream analysis (dashboards, compliance reports, anomaly detection) doesn't need to branch on execution mode. The event schema should include an `execution_mode` field for filtering but not require it for processing.

## References

- `core/mcpclient/client.go` — current stdio subprocess client
- `core/config/config.go` — current YAML configuration schema
- `cmd/aileron-mcp/gateway.go` — policy interception in `routeToolCall()`
- [MCP Specification — Streamable HTTP Transport](https://modelcontextprotocol.io/specification/2025-03-26/basic/transports#streamable-http)
