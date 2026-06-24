---
title: "Installing a Skill"
description: "Install a SKILL.md-format skill from a local path or git URL into the canonical store, see its action requirements resolve, and have it projected into every agent you launch."
---

This guide is for users adding skills to their local Aileron install. It covers `aileron skill install` for a local path or a git URL, `aileron skill list` to see what is installed, how a skill's `requires:` action references are resolved at install time, and how an installed skill becomes visible to every agent you launch through Aileron. By the end you will have one or more skills installed under `~/.aileron/skills/` and understand the install-once, launch-anywhere projection.

A skill is a [SKILL.md-format](https://agentskills.io) document: YAML frontmatter plus a Markdown body. The surrounding skill keys (`name`, `description`, `license`, and so on) follow the agentskills.io format. Aileron adds one optional, namespaced `aileron` frontmatter block that declares the skill's action requirements and trust contract. A skill with no `aileron` block is an instruction-only skill and installs cleanly with nothing to resolve.

## What gets installed

`aileron skill install <source>` fetches the skill, parses its SKILL.md, resolves any declared action requirements against your running daemon, and writes the skill into `~/.aileron/skills/<name>/`. The store is the single host-side writer. An installed skill is read-only after install; editing is an explicit future operation.

The install never touches credential values. A skill's trust contract declares the credential kind and placement only. The resolver matches action references against the actions you already have installed by name; the credential itself is injected at the network boundary at run time, never handed to skill code.

## Installing from a local path

```sh
aileron skill install ./my-skills/weekly-digest
```

The source may point at a directory that contains a `SKILL.md`, or directly at a `SKILL.md` file. The CLI parses it, resolves its requirements, copies the whole skill folder (SKILL.md plus any reference files) into the store, and prints where it landed:

```
Installed skill "weekly-metrics-digest" to /Users/you/.aileron/skills/weekly-metrics-digest
```

## Installing from a git URL

```sh
aileron skill install https://github.com/acme/weekly-digest.git
```

Aileron shallow-clones the repository, reads the `SKILL.md` at its root, and installs it the same way as a local path.

A git-URL install requires the `git` binary on your `PATH`. `aileron skill install` runs host-side, so it is unrestricted by the sandbox egress boundary. If `git` is missing the install fails with the underlying clone error.

> Installing by an [agentskills.io](https://agentskills.io) registry slug is not wired yet. Use a local path or a git URL for now.

## Action requirements and graceful degrade

When a skill declares `requires:` action references (for example `aileron:metrics.query_series`), the installer asks your running daemon which actions are installed and checks each reference. A reference is satisfied when an installed action matches both the connector and the action name.

Install is degrade-not-block. If a reference does not resolve, the install still succeeds and prints a warning naming the unsatisfied references:

```
warning: skill "weekly-metrics-digest" installed, but 1 action requirement(s) are not satisfiable here:
  - aileron:metrics.query_series
  Install the connectors/actions these refs name, or launch where they are available.
```

An instruction-only skill (no `aileron` block) skips requirement resolution entirely, so it installs cleanly and silently even when the daemon is not running.

## Listing installed skills

```sh
aileron skill list
```

prints the installed skill names, one per line. When the store is empty it tells you so and points at `aileron skill install`.

## Launch-anywhere projection

You install a skill once. At `aileron launch <agent>` in the sandbox, the launcher bind-mounts the canonical skill store read-only at the agent's skills path (for example `/home/agent/.claude/skills` for Claude Code), so the skill is visible to the agent without copying it per agent. The agent's MCP configuration already points at the Aileron MCP server, so a skill's `requires:` action references resolve through Aileron and stay gated by your approval policy.

Per-agent path differences are absorbed by the launcher, not by rewriting the skill. The mount is read-only: editing an installed skill is an explicit operation, not a side effect of launch.
