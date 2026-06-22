---
title: "v4 Manual Acceptance Runbook"
description: "The hands-on steps a person runs to confirm Aileron's containerized runtime works end to end on macOS and Windows: launch an agent in the sandbox and watch it use a real Aileron tool."
order: 11
---

This is the checklist a person runs by hand to confirm v4 works end to end. v4 ships Aileron as a containerized runtime: the AI agent runs inside a Docker sandbox, and Aileron mediates every real-world action it takes (approvals, credentials, audit) at the edge of that container.

We prove this automatically on Linux in CI, so you never run the Linux column by hand. GitHub's macOS and Windows runners can't run Linux Docker containers, so a person has to confirm those two by hand. That's what this page is for.

The full "is v4 done?" picture lives in [issue #747](https://github.com/ALRubinger/aileron/issues/747); you record the results of this runbook in [issue #962](https://github.com/ALRubinger/aileron/issues/962).

## What you're checking, in one sentence

For each agent on macOS and on Windows: **the agent launches inside the Docker sandbox and can see the `aileron` tool, including `draft_email`.**

If it sees the tool, the whole runtime is wired: the container started, Aileron is reachable from inside it, and the agent's tool catalog is coming from Aileron. That single check is the bar for a cell.

Once per operating system, on at least one agent, also run the optional full round-trip ([Step 6](#step-6-optional-prove-a-real-action-end-to-end)): ask the agent to draft an email, approve it, and confirm a real Gmail draft appears. That proves the action actually executes through to the real world, with a human approval in the middle.

## Before you start

You need:

- **Docker** installed and running. Docker Desktop on both macOS and Windows runs the same Linux sandbox image (Windows uses its WSL2 backend), so the steps are identical on both.
- A build of the `aileron` CLI on your `PATH` (see [Step 1](#step-1-build-and-install-aileron)).
- A Google account, for the action the smoke uses.
- `jq` on your `PATH`, only if you run the optional round-trip in Step 6.

## Step 1: Build and install Aileron

The sandbox launch needs two host binaries on `PATH`: the `aileron` CLI and its `aileron-mcp` sibling.

```bash
task build:cli && task build:mcp
export PATH="$PWD/build:$PATH"
```

Confirm both resolve:

```bash
aileron version && aileron-mcp --version
```

## Step 2: Install the Gmail actions

The smoke looks for the `draft_email` tool. It comes from the Google connector's action suite, so install the suite. This one command fetches the connector, installs its actions, and walks you through Google sign-in:

```bash
aileron action add-suite github://ALRubinger/aileron-connector-google/suite.toml@latest
```

On a fresh machine this prompts you to create a vault passphrase, trust the publisher, and sign in to Google once. The connector ships its own Google OAuth client, so there's no Google Cloud Console setup on your end. After it finishes, `aileron action list` shows `draft-email` among the installed actions.

## Step 3: Launch an agent in the sandbox

`aileron launch` runs the agent inside a Docker sandbox by default. Pick the agent you're checking and launch it:

```bash
AGENT=claude   # one of: claude | pi | goose | opencode | codex
aileron launch "$AGENT"
```

Aileron prepares and starts the container, wires the agent up to Aileron, and starts the agent. The first launch may prompt you to sign in to the agent (for example, Claude's Pro/Max login). That sign-in happens on your host terminal and browser, never inside the container, and it's reused on later launches.

If a launch fails before the agent starts, run `aileron sandbox check --agent="$AGENT"` first; it validates the image can run the agent and explains what's wrong.

## Step 4: Confirm the agent sees the `aileron` tool

This is the check. How you list tools depends on the agent:

| Agent | How to check |
|---|---|
| Claude | Type `/mcp`. Expect one server named `aileron`. Look for `draft_email`. |
| Codex | Type `/mcp`. Same as Claude. |
| Pi | Ask the agent to list its available tools, or just make a draft request (Step 6) and confirm it uses the Aileron tool. |
| Goose | Same as Pi. |
| OpenCode | Same as Pi. |

If the agent lists `aileron` with `draft_email`, **this cell passes.** Exit the agent.

Pay special attention to **Codex**: it's the one agent wired up differently from the others, so it's the most likely to surprise you. Check it deliberately on each operating system.

## Step 5: Record the result

Record each agent you checked, per operating system, in [issue #962](https://github.com/ALRubinger/aileron/issues/962) (edit the issue body, don't add a comment). Note your OS, architecture, Docker version, the `aileron version` output, and the agent's version. If anything didn't match these steps, write down what happened.

The matrix you're filling:

| Agent ↓ / OS → | macOS (by hand) | Linux (automatic, CI) | Windows (by hand) |
|---|---|---|---|
| Claude | ☐ | ✅ CI | ☐ |
| Pi | ☐ | ✅ CI | ☐ |
| Goose | ☐ | ✅ CI | ☐ |
| OpenCode | ☐ | ✅ CI | ☐ |
| Codex | ☐ | ✅ CI | ☐ |

The Linux column is green whenever CI is green; you never fill it by hand.

## Step 6 (optional): prove a real action end to end

Do this once per operating system, on at least one agent. It proves an action runs all the way through to the real world with a human approval in the middle.

1. With the agent running, ask it to draft an email:

   > Draft an email to alice@example.com saying I'm running late

2. The agent calls `draft_email`. Because drafting is a real-world write, Aileron pauses and surfaces an approval. Approve it, either in the webapp link the agent shows you, or in another terminal:

   ```bash
   aileron approval approve <id>
   ```

3. Confirm a draft actually landed in your Gmail Drafts folder.

4. (Optional) Confirm Aileron recorded the trail. List the recent audit entries and check you see the approval and the execution for the draft:

   ```bash
   aileron audit list --limit 10
   ```

   You should see the request being approved and then the action executing successfully.

If the draft appears in Gmail and the audit log shows the approval and execution, the round-trip passes for that operating system.

## Current status

**v4 is not yet fully delivered.** Everything in the v4 bar is built and the Linux column is automated and green. What remains is the hands-on confirmation on macOS and Windows (this runbook) plus the demo recording, both tracked under [#747](https://github.com/ALRubinger/aileron/issues/747):

| Remaining item | Status |
|---|---|
| macOS + Windows manual smoke, per agent ([#962](https://github.com/ALRubinger/aileron/issues/962)) | In progress. Claude on macOS has a full round-trip done; the rest are pending. |
| Demo script + recorded walkthrough ([#852](https://github.com/ALRubinger/aileron/issues/852)) | Pending. |

## Related pages

- [Sandbox MCP — Manual Verification Walkthrough](/development/sandbox-mcp-walkthrough/) — the detailed round-trip, per-agent registration detail, and troubleshooting.
- [Sandbox Agent Auth](/development/sandbox-agent-auth/) — signing an agent in ahead of time.
- [Sandbox Composition](/development/sandbox-composition/) — how `aileron sandbox` builds and checks the image.
