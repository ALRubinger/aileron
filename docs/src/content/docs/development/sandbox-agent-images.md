---
title: "Sandbox Agent Images"
description: "Supported agent commands, image requirements, and recipes for Aileron sandboxes."
order: 7
---

Sandbox launch runs the agent command inside the selected container image. Aileron prepares and validates the image, but the image must already contain the agent CLI.

Use `sandbox check` to validate an image before starting a daemon-backed session:

```bash
aileron sandbox check --runtime=docker --agent=claude
aileron sandbox check --runtime=docker --build=never --agent=codex
```

The check uses the same composition plan and minimal launch validation as `aileron launch --sandbox=...`: `/bin/sh`, the `/home/agent/workspace` mount, workspace write access, and the requested agent command on `PATH`.

## Support Matrix

| Agent | Command | Sandbox image support | MCP under `--sandbox=docker` | Notes |
|---|---|---|---|---|
| Claude Code | `claude` | Agent Feature | ✓ via `--mcp-config` | First-class Feature below. Use `sandbox check --agent=claude` before launch. |
| Codex | `codex` | Agent Feature | ✓ via bind-mounted `config.toml` | Feature below. Sandbox launch writes a generated `config.toml` to a host tempdir and bind-mounts it into `/home/agent/.codex/config.toml` ([ADR-0024](/adr/0024-sandbox-mcp-parity/)). Host `~/.codex/config.toml` is never touched. |
| Goose | `goose` | Command contract only | ✓ via `--with-extension` | List the agent Feature in Tier 1, or install the CLI in a BYO image; no maintained Feature yet. |
| OpenCode | `opencode` | Command contract only | ✓ via workspace `opencode.json` | Launcher writes `opencode.json` into the launch directory; the workspace bind-mount makes it readable in-container. |
| Pi | `pi` | Command contract only | ✓ via `--mcp-config` | Shares Claude's MCP wiring. |
| Other agents | varies | Unsupported | n/a | Add an Aileron launch agent and an image recipe before relying on sandbox launch. |

Under `--sandbox=docker` the launcher resolves the host-built `aileron-mcp` binary, bind-mounts it read-only at `/usr/local/bin/aileron-mcp`, builds an `mcpEnv` rewritten for the runtime (`host.docker.internal` on Docker), and calls each agent's `ConfigureMCP` hook. Four of the five agents (Claude, Pi, Goose, OpenCode) work without any agent-side code change because their config is either inline-with-exec (`--mcp-config`, `--with-extension`) or workspace-local (`opencode.json` in the bind-mounted workspace). Codex is the one exception — its host `~/.codex/config.toml` is irrelevant inside the container, so the launcher writes a generated `config.toml` to a host tempdir and bind-mounts it. See [ADR-0024](/adr/0024-sandbox-mcp-parity/) and the [manual walkthrough](/development/sandbox-mcp-walkthrough/) for the load-bearing flow.

Docker is the only supported sandbox runtime in v4. Podman is planned but not yet supported; its `host.containers.internal` host alias is the deferred re-add path, and passing `--runtime=podman` fails with `podman runtime is not supported yet (v4 is Docker-only); see ADR-0014` (see [ADR-0014](/adr/0014-spawn-sandbox-technology/)).

