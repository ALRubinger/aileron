---
title: "Sandbox Composition"
description: "How to configure the agent container with devcontainer.json, Aileron's sandbox-base image, and the aileron sandbox CLI."
order: 6
---

Sandbox composition is the contract for deciding which container image an agent session runs in. It is defined by [ADR-0017](/adr/0017-sandbox-composition/) and implemented by the `aileron sandbox` CLI.

This page covers the user-facing workflow. Runtime launch support is intentionally still staged: the current implementation can scaffold, inspect, build, and run the agent command in the prepared sandbox image, while later work adds runtime injection, discovery refresh, proxy bootstrap, and shell mediation.

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

## Run During Launch

Use `--sandbox` on `aileron launch` to have launch prepare the composition-selected image and start the agent inside it:

```bash
aileron launch --sandbox=auto claude
aileron launch --sandbox=docker codex
aileron launch --sandbox=podman goose
```

`auto` detects Docker or Podman from `PATH`. `docker` and `podman` select a runtime explicitly. The default is `--sandbox=off`, which preserves the current direct host launch path.

The project directory is mounted at `/home/agent/workspace`, and the agent starts there. Launch passes session-scoped Aileron daemon env into the container, including `AILERON_URL`, `AILERON_COMMS_URL`, `AILERON_SESSION_ID`, `AILERON_APPROVAL_URL`, and the sandbox image metadata (`AILERON_SANDBOX_IMAGE`, `AILERON_SANDBOX_TIER`, `AILERON_SANDBOX_RUNTIME`). For local daemon URLs, launch rewrites the container-facing host to `host.docker.internal` for Docker and `host.containers.internal` for Podman.

Before registering the session, launch validates the selected image with the same mount/workdir shape it will use for the agent. The image must:

- execute `/bin/sh` commands through the selected container runtime
- use `/home/agent/workspace` as the working directory
- allow a temporary file to be written in the mounted workspace
- resolve the agent command on `PATH`

The agent command must already exist in the selected image. For Tier 1, install the agent CLI in your devcontainer Dockerfile. Tier 2 uses the BYO image as supplied until runtime injection lands.

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

In BYO-image mode, launch currently uses the image as supplied. Later runtime launch work injects Aileron's runtime contract at launch: the `aileron` binary/shims, discovery files, proxy bootstrap, session CA, and shell mediation files.

## What Belongs in the Image

Put ordinary project tooling in the devcontainer: language runtimes, CLIs, package managers, private CA bundles, and internal helper tools.

Do not put Aileron credentials or user secrets in the image. Credentialed traffic is designed to flow through the Aileron HTTPS proxy/data plane. Runtime bootstrap supplies proxy configuration when that layer lands.

## What This Does Not Do Yet

This slice does not inject runtime files into BYO images, generate discovery shims, add proxy bootstrap, or mediate shell commands. Follow-on work adds runtime injection, the discovery watcher, proxy bootstrap, and shell-layer interception.
