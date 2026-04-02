# ADR-0003: Marketplace Dashboard UI

**Status:** Accepted
**Date:** 2026-04-02

## Context

ADR-0002 established the backend API for MCP marketplace integration — registry proxy, one-click install, CRUD on installed servers, and vault-backed credential storage. That ADR explicitly deferred the dashboard UI as a follow-up PR.

Without a visual interface, developers must interact with the marketplace through `curl` or scripts. This defeats the goal of making Aileron the frictionless first-install for agent workflows. A dashboard that matches the existing Aileron UI patterns (approvals, traces, policies) completes the marketplace feature as a usable product surface.

Two approaches were considered for the UI structure:

### Single page with tabs

A single `/servers` page combining marketplace browsing and installed server management in a tabbed layout.

**Problems:**
- Conflates two distinct workflows: discovery (browsing/searching) vs. management (configuring/monitoring).
- Tab state is lost on navigation, making it harder to deep-link or share URLs.
- The marketplace server list can be large; mixing it with the smaller installed-servers list creates visual noise.

### Separate pages (selected)

Dedicated routes for marketplace browsing (`/marketplace`) and installed server management (`/servers`, `/servers/{id}`), each with focused UX.

**Problems:**
- Slightly more files to maintain.
- Users must navigate between pages when installing then configuring.

The navigation cost is minimal — the install flow on `/marketplace` includes inline credential configuration, so users rarely need to leave the page mid-flow. The separation keeps each page simple and consistent with the existing one-concern-per-route pattern (approvals, traces, policies).

## Decision

Add three new routes to the SvelteKit frontend, following the same patterns as existing pages (Svelte 5 runes, inline styles, CSS custom properties, `onMount` data loading).

### Routes

| Route | Purpose |
|-------|---------|
| `/marketplace` | Browse and search the MCP Registry; install servers with optional credential configuration |
| `/servers` | List installed MCP servers with status and mode badges; delete servers |
| `/servers/[id]` | Server detail: configuration display, environment variables, credential management, delete |

### Marketplace Browser (`/marketplace`)

- **Search**: Text input with 300ms debounce calling `GET /v1/marketplace/servers?q=`.
- **Server grid**: Cards showing name, description, version, and credential count. Each card has an "Install" button or green "Installed" badge.
- **Post-install credential flow**: After `POST /v1/marketplace/servers/{registryId}/install`, if `required_credentials` is non-empty, an inline form appears for entering credential values. Each credential is stored via `POST /v1/mcp-servers/{id}/credentials`. Users can "Skip for now" and configure credentials later from the server detail page.

### Installed Servers (`/servers`)

- **Polling**: Refreshes every 5 seconds (matching the approvals page pattern) to reflect status changes.
- **Server cards**: Name, description, status badge (`stopped`/`running`/`error`), mode badge (`local`/`remote`), registry provenance, and timestamps. Each card links to the detail page.
- **Delete**: Inline remove button with confirmation dialog.
- **Empty state**: Links to `/marketplace` when no servers are installed.

### Server Detail (`/servers/[id]`)

- **Configuration display**: ID, description, command, mode, registry ID, policy mapping, and timestamps in a two-column grid.
- **Environment variables**: Key-value display with a lock indicator for `vault://` references.
- **Credential management**: Form to store new credentials in the vault by env var name and secret value.
- **Danger zone**: Delete button with confirmation, navigates back to `/servers` on success.

### API Client

New functions added to `ui/src/lib/api.ts`, all using the existing `apiFetch` helper:

| Function | Endpoint |
|----------|----------|
| `searchMarketplace(query?)` | `GET /v1/marketplace/servers?q=` |
| `installMarketplaceServer(registryId)` | `POST /v1/marketplace/servers/{registryId}/install` |
| `listMCPServers()` | `GET /v1/mcp-servers` |
| `getMCPServer(id)` | `GET /v1/mcp-servers/{id}` |
| `deleteMCPServer(id)` | `DELETE /v1/mcp-servers/{id}` |
| `setMCPServerCredential(id, envVarName, secretValue)` | `POST /v1/mcp-servers/{id}/credentials` |

The `apiFetch` helper was updated to handle HTTP 204 No Content responses (returned by DELETE) by returning `null` before attempting to parse JSON.

### Navigation

- **Nav bar**: "Marketplace" and "Servers" links added to the global layout alongside Approvals, Traces, and Policies.
- **Home page**: Two new cards added to the dashboard grid — "Marketplace" and "Servers".

## Consequences

### What this enables

- **Visual marketplace**: Developers can browse, search, and install MCP servers without leaving the browser. The registry's full catalog is searchable with instant results.
- **Guided credential setup**: The post-install credential flow reduces the chance of installing a server and forgetting to configure its API keys. The vault-backed storage from ADR-0002 is now accessible through the UI.
- **Server visibility**: The installed servers list provides at-a-glance status monitoring. Polling keeps the view current without manual refresh.
- **Consistent UX**: All three pages follow the exact same code patterns as existing pages — same state management, loading states, card styles, and color conventions. No new dependencies or abstractions were introduced.

### What this defers

- **Server start/stop/restart controls**: The `status` field is displayed but not actionable. Lifecycle management requires the dynamic gateway sync deferred in ADR-0002.
- **Server configuration editing**: The detail page displays configuration read-only. An edit form for command, env, and mode is a follow-up once the update workflow is validated.
- **Credential rotation/deletion**: Credentials can be set but not listed, rotated, or deleted through the UI. The vault API supports this; the UI does not yet surface it.
- **Pagination**: The marketplace grid and servers list render all results. Pagination is unnecessary at current registry and install-base sizes but will be needed as both grow.
- **Optimistic UI updates**: Install and delete operations reload the full list. Optimistic local state updates would improve perceived responsiveness.

## References

- ADR-0002: MCP Marketplace Integration (backend API this UI consumes)
- `ui/src/routes/marketplace/+page.svelte` — marketplace browser
- `ui/src/routes/servers/+page.svelte` — installed servers list
- `ui/src/routes/servers/[id]/+page.svelte` — server detail page
- `ui/src/lib/api.ts` — API client functions
- `ui/src/routes/+layout.svelte` — global navigation
