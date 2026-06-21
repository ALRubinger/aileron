---
title: "Add Your Own CLI Tool to the Sandbox"
description: "Compose a custom CLI tool into the agent sandbox with a devcontainer Feature, then graduate that tool to a credentialed CLI whose token Aileron seals at the network boundary."
---

This guide walks you from a plain command-line tool to a credentialed one baked into the agent sandbox. By the end you will have authored a devcontainer Feature that installs your tool, published it to your own container registry, and (for tools that talk to a third-party API) declared the credential story that lets Aileron seal the tool's token at the network boundary so the agent never holds it.

This is the task page. The authoritative reference for the composition model is [ADR-0017](/adr/0017-sandbox-composition/), the credential contract is [ADR-0026](/adr/0026-cli-capability-units/), and the full `aileron sandbox` CLI workflow lives in [Sandbox Composition](/development/sandbox-composition/). This guide links out to those rather than restating them.

## Orientation: devcontainers and Features

A [devcontainer](https://containers.dev/) is a standard, tool-agnostic way to describe the container a project develops in. The description lives in a `.devcontainer/devcontainer.json` file: it names a base image and lists the tools to layer on top. The standard is owned by the wider ecosystem, not by Aileron, so the same file works with VS Code, GitHub Codespaces, and any other devcontainer-aware tool.

A devcontainer **Feature** is one self-contained install unit. It is a small directory holding a manifest (`devcontainer-feature.json`) and an install script (`install.sh`) that runs on top of any base image. A Feature installs one thing, the GitHub CLI for example, and a project composes several Features together to assemble its full toolchain. Features publish to a container registry, so you reference one by its registry coordinate the same way you reference an image.

Aileron composes the agent sandbox from exactly these pieces. Aileron owns a minimal, harness-free base image and you extend it by listing Features in `devcontainer.json`. Aileron reads the standard devcontainer fields and stores its own settings under a `customizations.aileron` block, which is the standard's escape hatch for tool-specific configuration. Because composition is standard devcontainer Features, "Aileron's base plus an agent plus your own tools" is expressible as three independent Features on one base, with no image-lineage merging required. That model is the subject of [ADR-0017](/adr/0017-sandbox-composition/).

## Compose existing Features

Before authoring anything, you can compose Features that already exist. `aileron sandbox init` scaffolds a starter `.devcontainer/devcontainer.json` that lists Aileron's base image and an agent Feature:

```bash
aileron sandbox init
```

The scaffold is the Tier 1 customization tier. It writes one file: the base image, an agent Feature, and a commented slot for your own tooling Feature.

```jsonc
{
  "name": "Aileron sandbox",
  "image": "ghcr.io/alrubinger/aileron-sandbox-base:<version>",
  "features": {
    // The agent Feature installs the agent CLI onto the base image.
    "ghcr.io/alrubinger/aileron-features/claude:0": {}
    // Add your own tooling as its own Feature alongside the agent Feature:
    // "ghcr.io/acme/internal-tools:1": {}
  },
  "customizations": {
    "aileron": {
      "mediation": "default",
      "approval_surface": "both"
    }
  }
}
```

Each entry under `features` is a registry coordinate plus an options object. To add a tool that already ships as a Feature, drop its coordinate in alongside the agent Feature and rebuild. The rest of this guide is about the case where the tool you need does not yet have a Feature, so you author one.

## Author a Feature for a plain CLI

A Feature for a plain CLI is a directory with two files: the manifest and the install script. Mirror the layout of the worked `gh` Feature at [`images/sandbox-features/gh/`](https://github.com/ALRubinger/aileron/tree/main/images/sandbox-features/gh).

```
sandbox-features/
└── acme-cli/
    ├── devcontainer-feature.json
    └── install.sh
```

The manifest declares the Feature's id, version, and human-readable name. For a plain tool with no credential, that is all it needs:

```json
{
  "id": "acme-cli",
  "version": "0.0.1",
  "name": "Acme CLI",
  "description": "Installs the Acme CLI onto the Aileron sandbox base so it is on PATH for the non-root agent user.",
  "documentationURL": "https://example.com/acme-cli"
}
```

The install script runs as root during the image build and lands the binary on `PATH` for the non-root `agent` user. The Aileron base image is Alpine, so an `apk` recipe is the common case. Keep it idempotent and image-layer-clean:

```sh
#!/bin/sh
set -eu

apk add --no-cache acme-cli
```

If your tool ships only as a release binary or through another package manager, fetch and install it here instead. The install script is ordinary shell; Aileron does not constrain how the tool gets installed, only that it ends up on `PATH`.

### Publish the Feature

A Feature is consumed from a container registry, so publish it before you reference it. The official devcontainer CLI packages and pushes a directory of Features:

```bash
devcontainer features publish \
  ./sandbox-features \
  --namespace your-org/your-repo \
  --registry ghcr.io
```

This is the same command Aileron's own `.github/workflows/sandbox-features.yml` workflow runs over `images/sandbox-features/**` to publish the in-house Features. Publishing a manifest at version `0.0.1` emits the tag set `0.0.1`, `0.0`, `0`, and `latest`, so a coordinate pinned to the broadest major tag (`:0`) keeps resolving across patch bumps without re-editing your `devcontainer.json`.

The first publish of a new package needs a one-time manual step: a freshly pushed GHCR package starts private, and the sandbox build pulls Features anonymously, so flip the package to public in its GHCR settings after the first publish. This is per-package, not per-build.

Once published, reference it from your `devcontainer.json` alongside the agent Feature:

```jsonc
{
  "features": {
    "ghcr.io/alrubinger/aileron-features/claude:0": {},
    "ghcr.io/your-org/your-repo/acme-cli:0": {}
  }
}
```

Then build and verify with the `aileron sandbox` workflow:

```bash
aileron sandbox plan
aileron sandbox build
```

`plan` reports the composition tier Aileron infers, and `build` composes the Features onto the base image through the official `@devcontainers/cli` build engine. The full plan/build/check/launch workflow is documented in [Sandbox Composition](/development/sandbox-composition/).

## Advanced: a credentialed CLI

A CLI that talks to a third-party API needs a credential. The defining property of the Aileron sandbox is that the agent never holds that credential. Aileron seals it at the network boundary: the tool's outbound HTTPS request leaves the sandbox carrying a placeholder, and the Aileron proxy swaps in the real token at egress. The token lives in the Aileron vault, never in the container.

You declare this credential story as one **CLI-capability unit** under `customizations.aileron.cli` on the tool's Feature. The unit is one tool's complete credential story in one place: how the token is acquired, and how each outbound host is sealed. This is the subject of [ADR-0026](/adr/0026-cli-capability-units/), which is the authoritative contract for every field below.

The `gh` Feature is the worked example. Its manifest carries this `customizations.aileron.cli` block:

```json
{
  "customizations": {
    "aileron": {
      "cli": {
        "name": "gh",
        "key": "user/github",
        "presence": {
          "builtin": "base"
        },
        "acquisition": {
          "mode": "device-flow",
          "container_name": "aileron-auth-github",
          "login_cmd": ["gh", "auth", "login", "--hostname", "github.com", "--git-protocol", "https", "--web"],
          "token_cmd": ["gh", "auth", "token", "--hostname", "github.com"],
          "browser_shim": "echo"
        },
        "sealing": [
          {
            "host": "github.com",
            "scheme": "basic",
            "emit_mechanism": "inject",
            "username": "x-access-token"
          },
          {
            "host": "api.github.com",
            "scheme": "bearer",
            "emit_mechanism": "sentinel-swap",
            "sentinel": {
              "value": "ghp_AILERONSENTINELAAAAAAAAAAAAAAAAAAAAA",
              "env": "GH_TOKEN"
            }
          }
        ]
      }
    }
  }
}
```

Read the unit top to bottom:

- **`key`** is the tool's credential identity, declared once in `<kind>/<service>` form (`user/github` here). It is the single source of truth. Aileron derives the acquisition's storage location, the credential kind (`user`, the first path segment), and every sealing entry's credential reference from this one field. The unit must not re-declare any derived field, because that would let the tool's identity drift; re-declaring one is a load error.
- **`acquisition`** is the one-time login that mints the token. In `device-flow` mode Aileron runs `login_cmd` interactively in a throwaway container, you complete the device login in your browser, and Aileron reads the token back out with `token_cmd` and stores it in the vault.
- **`sealing`** is a list, one entry per outbound host. Each entry says which credential scheme that host expects and how Aileron emits it at the proxy. The `github.com` entry seals git-over-HTTPS with HTTP basic auth and injects unconditionally. The `api.github.com` entry seals the `gh` API call with a bearer token by planting a non-secret sentinel value that the proxy swaps for the real token at egress. The sentinel is a committable placeholder with no authority.
- **`presence`** records which built-in image tier already carries the tool. It is accepted and validated but drives no behavior yet.

The host reads this unit from the resolved sandbox image's `devcontainer.metadata` label at launch and projects it into the same credential-capture and proxy-binding machinery the rest of Aileron uses. You author one unit on one Feature, and no central file in Aileron changes. To adapt the example for your own tool, change the `name`, set `key` to your tool's `<kind>/<service>` identity, point `acquisition` at your tool's login and token commands, and add one `sealing` entry per host your tool calls. See [ADR-0026](/adr/0026-cli-capability-units/) for the full field-by-field descriptor contract.

## What this does not do yet

A few honest limitations apply today.

> - **Device-flow acquisition only.** The shipped acquisition mode is the interactive `device-flow` login above. The `direct-token` mode, for a tool you hand a token to directly rather than logging in, is reserved and rejected: a unit naming it fails validation with an explicit not-yet-implemented error ([ADR-0026](/adr/0026-cli-capability-units/)).
> - **A newly installed tool needs an MCP restart to surface.** Live in-session discovery refresh is a follow-on runtime layer, tracked in [#897](https://github.com/ALRubinger/aileron/issues/897). Rebuild and relaunch the sandbox after adding a Feature.
> - **Custom Features publish to your own registry with no Aileron co-sign.** The `devcontainer features publish` flow pushes to your GHCR namespace under your own trust. Aileron does not co-sign or vet third-party Features, and the trust tiers that would gate an untrusted unit are forward-declared, not shipped ([ADR-0026](/adr/0026-cli-capability-units/)).

## Where to go next

- [Sandbox Composition](/development/sandbox-composition/) for the full `aileron sandbox init` / `plan` / `build` / `check` / `launch` workflow and the composition tiers.
- [Sandbox Agent Images](/development/sandbox-agent-images/) for the Feature directory structure and GHCR publishing path in depth.
- [ADR-0017](/adr/0017-sandbox-composition/) for the composition model decision.
- [ADR-0026](/adr/0026-cli-capability-units/) for the CLI-capability unit contract.
