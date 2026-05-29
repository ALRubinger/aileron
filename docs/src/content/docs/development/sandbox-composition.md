---
title: "Sandbox Composition"
description: "How to configure the agent container with devcontainer.json, Aileron's sandbox-base image, and the aileron sandbox CLI."
order: 6
---

Sandbox composition is the contract for deciding which container image an agent session runs in. It is defined by [ADR-0017](/adr/0017-sandbox-composition/) and implemented by the `aileron sandbox` CLI.

This page covers the user-facing workflow. Runtime launch support is intentionally still staged: the current implementation can scaffold and inspect the composition plan, while later work wires that plan into container build and `aileron launch`.

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

This slice does not run containers or inject runtime files into BYO images. Follow-on work wires image builds into launch, adds BYO runtime injection and validation, and adds the discovery watcher. Shell-layer interception builds on top of that runtime substrate.
