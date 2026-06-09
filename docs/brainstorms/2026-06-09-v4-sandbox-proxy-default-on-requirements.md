---
date: 2026-06-09
topic: v4-sandbox-proxy-default-on
---

# v4 Sandbox HTTPS Proxy — Default-On Policy

## Summary

Make the v4 HTTPS proxy default-on for `aileron launch --sandbox=docker`, with a fail-fast preflight that refuses to launch when the image cannot meet the proxy contract. Provide `--sandbox-proxy=off` as the escape hatch and emit a structured `sandbox.proxy.disabled` audit event whenever a session runs without the proxy in force. This decision flips [ADR-0019](docs/src/content/docs/adr/0019-v4-https-data-plane.md) from Proposed to Accepted.

---

## Problem Frame

The v4 HTTPS data-plane shipped across twelve slices on `main` (PRs #906, #913, #914, #916, #918, #920, #921, #923, #930, #931, #935, #939, #940). The wire-up — session CA generation, container trust install, transparent CONNECT/TLS interception, daemon-side credential injection, spec matching, sanitized audit — is functionally complete and gated behind `AILERON_SANDBOX_PROXY_BOOTSTRAP=1`.

The v4 product claim is "credentialed third-party CLIs run in the container without raw credentials." That claim only holds while the proxy is on. With an internal-only opt-in flag as the gate, the claim is opt-in too — most operators never set the variable, the credential boundary stays inside the container env, and Aileron ships looking architecturally identical to a connector dispatcher with extra Docker steps.

The choice is whether to flip the proxy on by default for `--sandbox=docker` (the issue #896 section B option 1) or to keep it opt-in and document when an operator should turn it on (option 2). The downstream consequence is real: default-on makes the credential-sealing claim the floor of sandboxed launch and exposes BYO image compatibility as a launch-time concern; opt-in keeps current behavior and leaves the architectural-disciplines intent of [#747](https://github.com/ALRubinger/aileron/issues/747) unmet.

---

## Key Decisions

- **Default-on for `--sandbox=docker`.** Sandbox launch enables the proxy bootstrap unconditionally for the Docker sandbox mode. This makes the v4 credential-sealing claim the default posture, not an internal flag. Operators who can't or don't want to participate explicitly disable.

- **Fail-fast preflight over warn-and-disable.** When the resolved image does not meet the proxy contract, sandbox launch refuses to start and surfaces an actionable error citing the BYO contract documentation and the `--sandbox-proxy=off` escape hatch. Silently disabling the proxy after detecting a contract miss is rejected: the credential-sealing claim must never silently not hold. A loud preflight error is the correct failure mode because the alternative is a session that the operator believes is sealed but isn't.

- **`--sandbox-proxy=off` is the only escape hatch.** No `--unsafe-no-proxy` flag, no two-step ack, no per-session prompt. The flag value plus the audit event together carry the policy signal.

- **Structured audit for non-proxy sessions.** Whenever sandbox launch starts a session where the proxy is not in force, emit `sandbox.proxy.disabled` with a `reason` field (`user_opt_out`, `preflight_failed`, `unsupported_sandbox_mode`). Reviewers can query positively for sessions where the claim does not hold rather than inferring from the absence of `connector.proxy.proxied` / `sandbox.proxy.rejected` events.

- **Env var rename, no compat shim.** `AILERON_SANDBOX_PROXY_BOOTSTRAP` is renamed to `AILERON_SANDBOX_PROXY` with values that map to the flag (`on`, `off`, `auto`). The old variable is dropped, not honored. Aileron is pre-MVP and the project convention is no backwards-compatibility shims before release.

- **Off is narrower than it sounds.** `--sandbox-proxy=off` only disables the transparent HTTPS proxy. Spec-shim credential sealing through `/v1/connector-operations/run` still holds — generated connector shims continue to resolve credentials daemon-side and never write secrets into the container. The audit signal and user-facing docs need to reflect this distinction so "off" isn't read as "no credential boundary."

- **ADR-0019 flip depends on section D landing.** The ADR moves from Proposed to Accepted as part of this work, but only after the BYO image proxy contract is documented (issue #896 section D). Preflight errors that can't cite a public docs page are not actionable, so the BYO contract docs are a precondition for default-on, not an independent track.

---

## Requirements

**Default behavior and escape hatch**

- R1. `aileron launch --sandbox=docker` enables the HTTPS proxy bootstrap by default with no operator action.
- R2. `--sandbox-proxy=off` disables the proxy bootstrap for the session and proceeds to launch.
- R3. `--sandbox-proxy=on` forces the proxy bootstrap on; if the image cannot meet the contract, preflight fails as in R5.
- R4. `--sandbox-proxy=auto` is the default value and behaves identically to `--sandbox-proxy=on` for the Docker sandbox mode.

**Preflight and image contract**

- R5. When the proxy is requested (`on` or `auto`) and the resolved image does not meet the proxy contract, sandbox launch fails before starting the container, with an actionable error that names the missing contract element and points to the BYO contract documentation.
- R6. The preflight error message identifies the `--sandbox-proxy=off` escape hatch as the operator-side workaround.
- R7. Sandbox launch must not silently fall back from "proxy requested" to "proxy disabled" based on image inspection. The only paths to a non-proxy session are explicit operator opt-out (R2) or a non-Docker sandbox mode (R12).

**Audit and observability**

- R8. Whenever a sandbox session starts with the proxy not in force, Aileron emits a `sandbox.proxy.disabled` audit event for that session.
- R9. The `sandbox.proxy.disabled` event carries a `reason` field with one of `user_opt_out`, `preflight_failed`, `unsupported_sandbox_mode`.
- R10. The `sandbox.proxy.disabled` event conforms to the cross-cutting runtime audit schema tracked in [#898](https://github.com/ALRubinger/aileron/issues/898) (session id, source, result, timestamps, sanitized metadata; no credential bytes).

**Configuration surface and migration**

- R11. The `AILERON_SANDBOX_PROXY_BOOTSTRAP` environment variable is renamed to `AILERON_SANDBOX_PROXY` with values `on`, `off`, `auto`. The old name is no longer honored.
- R12. For sandbox modes other than `--sandbox=docker`, the proxy is not in force and `sandbox.proxy.disabled` is emitted with reason `unsupported_sandbox_mode`. `--sandbox-proxy=on` against an unsupported mode fails preflight with an actionable error.

**Documentation and ADR**

- R13. [ADR-0019](docs/src/content/docs/adr/0019-v4-https-data-plane.md) status flips from Proposed to Accepted, capturing the default-on decision and the fail-fast preflight rationale.
- R14. Sandbox composition and launch-flag documentation describe the default behavior, the `--sandbox-proxy` flag, the `AILERON_SANDBOX_PROXY` env var, and the operator-facing meaning of each preflight failure path.
- R15. Public documentation explicitly states that `--sandbox-proxy=off` disables only the transparent HTTPS proxy; spec-shim credential sealing via `/v1/connector-operations/run` continues to hold.

---

## Acceptance Examples

- AE1. **Default Docker launch with compliant image.** **Given** an operator runs `aileron launch --sandbox=docker` against `aileron/sandbox-base` (no `--sandbox-proxy` flag, no env var). **When** sandbox launch resolves the image. **Then** the proxy bootstrap runs, the session CA is mounted, `HTTPS_PROXY` is set in the container env, and no `sandbox.proxy.disabled` event is emitted. **Covers R1, R4.**

- AE2. **Default Docker launch with non-compliant BYO image.** **Given** an operator runs `aileron launch --sandbox=docker` against a BYO image with no `aileron-install-proxy-ca` helper. **When** sandbox launch validates the image. **Then** launch fails before the container starts with an error naming the missing helper, citing the BYO contract docs page, and surfacing `--sandbox-proxy=off` as the escape hatch. No container is started. A `sandbox.proxy.disabled` event with reason `preflight_failed` is emitted. **Covers R5, R6, R7, R8, R9.**

- AE3. **Operator opts out explicitly.** **Given** an operator runs `aileron launch --sandbox=docker --sandbox-proxy=off`. **When** sandbox launch starts the container. **Then** no proxy bootstrap runs, no session CA is mounted, `HTTPS_PROXY` is not set, the container launches normally, and a `sandbox.proxy.disabled` event with reason `user_opt_out` is emitted. **Covers R2, R8, R9.**

- AE4. **Operator forces on against non-compliant image.** **Given** an operator runs `aileron launch --sandbox=docker --sandbox-proxy=on` against a BYO image that does not meet the contract. **When** sandbox launch validates the image. **Then** launch fails before the container starts with the same actionable error as AE2. A `sandbox.proxy.disabled` event with reason `preflight_failed` is emitted. **Covers R3, R5, R8, R9.**

- AE5. **Non-Docker sandbox mode.** **Given** an operator runs `aileron launch` with a sandbox mode other than `docker`. **When** sandbox launch starts the session. **Then** the proxy is not in force and a `sandbox.proxy.disabled` event with reason `unsupported_sandbox_mode` is emitted. `--sandbox-proxy=on` against the same configuration fails preflight. **Covers R8, R9, R12.**

- AE6. **Legacy env var is no longer honored.** **Given** an operator's environment sets `AILERON_SANDBOX_PROXY_BOOTSTRAP=1` (the previous opt-in flag). **When** sandbox launch reads configuration. **Then** the legacy variable is ignored; the default-on behavior is governed entirely by `--sandbox-proxy` and `AILERON_SANDBOX_PROXY`. **Covers R11.**

- AE7. **Off disables only the transparent proxy.** **Given** an operator runs `aileron launch --sandbox=docker --sandbox-proxy=off`, and the session uses a generated GitHub connector shim. **When** the agent calls the connector op. **Then** the shim hits `/v1/connector-operations/run`, the daemon resolves the credential binding daemon-side, and the upstream call is credentialed without raw secrets ever entering the container env. **Covers R15.**

---

## Scope Boundaries

### In scope (this decision)

- Default-on policy for `--sandbox=docker`, the `--sandbox-proxy` flag, the renamed env var, the preflight failure mode, the audit event, and the ADR flip.

### Deferred to issue #896 implementation

- Section A: transparent proxy request body support for non-shim clients (request body plumbing, 1 MiB cap, `request_body_too_large` rejection).
- Section C: replace remaining 501 fail-closed sites with structured rejections + audit.
- Section D: BYO image proxy contract documentation and `aileron sandbox check` validation tooling. The ADR-Accepted flip in R13 depends on D landing.
- Section E: supported-CLI matrix documentation and an automated integration test (curl as the automated target was confirmed during planning).
- Section F: ADR-0019 References section listing the shipped PRs.
- Section G: audit schema cross-cut with [#898](https://github.com/ALRubinger/aileron/issues/898).

### Outside this product's identity

- **Detection-based silent fallback.** "Warn and disable the proxy if the image doesn't meet the contract" was considered and rejected. Silently downgrading the security claim is exactly the failure mode default-on is meant to eliminate.
- **`--unsafe-no-proxy` style ack flags.** Adding deliberate friction to the escape hatch was considered and rejected as too heavy for v4. The audit event carries the policy signal without forcing operators through an unsafe-flag ritual.
- **Backwards compatibility with `AILERON_SANDBOX_PROXY_BOOTSTRAP`.** Aileron is pre-MVP; the project convention is no compat shims before release.
- **Shell-layer mediation.** [#801](https://github.com/ALRubinger/aileron/issues/801) was descoped 2026-06-08 ([ADR-0021](docs/src/content/docs/adr/0021-v4-shell-layer-mediation.md) Withdrawn). Do not re-architect the proxy assuming shell mediation is coming.
- **Cloud / hosted topology.** v4 is customer-operated; v4.x BYOC and v5 SaaS are tracked separately.

---

## Dependencies / Assumptions

- **Section D lands before or with the default-on flip.** Preflight error messages must cite a published BYO contract docs page (R5, R6). The ADR-Accepted flip (R13) is gated on that documentation existing.
- **`aileron/sandbox-base` meets the contract.** The shipped first-party image already carries `aileron-install-proxy-ca` and the `/etc/aileron/proxy/` mount point. Default-on does not break the canonical image; only BYO images that haven't adopted the contract.
- **The runtime audit schema in #898 accommodates `sandbox.proxy.disabled`.** R10's conformance is taken on faith here; the cross-cut is resolved during #896 section G.
- **`auto` exists as a flag value for forward compatibility.** Today `auto` and `on` are equivalent for `--sandbox=docker` (the only mode with a proxy). The value space leaves room for image-aware or mode-aware semantics later without a flag rename.

---

## Outstanding Questions

### Deferred to planning

- **Env var precedence.** When both `AILERON_SANDBOX_PROXY` and `--sandbox-proxy` are set, the flag wins. The exact precedence rules for mismatched explicit-on env + explicit-off flag (and vice versa) are a planning concern, not a product one.
- **Exact preflight checks.** Whether preflight runs `aileron-install-proxy-ca --check`, inspects the image filesystem directly, or both, is implementation choice. The contract surface is documented in section D; the validation mechanics are planning's call.
- **Exact wording of the actionable error.** Planning + docs land the final message text.

---

## Sources / Research

- [Issue #896](https://github.com/ALRubinger/aileron/issues/896) — v4 HTTPS proxy/data-plane mediation tracking issue, including the section B options this brainstorm settles.
- [ADR-0019](docs/src/content/docs/adr/0019-v4-https-data-plane.md) — canonical decision journal for the twelve shipped slices.
- [Issue #747](https://github.com/ALRubinger/aileron/issues/747) — Milestone v4 umbrella; container-native enforcement as the architectural-disciplines floor.
- [Issue #898](https://github.com/ALRubinger/aileron/issues/898) — runtime audit schema cross-cut.
- `internal/launch/proxy_bootstrap.go` — current opt-in gate (`AILERON_SANDBOX_PROXY_BOOTSTRAP` env var, session CA generation, `HTTPS_PROXY` injection).
- `internal/app/handlers_sandbox_forward_proxy.go` — transparent CONNECT/TLS interception path; existing fail-closed sites.
- `internal/app/handlers_connector_operations.go` — daemon request boundary; spec resolution and credential binding.
- `images/sandbox-base/` — first-party image with the proxy contract scaffolding (`aileron-install-proxy-ca`, `aileron-run-with-proxy-ca`).
