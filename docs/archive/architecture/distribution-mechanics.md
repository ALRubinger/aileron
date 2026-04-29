---
title: "Two Distribution Mechanics"
description: "Connectors are content-addressed binaries; actions are files copied into the project (ShadCN-style)"
order: 7
---

> **Architecture:** part of the [Architecture](/architecture/) section of the Pivot strategy. See also [The Connector Model](/architecture/connector-model), [The Action Model](/architecture/action-model), and [Project Portability](/architecture/project-portability).

Aileron uses two distinct distribution models for two different kinds of artifact, each suited to its purpose:

- **Connectors are binaries.** Sandboxed WASM modules with capability declarations and signed publisher hashes. They live in a content-addressed store outside any specific project (similar to Docker's image cache or Cargo's crate cache). Action files reference them by name + exact version + hash. When an action loads, Aileron verifies the connector binary in the store matches the declared hash before executing.

- **Actions are declarative source files.** They get copied into the developer's project on install, live in `actions/` (or wherever the project chooses), are tracked by git, and become the developer's own to evolve. The Hub serves them as starting-point templates; once installed they're indistinguishable from action files the developer wrote themselves.

This is the ShadCN model for actions plus a content-addressed store for connectors. There is no project-level lock file — each action file declares its own dependencies (connector name + exact version + hash) and is therefore self-describing. Reproducibility comes from git.

The CLI surface follows from this:

- **`aileron connector install slack@1.2.0`** — fetches the binary into the local connector store, verifies the hash and signature, runs the install-time consent flow, registers capability bindings.
- **`aileron action add ship-update`** — fetches the action template from the Hub, copies it into `actions/ship-update.md`, resolves any missing connector dependencies, prompts for capability bindings. From that moment on, the developer owns the file.
- **`aileron sync`** — reads the action files in the project, installs missing connectors (verifying hashes), prompts for unbound credentials. Standard `npm ci`-shape, but driven by the action files themselves.

Update tooling assists where useful, always editing action files visibly:

- **`aileron action update <name>`** — fetches the latest version of an action from the Hub and generates a diff against the local file. The developer accepts or rejects via git review.
- **`aileron connector check`** — scans all action files, lists available updates for any connector referenced anywhere.
- **`aileron connector update <connector>`** — bumps a connector reference (name + version + hash) across all action files that use it, after explicit confirmation.
- **`aileron action audit`** — lists every action and connector the project uses, every declared capability, every binding identity. Single command, full picture.

Updates always show up as git diffs. Nothing happens silently.
