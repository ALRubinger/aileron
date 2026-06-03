---
title: "Sandbox Connector Specs"
description: "How installed connector specs become sandbox-visible tools and generated HTTPS shims."
order: 8
---

Sandbox connector specs are the machine-readable contract Aileron uses to expose installed connector operations inside sandboxed launch sessions.

When a connector package is installed, Aileron looks for:

```text
~/.aileron/store/connectors/sha256/<hash>/aileron.connector.v1.json
```

At launch time, Aileron reads those installed specs, renders `/etc/aileron/tools.txt`, and mounts generated command shims under `/usr/local/bin`. The shims are HTTPS clients that call the local Aileron daemon through `AILERON_API_URL`.

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

Optional operation metadata such as `summary`, `description`, `method`, `path`, `hosts`, `idempotency`, `approval`, `credential`, `inputs`, and `audit` is rendered into shim help and is available to the data-plane work that follows.

## Generated Tools

For each tool, launch writes one `tools.txt` entry:

```text
google  github://acme/aileron-connector-google -- Aileron connector operations: gmail.messages.search
```

It also writes a command shim:

```bash
google --help
google gmail.messages.search --args '{"q":"from:alice@example.com"}' --json
```

The shim posts this payload to `${AILERON_API_URL%/}/connector-operations/run`:

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

The endpoint is the stable sandbox-side contract for generated connector-operation shims. The mediated HTTPS proxy/data-plane implementation behind that endpoint is tracked separately in [#896](https://github.com/ALRubinger/aileron/issues/896).

In the current daemon cut, `/v1/connector-operations/run` resolves the connector, tool, and operation against installed specs, records an audit event for recognized direct-shim attempts, and fails closed with `501 not_implemented`. The HTTPS data-plane boundary also exposes an internal `POST /v1/sandbox-proxy/requests` endpoint for proxy attempts. It resolves candidate bodyless HTTPS requests against the same installed specs, requires method, path, and upstream host to match the operation, resolves any spec-declared credential binding in the daemon, injects supported credentials at the upstream request boundary, returns a sanitized response, and audits `connector.proxy.proxied` without credential bytes or query strings. Unknown connector operations are rejected before any execution attempt. Full forward-proxy integration and request-body transport remain tracked in [#896](https://github.com/ALRubinger/aileron/issues/896).

## Conflict Handling

Sandbox launch fails with an actionable error when:

- two installed connector specs resolve to the same tool command
- a connector spec tool command conflicts with an installed action shim
- a connector spec tool command conflicts with the selected agent command, such as `claude`

Installed action shims still come from action manifests. Spec-backed shims do not replace action dispatch; they add the connector-operation contract needed for direct, typed operations.
