---
title: "ADR-0019: v4 HTTPS Data-Plane Mediation"
description: "Credentialed calls from the agent container flow through an Aileron HTTPS data plane that injects credentials at the proxy boundary instead of exposing secrets in the container."
order: 19
---

<div class="meta">
<table>
  <tr><th>Status</th><td>Proposed</td></tr>
  <tr><th>Date</th><td>2026-06-01</td></tr>
  <tr><th>Tracking</th><td><a href="https://github.com/ALRubinger/aileron/issues/896">#896</a>, <a href="https://github.com/ALRubinger/aileron/issues/747">#747</a></td></tr>
</table>
</div>

## Context

The #796 cut line gives sandboxed agents a static runtime surface: session env, `/etc/aileron/tools.txt`, generated connector shims, and `AILERON_API_URL` calls back to the daemon.

That is enough for installed-action dispatch, but not for the broader v4 claim that credentialed third-party CLIs can run in the container without receiving raw credentials. For tools like `gh`, `aws`, or generated connector clients, the credential boundary must be outside the agent container.

## Decision

v4 credentialed network mediation uses an Aileron HTTPS data plane:

- Sandboxed launch sets `HTTPS_PROXY` when the proxy layer is enabled.
- Aileron creates a session-local CA and arranges for the selected image/container to trust it.
- Connector specs and runtime policy describe which requests can receive which credentials.
- The data plane injects credentials at the TLS/proxy boundary.
- Raw credential bytes are not written to container env, image layers, mounted project files, or command-line args.
- Audit events record the operation identity, session, credential binding reference, decision, and upstream destination without logging secret material.

The current `AILERON_API_URL` shim dispatch path remains valid for explicit installed-action execution. The HTTPS data plane adds the network mediation layer needed for credentialed CLIs and spec-generated HTTPS clients.

The first #896 implementation slice establishes the daemon-side connector-operation endpoint at `POST /v1/connector-operations/run`. Generated spec shims can reach this stable contract now; the daemon resolves installed spec metadata, audits recognized attempts, and fails closed with `501 not_implemented` until session CA bootstrap, `HTTPS_PROXY`, and credential injection are implemented behind the same contract.

The second #896 slice adds an internal opt-in bootstrap mode for launch validation and follow-on implementation work. When `AILERON_SANDBOX_PROXY_BOOTSTRAP=1` is set for a sandboxed launch, Aileron generates a session-local CA under the daemon state directory, mounts the public CA into the container at `/etc/aileron/proxy/ca.pem`, and sets `HTTPS_PROXY`, `HTTP_PROXY`, and `NO_PROXY` for the agent container. This does not yet install the CA into the image trust store, run a credential-injecting proxy, or make credentialed HTTPS execution succeed; it only establishes the launch/session metadata boundary that the data plane will use.

The third #896 slice adds the daemon-side proxy request boundary at `POST /v1/sandbox-proxy/requests`. This endpoint is internal to the HTTPS data-plane implementation: it resolves a candidate proxy request against installed connector specs, records sanitized proxy-vs-direct audit metadata for recognized HTTPS attempts, and fails closed with `501 not_implemented` until credential injection and upstream transport are implemented. It deliberately does not accept raw headers, request bodies, query strings, or credential material in the audit payload.

The fourth #896 slice adds the image-side trust-store helper contract. `aileron/sandbox-base` now carries `aileron-install-proxy-ca`, creates `/etc/aileron/proxy`, and launch validation requires the helper when proxy bootstrap mode is enabled. The helper can validate the mounted session CA as an unprivileged user and can install it with `update-ca-certificates` when invoked as root. The runtime still does not run a root pre-start trust install, proxy upstream HTTPS traffic, or inject credentials; those remain follow-on #896 work.

The fifth #896 slice wires that helper into sandbox launch. In internal proxy-bootstrap mode, the agent container starts as root, runs `aileron-run-with-proxy-ca` to install the mounted session CA into the container trust store, then drops back to the `agent` user and execs the requested agent command in the same container.

