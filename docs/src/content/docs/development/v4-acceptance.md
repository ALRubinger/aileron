---
title: "v4 Manual Acceptance Runbook"
description: "The hands-on steps an operator runs to perform the manual half of v4 acceptance: build Aileron, launch an agent in a Docker sandbox, and verify the agent sees the aileron tool surface."
order: 11
---

This page is the operator runbook for the manual half of v4 acceptance. It is the concrete, copy-pasteable sequence you run by hand to fill the macOS and Windows cells of the v4 matrix in [issue #747](https://github.com/ALRubinger/aileron/issues/747). The Linux column is automated in CI, so you never hand-run it (it is green when CI is green).

For the at-a-glance "is v4 done?" verdict and how each item maps to the #747 bar, jump to [Current status](#current-status) at the bottom. The rest of this page is the procedure.

## The acceptance bar, in one sentence

For each agent on each operating system, the bar is: **the agent launches inside a Docker container and lists `aileron` with its `draft_email` tool**. That is the whole bar for a macOS or Windows cell. Every step below exists to get you to that check.

A full action round-trip (through to a real Gmail draft, with the HITL approval and audit chain) is the optional gold-standard check on at least one agent per OS. It is covered in [Step 6](#step-6-optional-full-action-round-trip).

## Before you start

You need:

- **Docker** installed and running. Docker Desktop on macOS and Windows both run the same Linux `sandbox-base` image (Windows via the WSL2 backend).
- The **Go toolchain** and **[Task](https://taskfile.dev)** to build from source.
- A clone of this repo, and `jq` on `PATH` (used by the optional audit check in Step 6).
- For the optional full round-trip only: a Google OAuth client configured via `aileron binding setup gmail`. The light smoke does not need it.

## Step 1: Build the binaries

The sandbox launch needs two host binaries on `PATH`: the `aileron` CLI and its `aileron-mcp` sibling.

```bash
task build:cli && task build:mcp
export PATH="$PWD/build:$PATH"
```

`build:cli` produces `build/aileron`. `build:mcp` produces `build/aileron-mcp`, and on a macOS or Windows host it also cross-builds a `build/aileron-mcp-linux-<arch>` sibling, because the launcher bind-mounts a Linux `aileron-mcp` into the Linux container. Confirm both resolve:

```bash
aileron --version && aileron-mcp --version
```

## Step 2: Install the action the smoke looks for

The smoke checks that the agent sees `draft_email`. That tool comes from the Google connector's draft-email action, so install both to put it in the catalog:

```bash
aileron connector install github://ALRubinger/aileron-connector-google
aileron action install github://ALRubinger/aileron-connector-google/actions/draft-email
```

Without an installed action, the `aileron` MCP server registers but exposes no tools, and the smoke has nothing to look for.

## Step 3: Check the container image before launching

Validate that the sandbox image can run your chosen agent. This catches an image or arch problem before a daemon-backed launch starts:

```bash
AGENT=claude   # one of: claude | pi | goose | opencode | codex
aileron sandbox check --runtime=docker --agent="$AGENT"
```

Expect `support: ok`, along with the selected tier, runtime, image, and command. A development build resolves the floating `edge` base image tag, and launch pulls it automatically. No separate image build is needed for the smoke.

The working directory determines the resolved tier. A `.devcontainer/` in the directory you run from selects the devcontainer tier, which builds the project image from that authored config and can ignore `--agent`. If the project image was built without the requested agent's CLI, validate fails and now names the tier, the discovered `.devcontainer/devcontainer.json`, and the published per-agent image you could use instead. To pick the build-free published image, run from a directory without a `.devcontainer/`, or add the agent's Feature to the devcontainer.

## Step 4: Launch the agent inside the container

```bash
aileron launch --sandbox=docker "$AGENT"
```

The launcher resolves and pulls the base image, bind-mounts `aileron-mcp` into the container, rewrites `AILERON_URL` to `host.docker.internal:<port>`, registers `aileron-mcp` with the agent (the mechanism differs per agent, see the table in [Step 5](#step-5-verify-the-agent-sees-aileron--draft_email)), validates the container can run `aileron-mcp`, and starts the agent. If the agent has no vaulted credential, it runs its normal in-container login on first launch; see [Sandbox Agent Auth](/development/sandbox-agent-auth/) for seeding credentials ahead of time.

### Cold first-launch acceptance (host-side acquirer)

Run this variant on each OS with a host-login-capable agent (Claude or Codex) and an **empty vault** for that agent. Delete any existing entry first with `aileron vault delete agents/$AGENT/oauth`. Launch as above. The launcher detects the vault miss and runs the host-side acquirer before the container starts.

- Claude opens its consent page in the host browser and prompts on the host terminal for the code the page renders. Paste the code at the host prompt.
- Codex prints a verification URL and a one-time user code on the host terminal, opens the verification page in the host browser, and polls. Enter the code in the browser.

The acceptance pass: the prompt and the browser open on the **host**, never inside the container TTY, and once the host login completes the container starts silent with no in-container login wizard. A subsequent launch also starts silent, proving the acquired credential was seeded to the vault.

This is the manual Windows acceptance step for the no-container-TTY guarantee. The `cmd /c start "" <url>` browser invocation is unit-tested for argv shape on every platform, but a real Windows run is the only way to confirm the browser actually opens on the host and the paste lands on the host terminal rather than the container TTY. Record a pass or fail for this cell per OS. The Linux runner additionally exercises the silent-render launcher path in automated tests.

## Step 5: Verify the agent sees `aileron` + `draft_email`

This is the acceptance check. How you list tools depends on the agent:

| Agent | How to verify |
|---|---|
| Claude | Type `/mcp`. Expect one server named `aileron`. Look for `draft_email`. |
| Codex | Type `/mcp`. Same expectation as Claude. |
| Pi | Ask the agent to list its available tools, or make a draft request and confirm it invokes the Aileron tool. |
| Goose | Same as Pi. |
| OpenCode | Same as Pi. |

The runtime-agnostic proof, valid for every agent, is in the session log: `.aileron/session.log` under the launch directory records `aileron-mcp`'s discovery call against the daemon. If the agent lists `aileron` with `draft_email` (or the session log shows the successful discovery call), **that cell passes**. Exit the agent.

Codex is the highest-risk cell on any OS, because it is the only agent whose MCP registration is a bind-mounted `config.toml` rather than a CLI flag or workspace file. Validate it deliberately.

## Step 6 (optional): full action round-trip

For the gold-standard check on at least one agent per OS, run an action end to end: ask the agent to draft an email, approve the HITL request, and confirm the `approval.requested → approval.approved → execution.started → execution.succeeded` audit chain plus the real Gmail draft. The full sequence, the exact prompt, and the `jq` audit assertion are in the [Sandbox MCP Manual Verification Walkthrough](/development/sandbox-mcp-walkthrough/#run). That page also has the [troubleshooting](/development/sandbox-mcp-walkthrough/#troubleshooting) for the common failures (tools missing from a cross-arch host, `host.docker.internal` not resolving on Linux, baked-image version skew).

## Step 7: Record the result

Record each agent × OS cell you ran in [issue #962](https://github.com/ALRubinger/aileron/issues/962), in the issue body rather than a comment, so the matrix always reflects current state. Note the environment for each cell: OS, arch, Docker version, Aileron CLI commit, agent version, and any deviation. If you also ran the optional round-trip, note that the audit assertion printed `PASS` and the draft landed in Gmail.

### The matrix you are filling

The v4 bar requires every supported agent on Docker across all three operating systems. You hand-run only the macOS and Windows columns:

| Agent ↓ / Runtime → | macOS Docker (manual smoke) | Linux Docker (CI) | Windows Docker (manual smoke) |
|---|---|---|---|
| Claude | ☐ | automated | ☐ |
| Pi | ☐ | automated | ☐ |
| Goose | ☐ | automated | ☐ |
| OpenCode | ☐ | automated | ☐ |
| Codex | ☐ | automated | ☐ |

The Linux cells are covered by `TestSandboxMCPRegistration_Matrix` (unit suite) and `TestSandboxMCP` (integration job, build tag `integration_sandbox`), merged in PR [#1064](https://github.com/ALRubinger/aileron/pull/1064). They are green when CI is green.

## Current status

**v4 is NOT yet delivered.** Everything in the v4 bar is shipped except two gating items in the "Verify and prove" section of [#747](https://github.com/ALRubinger/aileron/issues/747):

| Gating item | Status | Notes |
|---|---|---|
| Integration test variants — in-container, never-approved, concurrency ([#960](https://github.com/ALRubinger/aileron/issues/960)) | ✅ | Merged via PR [#1037](https://github.com/ALRubinger/aileron/pull/1037). |
| Manual E2E ([#962](https://github.com/ALRubinger/aileron/issues/962)) | ☐ | Linux automated and green for all five agents. macOS and Windows light per-agent smoke pending (this runbook). Claude × macOS full round-trip done 2026-06-15. |
| Demo script + recorded walkthrough ([#852](https://github.com/ALRubinger/aileron/issues/852)) | ☐ | Pending. |

Not gating v4 closure: the agent-auth v4.x deferral ([#1027](https://github.com/ALRubinger/aileron/issues/1027), closed; its container integration test [#1025](https://github.com/ALRubinger/aileron/issues/1025) landed, and [#987](https://github.com/ALRubinger/aileron/issues/987) moved to v5 [#1065](https://github.com/ALRubinger/aileron/issues/1065)) and platform packaging ([#1094](https://github.com/ALRubinger/aileron/issues/1094)).

## Sources

- [Issue #747](https://github.com/ALRubinger/aileron/issues/747) — the v4 milestone, its "Verify and prove" bar, and the 2026-06-15 scope note.
- [Issue #962](https://github.com/ALRubinger/aileron/issues/962) — where you record the macOS and Windows manual-smoke results.
- [Sandbox MCP Manual Verification Walkthrough](/development/sandbox-mcp-walkthrough/) — the optional full round-trip steps, the per-agent registration detail, and troubleshooting.
- [Sandbox Agent Auth](/development/sandbox-agent-auth/) — seeding vault-backed agent credentials before launch.
- [Sandbox Composition](/development/sandbox-composition/) — `aileron sandbox` plan, build, and check; how the base image tag resolves.
