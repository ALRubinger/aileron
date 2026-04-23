---
title: "Building from Source"
description: "Build Aileron from source"
---

## Prerequisites

- [Go](https://go.dev/dl/) 1.25 or later
- [go-task](https://taskfile.dev/) task runner
- An AI coding agent installed (e.g., [Claude Code](https://claude.ai/code))

For the full stack (server, UI, docs):
- [Node.js](https://nodejs.org/) 24 (see `.nvmrc`)
- [pnpm](https://pnpm.io/) package manager
- [Docker](https://docs.docker.com/get-docker/) and Docker Compose

## Build the CLI

```sh
task build:cli       # builds build/aileron
task build:sh        # builds build/aileron-sh
```

Both binaries must be available together. The CLI looks for `aileron-sh` next to itself, then on PATH.

## Build everything

```sh
task build
```

## Individual components

```sh
task build:server    # Go server binary
task build:mcp       # MCP server binary
task build:enclave   # TEE enclave binary
task build:ui        # SvelteKit UI
task build:docs      # Documentation site
task build:docker    # Docker containers
```

## Enclave production image

Build and push the enclave container image to GCP Artifact Registry:

```sh
task build:enclave:production GCP_PROJECT=my-project GCP_REGION=us-central1
```

| Variable | Description |
|----------|-------------|
| `GCP_PROJECT` | GCP project ID (required) |
| `GCP_REGION` | Artifact Registry region, must match the region of your enclave repository (required) |

The task builds for `linux/amd64` (required by Confidential Space), pushes to Artifact Registry, and prints the image digest. You must have Docker running and `gcloud auth configure-docker` set up for the target region.

This is for manual one-off builds. CI handles production builds automatically via the [Enclave Publish](https://github.com/ALRubinger/aileron/actions/workflows/enclave-publish.yml) workflow. See the [TEE Enclave deployment guide](/deployment/tee-enclave/#3-build-and-push-the-enclave-container-image) for full setup context.
