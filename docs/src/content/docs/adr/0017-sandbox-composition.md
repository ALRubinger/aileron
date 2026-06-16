---
title: "ADR-0017: Sandbox Composition"
description: "Sandbox images are composed through devcontainer.json with Aileron-specific extensions under customizations.aileron. Aileron owns a minimal sandbox-base image and users extend or replace it using standard container workflows."
order: 17
---

<div class="meta">
<table>
  <tr><th>Status</th><td>Proposed</td></tr>
  <tr><th>Date</th><td>2026-05-29</td></tr>
  <tr><th>Tracking</th><td><a href="https://github.com/ALRubinger/aileron/issues/796">#796</a>, <a href="https://github.com/ALRubinger/aileron/issues/747">#747</a></td></tr>
</table>
</div>

> **Revision note, 2026-06-15:** This ADR originally described a launch-scoped static `tools.txt` manifest and `/usr/local/bin` connector shims as the in-container tool surface. [#959](https://github.com/ALRubinger/aileron/issues/959) retired that surface; `aileron-mcp` ([ADR-0024](/adr/0024-sandbox-mcp-parity)) is now the sole in-container tool surface. The composition tiers and the read-only manifest mounts are unchanged. The base image keeps `wget` for general tooling, but it is no longer a launch-validated shim requirement. Passages below have been updated to reflect the retired surface.

## Context

Aileron is moving from a host-launched MCP-first runtime toward the Aileron Way: the agent runs inside a container Aileron defines, with credentialed HTTPS traffic flowing through the Aileron data plane and shell/runtime boundaries mediated inside the container.

That shift creates an image-composition question: who decides what is in the agent container? Aileron needs to own the security substrate, but users still need ordinary development tools such as `gh`, `kubectl`, language runtimes, private CLIs, and internal certificates.

An Aileron-specific tool resolver was considered and rejected. A schema like `aileron.yaml` with `tools: [gh, kubectl, node@20]` would make Aileron responsible for package resolution, install recipes, version drift, and ecosystem-specific failure modes. That is not Aileron's lane.

## Decision

Use `.devcontainer/devcontainer.json` as the canonical project-local sandbox composition substrate. Aileron reads standard devcontainer build/image fields and stores Aileron-specific settings under `customizations.aileron`.

Aileron supports three tiers:

| Tier | Contract |
|---|---|
| Tier 0: base image | No `.devcontainer/devcontainer.json`; Aileron uses `aileron/sandbox-base:<version>` directly. |
| Tier 1: devcontainer | `.devcontainer/devcontainer.json` exists; Aileron composes the sandbox using its build/image settings. The starter path is `.devcontainer/Dockerfile` extending `aileron/sandbox-base:<version>`. |
| Tier 2: BYO image | `customizations.aileron.image` names a fully custom image. Aileron uses it as supplied and injects the runtime contract at launch. |

The Aileron extension block starts narrow:

```json
{
  "customizations": {
    "aileron": {
      "image": "ghcr.io/acme/agent:2026-05-29",
      "mediation": "default",
      "approval_surface": "both"
    }
  }
}
```

`image` selects the BYO-image tier. `mediation` and `approval_surface` are declared here so the config surface exists before the approval UI adds the runtime behavior. (The `mediation` slot was originally also intended to anchor container-only shell mediation; that surface was prototyped under [#801](https://github.com/ALRubinger/aileron/issues/801) and withdrawn in [#952](https://github.com/ALRubinger/aileron/issues/952). See [ADR-0021](/adr/0021-v4-shell-layer-mediation) (Withdrawn).)

The Aileron-owned base image contains only the runtime substrate: shell utilities and the proxy/session bootstrap helpers. CA installation hooks layer onto this base as those features land. The base image does not carry language runtimes or third-party development tools.

## Single-binary alignment

This ADR follows the updated sandbox runtime direction:

- Aileron uses one `aileron` binary with multiple modes.
- This composition contract does not introduce an `aileron-mcp` image or launch path.
- The canonical credentialed-action path is HTTPS through the Aileron proxy/data plane.
- Runtime bootstrap supplies `AILERON_API_URL`, `AILERON_TOKEN`, and launch session metadata for the in-container `aileron-mcp` tool surface and the data plane. Later proxy work can add `HTTPS_PROXY` and session CA configuration without changing the composition tiers; see [ADR-0019](/adr/0019-v4-https-data-plane).

## CLI surface

`aileron sandbox init` scaffolds:

- `.devcontainer/devcontainer.json`
- `.devcontainer/Dockerfile`

The Dockerfile extends `aileron/sandbox-base:<version>`, pre-fills the install recipe for the agent named in `--agent` (default `claude`), and ships additional tool snippets (GitHub CLI, Node.js, Python, kubectl, Terraform) commented out for users to enable as needed. The snippets are guidance, not a runtime resolver. Users own their container contents using normal Docker/devcontainer workflows.

`aileron sandbox plan` is an inspection helper that reports the normalized tier/image/dockerfile plan.

`aileron sandbox build` is the first user-facing build consumer of that plan. It builds Tier 0 from Aileron's local sandbox-base image definition and Tier 1 from the devcontainer Dockerfile through Docker. Tier 2 BYO images are selected as-is; launch validates the minimal runtime contract before running the agent.

`aileron sandbox check --agent=<command>` prepares the selected image with the same build policy as launch and validates that the image can run the requested agent command before starting a daemon-backed session. This is the user-facing preflight for the sandbox agent image support matrix tracked in [#894](https://github.com/ALRubinger/aileron/issues/894).

`aileron launch --sandbox=off|auto|docker` consumes the same build path to prepare the selected image, validates that it can execute `/bin/sh`, use a writable `/home/agent/workspace` mount, and resolve the agent command on `PATH`, then runs the agent command inside a one-shot Docker container. The project is mounted at `/home/agent/workspace` and used as the container working directory. Launch passes the session-scoped Aileron daemon env into the container, including `AILERON_API_URL` for the daemon `/v1` API that the in-container `aileron-mcp` and the data plane use. Local daemon URLs are rewritten to the Docker host alias `host.docker.internal`. Podman's `host.containers.internal` alias is the deferred re-add path for when Podman returns.

v4 is Docker-only. Podman is deferred to a later track, not rejected. An explicit `aileron launch --sandbox=podman` is rejected at validation time with `unsupported sandbox runtime "podman" (want off, auto, or docker)`. The runtime abstraction seam is preserved, so re-adding Podman is re-enabling it in `resolveRuntime` and the support matrix. See [ADR-0014](/adr/0014-spawn-sandbox-technology) and umbrella issue [#1050](https://github.com/ALRubinger/aileron/issues/1050) for the descope rationale.

Launch build behavior is controlled by `--sandbox-build=auto|always|never`. `auto` is the default and builds Tier 0/Tier 1 images only when the selected local image is missing. `always` forces a rebuild. `never` fails if the selected image is not already present. The explicit `aileron sandbox build` command keeps its manual-build behavior.

Sandbox launch also bind-mounts Aileron's installed action manifests and connector store metadata read-only under `/opt/aileron/manifests/actions` and `/opt/aileron/manifests/connectors` when the corresponding host directories exist. The daemon loads installed connector specs from that metadata to validate connector operations on the HTTPS data plane. The agent reaches Aileron's tools through `aileron-mcp` ([ADR-0024](/adr/0024-sandbox-mcp-parity)), which sets the launch session id from `AILERON_SESSION_ID` so daemon-side approvals retain session context. The static `tools.txt`/`/usr/local/bin` shim surface that #796 first shipped was retired in [#959](https://github.com/ALRubinger/aileron/issues/959). Live discovery refresh so a newly-installed action surfaces without an MCP restart is tracked in [#897](https://github.com/ALRubinger/aileron/issues/897).

The sandbox-base image has a dedicated CI/publish workflow. Pull requests build the image for `linux/amd64` and `linux/arm64` without publishing. Release tags publish the same multi-arch image to GitHub Container Registry as `ghcr.io/alrubinger/aileron-sandbox-base:<version>`.

## Consequences

Users with existing devcontainers get an upgrade path rather than a parallel Aileron-only config file.

Aileron keeps a clear boundary: it owns mediation, credentials, approvals, audit, and runtime bootstrap; users own development tooling in the image.

The first implementations establish the contract, image-build substrate, container execution path, validation, and launch-scoped discovery/action substrate. That is the #796 cut line: live discovery refresh ([#897](https://github.com/ALRubinger/aileron/issues/897)), proxy/session-CA bootstrap ([#896](https://github.com/ALRubinger/aileron/issues/896)), and agent image recipes ([#894](https://github.com/ALRubinger/aileron/issues/894)) build on this substrate only when later runtime layers need them. Shell interception ([ADR-0021](/adr/0021-v4-shell-layer-mediation), Withdrawn) was an earlier candidate and is no longer planned.

## Alternatives Considered

**Aileron-specific YAML resolver.** Rejected. It would require Aileron to maintain install recipes and version semantics for every common development tool.

**Dockerfile only.** Rejected as the top-level contract because devcontainer.json already standardizes Dockerfiles, images, features, mounts, and editor/tooling interop.

**Bake common CLIs into sandbox-base.** Rejected. It bloats the trusted base image and makes Aileron responsible for unrelated tool maintenance.

## References

- [Issue #796](https://github.com/ALRubinger/aileron/issues/796) — sandbox composition
- [Issue #747](https://github.com/ALRubinger/aileron/issues/747) — runtime-first milestone
- [Issue #894](https://github.com/ALRubinger/aileron/issues/894) — sandbox agent image support matrix
- [Issue #895](https://github.com/ALRubinger/aileron/issues/895) — connector specs and generated HTTPS shims
- [Issue #896](https://github.com/ALRubinger/aileron/issues/896) — HTTPS proxy/data-plane mediation
- [Issue #897](https://github.com/ALRubinger/aileron/issues/897) — dynamic discovery refresh
- [ADR-0015](/adr/0015-launch-audit-scope) — old host launch audit boundary
- [ADR-0018](/adr/0018-v4-single-binary-runtime) — single-binary runtime model
- [ADR-0019](/adr/0019-v4-https-data-plane) — HTTPS data-plane mediation
- [ADR-0020](/adr/0020-v4-connector-specs-and-shims) — connector specs and shims
- [ADR-0021](/adr/0021-v4-shell-layer-mediation) — shell-layer mediation (Withdrawn)
