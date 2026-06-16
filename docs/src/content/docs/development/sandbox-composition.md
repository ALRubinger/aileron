---
title: "Sandbox Composition"
description: "How to configure the agent container with devcontainer.json, Aileron's sandbox-base image, and the aileron sandbox CLI."
order: 6
---

Sandbox composition is the contract for deciding which container image an agent session runs in. It is defined by [ADR-0017](/adr/0017-sandbox-composition/) and implemented by the `aileron sandbox` CLI.

This page covers the user-facing workflow. Runtime launch support can scaffold, inspect, build, run the agent command in the prepared sandbox image, and register `aileron-mcp` as the in-container tool surface. Live discovery refresh is a follow-on runtime layer tracked in [#897](https://github.com/ALRubinger/aileron/issues/897).

## Choose a Composition Tier

| Tier | Use when | How to configure |
|---|---|---|
| Tier 0: base image | You want the minimal Aileron runtime substrate with no extra project tools. | Do not create `.devcontainer/devcontainer.json`. |
| Tier 1: devcontainer | You want to extend Aileron's base image with project tools. | Create `.devcontainer/devcontainer.json` and a Dockerfile. |
| Tier 2: BYO image | Your team already owns a compliant image. | Set `customizations.aileron.image` in `.devcontainer/devcontainer.json`. |

## Scaffold a Starter Devcontainer

From a project root:

```bash
aileron sandbox init
```

This creates:

```text
.devcontainer/
  devcontainer.json
  Dockerfile
```

The generated `devcontainer.json` is deliberately small:

```json
{
  "name": "Aileron sandbox",
  "build": {
    "dockerfile": "Dockerfile"
  },
  "customizations": {
    "aileron": {
      "mediation": "default",
      "approval_surface": "both"
    }
  }
}
```

The generated Dockerfile starts from `aileron/sandbox-base:<version>`, switches to `USER root` for installs, and switches back to `USER agent` before launch. The chosen agent's install recipe is pre-filled and ready to build; additional tool snippets (GitHub CLI, Node.js, Python, kubectl, Terraform) ship commented out for you to enable as needed.

By default `aileron sandbox init` scaffolds for Claude Code. Pass `--agent=<name>` to scaffold for a different agent — Aileron writes a ready-to-build recipe when one is documented, or a `TODO` install stub otherwise:

```bash
aileron sandbox init --agent=codex
```

Use `--force` only when you intentionally want to replace the existing scaffold:

```bash
aileron sandbox init --force
```

## Inspect the Plan

Use `sandbox plan` to see what Aileron currently infers from the project:

```bash
aileron sandbox plan
```

With no `.devcontainer/devcontainer.json`, the output is Tier 0:

```text
tier: base
image: aileron/sandbox-base:latest
```

With the starter scaffold, the output is Tier 1 and includes the Dockerfile:

```text
tier: devcontainer
image: aileron/sandbox-base:latest
devcontainer: .devcontainer/devcontainer.json
dockerfile: Dockerfile
```

## Build the Image

Use `sandbox build` to build the image selected by the plan:

```bash
aileron sandbox build
```

Aileron detects Docker from `PATH`. You can choose it explicitly:

```bash
aileron sandbox build --runtime=docker
aileron sandbox build --runtime=docker --tag=ghcr.io/acme/agent-dev:local
```

Docker is the only supported sandbox runtime in v4. Podman is planned but not yet supported ([ADR-0014](/adr/0014-spawn-sandbox-technology/)); passing `--runtime=podman` fails with `podman runtime is not supported yet (v4 is Docker-only); see ADR-0014`.

Build behavior by tier:

| Tier | Build behavior |
|---|---|
| Tier 0 | Builds the local `images/sandbox-base/Containerfile` as `aileron/sandbox-base:<version>`. |
| Tier 1 | Builds the devcontainer Dockerfile and tags it as a deterministic local `aileron/sandbox-project:<hash>` image unless `--tag` is supplied. |
| Tier 2 | Does not build. The BYO image is reported as-is; launch validates it before agent startup. |

When building the base image outside the source tree, set `AILERON_SANDBOX_BASE_CONTEXT` to the directory containing the sandbox-base `Containerfile`.

Release tags also build the sandbox-base image for `linux/amd64` and `linux/arm64` and publish it to GitHub Container Registry as `ghcr.io/alrubinger/aileron-sandbox-base:<version>`. Pull-request runs build both platforms without publishing, so image regressions are caught before release.

## Check Agent Support

Use `sandbox check` to validate that the selected image can run an agent command before starting a daemon-backed launch session:

```bash
aileron sandbox check --runtime=docker --agent=claude
aileron sandbox check --runtime=docker --build=never --agent=codex
```

`sandbox check` uses the same composition plan, build policy, and minimal image validation as sandbox launch. It reports the selected tier, runtime, image, command, and `support: ok` when the command is available. Agent-specific image recipes and support status live in the [sandbox agent image matrix](/development/sandbox-agent-images/).

## Run During Launch

Use `--sandbox` on `aileron launch` to have launch prepare the composition-selected image and start the agent inside it:

```bash
aileron launch --sandbox=auto claude
aileron launch --sandbox=docker codex
aileron launch --sandbox=docker goose
```

`auto` detects Docker from `PATH`. `docker` selects the runtime explicitly. The default is `--sandbox=off`, which preserves the current direct host launch path. Podman is planned but not yet supported ([ADR-0014](/adr/0014-spawn-sandbox-technology/)); passing `--sandbox=podman` fails with `podman runtime is not supported yet (v4 is Docker-only); see ADR-0014`.

Launch uses `--sandbox-build=auto` by default. Build policy options are:

| Policy | Behavior |
|---|---|
| `auto` | Use the selected image if it already exists locally; build Tier 0/Tier 1 images only when missing. |
| `always` | Rebuild Tier 0/Tier 1 images before validation and launch. |
| `never` | Do not build; fail with an actionable error if the selected image is missing locally. |

Examples:

```bash
aileron launch --sandbox=docker --sandbox-build=always claude
aileron launch --sandbox=docker --sandbox-build=never codex
```

`aileron sandbox build` remains the explicit manual build command and always invokes the selected runtime build for Tier 0/Tier 1.

The project directory is mounted at `/home/agent/workspace`, and the agent starts there. Launch passes session-scoped Aileron daemon env into the container, including `AILERON_URL`, `AILERON_API_URL`, `AILERON_COMMS_URL`, `AILERON_SESSION_ID`, `AILERON_APPROVAL_URL`, and the sandbox image metadata (`AILERON_SANDBOX_IMAGE`, `AILERON_SANDBOX_TIER`, `AILERON_SANDBOX_RUNTIME`). `AILERON_API_URL` points at the daemon's `/v1` API and is the stable endpoint for sandbox-side data-plane operations. For local daemon URLs, launch rewrites the container-facing host to `host.docker.internal` for Docker. Podman's `host.containers.internal` alias is the deferred re-add path, not yet supported.

The HTTPS proxy bootstrap is default-on for `--sandbox=docker`. Sandbox launch generates a session-local CA, mounts the public CA at `/etc/aileron/proxy/ca.pem`, and sets standard proxy env (`HTTPS_PROXY`, `HTTP_PROXY`, `NO_PROXY`) plus Aileron metadata (`AILERON_SANDBOX_PROXY_MODE`, `AILERON_SANDBOX_PROXY_URL`, `AILERON_SANDBOX_PROXY_CA_FILE`). The proxy URL uses standard proxy userinfo so clients can send `Proxy-Authorization`; it carries the launch session id and, when present, the local daemon token. Images used with this mode must provide `aileron-install-proxy-ca` and `aileron-run-with-proxy-ca` (see the [BYO image proxy contract](/development/sandbox-agent-images/#byo-image-proxy-contract)); the current sandbox-base image includes both and launch validation checks both before the agent starts. The container starts through `aileron-run-with-proxy-ca`, installs the mounted CA as root, then drops back to the `agent` user before executing the requested agent command.

Use `--sandbox-proxy=auto|on|off` (default `auto`) or `AILERON_SANDBOX_PROXY=auto|on|off` to control bootstrap. The flag wins over the env var; the env var wins over the default. `auto` resolves to `on` for `docker` and to `off` for every other `--sandbox` mode. `on` forces bootstrap; if the selected sandbox mode cannot support bootstrap (e.g. `--sandbox=off`), launch refuses with an actionable error before the container starts. `off` skips bootstrap for the session, and the daemon records a `sandbox.proxy.disabled` audit event with reason `user_opt_out`.

When bootstrap is requested but the selected image lacks the BYO contract helpers, launch fails preflight before the container starts, prints an actionable error citing the contract docs and the `--sandbox-proxy=off` opt-out, and records a `sandbox.proxy.disabled` audit event with reason `preflight_failed`. Non-container sandbox modes record reason `unsupported_sandbox_mode`. Tip: pre-existing pipelines that set `AILERON_SANDBOX_PROXY_BOOTSTRAP` should switch to `AILERON_SANDBOX_PROXY`; the former is no longer honored.

The daemon-side `/sandbox-proxy/requests` boundary can proxy recognized bodyless HTTPS requests with daemon-side credential injection, and `/connector-operations/run` can route eligible connector operations through that boundary with `GET`/`DELETE`/`HEAD` args encoded as query parameters and `POST`/`PATCH`/`PUT` args sent as JSON request bodies. The daemon also recognizes standard proxy-shaped requests, authenticates their `Proxy-Authorization`, completes authenticated `CONNECT host:443` TLS interception with the session CA, and routes decrypted requests through the same sandbox proxy boundary when they uniquely match an installed connector spec operation by method, host, and path. Smoke coverage confirms standard proxy URL userinfo can authenticate a normal HTTPS client through this transparent path. Missing or ambiguous transparent matches fail closed.

When installed action manifests or connector store metadata exist on the host, launch mounts them read-only under `/opt/aileron/manifests/actions` and `/opt/aileron/manifests/connectors`. The daemon loads installed `aileron.connector.v1.json` specs to validate connector operations on the HTTPS data plane (see [Sandbox Connector Specs](/development/sandbox-connector-specs/)).

`aileron-mcp` is the sole in-container tool surface. The launcher bind-mounts the host-built `aileron-mcp` at `/usr/local/bin/aileron-mcp` and registers it with the agent so MCP-capable agents see the same first-class tool catalog they do under host launch ([ADR-0024](/adr/0024-sandbox-mcp-parity/)). The static `tools.txt`/`/usr/local/bin` shim surface was retired in [#959](https://github.com/ALRubinger/aileron/issues/959); the two reasons it was once load-bearing, BYOCLI tool-catalog cost and shim-based credential mediation, are both gone. Live discovery refresh so a newly-installed action surfaces without an MCP restart is tracked in [#897](https://github.com/ALRubinger/aileron/issues/897).

Before running the agent, launch validates the selected image with the same env, mount, and workdir shape it will use for the agent. The image must:

- execute `/bin/sh` commands through the selected container runtime
- use `/home/agent/workspace` as the working directory
- allow a temporary file to be written in the mounted workspace
- resolve the agent command on `PATH`

The agent command must already exist in the selected image. For Tier 1, install the agent CLI in your devcontainer Dockerfile. Tier 2 uses the BYO image as supplied while Aileron's runtime injection remains limited to session env, manifest mounts, and the `aileron-mcp` tool surface. See the [sandbox agent image matrix](/development/sandbox-agent-images/) for the current support contract and recipes.

## Use a BYO Image

Set `customizations.aileron.image` when your team owns the complete image:

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

In BYO-image mode, launch uses the image as supplied and layers on Aileron's session env, manifest mounts, and the `aileron-mcp` tool surface. Images that participate in the v4 HTTPS proxy must include `aileron-install-proxy-ca` and `aileron-run-with-proxy-ca` helpers that meet the [BYO Image Proxy Contract](/development/sandbox-agent-images/#byo-image-proxy-contract). `aileron sandbox check --agent=...` validates both contracts for every Docker run.

## What Belongs in the Image

Put ordinary project tooling in the devcontainer: language runtimes, CLIs, package managers, private CA bundles, and internal helper tools.

Do not put Aileron credentials or user secrets in the image. The agent reaches Aileron's tools through `aileron-mcp`, which calls the daemon API with the launch token and session context. Credentialed network flows route through the Aileron HTTPS proxy/data plane.

## What This Does Not Do Yet

This runtime path does not add live discovery refresh or polished arbitrary-client proxy support. The launcher wires `aileron-mcp` as an in-container stdio subprocess so MCP-capable agents see the same first-class tool catalog they do under host launch ([ADR-0024](/adr/0024-sandbox-mcp-parity/) and the [manual walkthrough](/development/sandbox-mcp-walkthrough/)). Eligible connector operations flow through the daemon HTTPS proxy boundary with credential injection and `connector.proxy.proxied` audit records, including JSON request bodies for `POST`, `PATCH`, and `PUT`. Proxy-bootstrap launches can install the session CA in the container trust store before the agent starts, authenticate standard proxy-shaped requests back to the daemon, and route uniquely matched decrypted `CONNECT` requests through the daemon credential boundary; standard proxy URL userinfo has smoke coverage for this path. Follow-on work adds broader proxy/client integration and polish for arbitrary HTTPS clients ([#896](https://github.com/ALRubinger/aileron/issues/896)), live discovery refresh only if dynamic in-session connector changes require it ([#897](https://github.com/ALRubinger/aileron/issues/897)), and final credentialed HTTPS audit semantics ([ADR-0019](/adr/0019-v4-https-data-plane/)). Container-only shell-layer interception was prototyped under [#801](https://github.com/ALRubinger/aileron/issues/801) and withdrawn in [#952](https://github.com/ALRubinger/aileron/issues/952); container isolation, the HTTPS proxy, and tool-level HITL cover the named risks (see [ADR-0021](/adr/0021-v4-shell-layer-mediation/), Withdrawn).
