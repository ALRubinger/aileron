---
title: "Sandbox Composition"
description: "How to configure the agent container with devcontainer.json, Aileron's sandbox-base image, and the aileron sandbox CLI."
order: 6
---

Sandbox composition is the contract for deciding which container image an agent session runs in. It is defined by [ADR-0017](/adr/0017-sandbox-composition/) and implemented by the `aileron sandbox` CLI.

This page covers the user-facing workflow. Runtime launch support can scaffold, inspect, build, run the agent command in the prepared sandbox image, and inject static discovery/action and connector-spec shims. Live discovery refresh, proxy/session CA bootstrap, and shell mediation are follow-on runtime layers tracked in [#897](https://github.com/ALRubinger/aileron/issues/897), [#896](https://github.com/ALRubinger/aileron/issues/896), and [#801](https://github.com/ALRubinger/aileron/issues/801).

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

## Check Agent Support

Use `sandbox check` to validate that the selected image can run an agent command before starting a daemon-backed launch session:

```bash
aileron sandbox check --runtime=docker --agent=claude
aileron sandbox check --runtime=podman --build=never --agent=codex
```

`sandbox check` uses the same composition plan, build policy, and minimal image validation as sandbox launch. It reports the selected tier, runtime, image, command, and `support: ok` when the command is available. Agent-specific image recipes and support status live in the [sandbox agent image matrix](/development/sandbox-agent-images/).

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

An internal proxy-bootstrap mode is available for development of the #896 HTTPS data plane. When `AILERON_SANDBOX_PROXY_BOOTSTRAP=1` is set, sandbox launch generates a session-local CA, mounts the public CA at `/etc/aileron/proxy/ca.pem`, and sets standard proxy env (`HTTPS_PROXY`, `HTTP_PROXY`, `NO_PROXY`) plus Aileron metadata (`AILERON_SANDBOX_PROXY_MODE`, `AILERON_SANDBOX_PROXY_URL`, `AILERON_SANDBOX_PROXY_CA_FILE`). The proxy URL uses standard proxy userinfo so clients can send `Proxy-Authorization`; it carries the launch session id and, when present, the local daemon token. Images used with this mode must provide `aileron-install-proxy-ca` and `aileron-run-with-proxy-ca`; the current sandbox-base image includes both and launch validation checks both before the agent starts. In this internal mode, the container starts through `aileron-run-with-proxy-ca`, installs the mounted CA as root, then drops back to the `agent` user before executing the requested agent command. The daemon-side `/sandbox-proxy/requests` boundary can proxy recognized bodyless HTTPS requests with daemon-side credential injection, and `/connector-operations/run` can route eligible generated-shim operations through that boundary with `GET`/`DELETE`/`HEAD` args encoded as query parameters and `POST`/`PATCH`/`PUT` args sent as JSON request bodies. The daemon also recognizes standard proxy-shaped requests, authenticates their `Proxy-Authorization`, completes authenticated `CONNECT host:443` TLS interception with the session CA, and routes decrypted requests through the same sandbox proxy boundary when they uniquely match an installed connector spec operation by method, host, and path. Smoke coverage confirms standard proxy URL userinfo can authenticate a normal HTTPS client through this transparent path. Missing or ambiguous transparent matches fail closed. This mode is still not a complete user-facing credential mediation feature: broader arbitrary-client integration and polish remain follow-on work.

When installed action manifests or connector store metadata exist on the host, launch mounts them read-only under `/opt/aileron/manifests/actions` and `/opt/aileron/manifests/connectors`. Launch also generates a session-scoped static `/etc/aileron/tools.txt` manifest and read-only connector shim scripts under `/usr/local/bin` from two sources:

- installed action manifests, which create shims that can execute an explicit installed action name through `AILERON_API_URL` with optional raw JSON args
- installed `aileron.connector.v1.json` specs, which create operation shims that post to the stable `/connector-operations/run` daemon API contract

Both shim types support `--help` for discovery and include the launch session id when `AILERON_SESSION_ID` is set, so daemon-side approval context stays tied to the sandbox session. Generated connector shims require `wget`; Aileron's sandbox-base image includes it, and BYO/devcontainer images that receive shims are validated for it before agent startup. The connector-spec format and conflict rules are documented in [Sandbox Connector Specs](/development/sandbox-connector-specs/). This static launch-scoped discovery/action surface is the current sandbox runtime contract. Live `tools.txt` refresh and watcher processes can layer on later when in-session connector changes need them.

Before running the agent, launch validates the selected image with the same env, mount, and workdir shape it will use for the agent. The image must:

- execute `/bin/sh` commands through the selected container runtime
- use `/home/agent/workspace` as the working directory
- allow a temporary file to be written in the mounted workspace
- resolve the agent command on `PATH`
- provide `wget` when generated connector shims are mounted

The agent command must already exist in the selected image. For Tier 1, install the agent CLI in your devcontainer Dockerfile. Tier 2 uses the BYO image as supplied while Aileron's runtime injection remains limited to session env, manifest mounts, `tools.txt`, and connector shims. See the [sandbox agent image matrix](/development/sandbox-agent-images/) for the current support contract and recipes.

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

In BYO-image mode, launch uses the image as supplied and layers on Aileron's session env, manifest mounts, generated discovery files, and connector shims. Images that opt into internal proxy-bootstrap development must include `aileron-install-proxy-ca` and `aileron-run-with-proxy-ca` helpers compatible with the sandbox-base contract. Sandbox-base now also carries the first #801 shell-mediation helper files (`aileron-shell-mediator` and `/etc/aileron/shell/aileron-bashrc`), but launch does not enable shell mediation or require those files for BYO images yet.

## What Belongs in the Image

Put ordinary project tooling in the devcontainer: language runtimes, CLIs, package managers, private CA bundles, and internal helper tools.

Do not put Aileron credentials or user secrets in the image. Current generated action shims call the Aileron daemon API with the launch token and session context. Later credentialed network flows are designed to use the Aileron HTTPS proxy/data plane when that layer lands.

## What This Does Not Do Yet

This runtime path does not add live discovery refresh, polished arbitrary-client proxy support, or active shell command mediation. Generated session-scoped `/etc/aileron/tools.txt` and read-only connector shims support `--help` discovery; action shims can execute installed actions via `AILERON_API_URL`, and spec-backed operation shims call the stable `/connector-operations/run` daemon API contract. Eligible spec-backed operations can now flow through the daemon HTTPS proxy boundary with credential injection and `connector.proxy.proxied` audit records, including JSON request bodies for `POST`, `PATCH`, and `PUT`. Proxy-bootstrap launches can install the session CA in the container trust store before the agent starts, authenticate standard proxy-shaped requests back to the daemon, and route uniquely matched decrypted `CONNECT` requests through the daemon credential boundary; standard proxy URL userinfo has smoke coverage for this path. The shell-mediation track now connects the image-side helper to that allow-only daemon decision contract at `/sandbox-shell/decide`. When `AILERON_SANDBOX_SHELL_MEDIATION=1` is set, `aileron-shell-mediator intercept` POSTs the command to the endpoint and fails closed on anything but a clean allow, and `aileron-bashrc` installs a bash `DEBUG` trap that routes each about-to-run command through the mediator and vetoes it when the mediator exits nonzero. The endpoint stays allow-only, and launch still does not route `/bin/sh`, `/bin/bash`, or agent commands through the helper, so the live agent session is not yet mediated. Follow-on work adds broader proxy/client integration and polish for arbitrary HTTPS clients ([#896](https://github.com/ALRubinger/aileron/issues/896)), live discovery refresh only if dynamic in-session connector changes require it ([#897](https://github.com/ALRubinger/aileron/issues/897)), final credentialed HTTPS audit semantics ([ADR-0019](/adr/0019-v4-https-data-plane/)), and active shell-layer interception ([#801](https://github.com/ALRubinger/aileron/issues/801), [ADR-0021](/adr/0021-v4-shell-layer-mediation/)).
