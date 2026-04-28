---
title: "The Action Model"
description: "Atomic actions that depend on connectors with explicit version + hash + capability subsets; ShadCN-style copy-on-install"
order: 3
---

> **Architecture:** part of the [Architecture](/pivot/architecture/) section of the Pivot strategy. See also [The Connector Model](/pivot/architecture/connector-model), [The Capability Model](/pivot/architecture/capability-model), and [Two Distribution Mechanics](/pivot/architecture/distribution-mechanics).

Actions are the composable units developers work with. Each action is a declarative manifest that describes what intent it matches, which connectors it uses, and what those connectors do during execution.

Actions are **atomic and do not depend on each other.** If a developer wants a compound operation — "ship-update + create-followup-ticket + block-calendar" — they either let the agent orchestrate the actions in sequence (the natural mode) or write a *new* action that performs all three using connectors. Action-to-action dependencies are deliberately not modeled. They open a dependency-graph problem unnecessary for the value we're after.

Actions **do depend on connectors**, with explicit version ranges and capability subsets:

An action file lives at `actions/ship-update.md` and is yours to evolve once installed:

````markdown
+++
name = "ship-update"
version = "1.0.0"
source = "hub://aileron/ship-update@1.0.0"

[[requires.connectors]]
name = "slack"
version = "1.2.0"
hash = "sha256:abc123..."
capabilities = ["chat:write", "channels:read"]

[[requires.connectors]]
name = "git"
version = "2.1.0"
hash = "sha256:def456..."
capabilities = ["read"]

[match]
intent = "tell team I shipped"

[[execute]]
id = "recent_merge"
connector = "git"
op = "read_recent_merge"

[[execute]]
id = "post"
connector = "slack"
op = "post_message"

[execute.inputs]
channel = "${args.channel}"
message = "${recent_merge.summary} → ${recent_merge.pr_url}"
+++

# Ship Update

Posts a "shipped" announcement to a Slack channel with the merged PR link.

## When it fires

Triggered when the user tells their agent things like:

- "tell team I shipped the migration"
- "post a ship update to #engineering"
- "let the team know I merged the PR"

## What it does

1. Reads the most recent merge commit from local git.
2. Extracts the PR URL from the commit body.
3. Formats a message and posts it to the specified Slack channel.
````

The TOML frontmatter is the contract Aileron executes; the Markdown body is human-facing documentation that doubles as the function description when this action is surfaced to the LLM as a tool.

Three things make this declaration the source of truth:

**1. Declared capabilities are enforced at runtime.** If `ship-update` declares `slack: chat:write` but at runtime tries to call `chat:read`, Runtime refuses. The connector might be capable of `chat:read`, but the action didn't declare it. Capability creep is blocked at the action boundary, not just the connector boundary. Defense in depth.

**2. Audit becomes precise.** A reviewer reading the action manifest knows exactly what capabilities the action uses, without reading the execution body. Reviews stay tractable.

**3. Install resolution becomes deterministic.** When a developer runs `aileron action add ship-update`, Aileron resolves the dependency graph in a single bundled consent moment, not a series of surprise prompts at first invocation:

```
$ aileron action add ship-update
→ Fetching action 'ship-update' from Aileron Hub...
✓ Action file written to actions/ship-update.md
  Declares connectors: slack@1.2.0, git@2.1.0

This action requires:
  ✓ slack 1.2.0        (installed)
    Capabilities:
      ✓ chat:write     (already granted)
      ✗ channels:read  (NOT granted to slack/work)
  ✗ git 2.1.0          (not installed)

Resolving:
  → Re-bind slack connector to grant 'channels:read'?
  → Install missing connector 'git' (hash verified)?

[Continue]  [Customize…]  [Cancel]
```

There is no separate lock file. The action file itself is the contract — version pins, content hashes, declared capabilities, and execution steps, all in one place, owned by the developer in their git repo. Runtime verifies these constraints on every action invocation before execution begins. If any check fails, the action fails fast with a precise error rather than crashing mid-execution.

**Action files are owned, not installed.** When `aileron action add ship-update` runs, the action file is *copied* into the developer's project and tracked by git. From that moment forward, the developer owns the file: customize it to fit project conventions, modify the connector versions, refine the templates, evolve it as the project evolves. The Aileron Hub is a curated catalog of starting-point templates; installation is a copy operation, not a runtime dependency. This follows the ShadCN distribution model — the right pattern for declarative source code that wants to live alongside the developer's other source files.
