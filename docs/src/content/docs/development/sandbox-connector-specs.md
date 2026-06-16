---
title: "Sandbox Connector Specs"
description: "How installed connector specs drive data-plane operation validation in sandboxed launch sessions."
order: 8
---

Sandbox connector specs are the machine-readable contract Aileron uses to validate and mediate connector operations inside sandboxed launch sessions.

When a connector package is installed, Aileron looks for:

```text
~/.aileron/store/connectors/sha256/<hash>/aileron.connector.v1.json
```

At launch time, Aileron reads those installed specs and derives the connector tools and operations the daemon uses to validate requests on the HTTPS data plane. The agent reaches connector operations through `aileron-mcp` ([ADR-0024](/adr/0024-sandbox-mcp-parity/)), which is the sole in-container tool surface; the generated `tools.txt`/shim surface was retired in [#959](https://github.com/ALRubinger/aileron/issues/959).

## Schema

The current schema version is `aileron.connector.v1`.

```json
{
  "schema_version": "aileron.connector.v1",
  "connector": {
    "fqn": "github://acme/aileron-connector-google",
    "version": "1.2.3"
  },
  "tools": [
    {
      "name": "google",
      "description": "Google APIs",
      "operations": [
        {
          "name": "gmail.messages.search",
          "summary": "Search Gmail messages",
          "method": "GET",
          "path": "/gmail/v1/users/me/messages",
          "hosts": ["gmail.googleapis.com"],
          "idempotency": "idempotent",
          "credential": "oauth2",
          "inputs": [
            {
              "name": "q",
              "type": "string",
              "required": false,
              "description": "Gmail search query"
            }
          ]
        }
      ]
    }
  ]
}
```

Required fields:

| Field | Requirement |
|---|---|
| `schema_version` | Must be `aileron.connector.v1`. |
| `connector.fqn` | Must be a valid connector FQN. |
| `tools[].name` | Must be unique within the spec and use only letters, digits, dots, dashes, underscores, or colons. |
| `tools[].operations[]` | Each tool must declare at least one operation. |
| `operations[].name` | Must be unique within its tool and use only letters, digits, dots, dashes, underscores, or colons. |
| `operations[].hosts[]` | Required for proxy transport. Each entry is an allowed upstream host, with an optional port, and must not include a URL scheme or path. |
| `operations[].inputs[].name` | Optional, but when present must be unique within the operation and use the same restricted character set. |
| `operations[].audit[].name` | Optional, but when present must be unique within the operation and use the same restricted character set. |

Optional operation metadata such as `summary`, `description`, `method`, `path`, `hosts`, `idempotency`, `approval`, `credential`, `inputs`, and `audit` is carried into the derived operation help the daemon uses for data-plane validation.

## Data-Plane Operation Contract

The daemon resolves connector operations against installed specs at the `/v1/connector-operations/run` data-plane endpoint. A request names the connector, tool, and operation, and carries the operation args:

```json
{
  "connector_fqn": "github://acme/aileron-connector-google",
  "tool": "google",
  "operation": "gmail.messages.search",
  "args": {
    "q": "from:alice@example.com"
  }
}
```

`GET`, `DELETE`, and `HEAD` operations with spec-declared method, path, and upstream hosts are mediated through the sandbox HTTPS proxy boundary with args encoded as query parameters. `POST`, `PATCH`, and `PUT` operations send args upstream as an `application/json` request body. The proxy boundary resolves any spec-declared credential binding in the daemon, injects supported credentials at the upstream request boundary, returns a sanitized response, and audits `connector.proxy.proxied` without credential bytes, query strings, or request body values. Unknown connector operations are rejected before any execution attempt. Full forward-proxy integration for arbitrary HTTPS clients remains tracked in [#896](https://github.com/ALRubinger/aileron/issues/896).

## Conflict Handling

Spec loading fails with an actionable error when two installed connector specs resolve to the same tool name. The tool-name sanitizer normalizes each `tools[].name` before the comparison, so two specs whose names normalize to the same value are reported as a conflict.
