---
title: "Actions"
description: "Actions are the unit of agent capability — single declarative files that describe what the agent can do on your behalf and exactly how it will do it."
order: 2
---

An **action** is a single file that describes one thing the agent can do for you. Posting a ship-update to Slack, filing a bug in Linear, sending an email reply, creating a calendar event — each of these is an action.

Actions are the unit of agent capability in Aileron. Installing an action means giving the agent the ability to do that one thing, deterministically, on your behalf.

## What an action is

An action file lives at `~/.aileron/actions/<name>.md`. The file has two parts:

- **TOML frontmatter** — the contract Aileron executes. It declares which connectors the action uses, which capability subset it exercises, what intent it matches, and what execution steps it runs.
- **Markdown body** — documentation that doubles as the LLM's understanding of when to invoke the action.

A single file is the entire action. There's no compiled artifact, no bundled binary, no separate manifest. Reading the file tells you exactly what will happen if the agent invokes it.

```markdown
+++
name = "ship-update"
version = "1.0.0"
source = "hub://aileron/ship-update@1.0.0"

[[requires.connectors]]
name = "github://aileron/slack"
version = "1.2.0"
hash = "sha256:abc123..."
capabilities = ["chat:write", "channels:read"]

[[requires.connectors]]
name = "github://aileron/git"
version = "2.1.0"
hash = "sha256:def456..."
capabilities = ["read"]

[match]
intent = "tell team I shipped"
+++

# Ship Update

Posts a "shipped" announcement to a Slack channel with the merged PR link.

## When it fires

- "tell team I shipped the migration"
- "post a ship update to #engineering"
- "let the team know I merged the PR"
```

Actions are **atomic** — each does one thing. Compound flows (post the update *and* file the follow-up ticket) emerge naturally from the agent stringing several actions together in conversation, not from a workflow language inside the action file.

## How you get actions

You install actions individually. The Hub is a curated catalog of starting-point templates; `github://` and `gitlab://` are open sources where anyone can publish.

```sh
$ aileron action add hub://aileron/ship-update@1.0.0
✓ Action file written to ~/.aileron/actions/ship-update.md

$ aileron action add github://acme/templates/actions/file-bug@2.1.0
✓ Action file written to ~/.aileron/actions/file-bug.md
```

This follows the **ShadCN distribution model**: install copies the template into your home directory, and from that moment **you own the file**. You can edit it — change the trigger phrases, swap a connector version, modify the prose, retitle it — without anyone's permission. The Hub's version becomes a starting point, not a runtime dependency.

This matters because action files are an authoring surface, not a black box. If the hosted version of `ship-update` posts in a tone that doesn't match your team's voice, you change it.

## How the agent invokes actions

Aileron sits at the LLM endpoint your agent points to. For every chat completion request, Aileron reads your installed actions and adds them to the agent's tool catalog (see [Deterministic Agentic Execution](/concepts/deterministic-agentic-execution/) for how this composes with tool calling and MCP). The LLM picks among the merged set; when it picks an Aileron-installed action, Aileron intercepts and runs it.

The agent itself doesn't know which tools were Aileron's. From its perspective, it just called a function and got a result. The execution layer is invisible to the agent — and that's structurally important: the agent has no way to bypass Aileron, no flag to flip, no SDK to swap. The runtime guarantees are enforced at the seam.

The Markdown body of the action file *is* the description the LLM sees. Authors write one piece of prose and four readers consume it: the developer reading the file, the Hub rendering it for browsing, the LLM choosing whether to invoke it, and the user reading the audit log. There's no separate "LLM hint" field to keep in sync.

## What an action declares about its capabilities

Every action declares the *subset* of each connector's capabilities it actually uses. The Slack connector might be capable of `chat:write`, `chat:read`, `users:read`, `files:upload`. If `ship-update` only needs `chat:write` and `channels:read`, that's all the action declares.

This isn't documentation — it's enforced at runtime. If the action's execution somehow tries to invoke `chat:read`, the runtime denies the call at the *action boundary*, even though the connector itself permits it. Two checks happen in series: the connector cannot exceed its manifest grant; the action cannot exceed its declared subset.

The benefit shows up two ways:

- **Audit is precise.** You can read the `[[requires.connectors]]` blocks at the top of any action file and know exactly what surface area the action touches. No need to read the execution body.
- **Capability creep is visible.** If a future template update adds a new capability, it shows up as a TOML diff. Adding capability to an action you've installed is always an explicit, reviewable change.

## What actions don't do

A few things actions deliberately are *not*:

- **Actions are not workflows.** There's no orchestration language for chaining actions. If you want a compound flow, you let the agent compose actions in conversation, or you write a new atomic action that exercises multiple connectors directly. The dependency graph stays at depth 1: actions use connectors, connectors use primitive capabilities.
- **Actions are not background services.** Each invocation is a tool call from the agent. Scheduled or async actions (where Aileron acts on your behalf while you're offline) are post-MVP and depend on the hosted backend.
- **Actions are not project-coordination state.** Actions live in your home directory, not in any project's git repo. Your installed action set is personal — like your shell aliases or browser extensions. Your teammate may have a completely different set, and that's fine.

## Updates are visible, never silent

When an action template publishes a new version, your local file doesn't change automatically. `aileron action update <name>` fetches the new template and produces a diff against your local copy. You accept, reject, or merge manually.

There's no "follow latest" mode. The action file you have is the contract Aileron will execute, until you explicitly change it.
