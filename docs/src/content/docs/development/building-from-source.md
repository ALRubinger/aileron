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
