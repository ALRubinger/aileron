# ADR-0002: MCP Marketplace Integration

**Status:** Accepted
**Date:** 2026-04-01

## Context

Aileron's value proposition depends on being the first thing developers install when working with AI agents. Today, configuring downstream MCP servers requires hand-editing `aileron.yaml` — the developer must know the server's npm package name, its command-line arguments, and which environment variables it needs. This friction works against the goal of making Aileron the natural starting point for agent-based workflows.

The MCP ecosystem has matured to include an **official MCP Registry** (`registry.modelcontextprotocol.io`) backed by Anthropic, GitHub, and Microsoft. The registry exposes a public REST API (`GET /v0/servers`) returning server metadata, package coordinates, and required environment variables. Third-party directories (Smithery, Glama, PulseMCP) also exist, each with varying API surfaces and data enrichments.

Three approaches were considered for enabling click-to-install MCP server management through Aileron:

### Build a proprietary catalog

Aileron maintains its own curated server catalog with metadata, installation scripts, and compatibility ratings.

**Problems:**
- Duplicates effort already invested in the official registry.
- Requires ongoing curation and maintenance of server metadata.
- Fragments the ecosystem — server authors must publish to yet another registry.
- Delays launch: the catalog must reach critical mass before it's useful.

### Aggregate multiple registries

Proxy the official registry plus Smithery, Glama, and PulseMCP to maximize coverage.

**Problems:**
- Each registry has a different API shape, authentication model, and data schema.
- Deduplication across sources is non-trivial (same server, different metadata).
- Availability depends on multiple third-party services.
- Increased complexity for marginal coverage gain — the official registry is already the authoritative source.

### Proxy the official registry (selected)

Proxy the official MCP Registry as the sole discovery source. Store installed server configurations locally through Aileron's existing store layer. Integrate credential storage with the existing vault.

## Decision

Proxy the **official MCP Registry** for server discovery, and manage installed server configurations through Aileron's existing REST API and store layer.

### Architecture

```
┌─────────────────────────────────────────────┐
│              Aileron Dashboard              │
│                                             │
│  ┌──────────────┐  ┌────────────────────┐  │
│  │  Installed   │  │    Marketplace     │  │
│  │  Servers     │  │    Browser         │  │
│  └──────┬───────┘  └────────┬───────────┘  │
└─────────┼───────────────────┼──────────────┘
          │                   │
          ▼                   ▼
┌─────────────────────────────────────────────┐
│            Aileron Control Plane            │
│                                             │
│  /v1/mcp-servers     /v1/marketplace        │
│  (CRUD)              /servers (proxy)       │
│                      /servers/{id}/install  │
│                                             │
│  ┌──────────────┐  ┌────────────────────┐  │
│  │ MCPServer    │  │  Registry Client   │  │
│  │ Store        │  │  (cached proxy)    │  │
│  └──────────────┘  └────────┬───────────┘  │
│                              │              │
│  ┌──────────────┐            │              │
│  │   Vault      │            │              │
│  │ (credentials)│            │              │
│  └──────────────┘            │              │
└──────────────────────────────┼──────────────┘
                               │ HTTPS
                               ▼
                ┌──────────────────────────┐
                │  Official MCP Registry   │
                │  registry.model          │
                │  contextprotocol.io      │
                └──────────────────────────┘
```

### API Surface

**Server management** (CRUD on installed configs):

| Method | Endpoint | Purpose |
|--------|----------|---------|
| `GET` | `/v1/mcp-servers` | List installed MCP servers |
| `POST` | `/v1/mcp-servers` | Register a server manually |
| `GET` | `/v1/mcp-servers/{id}` | Get server config |
| `PUT` | `/v1/mcp-servers/{id}` | Update server config |
| `DELETE` | `/v1/mcp-servers/{id}` | Remove a server |
| `POST` | `/v1/mcp-servers/{id}/credentials` | Store a credential in vault |

**Marketplace** (registry proxy):

| Method | Endpoint | Purpose |
|--------|----------|---------|
| `GET` | `/v1/marketplace/servers?q=` | Browse/search, enriched with `installed` status |
| `POST` | `/v1/marketplace/servers/{registryId}/install` | One-click install from registry |

### Install Flow

