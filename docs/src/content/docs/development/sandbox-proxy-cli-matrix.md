---
title: "Sandbox Proxy — CLI Verification Matrix"
description: "Verify Aileron's v4 sandbox HTTPS proxy works with common CLIs (curl, gh, aws). Install, configure, observe."
order: 10
---

This page is how you confirm the v4 sandbox HTTPS proxy is mediating credentials correctly from the perspective of common command-line HTTPS clients. Each section is a recipe you can run by hand inside an `aileron launch --sandbox=docker` session against a published Aileron connector spec.

The matrix complements the [Sandbox MCP walkthrough](/development/sandbox-mcp-walkthrough/) (which exercises the same data plane from the agent's MCP transport) and the [BYO Image Proxy Contract](/development/sandbox-agent-images/#byo-image-proxy-contract) (which is what these CLIs depend on inside the container).

## Per-CLI sealing properties

Each CLI has two properties that govern how Aileron seals its traffic. These are documentation properties, not config fields.

| CLI | proxy-sealable | emit-mechanism |
|---|---|---|
| curl | yes | inject |
| gh | yes | sentinel-swap |
| aws | yes | inject |

`proxy-sealable` means the daemon can inject the real credential at the TLS forward-proxy boundary so the container never holds it.

`emit-mechanism` is how the in-container client is made to emit a request the proxy can seal. It is independent of the injection scheme (bearer, basic, and so on), which is how the proxy writes the credential once the request arrives.

- The `inject` mechanism: the launcher plants no token. The client emits an unauthenticated request, and the proxy injects the bound credential unconditionally at egress. `curl` issues the request with no token. `git`-over-HTTPS issues an unauthenticated request because the launcher mounts a no-op credential helper.
- The `sentinel-swap` mechanism: the launcher plants a non-secret, format-mimicking sentinel token. Some clients short-circuit locally when they hold no token, so they never issue the request and there is nothing for the proxy to seal. The sentinel makes the client treat itself as authenticated and issue the request. The proxy then swaps the sentinel for the real credential at egress. The proxy swaps only the sentinel it itself planted. A foreign token on a `sentinel-swap` host is forwarded unchanged. See [ADR-0019](/adr/0019-v4-https-data-plane/).

Note: `aws`'s `emit-mechanism` is placeholder-plant plus `sigv4-resign`. The launcher plants placeholder static credentials so botocore signs locally, then the proxy strips that signature and re-signs with the real secret at egress. This is distinct from the unauthenticated-emit `inject` case above, where the client emits no token at all. The table marks `aws` as `inject` to keep the column to two canonical values, and the `aws` section below documents the `sigv4-resign` scheme in full.

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

`ghcr.io/alrubinger/aileron-sandbox-base` ships with `curl`. For other base images:

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

- The upstream's actual HTTP response. `example.com` returns 200 with its example page; an unreachable upstream returns 502 from the proxy.
- A `sandbox.proxy.passthrough` audit event. Confirm:

  ```bash
  aileron audit list --limit 5
  ```

  Look for `aileron.proxy.decision: "passthrough"` and `aileron.proxy.upstream.host: "example.com"`. The full URL is not in the payload, only the upstream host, path, and method. Under the credential-injection-only model (ADR-0019) the proxy does not inject a credential on unmatched requests, and it does not refuse them. Container-level egress hardening lives at a different layer.

## gh (GitHub CLI)

`gh` is `proxy-sealable` via `sentinel-swap`. It honors `HTTPS_PROXY` once you set it, and it picks up the system CA store, so it works through the Aileron proxy with no extra config. Aileron seals it against the built-in `api.github.com` host binding, so you do not need an installed GitHub connector spec for `gh api` to work.

`gh` is the reason the `sentinel-swap` mechanism exists. `gh` validates its auth state locally before making a request. With no token it short-circuits and never issues the request, so the `inject` mechanism (plant nothing, seal the emitted request) has nothing to seal. The launcher closes this gap by planting a non-secret, format-mimicking sentinel as `GH_TOKEN` when a `user/github` entry exists in your vault. The sentinel passes `gh`'s own local validation, so `gh` issues its request. The daemon recognizes the sentinel at the TLS boundary and swaps in your real GitHub token before the request leaves the host. The real token never enters the container. The sentinel is non-secret and safe to see in `echo $GH_TOKEN` output; presenting it to GitHub authenticates nothing.

Store your token once with `aileron auth github`. The launcher does the rest on every sandbox launch.

### Install

| Distro | Install |
|---|---|
| Debian / Ubuntu | See [github.com/cli/cli](https://github.com/cli/cli/blob/trunk/docs/install_linux.md) for the official apt repo. |
| Alpine | `apk add github-cli` (Alpine's community repo). |
| RHEL / Fedora | `dnf install -y gh`. |

### Configure

There is nothing to configure inside the container. The launcher already set `HTTPS_PROXY` and planted the sentinel `GH_TOKEN`. You can confirm both:

```bash
# Set by the launcher
echo $HTTPS_PROXY

# The non-secret sentinel the launcher planted. This is NOT your real
# token; the daemon swaps it for the real one at egress.
echo $GH_TOKEN
```

Do not run `gh auth login` with a hand-typed placeholder. The launcher's sentinel is the only token `gh` needs, and the daemon owns the swap.

### Success case

```bash
gh api user
```

Expected:

- 200 with the authenticated user's JSON.
- A `sandbox.proxy.binding_injected` audit event with `aileron.proxy.binding.host: "api.github.com"` and `aileron.proxy.binding.scheme: "bearer"`. The daemon swapped the sentinel for the real token; the upstream saw `Authorization: Bearer <real-token>` and the audit payload holds no token or sentinel bytes.

### Foreign-token case

If you supply your own token instead of the sentinel (for example you ran `gh auth login --with-token` with a real token), the proxy does not swap it. It seals only the sentinel it planted.

```bash
echo "ghp_your_own_real_token" | gh auth login --with-token
gh api user
```

Expected:

- The upstream sees your own token unchanged. The daemon injects nothing.
- A `sandbox.proxy.foreign_token_not_swapped` audit event with `aileron.proxy.binding.host: "api.github.com"`. The payload holds no token bytes.

### Verification status

The `gh` sentinel-swap is asserted by the deterministic end-to-end test `TestSentinelSwap_*` in `internal/app/sandbox_forward_proxy_sentinel_swap_test.go`, which drives a sentinel-bearing and a foreign-token request through the real CONNECT/TLS proxy boundary against a fake `api.github.com` upstream and asserts the swap, the no-swap, the isolation, and the audit shape. A real `gh` against live `api.github.com` over the network is not exercised in CI (it requires a real GitHub token and network egress from the test runner), so this row is asserted by the deterministic test rather than by an empirical live-network run.

## aws (AWS CLI)

`aws`'s botocore HTTP layer reads `HTTPS_PROXY` from the environment, so the launcher-set proxy applies with no extra config. You only need to point `aws` at Aileron's session CA. Set `AWS_CA_BUNDLE` to the mounted CA so botocore trusts the proxy's intercepted TLS.

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

`aws` is `proxy-sealable` via `sigv4-resign`. The placeholder credentials above let botocore sign the request locally so it leaves the client. At the TLS proxy boundary the daemon strips that local signature and re-signs the request with the real secret access key resolved host-side, using the access key id declared in the `sigv4-resign` binding. The binding carries only the access key id and is keyed by its credential identity rather than a host. The daemon derives the signing region and service from the resolved upstream host, so the binding carries no region or service and no second copy of the region can drift from the host being signed for. The real secret access key never enters the container; it is HMAC key material the daemon holds in the vault. See [ADR-0019](/adr/0019-v4-https-data-plane/).

### Success case

For a `sigv4-resign` host binding on `s3.amazonaws.com` whose `credential_ref` resolves the secret access key from your vault (`user/aws`):

```bash
aws s3api list-buckets
```

Expected:

- 200 with the upstream's response body.
- A `sandbox.proxy.binding_injected` audit event with `aileron.proxy.binding.host: "s3.amazonaws.com"` and `aileron.proxy.binding.scheme: "sigv4-resign"`. The upstream received a well-formed `Authorization: AWS4-HMAC-SHA256 …` header whose `Credential=<access-key-id>/.../<region>/<service>/aws4_request` scope and signature validate against the daemon's secret. The placeholder secret you configured locally never reaches the upstream, and the audit payload holds no secret bytes.

The verification point is that the secret access key is present in the daemon vault and nowhere in the container. The container's static credentials are placeholders. The signature the upstream accepts was computed host-side with the real secret.

### Unmatched case

```bash
aws ec2 describe-instances --region us-east-1  # unmatched if no sigv4-resign binding covers ec2
```

Expected:

- `aws` reports the EC2 endpoint's actual response. With no `sigv4-resign` binding for that host the proxy injects nothing, so the placeholder credentials botocore signed with reach the upstream and AWS returns a SigV4 authentication error.
- `sandbox.proxy.passthrough` audit, `aileron.proxy.upstream.host: "ec2.us-east-1.amazonaws.com"`. The proxy does not re-sign the request and does not refuse it. A matched host gets a real signature; an unmatched host passes through unchanged.

## What "no credential leak" means in practice

After each of the recipes above, run:

```bash
aileron audit list --limit 10 | jq '.[] | .payload' | grep -i -E "bearer|token|api[_-]key|secret"
```

This should return nothing. The audit subsystem sanitizes payloads before they touch the log — even if you accidentally pasted a real credential into a `curl` body, the daemon's proxy boundary strips it before recording.

## Automated coverage

Two real-client subprocess integration tests live under the `integration_sandbox` build tag and run together with one command:

```bash
task test:integration:sandbox-proxy-clients
```

The target is fail-fast and requires both `curl` and `aws` on PATH. It does not self-skip.

A `curl`-driven Go integration test lives at `internal/app/sandbox_proxy_curl_integration_test.go`. The test stands up a fake HTTPS upstream, runs `curl` as a host subprocess pointed at the in-process proxy with a generated session CA, and asserts:

1. `curl` performs CONNECT-based HTTPS interception through the proxy. The test runs `curl` with the `--proxy` flag and an empty environment (no `HTTPS_PROXY` set), so it proves the proxy intercepts a CONNECT tunnel and seals the TLS stream. The env-driven `HTTPS_PROXY` + `AWS_CA_BUNDLE` configuration path is proven separately by the `aws` test below.
2. The proxy injects the credential at the boundary (the fake upstream sees `Authorization: Bearer <secret>`, never the `curl` invocation).
3. The container's `curl` invocation does not carry the credential (proves credential isolation).
4. A `connector.proxy.proxied` audit event is emitted with the matched connector FQN, tool, operation, upstream host (not URL), and `aileron.connector.boundary: https_proxy`.

An `aws`-driven SigV4 integration test lives at `internal/app/sandbox_proxy_aws_sigv4_integration_test.go`. The test stands up a SigV4-validating fake HTTPS upstream, runs `aws s3api list-buckets` as a host subprocess against a `sigv4-resign` host binding with the secret access key in the daemon vault and placeholder static credentials in the subprocess environment, and asserts:

1. `aws` honors `HTTPS_PROXY` and `AWS_CA_BUNDLE` (proves env-driven configuration works for the AWS CLI).
2. The upstream receives a syntactically and cryptographically valid `AWS4-HMAC-SHA256` Authorization header whose signature the upstream recomputes with the same secret the daemon holds and confirms matches (proves a genuine valid SigV4 reached upstream, not merely a 200).
3. The real secret access key appears in none of the `aws` subprocess environment, the `aws` argv, or any upstream request header, query, or body (proves credential isolation).
4. A `sandbox.proxy.binding_injected` audit event is emitted with `aileron.proxy.binding.scheme: "sigv4-resign"` and the upstream host, and no secret bytes in the payload.

The upstream is a local SigV4-validating stub, not live AWS. The stub recomputes the canonical request and signature from what it received using the secret the daemon vault holds, which proves the proxy emitted a valid signature without requiring network egress to AWS or a real account.

The `gh` sentinel-swap is covered deterministically by `internal/app/sandbox_forward_proxy_sentinel_swap_test.go`, which drives sentinel-bearing and foreign-token requests through the in-process proxy against a fake `api.github.com` upstream and asserts the swap, the no-swap, the sentinel-never-reaches-upstream isolation, and the no-secret audit shape. It does not require real `gh`, a real token, or real network.
