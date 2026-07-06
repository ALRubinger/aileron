---
title: "Installing a Skill"
description: "Install a SKILL.md-format skill from a local path, a git URL, or a published OCI reference into the canonical store, see its action requirements resolve, and have it projected into every agent you launch."
---

This guide is for users adding skills to their local Aileron install. It covers `aileron skill install` for a local path, a git URL, or an OCI reference to a published Flight Plan, `aileron skill list` to see what is installed, how a skill's `requires:` action references are resolved at install time, and how an installed skill becomes visible to every agent you launch through Aileron. By the end you will have one or more skills installed under `~/.aileron/skills/` and understand the install-once, launch-anywhere projection.

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

## Installing from an OCI reference

```sh
aileron skill install ghcr.io/acme/weekly-digest:v1a2b3c4d5e6f
```

An OCI reference installs a Flight Plan that someone published with `aileron skill publish`. The reference has a registry host, a repository path, and a version tag. Aileron pulls the signed artifact (the frozen SKILL.md, its lockfile, the signature, and the author public key) from the registry over your existing Docker credentials.

The install verifies the artifact before anything lands on disk. It checks the author signature and the content hash, then the publisher-trust gate against your keyring, the same checks `aileron skill launch` runs. A tampered artifact fails the signature check. A plan from a publisher you have not trusted fails the trust gate. Either failure aborts the install and writes nothing to the store. A plan that declares no publisher installs on a valid signature alone.

An OCI install writes a frozen version, so it appears under `~/.aileron/skills/<name>/versions/<id>/` and prints the version it installed:

```
Installed frozen version v1a2b3c4d5e6f of skill "weekly-metrics-digest" to /Users/you/.aileron/skills/weekly-metrics-digest/versions/v1a2b3c4d5e6f
Run `aileron skill bind "weekly-metrics-digest"` to supply this plan's credentials
```

The version id is the frozen content-hash slug, so a re-install of the same published plan is a no-op that reuses the same directory. Install does not pull the plan's container image. The image pin recorded in the lockfile is resolved at `aileron skill launch`, not at install. The install prints a one-line pointer to `aileron skill bind` so you know how to onboard the plan's credentials next.

> Installing by an [agentskills.io](https://agentskills.io) registry slug is not supported. agentskills.io is a format spec, not a registry, so a slug has no canonical location to resolve against. Install from a local path, a git URL, or an OCI reference instead.

## Onboarding credentials

A published Flight Plan declares the credentials it needs in its frozen trust contracts, but it never carries the secrets. After you install a frozen plan, run `aileron skill bind <name>` to supply them:

```sh
aileron skill bind weekly-metrics-digest
```

The verb derives the plan's credential requirements from its verified frozen trust contracts, then fills only the gaps. For each requirement it checks two places: your vault for the secret, and `~/.aileron/binding-descriptors.yaml` for the non-secret binding. A requirement already present in both is reported as satisfied and left untouched. A missing requirement prompts you for its secret with a hidden prompt that writes straight to the vault, prompts for any non-secret parameter such as an AWS access key ID, and writes the binding descriptor entry.

The write is programmatic, so you do not hand-edit any file. A single run stores the vault secret, writes the descriptor entry, prints a per-requirement summary, and reminds you to restart the daemon. The daemon reads binding descriptors once at startup, so a new binding takes effect only after a restart.

The same verb onboards a second operator on their own machine. They install the same frozen plan, run `aileron skill bind <name>`, and supply their own identity and key. The plan is shared once; each operator binds their own credentials against it. A requirement whose host carries an unresolved input template is surfaced as an advisory rather than written automatically, since the concrete host is not known until launch resolves the input.

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