1. User browses `/v1/marketplace/servers`, optionally filtering by search query.
2. Each result is enriched with an `installed` boolean by cross-referencing the `MCPServerStore` by `registry_id`.
3. User calls `POST /v1/marketplace/servers/{registryId}/install`.
4. The handler looks up the server in the registry, selects the best package (preferring npm), derives the `command` array, and creates an `MCPServerConfig` with `status=stopped`.
5. The response includes the created config and a `required_credentials` list (from the registry's `environmentVariables`).
6. For each required credential, the user calls `POST /v1/mcp-servers/{id}/credentials` with the env var name and secret value.
7. The handler stores the secret in the vault at `mcp-servers/{id}/{env_var_name}` and updates the server config's `env` map to `vault://mcp-servers/{id}/{env_var_name}`.
8. The existing `config.ResolveVaultEnv` mechanism resolves these references when the gateway launches downstream servers.

### Registry Client Design

- **Single source**: Proxies `registry.modelcontextprotocol.io/v0/servers` only.
- **Caching**: In-memory cache with 15-minute TTL. The registry is public and changes infrequently. Cache is invalidated on TTL expiry; no manual invalidation needed.
- **Search**: Client-side substring matching on server name and description. Sufficient for the current registry size; can move to server-side filtering if the registry adds query parameters.
- **No authentication required**: The official registry is public-read.

### Extended MCPServerConfig Schema

The existing `MCPServerConfig` schema gains fields to support lifecycle tracking and marketplace provenance:

| Field | Type | Purpose |
|-------|------|---------|
| `description` | `string` | Human-readable description |
| `mode` | `enum(local, remote)` | Execution mode (defaults to `local`) |
| `status` | `enum(stopped, running, error)` | Current lifecycle state (read-only) |
| `registry_id` | `string` | Registry identifier if installed from marketplace |
| `created_at` | `datetime` | Creation timestamp (read-only) |
| `updated_at` | `datetime` | Last update timestamp (read-only) |

## Consequences

### What this enables

- **Click-to-install**: Developers browse available MCP servers and install with a single API call. The dashboard (follow-up PR) will provide a visual marketplace experience.
- **Credential safety**: Secrets are stored in the vault immediately, never persisted in plaintext config. The `vault://` reference pattern is already proven in the gateway's credential injection path.
- **Provenance tracking**: The `registry_id` field links installed servers back to their marketplace origin, enabling update notifications and version tracking in future work.
- **API-first**: The dashboard is a client of the same REST API available to CLI tools, scripts, and integrations. A developer could `curl` the install endpoint in a setup script.

### What this defers

- **Dynamic gateway sync**: Installing a server via the API does not hot-reload the running gateway. A restart is needed to pick up new servers. This is acceptable for the install-time use case — developers install servers occasionally, not continuously. Dynamic sync (runtime add/remove with `notifications/tools/list_changed`) is a follow-up.
- **Server lifecycle management**: The `status` field exists but is not yet driven by actual process state. Start/stop/restart controls require the gateway sync work above.
- **Multiple registry sources**: The architecture supports adding Smithery, PulseMCP, or private registries behind the same `/v1/marketplace` API surface. The `RegistryClient` interface can be extended to aggregate sources with deduplication. Not needed until the official registry proves insufficient.
- **Version updates**: No mechanism yet to detect that an installed server has a newer version in the registry. The `registry_id` and `version` fields provide the data model; the update-check logic is future work.
- **Dashboard UI**: This PR is backend-only. The marketplace browser and server management pages are a follow-up PR.

### Risks

- **Registry availability**: If `registry.modelcontextprotocol.io` is down, marketplace browsing fails. The 15-minute cache provides short-term resilience. The install flow and all CRUD operations on installed servers are unaffected (they use local state). For a more robust fallback, a longer-lived disk cache could be added.
- **Registry schema evolution**: The registry is at API v0 with a stated freeze. If the schema changes, the `core/registry/types.go` structs will need updating. The types are intentionally kept as a thin mapping layer to minimize coupling.
- **Package derivation heuristics**: The install flow picks the "best" package from the registry entry (preferring npm, then any package with a runtime command). This heuristic may not match every server's intended installation method. Manual server registration (`POST /v1/mcp-servers`) remains available as a fallback.

## References

- `core/registry/client.go` — registry proxy client with caching
- `core/registry/types.go` — Go types mapping the registry JSON schema
- `core/store/mem/mcpserver.go` — in-memory MCPServerStore implementation
- `core/app/handlers.go` — CRUD, marketplace, and credential handlers
- `core/api/openapi.yaml` — extended API specification
- [Official MCP Registry](https://registry.modelcontextprotocol.io/)
- [MCP Registry API specification](https://registry.modelcontextprotocol.io/docs)
- ADR-0001: Hybrid MCP Execution Model (establishes the `local`/`remote` mode distinction)
