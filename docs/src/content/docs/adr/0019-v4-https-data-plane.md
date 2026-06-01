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
