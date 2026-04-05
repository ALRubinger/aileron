# ADR-0008: Per-User and Enterprise-Governed MCP Server Management

**Status:** Accepted
**Date:** 2026-04-04
**Relates to:** ADR-0004

## Context

MCP server installations were system-wide — when any user installed a server, it appeared for all users. The `MCPServerConfig` entity had no `user_id` field and no ownership concept, and MCP endpoints did not enforce authentication or authorization. There was no mechanism for enterprise administrators to govern which MCP servers their members could use.

ADR-0004 established the enterprise account model with roles (owner/admin/member) and personal vs organizational enterprises, but did not address resource scoping.

## Decision

A two-layer scoping model for MCP server management:

1. **Per-user scoping**: Added `user_id` (readOnly) and `source` (enum: personal|enterprise, readOnly) to `MCPServerConfig`. All MCP CRUD operations are scoped to the authenticated user. When auth is disabled (local dev), operations fall back to unscoped behavior.

2. **Enterprise governance**: New `EnterpriseMCPServer` entity with `enterprise_id`, `auto_enabled` boolean, and standard server config fields. Enterprise admins (owner/admin roles) manage an approved list of servers. Servers marked `auto_enabled: true` are mandatory for all enterprise members.

3. **Merged user view**: `GET /v1/mcp-servers` returns the union of the user's personal installations (source: personal) and enterprise auto-enabled servers (source: enterprise). Enterprise-sourced servers cannot be modified or deleted by members.

4. **Enterprise endpoints**: Six new endpoints under `/v1/enterprise/mcp-servers` for CRUD + credentials. Write operations require owner/admin role. Read operations available to any authenticated enterprise member.

5. **ID prefix convention**: User servers use `mcp_` prefix, enterprise servers use `emcp_` prefix, preventing collision and making store routing unambiguous.

6. **Marketplace integration**: The `installed` flag on marketplace listings reflects the user's complete view (personal + enterprise auto-enabled).

7. **Personal accounts**: Gmail/personal email users are the sole owner of their personal enterprise, so they naturally have full admin rights over their own enterprise approved list.

8. **Auth enforcement**: All MCP and marketplace endpoints now enforce authentication (401 if not authenticated). When auth is disabled (`s.users == nil`), endpoints gracefully fall back to unscoped behavior for local development.

## Consequences

- Users have isolated MCP server environments — installing a server no longer affects other users
- Enterprise admins can enforce a standard set of MCP servers across their organization
- The `source` field in API responses lets UIs distinguish personal from enterprise-managed servers and adjust controls accordingly (e.g., hide delete buttons for enterprise servers)
- Vault paths for enterprise servers use a separate namespace (`mcp-servers/enterprise/{ent_id}/{id}/{env_var}`) to prevent credential collision
- The in-memory store is ephemeral, so no data migration is needed. A future Postgres implementation will need to handle the `user_id` and `enterprise_id` columns
- The store SPI (`MCPServerFilter.UserID`, new `EnterpriseMCPServerStore` interface) is ready for a Postgres implementation
