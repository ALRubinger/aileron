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

> **Revision note, 2026-06-16:** Umbrella [#1080](https://github.com/ALRubinger/aileron/issues/1080) moves each agent install from a per-agent baked Dockerfile layer to a devcontainer Feature. The base image stays harness-free and gains no agent CLIs. Aileron CI bakes prebuilt per-agent images from the same Feature, and those images become the Tier 0 default (their publishing and launch-time auto-resolution are owned by [#965](https://github.com/ALRubinger/aileron/issues/965), forward-referenced here). Customers compose the same agent Feature alongside their own tooling Feature for the customization tier. The build engine, the diamond-inheritance resolution, and the CLI surface prose below are updated to reflect the Feature model. The CLI changes that this model implies land in sibling issues ([#1082](https://github.com/ALRubinger/aileron/issues/1082), [#1083](https://github.com/ALRubinger/aileron/issues/1083), [#1084](https://github.com/ALRubinger/aileron/issues/1084)); this ADR records the model and the build-engine decision.

## Context

Aileron is moving from a host-launched MCP-first runtime toward the Aileron Way: the agent runs inside a container Aileron defines, with credentialed HTTPS traffic flowing through the Aileron data plane and shell/runtime boundaries mediated inside the container.

That shift creates an image-composition question: who decides what is in the agent container? Aileron needs to own the security substrate, but users still need ordinary development tools such as `gh`, `kubectl`, language runtimes, private CLIs, and internal certificates.

An Aileron-specific tool resolver was considered and rejected. A schema like `aileron.yaml` with `tools: [gh, kubectl, node@20]` would make Aileron responsible for package resolution, install recipes, version drift, and ecosystem-specific failure modes. That is not Aileron's lane.

## Decision

Use `.devcontainer/devcontainer.json` as the canonical project-local sandbox composition substrate. Aileron reads standard devcontainer build/image fields and stores Aileron-specific settings under `customizations.aileron`.

Aileron supports three tiers:

| Tier | Contract |
|---|---|
| Tier 0: prebuilt per-agent image | No `.devcontainer/devcontainer.json`; launch auto-resolves the prebuilt per-agent image `ghcr.io/alrubinger/aileron-sandbox-<agent>` baked from the agent Feature. Publishing and launch-time auto-resolution are owned by [#965](https://github.com/ALRubinger/aileron/issues/965). The bare `aileron/sandbox-base:<version>` stays the harness-free root that each agent Feature layers onto. |
| Tier 1: devcontainer | `.devcontainer/devcontainer.json` exists; Aileron composes the sandbox from the Features it lists. The starter path lists `aileron/sandbox-base:<version>` as the base plus an agent Feature plus an optional customer-tooling Feature under `features`. |
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

The Aileron-owned base image contains only the runtime substrate: shell utilities and the proxy/session bootstrap helpers. CA installation hooks layer onto this base as those features land. The base image stays harness-free and does not carry language runtimes, third-party development tools, or agent CLIs.

## Composition model: Features

Each agent install is authored once as a devcontainer Feature. A Feature is a self-contained install unit that runs on top of any base, so the agent CLI is no longer a baked layer inside a per-agent image. A customer's own tooling is its own Feature and stays agent-agnostic. A Tier 1 sandbox is composed by listing the base image and these Features in `devcontainer.json`:

```jsonc
{
  "image": "aileron/sandbox-base:<version>",
  "features": {
    "ghcr.io/alrubinger/aileron-features/<agent>:0": {},
    "ghcr.io/acme/internal-tools:1": {}
  }
}
```

The agent Feature is the single source of truth. Aileron CI bakes the prebuilt per-agent images ([#965](https://github.com/ALRubinger/aileron/issues/965)) from the same Feature that customers compose directly. There is one install recipe per agent, consumed two ways.

### Diamond-inheritance resolution

Docker `FROM` is single-parent. A per-agent baked layer cannot also carry arbitrary customer tooling unless the customer rebuilds the whole agent layer on their own base, because "base + customer tools" and "base + agent" are two lineages that never merge into "base + customer tools + agent". Features compose independently onto one base, so "base + customer tools + agent" is expressible without merging two image lineages. This is the diamond-inheritance resolution that authoring the agent as a Feature provides. The spawn-sandbox technology decision in [ADR-0014](/adr/0014-spawn-sandbox-technology) and the harness-free base in [ADR-0024](/adr/0024-sandbox-mcp-parity) are the substrate the Feature model builds on.

### Build engine

Composing standard `features` requires either the official `@devcontainers/cli` or a reimplementation of Feature resolution and ordering. The decision is to drive sandbox builds through `@devcontainers/cli` so standard `features` compose without Aileron reimplementing the resolver. A raw `docker build` that runs each Feature's install script in order is retained only as a contingency fallback should the CLI dependency prove unworkable. It is not a co-equal option, and it is also the existing path for a Tier 1 plan that declares no `features`.

As of [#1083](https://github.com/ALRubinger/aileron/issues/1083), `composition.Discover` parses the `features` block into the plan (raw option payloads carried verbatim, never interpreted by Aileron), and the Tier 1 build path branches on it. When a Tier 1 plan carries `features`, the build routes through `@devcontainers/cli` (resolved as a pinned `npx --yes @devcontainers/cli@<version>`), which reads `features` from `devcontainer.json` and shells out to Docker under the hood. The `@devcontainers/cli` invocation flows through the same `container.Runner` seam as `docker build`, so no second runtime is introduced and the Docker-only guard ([ADR-0014](/adr/0014-spawn-sandbox-technology)) still applies. A Tier 1 plan **without** `features` keeps the raw `docker build` path unchanged (the recorded fallback). A Tier 2 BYO image ([ADR-0024](/adr/0024-sandbox-mcp-parity)) still short-circuits to the as-is image; any `features` it lists are parsed for inspection but inert. Local Feature composition is complete; publishing and launch-time auto-resolution of Feature/agent refs remain owned by [#965](https://github.com/ALRubinger/aileron/issues/965) under umbrella [#1080](https://github.com/ALRubinger/aileron/issues/1080).

## Single-binary alignment

This ADR follows the updated sandbox runtime direction:

- Aileron uses one `aileron` binary with multiple modes.
- This composition contract does not introduce an `aileron-mcp` image or launch path.
- The canonical credentialed-action path is HTTPS through the Aileron proxy/data plane.
- Runtime bootstrap supplies `AILERON_API_URL`, `AILERON_TOKEN`, and launch session metadata for the in-container `aileron-mcp` tool surface and the data plane. Later proxy work can add `HTTPS_PROXY` and session CA configuration without changing the composition tiers; see [ADR-0019](/adr/0019-v4-https-data-plane).

## CLI surface

`aileron sandbox init` scaffolds a Feature-composing `.devcontainer/devcontainer.json` for the customization tier. The scaffold lists `aileron/sandbox-base:<version>` as the base image and an agent Feature plus an optional customer-tooling Feature under `features`, so users compose their own tooling alongside the agent through standard devcontainer workflows. Under the Feature model `init` no longer scaffolds a per-agent `.devcontainer/Dockerfile`, and the `--agent` flag is removed because the agent is selected by the listed Feature rather than a baked recipe.

`aileron sandbox plan` is an inspection helper that reports the normalized tier/image/dockerfile plan, including a `features:` summary line listing the parsed Feature references when the devcontainer declares any.

`aileron sandbox build` is the first user-facing build consumer of that plan. It resolves the Tier 0 prebuilt per-agent image and composes the Tier 1 devcontainer Features through the build engine recorded above. Tier 2 BYO images are selected as-is; launch validates the minimal runtime contract before running the agent. The `features`-composing build path lands in [#1083](https://github.com/ALRubinger/aileron/issues/1083).

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
- [Issue #1080](https://github.com/ALRubinger/aileron/issues/1080) — agent-as-Feature sandbox model umbrella
- [Issue #965](https://github.com/ALRubinger/aileron/issues/965) — prebuilt per-agent image publishing and auto-resolution
- [ADR-0014](/adr/0014-spawn-sandbox-technology) — spawn sandbox technology
- [ADR-0024](/adr/0024-sandbox-mcp-parity) — harness-free base and in-container MCP parity
- [ADR-0015](/adr/0015-launch-audit-scope) — old host launch audit boundary
- [ADR-0018](/adr/0018-v4-single-binary-runtime) — single-binary runtime model
- [ADR-0019](/adr/0019-v4-https-data-plane) — HTTPS data-plane mediation
- [ADR-0020](/adr/0020-v4-connector-specs-and-shims) — connector specs and shims
- [ADR-0021](/adr/0021-v4-shell-layer-mediation) — shell-layer mediation (Withdrawn)
