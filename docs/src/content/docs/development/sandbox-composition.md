---
title: "Sandbox Composition"
description: "How to configure the agent container with devcontainer.json, Aileron's sandbox-base image, and the aileron sandbox CLI."
order: 6
---

Sandbox composition is the contract for deciding which container image an agent session runs in. It is defined by [ADR-0017](/adr/0017-sandbox-composition/) and implemented by the `aileron sandbox` CLI.

This page covers the user-facing workflow. Runtime launch support can scaffold, inspect, build, run the agent command in the prepared sandbox image, and inject static discovery/action shims. Live discovery refresh, proxy/session CA bootstrap, and shell mediation are follow-on runtime layers tracked in [#897](https://github.com/ALRubinger/aileron/issues/897), [#896](https://github.com/ALRubinger/aileron/issues/896), and [#801](https://github.com/ALRubinger/aileron/issues/801).

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

The generated Dockerfile starts from `aileron/sandbox-base:<version>` and includes commented recipes for common tools such as GitHub CLI, Node.js, Python, kubectl, and Terraform. Uncomment and edit the snippets your project needs.

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

Aileron detects Docker or Podman from `PATH`. You can choose explicitly:

```bash
aileron sandbox build --runtime=podman
aileron sandbox build --runtime=docker --tag=ghcr.io/acme/agent-dev:local
```

Build behavior by tier:

| Tier | Build behavior |
|---|---|
| Tier 0 | Builds the local `images/sandbox-base/Containerfile` as `aileron/sandbox-base:<version>`. |
| Tier 1 | Builds the devcontainer Dockerfile and tags it as a deterministic local `aileron/sandbox-project:<hash>` image unless `--tag` is supplied. |
| Tier 2 | Does not build. The BYO image is reported as-is; launch validates it before agent startup. |

When building the base image outside the source tree, set `AILERON_SANDBOX_BASE_CONTEXT` to the directory containing the sandbox-base `Containerfile`.

Release tags also build the sandbox-base image for `linux/amd64` and `linux/arm64` and publish it to GitHub Container Registry as `ghcr.io/alrubinger/aileron-sandbox-base:<version>`. Pull-request runs build both platforms without publishing, so image regressions are caught before release.

## Run During Launch

Use `--sandbox` on `aileron launch` to have launch prepare the composition-selected image and start the agent inside it:

```bash
aileron launch --sandbox=auto claude
aileron launch --sandbox=docker codex
aileron launch --sandbox=podman goose
```

`auto` detects Docker or Podman from `PATH`. `docker` and `podman` select a runtime explicitly. The default is `--sandbox=off`, which preserves the current direct host launch path.

Launch uses `--sandbox-build=auto` by default. Build policy options are:

| Policy | Behavior |
|---|---|
| `auto` | Use the selected image if it already exists locally; build Tier 0/Tier 1 images only when missing. |
| `always` | Rebuild Tier 0/Tier 1 images before validation and launch. |
| `never` | Do not build; fail with an actionable error if the selected image is missing locally. |

Examples:

```bash
aileron launch --sandbox=docker --sandbox-build=always claude
aileron launch --sandbox=podman --sandbox-build=never codex
```

`aileron sandbox build` remains the explicit manual build command and always invokes the selected runtime build for Tier 0/Tier 1.

The project directory is mounted at `/home/agent/workspace`, and the agent starts there. Launch passes session-scoped Aileron daemon env into the container, including `AILERON_URL`, `AILERON_API_URL`, `AILERON_COMMS_URL`, `AILERON_SESSION_ID`, `AILERON_APPROVAL_URL`, discovery hints (`AILERON_TOOLS_FILE`, `AILERON_SHIMS_DIR`), and the sandbox image metadata (`AILERON_SANDBOX_IMAGE`, `AILERON_SANDBOX_TIER`, `AILERON_SANDBOX_RUNTIME`). `AILERON_API_URL` points at the daemon's `/v1` API and is the stable endpoint for sandbox-side execution shims. For local daemon URLs, launch rewrites the container-facing host to `host.docker.internal` for Docker and `host.containers.internal` for Podman.

When installed action manifests or connector store metadata exist on the host, launch mounts them read-only under `/opt/aileron/manifests/actions` and `/opt/aileron/manifests/connectors`. When installed actions declare connector dependencies, launch also generates a session-scoped static `/etc/aileron/tools.txt` manifest and read-only connector shim scripts under `/usr/local/bin`. These shims support `--help` for discovery and can execute an explicit installed action name through `AILERON_API_URL` with optional raw JSON args. Shim calls include the launch session id when `AILERON_SESSION_ID` is set, so daemon-side approval context stays tied to the sandbox session. Generated connector shims require `wget`; Aileron's sandbox-base image includes it, and BYO/devcontainer images that receive shims are validated for it before agent startup. This static launch-scoped discovery/action surface is the current sandbox runtime contract. Live `tools.txt` refresh and watcher processes can layer on later when in-session connector changes need them.

Before registering the session, launch validates the selected image with the same mount/workdir shape it will use for the agent. The image must:

- execute `/bin/sh` commands through the selected container runtime
- use `/home/agent/workspace` as the working directory
- allow a temporary file to be written in the mounted workspace
- resolve the agent command on `PATH`
- provide `wget` when generated connector shims are mounted

The agent command must already exist in the selected image. For Tier 1, install the agent CLI in your devcontainer Dockerfile. Tier 2 uses the BYO image as supplied while Aileron's runtime injection remains limited to session env, manifest mounts, `tools.txt`, and connector shims. The supported-agent image matrix and first-class recipes are tracked in [#894](https://github.com/ALRubinger/aileron/issues/894).

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

In BYO-image mode, launch uses the image as supplied and layers on Aileron's session env, manifest mounts, generated discovery files, and connector shims. Later runtime launch work can extend that contract with proxy bootstrap, session CA, and shell mediation files.

## What Belongs in the Image

Put ordinary project tooling in the devcontainer: language runtimes, CLIs, package managers, private CA bundles, and internal helper tools.

Do not put Aileron credentials or user secrets in the image. Current generated action shims call the Aileron daemon API with the launch token and session context. Later credentialed network flows are designed to use the Aileron HTTPS proxy/data plane when that layer lands.

## What This Does Not Do Yet

This runtime path does not add live discovery refresh, proxy bootstrap, or shell command mediation. Generated session-scoped `/etc/aileron/tools.txt` and read-only connector shims support `--help` discovery, and the shims can execute installed actions via `AILERON_API_URL` with optional raw JSON args. Follow-on work adds live discovery refresh only if dynamic in-session connector changes require it ([#897](https://github.com/ALRubinger/aileron/issues/897)); proxy/session CA bootstrap and credentialed HTTPS data-plane mediation are tracked in [#896](https://github.com/ALRubinger/aileron/issues/896) and [ADR-0019](/adr/0019-v4-https-data-plane/); shell-layer interception is tracked in [#801](https://github.com/ALRubinger/aileron/issues/801) and [ADR-0021](/adr/0021-v4-shell-layer-mediation/).
