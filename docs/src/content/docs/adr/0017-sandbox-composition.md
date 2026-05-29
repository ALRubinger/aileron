---
title: "ADR-0017: Sandbox Composition"
description: "v4 sandbox images are composed through devcontainer.json with Aileron-specific extensions under customizations.aileron. Aileron owns a minimal sandbox-base image and users extend or replace it using standard container workflows."
order: 17
---

<div class="meta">
<table>
  <tr><th>Status</th><td>Proposed</td></tr>
  <tr><th>Date</th><td>2026-05-29</td></tr>
  <tr><th>Tracking</th><td><a href="https://github.com/ALRubinger/aileron/issues/796">#796</a>, <a href="https://github.com/ALRubinger/aileron/issues/747">#747</a></td></tr>
</table>
</div>

## Context

v4 moves Aileron from a host-launched MCP-first runtime toward the Aileron Way: the agent runs inside a container Aileron defines, with credentialed HTTPS traffic flowing through the Aileron data plane and shell/runtime boundaries mediated inside the container.

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

`image` selects the BYO-image tier. `mediation` and `approval_surface` are declared here so the config surface exists before #801 and #802 add the runtime behavior.

The Aileron-owned base image contains only the runtime substrate: the `aileron` binary/shim entrypoints, discovery files, proxy/session bootstrap, CA installation hooks, and shell mediation files as those features land. It does not carry language runtimes or third-party development tools.

## Single-binary alignment

This ADR follows the updated #747 v4 direction:

- v4 uses one `aileron` binary with multiple modes.
- This composition contract does not introduce an `aileron-mcp` image or launch path.
- The canonical credentialed-action path is HTTPS through the Aileron proxy/data plane.
- Runtime bootstrap supplies `HTTPS_PROXY` and `AILERON_TOKEN` when container/non-loopback daemon access is enabled.

## CLI surface

`aileron sandbox init` scaffolds:

- `.devcontainer/devcontainer.json`
- `.devcontainer/Dockerfile`

The Dockerfile extends `aileron/sandbox-base:<version>` and includes commented snippets for common tools. The snippets are guidance, not a runtime resolver. Users own their container contents using normal Docker/devcontainer workflows.

`aileron sandbox plan` is an inspection helper that reports the normalized tier/image/dockerfile plan. Later launch work consumes the same composition contract.

## Consequences

Users with existing devcontainers get an upgrade path rather than a parallel Aileron-only config file.

Aileron keeps a clear boundary: it owns mediation, credentials, approvals, audit, and runtime bootstrap; users own development tooling in the image.

The first implementation can establish the contract without also implementing runtime orchestration, watcher processes, shell interception, or proxy bootstrap. Those build on this substrate in later #796/#801 slices.

## Alternatives Considered

**Aileron-specific YAML resolver.** Rejected. It would require Aileron to maintain install recipes and version semantics for every common development tool.

**Dockerfile only.** Rejected as the top-level contract because devcontainer.json already standardizes Dockerfiles, images, features, mounts, and editor/tooling interop.

**Bake common CLIs into sandbox-base.** Rejected. It bloats the trusted base image and makes Aileron responsible for unrelated tool maintenance.

## References

- [Issue #796](https://github.com/ALRubinger/aileron/issues/796) — v4 sandbox composition
- [Issue #747](https://github.com/ALRubinger/aileron/issues/747) — v4 runtime-first milestone
- [ADR-0015](/adr/0015-launch-audit-scope) — old host launch audit boundary
