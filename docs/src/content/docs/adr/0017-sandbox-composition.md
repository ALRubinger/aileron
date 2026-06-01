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

`image` selects the BYO-image tier. `mediation` and `approval_surface` are declared here so the config surface exists before shell mediation and the approval UI add the runtime behavior.

The Aileron-owned base image contains only the runtime substrate: the `aileron` binary/shim entrypoints, discovery files, the minimal HTTP client used by generated connector shims, proxy/session bootstrap, CA installation hooks, and shell mediation files as those features land. It does not carry language runtimes or third-party development tools.

## Single-binary alignment

This ADR follows the updated sandbox runtime direction:

- Aileron uses one `aileron` binary with multiple modes.
- This composition contract does not introduce an `aileron-mcp` image or launch path.
- The canonical credentialed-action path is HTTPS through the Aileron proxy/data plane.
- Runtime bootstrap supplies `HTTPS_PROXY` and `AILERON_TOKEN` when container/non-loopback daemon access is enabled.

## CLI surface

`aileron sandbox init` scaffolds:

- `.devcontainer/devcontainer.json`
- `.devcontainer/Dockerfile`

The Dockerfile extends `aileron/sandbox-base:<version>` and includes commented snippets for common tools. The snippets are guidance, not a runtime resolver. Users own their container contents using normal Docker/devcontainer workflows.

`aileron sandbox plan` is an inspection helper that reports the normalized tier/image/dockerfile plan.

`aileron sandbox build` is the first user-facing build consumer of that plan. It builds Tier 0 from Aileron's local sandbox-base image definition and Tier 1 from the devcontainer Dockerfile through Docker or Podman. Tier 2 BYO images are selected as-is; launch validates the minimal runtime contract before running the agent. Later launch work extends the initial runtime-injection substrate.

`aileron launch --sandbox=auto|docker|podman` consumes the same build path to prepare the selected image, validates that it can execute `/bin/sh`, use a writable `/home/agent/workspace` mount, and resolve the agent command on `PATH`, then runs the agent command inside a one-shot Docker/Podman container. When generated connector shims are mounted, launch also validates that `wget` is available because those shims use it to reach `AILERON_API_URL`. The project is mounted at `/home/agent/workspace` and used as the container working directory. Launch passes the session-scoped Aileron daemon env into the container, including `AILERON_API_URL` for the daemon `/v1` API used by sandbox-side execution shims, plus discovery hints `AILERON_TOOLS_FILE=/etc/aileron/tools.txt` and `AILERON_SHIMS_DIR=/usr/local/bin`. Local daemon URLs are rewritten to the runtime host alias (`host.docker.internal` for Docker, `host.containers.internal` for Podman).

Launch build behavior is controlled by `--sandbox-build=auto|always|never`. `auto` is the default and builds Tier 0/Tier 1 images only when the selected local image is missing. `always` forces a rebuild. `never` fails if the selected image is not already present. The explicit `aileron sandbox build` command keeps its manual-build behavior.

Sandbox launch also bind-mounts Aileron's installed action manifests and connector store metadata read-only under `/opt/aileron/manifests/actions` and `/opt/aileron/manifests/connectors` when the corresponding host directories exist. When installed action manifests declare connector dependencies, launch renders a session-scoped static `tools.txt` manifest and bind-mounts it read-only at `/etc/aileron/tools.txt`; it also renders read-only connector shim scripts under `/usr/local/bin` for `--help` discovery and explicit installed-action execution through `AILERON_API_URL`. This is the first runtime-injection substrate for discovery and action dispatch; live `tools.txt` refresh and watcher processes remain follow-on work.

The sandbox-base image has a dedicated CI/publish workflow. Pull requests build the image for `linux/amd64` and `linux/arm64` without publishing. Release tags publish the same multi-arch image to GitHub Container Registry as `ghcr.io/alrubinger/aileron-sandbox-base:<version>`.

## Consequences

Users with existing devcontainers get an upgrade path rather than a parallel Aileron-only config file.

Aileron keeps a clear boundary: it owns mediation, credentials, approvals, audit, and runtime bootstrap; users own development tooling in the image.

The first implementations establish the contract, image-build substrate, minimal container execution path, and initial runtime-injection substrate. Live discovery refresh, watcher processes, shell interception, and proxy bootstrap build on this substrate in later sandbox and shell-mediation work.

## Alternatives Considered

**Aileron-specific YAML resolver.** Rejected. It would require Aileron to maintain install recipes and version semantics for every common development tool.

**Dockerfile only.** Rejected as the top-level contract because devcontainer.json already standardizes Dockerfiles, images, features, mounts, and editor/tooling interop.

**Bake common CLIs into sandbox-base.** Rejected. It bloats the trusted base image and makes Aileron responsible for unrelated tool maintenance.

## References

- [Issue #796](https://github.com/ALRubinger/aileron/issues/796) — sandbox composition
- [Issue #747](https://github.com/ALRubinger/aileron/issues/747) — runtime-first milestone
- [ADR-0015](/adr/0015-launch-audit-scope) — old host launch audit boundary