The sixth #896 slice adds the first daemon-side upstream transport path behind `POST /v1/sandbox-proxy/requests`. Recognized, bodyless HTTPS requests now resolve the matching connector spec operation by method, path, and explicit upstream host allowlist; resolve the spec-declared credential binding in the daemon; inject `api_key` or `oauth2` credentials as an upstream bearer token; proxy the request; return a sanitized response body; and audit `connector.proxy.proxied` without recording credential bytes or query strings. The full forward-proxy/TLS interception surface, request bodies, and generated client integration remain follow-on work.

The seventh #896 slice bridges generated spec shims onto the existing proxy request boundary for bodyless operations. `POST /v1/connector-operations/run` can now proxy eligible `GET`, `DELETE`, and `HEAD` operations by building the upstream HTTPS URL from the installed spec's method, path, and first allowed host, then encoding shim args as query parameters.

The eighth #896 slice adds generated-shim JSON request bodies for write-style methods. `POST`, `PATCH`, and `PUT` operations now send the shim `args` object upstream as an `application/json` body through the same daemon proxy boundary. Audit remains sanitized: it records operation identity and upstream destination/status, not credential bytes, query strings, or request body values. Full transparent `HTTPS_PROXY` / TLS interception for arbitrary clients remains follow-on work.

The ninth #896 slice prepares the transparent proxy entrypoint. Internal proxy-bootstrap launch now embeds the launch session id, and the local daemon token when available, into the standard proxy URL userinfo so HTTPS clients send `Proxy-Authorization` on proxy requests. The daemon recognizes proxy-shaped requests before normal webapp/API routing, validates standard Basic proxy auth, associates the request with the sandbox session, and fails closed with `501 not_implemented` until the CONNECT/TLS interception transport lands. This separates proxy authentication/session binding from the later request mediation implementation.

The tenth #896 slice proves the transparent proxy transport boundary. Authenticated `CONNECT host:443` requests can now load the session CA/key generated under the daemon state directory, complete a TLS server handshake with a per-host leaf certificate, read the first decrypted HTTP request from the tunnel, and fail closed with `501 not_implemented`. This still does not match the decrypted request to connector specs, inject credentials, proxy upstream, or audit successful transparent-proxy execution; those remain the next data-plane slices.

The eleventh #896 slice routes the first transparent proxy requests through the same credential-injection boundary. After authenticated `CONNECT` and TLS interception, the daemon matches the decrypted request by method, host, and path against installed connector specs. A unique match reuses the existing sandbox proxy executor to resolve the daemon-side credential binding, inject credentials, proxy the upstream request, and return the sanitized upstream response through the TLS tunnel. Missing or ambiguous matches, binding failures, invalid decrypted requests, and oversized request bodies fail closed. This still remains an internal proxy-bootstrap path pending broader client smoke coverage and final audit-schema work.

## Consequences

Images must be able to trust the session CA or fail before the agent starts. BYO images need documented trust-store requirements and actionable launch validation.

CLIs that cannot use proxy-mediated HTTPS without env-var credentials remain unsupported for sealed-credential v4 flows unless a dedicated integration is designed later.

The proxy/data-plane implementation is tracked separately from shell interception. #801 can use the same policy/audit pipeline, but shell mediation is not a prerequisite for proxy-based credential injection.

## Alternatives Considered

**Inject credentials into container env.** Rejected. It breaks the credential-sealing claim because the agent can read env vars and process state.

**Per-CLI credential files in the container.** Rejected as a default. Placeholder config files can point CLIs at normal auth paths, but real secrets must be injected at the proxy boundary.

**Only generated shims, no proxy.** Insufficient for third-party CLIs users expect to run inside the sandbox.

## References

- [Issue #896](https://github.com/ALRubinger/aileron/issues/896) — HTTPS proxy/data-plane implementation
- [ADR-0017](/adr/0017-sandbox-composition) — current sandbox runtime cut
- [ADR-0018](/adr/0018-v4-single-binary-runtime) — single-binary runtime model
