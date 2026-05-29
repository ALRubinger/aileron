---
title: "Sandbox Composition"
description: "How to configure the agent container with devcontainer.json, Aileron's sandbox-base image, and the aileron sandbox CLI."
order: 6
---

Sandbox composition is the contract for deciding which container image an agent session runs in. It is defined by [ADR-0017](/adr/0017-sandbox-composition/) and implemented by the `aileron sandbox` CLI.

This page covers the user-facing workflow. Runtime launch support is intentionally still staged: the current implementation can scaffold, inspect, and build sandbox images, while later work wires those images into `aileron launch`.

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
| Tier 2 | Does not build. The BYO image is reported as-is; runtime injection and launch-time validation land later. |

When building the base image outside the source tree, set `AILERON_SANDBOX_BASE_CONTEXT` to the directory containing the sandbox-base `Containerfile`.

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

In BYO-image mode, later runtime launch work will use the image as supplied and inject Aileron's runtime contract at launch: the `aileron` binary/shims, discovery files, proxy bootstrap, session CA, and shell mediation files.

## What Belongs in the Image

Put ordinary project tooling in the devcontainer: language runtimes, CLIs, package managers, private CA bundles, and internal helper tools.

Do not put Aileron credentials or user secrets in the image. Credentialed traffic is designed to flow through the Aileron HTTPS proxy/data plane. Runtime bootstrap supplies `HTTPS_PROXY` and `AILERON_TOKEN` when container launch support lands.

## What This Does Not Do Yet

This slice does not run containers or inject runtime files into BYO images. Follow-on work wires built images into launch, adds BYO runtime injection and validation, and adds the discovery watcher. Shell-layer interception builds on top of that runtime substrate.
