---
title: "Development"
description: "How to build, test, and contribute to Aileron."
order: 0
---

This section is for contributors. It covers how the repo is laid out, which binaries make up an Aileron install, how to build them from source, how to run the test suites, and what's expected of a change before it lands on `main`.

If you're looking for end-user docs, the [Getting Started](/getting-started/) guide and the [Guides](/guides/) section are likely what you want. The pages here go one level deeper into the codebase.

## What's in this section

- [Repo Layout](/development/repo-layout/) — the directory tree, where each concern lives, and how the workspace is wired.
- [Binary Architecture](/development/binary-architecture/) — the four binaries Aileron ships, who calls whom, and which process owns which trust boundary.
- [Building from Source](/development/building-from-source/) — prerequisites, the Taskfile entry points, and how the embedded assets (webapp, forwarder WASM) get folded in.
- [Running Tests](/development/running-tests/) — unit, integration, race, coverage. What CI runs, and how to reproduce a CI failure locally.
- [Sandbox Composition](/development/sandbox-composition/) — how Aileron uses devcontainer.json, `aileron/sandbox-base`, and `aileron sandbox` to define the agent container image.
- [Sandbox Agent Images](/development/sandbox-agent-images/) — which agent commands are supported by the selected sandbox image and how to check them before launch.
- [Sandbox Connector Specs](/development/sandbox-connector-specs/) — how installed connector specs become sandbox-visible tools and generated HTTPS shims.
- [Sandbox Proxy CLI Verification Matrix](/development/sandbox-proxy-cli-matrix/) — verify the v4 HTTPS proxy works with `curl`, `gh`, and `aws`; success and failure cases with expected audit events.
- [Submitting Changes](/development/submitting-changes/) — branch and PR conventions, commit message format, ADR amendments, what gets reviewed.
- [Adding an Agent](/development/adding-an-agent/) — how to wire a new AI coding agent into `aileron launch` via the `Agent` SPI.

## A note on the docs site versus the repo

The repo's `README.md` is the entry point for someone landing on the GitHub page. The docs here are the longer-form companion. When you change build flags, tasks, or binaries, update both: README for the overview, this section for the depth.
