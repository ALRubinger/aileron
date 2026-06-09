---
title: "Sandbox Proxy — CLI Verification Matrix"
description: "Verify Aileron's v4 sandbox HTTPS proxy works with common CLIs (curl, gh, aws). Install, configure, observe."
order: 10
---

This page is how you confirm the v4 sandbox HTTPS proxy is mediating credentials correctly from the perspective of common command-line HTTPS clients. Each section is a recipe you can run by hand inside an `aileron launch --sandbox=docker` session against a published Aileron connector spec.

The matrix complements the [Sandbox MCP walkthrough](/development/sandbox-mcp-walkthrough/) (which exercises the same data plane from the agent's MCP transport) and the [BYO Image Proxy Contract](/development/sandbox-agent-images/#byo-image-proxy-contract) (which is what these CLIs depend on inside the container).

## What the proxy guarantees

When you call an installed connector operation through the proxy:

- Aileron resolves the operation against your installed connector specs.
- The matching credential binding is resolved on the daemon side and injected into the upstream request at the boundary.
- The container never sees the raw credential. The CLI inside the container only knows the proxy URL.
- A `connector.proxy.proxied` audit event is recorded with the matched connector FQN, tool, operation, upstream host (not the full URL), and a sanitized status. No credential bytes leak into the audit log.

When the CLI calls something the proxy does not recognize (no matching connector operation, ambiguous match, oversized body, etc.), the proxy fails closed with `sandbox.proxy.rejected` (or `connector.proxy.rejected` for matched-but-rejected). The container sees the rejection envelope, never an upstream response.

## Common prerequisites

Every recipe below assumes:

1. A running `aileron launch --sandbox=docker <agent>` session. The proxy is on by default (see [ADR-0019](/adr/0019-v4-https-data-plane/)).
2. At least one installed connector spec whose operation matches the request you're about to make. Use `aileron connector list` to see what's installed; pick an operation host + path you can hit from inside the container.
3. The Aileron-mounted CA at `/etc/aileron/proxy/ca.pem`. The launcher mounts this automatically; the BYO image's `aileron-install-proxy-ca` adds it to the trust store before the agent starts.

The proxy URL inside the container is `$HTTPS_PROXY`. The launcher sets it; you can confirm with `echo $HTTPS_PROXY` from within the agent's shell. It looks like `http://<session-id>:<daemon-token>@host.docker.internal:<port>`.

## curl

`curl` is the canonical test target. It respects `HTTPS_PROXY` out of the box and lets you control the CA bundle explicitly with `--cacert`.

### Install

`aileron/sandbox-base` ships with `curl`. For other base images:

| Distro | Install |
|---|---|
| Debian / Ubuntu | `apt-get install -y curl` |
| Alpine | `apk add curl` |
| RHEL / Fedora | `dnf install -y curl` |

### Configure

The launcher sets `HTTPS_PROXY` for you, so a bare `curl` call already routes through the proxy. The CA bundle is in the system trust store (installed by `aileron-install-proxy-ca`), so `curl` validates Aileron's session CA without extra flags. You can pin the CA bundle explicitly with `--cacert` if you want to be defensive:

```bash
curl --cacert /etc/aileron/proxy/ca.pem https://api.example.test/path
```

### Success case

Pick an installed connector operation. For a hypothetical Linear connector with `GET /graphql` on `api.linear.app`:

```bash
curl --silent --show-error \
  https://api.linear.app/graphql \
  -G --data-urlencode "query=query{viewer{id}}"
```

Expected:

- HTTP 200 with the upstream's JSON body.
- A `connector.proxy.proxied` audit event. Confirm with:

  ```bash
  aileron audit list --limit 5
  ```

  Look for an entry like:

  ```json
  {
    "event_type": "connector.proxy.proxied",
    "payload": {
      "aileron.connector.fqn": "github://acme/aileron-connector-linear",
      "aileron.connector.tool": "linear",
      "aileron.connector.operation": "viewer.get",
      "aileron.connector.boundary": "https_proxy",
      "aileron.connector.mediation": "https_proxy",
      "aileron.connector.decision": "proxied",
      "aileron.proxy.source": "transparent_connect_tls",
      "aileron.proxy.upstream.host": "api.linear.app",
      "aileron.proxy.upstream.status": 200
    }
  }
  ```

  No credential bytes anywhere in the payload. The query string and request body are not echoed.

### Failure case

Hit an unmatched upstream:

```bash
curl --silent --show-error -i https://example.com/
```

Expected:

- HTTP 403 with a plain-text body: `sandbox proxy decrypted request did not match an installed connector operation`.
- A `sandbox.proxy.rejected` audit event. Confirm:

  ```bash
  aileron audit list --limit 5
  ```

  Look for `aileron.proxy.reject_reason: "operation_not_matched"`. The full URL is not in the payload, only the upstream host.

## gh (GitHub CLI)

`gh` honors `HTTPS_PROXY` once you set it, and it picks up the system CA store, so it works through the Aileron proxy with no extra config — as long as you have an installed GitHub connector spec whose host is `api.github.com` (or `github.com` for `git`-shaped operations).

### Install

| Distro | Install |
|---|---|
| Debian / Ubuntu | See [github.com/cli/cli](https://github.com/cli/cli/blob/trunk/docs/install_linux.md) for the official apt repo. |
| Alpine | `apk add github-cli` (Alpine's community repo). |
| RHEL / Fedora | `dnf install -y gh`. |

### Configure

```bash
# Confirm HTTPS_PROXY is set by the launcher
echo $HTTPS_PROXY

# gh requires you to "log in" to a host once per profile.
# Through the Aileron proxy, the credential is injected by the daemon,
# so use the "token" flow with a placeholder — gh's own auth state
# only needs to think it's authenticated.
echo "placeholder-token-aileron-injects-real" | gh auth login --with-token
```

### Success case

```bash
gh api user
```

Expected:

- 200 with the authenticated user's JSON.
- `connector.proxy.proxied` audit, `aileron.proxy.upstream.host: "api.github.com"`.

### Failure case

```bash
gh repo list --json id  # unmatched if no `repos` operation is in the spec
```

Expected:

- `gh` reports an HTTP 403 from upstream.
- `sandbox.proxy.rejected` audit, reason `operation_not_matched`.

## aws (AWS CLI)

`aws` does not read `HTTPS_PROXY` from the standard env by default — it reads `HTTPS_PROXY` only when configured to do so via `~/.aws/config`, or via the deprecated `--no-verify-ssl` workaround. The cleanest path is to set `HTTPS_PROXY` and `AWS_CA_BUNDLE` env vars and rely on `aws`'s standard env support.

### Install

| Distro | Install |
|---|---|
| Debian / Ubuntu | Follow the [official bundled-installer](https://docs.aws.amazon.com/cli/latest/userguide/getting-started-install.html) flow. |
| Alpine | `apk add aws-cli` (Alpine's community repo). |
| RHEL / Fedora | `dnf install -y awscli2`. |

### Configure

```bash
export AWS_CA_BUNDLE=/etc/aileron/proxy/ca.pem
# HTTPS_PROXY is already set by the launcher; aws's botocore reads it.

# Like gh, aws still wants local credentials — use a placeholder. The
# real credential is injected by the daemon proxy.
aws configure set aws_access_key_id placeholder-aileron-injects-real
aws configure set aws_secret_access_key placeholder-aileron-injects-real
aws configure set region us-east-1
```

### Success case

For an installed AWS connector spec with, say, `GET /` on `sts.us-east-1.amazonaws.com`:

```bash
aws sts get-caller-identity
```

Expected:

- 200 with the calling identity.
- `connector.proxy.proxied` audit, `aileron.proxy.upstream.host: "sts.us-east-1.amazonaws.com"`.

### Failure case

```bash
aws ec2 describe-instances  # unmatched if no ec2 operation is in the spec
```

Expected:

- 403 from the proxy.
- `sandbox.proxy.rejected` audit, reason `operation_not_matched`.

## What "no credential leak" means in practice

After each of the recipes above, run:

```bash
aileron audit list --limit 10 | jq '.[] | .payload' | grep -i -E "bearer|token|api[_-]key|secret"
```

This should return nothing. The audit subsystem sanitizes payloads before they touch the log — even if you accidentally pasted a real credential into a `curl` body, the daemon's proxy boundary strips it before recording.

## Automated coverage

A `curl`-driven Go integration test lives at `internal/app/sandbox_proxy_curl_integration_test.go` and runs under the `integration_sandbox` build tag:

```bash
task test:integration:sandbox
```

The test stands up a fake HTTPS upstream, runs `curl` as a host subprocess pointed at the in-process proxy with a generated session CA, and asserts:

1. `curl` honors the `HTTPS_PROXY` env var (proves env-driven configuration works for the canonical case).
2. The proxy injects the credential at the boundary (the fake upstream sees `Authorization: Bearer <secret>`, never the `curl` invocation).
3. The container's `curl` invocation does not carry the credential (proves credential isolation).
4. A `connector.proxy.proxied` audit event is emitted with the matched connector FQN, tool, operation, upstream host (not URL), and `aileron.connector.boundary: https_proxy`.

The `gh` and `aws` recipes above are manual-only for now; the same harness can be extended to cover them when there's a published Aileron connector spec for each upstream.