The harness-free `ghcr.io/alrubinger/aileron-sandbox-base` intentionally does not include agent CLIs ([ADR-0017](/adr/0017-sandbox-composition/)). Each agent install is authored once as a devcontainer Feature, the single source of truth that Aileron CI bakes into prebuilt per-agent images and that customers compose for Tier 1. The prebuilt per-agent image is the zero-build Tier 0 default, owned by [#965](https://github.com/ALRubinger/aileron/issues/965). Use Tier 1 when you want Aileron's base runtime plus an agent plus your own tools, or Tier 2 when your team owns the full image.

## Build-Free Default

With no `.devcontainer` in the project, `aileron launch --sandbox=docker <agent>` is build-free for a published agent. The launcher resolves the prebuilt per-agent image `ghcr.io/alrubinger/aileron-sandbox-<agent>` and pulls it. There is no `sandbox init`, no Dockerfile, and no local image build. The published agents today are `claude` and `codex`.

`sandbox check --agent=<agent>` resolves the same image, so a passing check matches what launch will run.

The launcher resolves the image for this build-free default in two ways. A release build with a recorded digest pin pulls a fixed image by `@sha256` (see [Reproducible releases](#reproducible-releases-digest-pinning) below). Otherwise it resolves a floating tag: a release build with no recorded pin pulls `latest` (which the image workflows move only on a `v*` tag), and a dev build off `main` pulls `edge` (republished on every merge to `main` that touches the image source, and on `workflow_dispatch`). So `latest` always names the most recent release and a dev run never clobbers it. A version-pinned tag (`<aileron-version>-<agent-cli-version>`) needs the agent CLI version, which the launcher does not know at resolve time, so the floating tag is the fallback when no digest is pinned. The freshness policy that keeps `edge` current is owned by [#1088](https://github.com/ALRubinger/aileron/issues/1088).

When the requested agent has no published image, launch falls back to the customization tier. The image validation then emits the actionable message to install the agent CLI in the sandbox image or launch with `--local`.

## Prebuilt Per-Agent Images

Aileron CI publishes one multi-arch image per agent to GHCR. Each image is baked from the GHCR sandbox base plus that agent's devcontainer Feature install script, so the Feature stays the single source of truth.

| Agent | Image |
|---|---|
| Claude Code | `ghcr.io/alrubinger/aileron-sandbox-claude` |
| Codex | `ghcr.io/alrubinger/aileron-sandbox-codex` |

Each image is built for `linux/amd64` and `linux/arm64`, so a `docker pull` resolves the manifest for your platform automatically.

The base image (`sandbox-base.yml`) and the per-agent images (`sandbox-agents.yml`) are two separate workflows, both triggered directly by a `v*` tag push, so they run concurrently on the same tag. A per-agent build `FROM`s the version-pinned base tag, so before that `FROM` it waits for the base tag to publish: it polls the registry on a bounded retry loop until the concurrently-building base tag appears. A base that never publishes surfaces a precise timeout error naming the absent tag rather than failing mid-build.

On a merge to `main` the two workflows run in sequence rather than concurrently. The base workflow runs on the `main` push (filtered to image-affecting paths) and republishes `base:edge`. The per-agent workflow then runs as a `workflow_run` cascade keyed on the base workflow's completion, so it starts only after the base finishes its multi-arch push and composes the agents onto the freshly republished `base:edge`, never a stale one. The cascade leg runs only when the triggering base run succeeded on `main`. This keeps the image source and the `edge` tags in lockstep, so an in-repo change to the image (for example a new in-image entrypoint helper) cannot leave dev/main consumers pulling a base that predates it.

The tag scheme is `<aileron-version>-<agent-cli-version>` plus two floating tags: `latest`, moved only by a `v*` tag release, and `edge`, republished on every merge to `main` that touches the image source and on a `workflow_dispatch`. The `<aileron-version>` is the Aileron release the image was built from. The `<agent-cli-version>` is the agent CLI version baked into the image, resolved at build time from the installed package. A git-traceability tag `git-<sha>` is also published.

Pull the most recent release (`latest`), or the latest dev publish off `main` (`edge`):

```bash
docker pull ghcr.io/alrubinger/aileron-sandbox-claude:latest
docker pull ghcr.io/alrubinger/aileron-sandbox-codex:latest
# tip of main, republished on every image-affecting merge to main:
docker pull ghcr.io/alrubinger/aileron-sandbox-claude:edge
```

Pull a pinned version, for example Aileron `0.0.1` with the Claude CLI at `2.1.179`:

```bash
docker pull ghcr.io/alrubinger/aileron-sandbox-claude:0.0.1-2.1.179
```

CI smoke-tests every published image for launchability before it ships. The smoke asserts the agent CLI resolves on `PATH` and that the launcher's image validation succeeds.

A daily watcher workflow (`sandbox-agents-watch.yml`) keeps the `edge` images fresh against upstream agent-CLI releases. It polls npm for the latest `@anthropic-ai/claude-code` and `@openai/codex` versions. It compares each against the CLI version baked into the `edge` image, recovered from the `dev-<cli-version>` tag co-located on the `edge` manifest digest. On drift it re-triggers `sandbox-agents.yml`, which rebuilds from the unpinned Feature install scripts and so bakes the latest CLI. The refreshed build publishes `edge` and `dev-<cli-version>`. Dev and main consumers pull `edge`, so they pick up new agent CLIs automatically without a release.

`latest` and the release-pinned `<aileron-version>-<agent-cli-version>` tags move on `v*` releases only, by design. A released user on `latest` stays pinned to that release's CLI version until the next release. This is intentional. A release is an immutable point and `latest` names the most-recent release, so a background job must never clobber it. Keeping `latest` fresh between releases is an accepted gap, not a bug.

The watcher supports a `dry_run` `workflow_dispatch` input for demonstration, which detects and reports drift without dispatching a rebuild. It uses only `GITHUB_TOKEN` and bakes no credentials anywhere.

## Reproducible Releases: Digest Pinning

The floating `latest` tag is mutable. The freshness watcher republishes images between releases, so a `latest`-resolving launcher could pull a different image than the one a release was cut against. Digest pinning ([#1233](https://github.com/ALRubinger/aileron/issues/1233)) removes that drift: a release pins each per-agent image to its immutable `@sha256` digest, so the same release binary always pulls the same image.

The pins live in a committed lockfile, `internal/sandbox/composition/agent-images.lock.json`, embedded into the binary via `go:embed`. It maps each published agent to a `sha256:...` digest. `PublishedAgentImage` consumes it: a release build (a real version, which resolves to `latest`) returns `ghcr.io/alrubinger/aileron-sandbox-<agent>@sha256:...` when the lockfile records the agent, and falls back to the floating tag otherwise. A dev build keeps `edge` and is never pinned, so tip-of-`main` development always tracks the freshest image. An empty lockfile means no agent is pinned yet and releases keep pulling `latest`, the pre-#1233 behavior.

Regenerate the lockfile at release prep, after the release's images are published and before tagging the binary:

```bash
task generate:agent-digests
```

The generator (`internal/tools/agentdigests`) resolves each published agent's manifest digest from the registry via `docker buildx imagetools inspect` and writes the lockfile in canonical form. Commit the result, then tag the release. Override the resolved tag with `TAG=<tag>` (default `latest`). The generated file is deterministic, so a regenerate with no registry change is a no-op diff. A test asserts the committed lockfile is canonical, so a hand-edit or a stale regenerate fails CI.

## Reproducible Toolchains: Node Version + Checksum Pin

The managed-toolchain devcontainer build ([#1525](https://github.com/ALRubinger/aileron/issues/1525)) installs a fixed Node.js into the sandbox. To keep that install reproducible it is pinned the same way images are: a committed lockfile, `internal/sandbox/container/tools.lock.json`, embedded into the binary via `go:embed`. It records the pinned Node version and, per supported platform (`darwin-arm64`, `darwin-x64`, `linux-arm64`, `linux-x64`, `win-x64`), the sha256 of that platform's distribution archive. The version pin lives alongside the `@devcontainers/cli` pin in `internal/sandbox/container/runtime.go`.

`VerifyNodeChecksum` is the regression guard the toolchain fetcher calls at the network boundary. It looks up the pin for the requested version and the build platform's `GOOS/GOARCH` and rejects a downloaded archive whose sha256 disagrees, naming the platform and both hashes, so a tampered or drifted distribution fails the build rather than installing silently. Unlike the agent-images lock, the Node toolchain has no floating fallback: a version pin with no checksums is rejected at parse time.

Regenerate the toolchain lock when bumping the pinned Node version:

```bash
task generate:tools-lock VERSION=22.14.0
```

The generator (`internal/tools/toolslock`) resolves each supported platform's archive sha256 from Node's published `SHASUMS256.txt` and writes the lockfile in canonical form. It does not verify the detached GPG signature on that file; the signature-verified fetch pipeline is a separate concern under #1525. The generated file is deterministic, so a regenerate with no version change is a no-op diff. A test asserts the committed lockfile is canonical, so a hand-edit or a stale regenerate fails CI.

## Managed Toolchain Build

A Tier 1 devcontainer that declares `features` cannot be built with raw `docker build`, so Aileron routes it through `@devcontainers/cli`, which needs a Node runtime. By default Aileron resolves both through the host's `npx` (`npx --yes @devcontainers/cli@<pinned>`), so the host must have Node installed. The managed toolchain removes that host prerequisite: Aileron provisions a verified, pinned Node and the pinned CLI itself, leaving Docker as the only host prerequisite for a Features build.

Select the managed toolchain on a Features build with the `--toolchain` flag:

```bash
aileron sandbox build --toolchain=managed
aileron sandbox check --toolchain=managed --agent=claude
```

The same selection is available through the `AILERON_SANDBOX_TOOLCHAIN` environment variable, which `aileron launch` also reads. Precedence is flag, then environment, then the default. The default is host-npx in this release; a later change flips the default to managed ([#1530](https://github.com/ALRubinger/aileron/issues/1530)).

On first managed build Aileron fetches the pinned Node distribution into a content-addressed cache, verifies it against `tools.lock.json` at the network boundary, and installs the pinned `@devcontainers/cli` into the same cache. Both steps are keyed by their pins, so subsequent builds reuse the cache without re-downloading. Only the host-npx and managed argv prefixes differ between the two paths; the `build --workspace-folder … --image-name … --build-arg …` tail is identical.

For a hermetic or offline host, point Aileron at a pre-staged Node binary and CLI entrypoint with the escape hatch, which skips provisioning entirely:

```bash
aileron sandbox build --toolchain=managed \
  --node=/opt/node/bin/node \
  --devcontainer-cli=/opt/devcontainer-cli/devcontainer.js
```

The escape hatch is also available through `AILERON_SANDBOX_NODE` and `AILERON_DEVCONTAINER_CLI`. Both paths must be supplied together and must exist on disk; a half-configured escape hatch is rejected rather than partially provisioned.

## Claude Code Feature

The Claude Code agent Feature installs the `claude` CLI onto `ghcr.io/alrubinger/aileron-sandbox-base`. It is the single source of truth that Aileron CI bakes into the prebuilt Claude image and that you compose for Tier 1. The Feature lives at `images/sandbox-features/claude/` (`devcontainer-feature.json` plus `install.sh`).

For the Tier 0 zero-build path, launch the prebuilt image directly:

```bash
aileron launch --sandbox=docker claude
```

For the Tier 1 customization path, list the Claude Feature in your `devcontainer.json` (see [Scaffold a Starter Devcontainer](/development/sandbox-composition/#scaffold-a-starter-devcontainer)), then validate and launch:

```bash
aileron sandbox build --runtime=docker
aileron sandbox check --runtime=docker --agent=claude
aileron launch --sandbox=docker claude
```

Claude Code still owns its own authentication flow. Do not bake Claude, Anthropic, cloud, or Aileron credentials into the image.

## Codex Feature

The Codex agent Feature installs the `codex` CLI onto `ghcr.io/alrubinger/aileron-sandbox-base`. The `@openai/codex` npm package ships prebuilt musl binaries, so it installs cleanly on the Alpine base. Like the Claude Feature, it is baked into the prebuilt Codex image and composable for Tier 1. The Feature lives at `images/sandbox-features/codex/` (`devcontainer-feature.json` plus `install.sh`).

For the Tier 0 zero-build path:

```bash
aileron launch --sandbox=docker codex
```

For the Tier 1 customization path, list the Codex Feature in your `devcontainer.json`, then validate and launch:

```bash
aileron sandbox build --runtime=docker
aileron sandbox check --runtime=docker --agent=codex
aileron launch --sandbox=docker codex
```

Codex owns its own authentication flow. Do not bake OpenAI, cloud, or Aileron credentials into the image.

## BYO Image Contract

A BYO image must provide:

- `/bin/sh`
- a writable `/home/agent/workspace` bind mount when launched by Docker
- the requested agent command on `PATH`

On Linux + Docker the launcher additionally routes the container through the `aileron-remap-agent-uid` entrypoint (see the proxy contract below). A BYO image that runs as a non-root user must ship that helper so the workspace bind mount is writable by the agent. See [Workspace ownership on Linux](#workspace-ownership-on-linux).

Validate a BYO image by setting `customizations.aileron.image` in `.devcontainer/devcontainer.json` and running:

```bash
aileron sandbox check --runtime=docker --build=never --agent=claude
```

## BYO Image Proxy Contract

`aileron launch --sandbox=docker` runs the HTTPS proxy by default ([ADR-0019](/adr/0019-v4-https-data-plane/)). The launcher mounts a session-scoped CA at `/etc/aileron/proxy/ca.pem`, sets standard proxy env (`HTTPS_PROXY`, `HTTP_PROXY`, `NO_PROXY`), and runs the agent through the `aileron-run-with-proxy-ca` wrapper. For the proxy to terminate TLS without breaking the agent's HTTPS clients, the in-container trust store must include that CA before the agent starts.

`aileron sandbox check --agent=<command>` validates the proxy contract for every `--runtime=docker` invocation. The check exits non-zero with an actionable message when the image is missing any of the required pieces below. The launch-time validation runs the same script.

A BYO image meets the proxy contract by providing two helpers on `PATH`:

| Helper | Purpose |
|---|---|
| `aileron-install-proxy-ca` | Installs the mounted CA into the in-container trust store. Must accept `--check` to dry-run the trust-store probe without writing anything, and must accept an optional positional CA file argument (default `${AILERON_SANDBOX_PROXY_CA_FILE:-/etc/aileron/proxy/ca.pem}`). Exits 0 on success, 2 when the CA file is missing or empty, 126 when invoked unprivileged for an install, 127 when the underlying trust-store tooling is missing. |
| `aileron-run-with-proxy-ca` | Entrypoint wrapper that installs the CA as root, then drops privileges to the `agent` user and executes the requested agent command. The launcher always starts the container through this wrapper when the proxy is in force. |
| `aileron-remap-agent-uid` | Entrypoint wrapper that, started as root, remaps the in-container `agent` user/group to the numeric uid/gid owning the mounted workspace, then execs the rest of its argv still as root. The launcher prepends it on Linux + Docker so the workspace bind mount is writable by the agent. See [Workspace ownership on Linux](#workspace-ownership-on-linux). |

The canonical implementations ship with the `ghcr.io/alrubinger/aileron-sandbox-base` image. BYO authors who derive from another base distro can write drop-in equivalents — the launcher only cares about the CLI shape, not the trust-store mechanism. Pick the mechanism that matches the base:

| Base distro | Install file at | Apply with | Notes |
|---|---|---|---|
| Debian / Ubuntu | `/usr/local/share/ca-certificates/aileron-sandbox-proxy-ca.crt` | `update-ca-certificates` | Requires the `ca-certificates` package. |
| Alpine | `/usr/local/share/ca-certificates/aileron-sandbox-proxy-ca.crt` | `update-ca-certificates` | Provided by the `ca-certificates` package. The sandbox-base image's helper already works on Alpine because Alpine's `update-ca-certificates` accepts the same input directory. |
| RHEL / Fedora / Amazon Linux | `/etc/pki/ca-trust/source/anchors/aileron-sandbox-proxy-ca.crt` | `update-ca-trust extract` | Requires the `ca-certificates` package. Write a small wrapper that mirrors the Debian helper's CLI but switches the install path and update command. |

Two operational requirements apply to every equivalent helper:

1. The CA must be installed as `root` once at container start, before the agent process runs. This is what `aileron-run-with-proxy-ca` guarantees by switching back to the `agent` user with `exec` after the install.
2. The install step must be idempotent — the same helper is invoked on every container start, and the same CA is installed every time. Existing `update-ca-certificates` / `update-ca-trust` implementations are naturally idempotent.

Validate a BYO image meets both the agent and proxy contracts with:

```bash
aileron sandbox check --runtime=docker --build=never --agent=claude
```

The check reports `support: ok` only when the agent command and both proxy helpers are present and the `--check` probe succeeds. To launch without the proxy — useful for images that cannot meet the contract during initial bring-up — pass `--sandbox-proxy=off` on `aileron launch`. `sandbox check` does not honor that opt-out; it always exercises the full contract so BYO authors see the same failures the launcher would see.

## Workspace ownership on Linux

`aileron launch --sandbox=docker` bind-mounts your current working directory into the container at `/home/agent/workspace`. The container runs as the image's non-root `agent` user. On Linux + Docker the workspace bind mount keeps the host directory's owner uid (your operator uid, e.g. `1000`), while the `agent` user has its own uid baked into the image. When those differ, the directory's `0755` permissions deny the agent write access, so the agent cannot create files in your workspace.

To fix this, the launcher routes the container through the `aileron-remap-agent-uid` entrypoint on Linux + Docker. Started as root, it reads the workspace owner's uid/gid, remaps the in-container `agent` user/group to match, then drops to the agent user before the agent runs. The remap composes with the proxy contract: the launcher chains `aileron-remap-agent-uid` ahead of `aileron-run-with-proxy-ca` so the order is remap uid, then install the CA, then drop to the agent user.

This is scoped to Linux + Docker. On macOS and Windows, Docker Desktop's file-sharing layer translates uids at the boundary, so the mismatch never surfaces and the remap is skipped.

A third operational requirement therefore applies to BYO images that run as a non-root user on Linux:

3. The `aileron-remap-agent-uid` helper must be on `PATH` and the image must let it run as root at startup before dropping to the agent user. The helper needs `usermod`/`groupmod` (the `shadow` package on Alpine, `passwd`/`shadow-utils` on Debian/RHEL) and a `stat` that supports `-c` (GNU coreutils). The canonical implementation ships with the sandbox-base image.

> This addresses a different failure than SELinux relabeling: the `:z` relabel handles SELinux MAC denials on enforcing hosts, while the uid remap handles the DAC permission mismatch that occurs on every Linux + Docker host regardless of SELinux. Both can apply on the same host.

## Current Limits

The support matrix covers image contents only. It does not add live discovery refresh.
