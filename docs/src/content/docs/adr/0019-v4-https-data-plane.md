---
title: "ADR-0019: v4 HTTPS Data-Plane Mediation"
description: "Credentialed calls from the agent container flow through an Aileron HTTPS data plane that injects credentials at the proxy boundary instead of exposing secrets in the container."
order: 19
---

<div class="meta">
<table>
  <tr><th>Status</th><td>Accepted</td></tr>
  <tr><th>Date</th><td>2026-06-09</td></tr>
  <tr><th>Tracking</th><td><a href="https://github.com/ALRubinger/aileron/issues/896">#896</a>, <a href="https://github.com/ALRubinger/aileron/issues/747">#747</a></td></tr>
</table>
</div>

> **Forward pointer, 2026-06-15:** Where this ADR describes generated connector shims as data-plane clients, note that the rendered shim surface was retired in [#959](https://github.com/ALRubinger/aileron/issues/959). The HTTPS data plane and credential injection this ADR defines are unchanged; `aileron-mcp` ([ADR-0024](/adr/0024-sandbox-mcp-parity)) is now the sole in-container tool surface and the data plane's caller.

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

The twelfth #896 slice adds standard-client smoke coverage for the proxy URL shape that sandbox launch emits. A normal HTTP client configured from `HTTPS_PROXY=http://<session-id>:<daemon-token>@<daemon-host>` sends `Proxy-Authorization` through `CONNECT`; the daemon authenticates that userinfo-derived header, completes TLS interception with the session CA, and reaches the same connector credential-injection path. This confirms the internal proxy-bootstrap contract for clients that honor standard proxy environment variables without container-side secret injection.

### Credential injection only (Model A)

The proxy mediates credential injection at the TLS boundary. It is not an egress allowlist.

Cooperative HTTPS requests whose decrypted target does not uniquely match an installed connector spec operation are forwarded to the upstream unmodified. No credential is added. The full request and response stream through the established TLS connection so the in-container client sees the upstream's exact status, headers, and body. These forwarded calls record `sandbox.proxy.passthrough` audit events with the upstream host, path, method, and response status.

`sandbox.proxy.rejected` is reserved for protocol-level failures: non-CONNECT proxy requests, missing session CA material, connector specs invalid or unavailable, and upstream-reachability failures during passthrough. Match-failure outcomes such as no spec matched or ambiguous match are passthrough, not rejection.

The product claim is "sealed credentials" full stop. The agent never sees raw secret bytes, the daemon controls every credentialed call. The proxy is cooperative with the in-container client and is not a hard egress boundary. A hostile in-container process can still bypass the proxy by opening raw TCP sockets to an external host; container-level network hardening (network namespaces, egress firewalls) lives at a different layer and is out of scope for this ADR.

### Credential sealing as a general substrate

Model A above resolves credentials from installed connector specs. That is the first source the proxy consults, and it stays authoritative. The same TLS-boundary injection generalizes into a substrate that also seals credentials the user has bound to a host directly, without any connector spec. The substrate is the union of the two match sources plus passthrough, and the subsections below define its match sources, its scheme set, its emit mechanisms, and the rule that decides whether a given credential is sealable at all.

#### User-level host→credential bindings

A user can declare a binding as the triple `{host pattern, vault credential-ref, injection scheme}`. The binding is keyed on the host the request targets, not on the name of any CLI. There is no fake "github connector" spec standing in for `gh`; the binding names the host (`api.github.com`) and the vault credential-ref directly. This is a second, narrower match source that sits alongside connector specs. It does not replace connector specs and it does not outrank them.

Host-keying is the right shape because raw CLIs are not MCP tools. The rendered CLI-shim surface was retired in [#959](https://github.com/ALRubinger/aileron/issues/959), and `aileron-mcp` ([ADR-0024](/adr/0024-sandbox-mcp-parity)) is now the sole in-container tool surface. A tool like `gh` or `git` runs as a plain CLI inside the sandbox and talks HTTPS to its host; the substrate seals it at the host boundary it actually targets, with no per-CLI shim to model.

When the proxy decrypts a target it resolves the request in this order: connector-spec operation match, then user binding match, then passthrough. A request that uniquely matches an installed connector spec operation takes the connector path. A request that matches no connector spec but does match a user binding takes the binding path and receives the bound credential via the binding's declared scheme. A request that matches neither is forwarded unmodified as passthrough, exactly as in Model A. The connector path stays authoritative because it is consulted first; user bindings cover hosts the user wants sealed that no installed spec describes.

Connector specs carry governance that bare host bindings do not. A connector operation gives typed audit and per-operation approval gating under [ADR-0009](/adr/0009-user-channel) and [ADR-0010](/adr/0010-failure-handling). A user binding seals a credential for a host but offers no per-operation typing or approval surface. Users who need governed, approvable operations install a connector; users who only need a host's credential injected reach for a binding.

#### Injection schemes

The injection scheme set is closed. The five schemes are `bearer`, `basic`, `header-template`, `query-param`, and `sigv4-resign`. `bearer` attaches the credential as an `Authorization: Bearer` token. `basic` attaches it as HTTP Basic auth. `header-template` writes the credential into an arbitrary header per a verbatim template. `query-param` attaches it as a named query-string parameter. `sigv4-resign` re-signs the request with an AWS SigV4 signature derived from the credential.

The concrete header shape is per service, not per scheme alone. The GitHub API takes `bearer`. Git over HTTPS takes `basic`. Linear takes `header-template` with its header written verbatim. The scheme names the mechanism; the service determines the exact wire form.

All five schemes are implemented. The SigV4 signing core is std-library-only, built on `crypto/hmac` and `crypto/sha256`, and pulls in no AWS SDK. A `sigv4-resign` binding carries three non-secret params: the access key id, the region, and the service. These are explicit binding fields and are never inferred from the request host. The bound credential value carries the AWS secret access key, which the signer consumes only as HMAC key material and never writes onto any header, error, or audit surface. Static keys are supported today. Session-token handling via `X-Amz-Security-Token` is a declared follow-up.

#### Emit mechanisms

The substrate emits the injected credential to the upstream by one of two mechanisms. A binding descriptor names the mechanism in its `emit_mechanism` field, with the value `inject` or `sentinel-swap`.

The first is `inject`. The agent holds no credential material and the request carries no placeholder. The proxy adds the credential at the TLS boundary on a request that arrived without one. This is the default for a binding that declares no `emit_mechanism`.

The second is `sentinel-swap`. The agent holds a non-secret, format-mimicking placeholder that looks like a credential of the right shape but carries no secret value. The agent puts the sentinel where the credential would go, and the proxy swaps the sentinel for the real secret at egress. This covers clients that refuse to issue a request unless a syntactically valid credential is present.

Both mechanisms preserve the sealing claim. The agent only ever holds the sentinel, which is useless on its own, so the "agent never sees raw secret bytes" decision above still holds. Raw credential bytes are still never written to container env, image layers, mounted project files, or command-line args; the swap happens at the proxy, not in the container.

#### Sealability rule and residual exception

A credential is proxy-sealable if and only if some scheme plus emit-mechanism combination completes its request at the proxy boundary. Most credentials are sealable: pick the scheme the service expects, pick `inject` or `sentinel-swap` depending on whether the client tolerates a missing credential, and the proxy finishes the request without the agent ever holding the secret.

The residual exception is narrow. It is the conjunction of two conditions: a CLI that short-circuits the proxy (it does not honor `HTTPS_PROXY` or otherwise bypasses the TLS boundary) and that also rejects a syntactically valid sentinel (so sentinel-swap cannot satisfy it). A CLI that does only one of these is still sealable. Only the conjunction forces env injection, which is unsealed. This sits inside the existing threat-model scope: the cooperative-client assumption already governs whether the proxy sees the request, and a short-circuiting client is the same class of non-cooperative behavior described in [the threat model scope](#threat-model-scope) below.

When a credential cannot be sealed and must be env-injected, the daemon emits a `sandbox.proxy.credential_unsealed` audit event. The name sits in the established `sandbox.proxy.*` namespace alongside `sandbox.proxy.passthrough`, `sandbox.proxy.rejected`, `sandbox.proxy.binding_injected`, and `sandbox.proxy.disabled`. The event marks that an env-injected, unsealed credential was used for a launch, so the unsealed path is never silent. Like every other event in this ADR it carries no secret bytes: it records the credential binding reference, the host, and the reason the credential could not be sealed, not the credential value. The emission of this event lands with the code sub-issues that implement the substrate; this ADR names and describes it as design-of-record.

### Threat model scope

The HTTPS data plane assumes the in-container client cooperates with `HTTPS_PROXY`. Common third-party CLIs (`gh`, `aws`, `curl`, etc.) honor `HTTPS_PROXY` by default, so the cooperative model covers the dominant case. Hostile or proxy-bypassing client behavior is not in scope for this control. The sealing claim covers credentials, not exfiltration; an exfiltration threat is a different control and a different ADR.

The proxy seals Aileron-managed credentials. It does not strip credentials the in-container client supplies itself. An agent that has its own bearer token (a personal access token the user gave it, for instance) and chooses to attach `Authorization` to a passthrough request will have that header forwarded to the upstream unmodified. The proxy never adds credentials on the passthrough path.

Passthrough refuses IP-literal targets in loopback, link-local, RFC1918, IPv6 ULA, and carrier-grade NAT ranges. The daemon runs in the host network namespace, so an unfiltered passthrough would let an in-container client coerce the daemon into dialing 169.254.169.254 (cloud metadata) or other host-local addresses, exfiltrating credentials the agent should never see. Hostnames that resolve to private IPs are not blocked at this layer (DNS-rebinding protection is a deeper control that belongs at the container's network policy).

The passthrough request body is forwarded byte-for-byte with no daemon-side size cap. Operators bound concurrent connections, CPU, and outbound bandwidth at the container runtime layer. The matched/credentialed path separately buffers the request body because that path re-issues the request after credential injection.

The first #898 audit-schema slice distinguishes the HTTPS data-plane entry source in audit payloads. Connector-resolved proxy events now include `aileron.proxy.source` as `generated_connector_shim`, `daemon_request_boundary`, or `transparent_connect_tls`. (Historical: that slice also recorded `sandbox.proxy.rejected` for transparent proxy attempts whose decrypted target did not uniquely match an installed connector spec. The Model A amendment above replaced that behavior with `sandbox.proxy.passthrough`; `sandbox.proxy.rejected` now records only protocol-level failures.) These events continue to omit credential bytes, raw headers, request bodies, query strings, and full upstream URLs.

The finishing #896 work flips the v4 HTTPS proxy from opt-in to **default on** for `aileron launch --sandbox=docker`. The v4 product claim — credentialed third-party CLIs run in the container without raw credentials — only holds while the proxy is on, so the claim must hold out of the box. A new `--sandbox-proxy=auto|on|off` flag and `AILERON_SANDBOX_PROXY` env var control the policy explicitly. The flag takes precedence over the env var, which takes precedence over the default. `auto` (the default) behaves identically to `on` for Docker. `off` disables the bootstrap and records a `sandbox.proxy.disabled` audit event with reason `user_opt_out`. The previous `AILERON_SANDBOX_PROXY_BOOTSTRAP` env var is removed (pre-MVP policy: no backwards-compat shims).

v4 is Docker-only. Podman is deferred to a later track, not rejected. The runtime abstraction seam is preserved, so re-adding Podman is re-enabling it in `resolveRuntime` and the support matrix. See [ADR-0014](/adr/0014-spawn-sandbox-technology) and umbrella issue [#1050](https://github.com/ALRubinger/aileron/issues/1050) for the descope rationale.

The same finishing work adds a **fail-fast preflight**: when the proxy is requested but the BYO image does not meet the proxy contract (`aileron-install-proxy-ca` + `aileron-run-with-proxy-ca` on `PATH` + a writable trust store), launch refuses to start the container, prints an actionable error citing the [BYO Image Proxy Contract](/development/sandbox-agent-images/#byo-image-proxy-contract) docs and `--sandbox-proxy=off`, and emits `sandbox.proxy.disabled` with reason `preflight_failed`. Silent fallback from "proxy requested" to "proxy disabled" was rejected explicitly — silent fallback would break the credential-sealing claim with no operator-visible signal. Non-Docker sandbox modes emit `sandbox.proxy.disabled` with reason `unsupported_sandbox_mode`; `--sandbox-proxy=on` against an unsupported mode fails preflight.

## Consequences

Images must be able to trust the session CA or fail before the agent starts. BYO images need documented trust-store requirements and actionable launch validation.

CLIs that cannot use proxy-mediated HTTPS without env-var credentials remain unsupported for sealed-credential v4 flows unless a dedicated integration is designed later.

The proxy/data-plane implementation stands on its own; it never depended on shell-layer enforcement. (Container-only shell mediation was prototyped under [#801](https://github.com/ALRubinger/aileron/issues/801) and withdrawn in [#952](https://github.com/ALRubinger/aileron/issues/952); see [ADR-0021](/adr/0021-v4-shell-layer-mediation), Withdrawn.)

## Alternatives Considered

**Inject credentials into container env.** Rejected. It breaks the credential-sealing claim because the agent can read env vars and process state.

**Per-CLI credential files in the container.** Rejected as a default. Placeholder config files can point CLIs at normal auth paths, but real secrets must be injected at the proxy boundary.

**Only generated shims, no proxy.** Insufficient for third-party CLIs users expect to run inside the sandbox.

**Model B: proxy as egress allowlist (rejected).** An earlier shape of this ADR refused unmatched HTTPS upstreams at the proxy boundary and emitted `sandbox.proxy.rejected reason=operation_not_matched`. That shape claimed two things: credential sealing AND that the proxy was a hard egress boundary against unknown upstreams. The second claim was unenforceable against the threats it appeared to address (a hostile in-container process can open raw TCP sockets and bypass the proxy entirely), and it created visible friction for legitimate calls (a CLI hitting `httpbin.org` or a package index got an opaque proxy 403). The credential-sealing claim sharpens when the egress promise is dropped.

## References

- [Issue #896](https://github.com/ALRubinger/aileron/issues/896) — HTTPS proxy/data-plane implementation
- [Issue #898](https://github.com/ALRubinger/aileron/issues/898) — runtime audit-schema cross-cut
- [ADR-0009](/adr/0009-user-channel) — typed audit and per-operation governance that connector specs carry and bare host bindings do not
- [ADR-0010](/adr/0010-failure-handling) — per-operation approval gating connector operations receive
- [ADR-0017](/adr/0017-sandbox-composition) — current sandbox runtime cut
- [ADR-0018](/adr/0018-v4-single-binary-runtime) — single-binary runtime model
- [ADR-0024](/adr/0024-sandbox-mcp-parity) — `aileron-mcp` as the sole in-container tool surface; raw CLIs are not MCP tools, so the substrate seals them at the host boundary
- [Issue #959](https://github.com/ALRubinger/aileron/issues/959) — retirement of the rendered CLI-shim surface
- [Umbrella #1191](https://github.com/ALRubinger/aileron/issues/1191) — credential-sealing substrate generalization; this amendment is wave 1
- [Feedback #1181](https://github.com/ALRubinger/aileron/issues/1181) — the dogfooding feedback that motivated generalizing Model A into a substrate
- [BYO Image Proxy Contract](/development/sandbox-agent-images/#byo-image-proxy-contract) — the two helpers the proxy bootstrap requires from the in-container trust store
- [Sandbox Proxy CLI Verification Matrix](/development/sandbox-proxy-cli-matrix/) — manual recipes for `curl`, `gh`, `aws`, plus the automated `curl` integration test contract

### Shipped PRs

The slices that brought this ADR to Accepted, in chronological order:

- [#906](https://github.com/ALRubinger/aileron/pull/906) — first #896 slice: daemon-side connector-operation endpoint at `POST /v1/connector-operations/run`, 501-fail-closed.
- [#913](https://github.com/ALRubinger/aileron/pull/913) — second slice: opt-in proxy bootstrap mode, session-local CA generation, `HTTPS_PROXY` injection.
- [#914](https://github.com/ALRubinger/aileron/pull/914) — third slice: daemon-side proxy request boundary `POST /v1/sandbox-proxy/requests`.
- [#916](https://github.com/ALRubinger/aileron/pull/916) — fourth slice: image-side `aileron-install-proxy-ca` helper contract.
- [#918](https://github.com/ALRubinger/aileron/pull/918) — fifth slice: `aileron-run-with-proxy-ca` wired into sandbox launch.
- [#920](https://github.com/ALRubinger/aileron/pull/920) — sixth slice: first daemon-side upstream transport for bodyless HTTPS proxy requests with credential injection.
- [#921](https://github.com/ALRubinger/aileron/pull/921) — seventh slice: generated spec shim bridge for `GET`/`DELETE`/`HEAD`.
- [#923](https://github.com/ALRubinger/aileron/pull/923) — eighth slice: generated-shim JSON request bodies for `POST`/`PATCH`/`PUT`.
- [#930](https://github.com/ALRubinger/aileron/pull/930) — ninth slice: transparent proxy entrypoint with proxy URL userinfo + Proxy-Authorization.
- [#931](https://github.com/ALRubinger/aileron/pull/931) — tenth slice: transparent proxy transport boundary with CONNECT + TLS interception.
- [#935](https://github.com/ALRubinger/aileron/pull/935) — eleventh slice: transparent proxy → credential injection boundary via connector spec match.
- [#939](https://github.com/ALRubinger/aileron/pull/939) — twelfth slice: standard-client smoke coverage for the proxy URL shape.
- [#940](https://github.com/ALRubinger/aileron/pull/940) — first #898 audit-schema slice: `aileron.proxy.source` discriminator + `sandbox.proxy.rejected` for unresolved transparent attempts.
- [#967](https://github.com/ALRubinger/aileron/pull/967) — finishing Section A: end-to-end transparent-proxy body coverage.
- [#968](https://github.com/ALRubinger/aileron/pull/968) — finishing Section D: BYO Image Proxy Contract docs + `aileron sandbox check --agent` validation extension.
- [#970](https://github.com/ALRubinger/aileron/pull/970) — finishing Section B: default-on flip + `--sandbox-proxy` flag + `sandbox.proxy.disabled` audit event.
- [#971](https://github.com/ALRubinger/aileron/pull/971) — finishing Section C: replacement of the three honest-stub 501 sites with structured rejections.
- [#972](https://github.com/ALRubinger/aileron/pull/972) — finishing Section E: CLI matrix docs page + `curl` integration test.
- [#974](https://github.com/ALRubinger/aileron/pull/974) — finishing Section G: audit-schema reconciliation in the observability guide + shape conformance test.
