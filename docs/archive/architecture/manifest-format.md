---
title: "Manifest Format"
description: "Markdown body + TOML frontmatter for actions; pure TOML for connectors and project config; JSON for IPC"
order: 12
---

> **Architecture:** part of the [Architecture](/architecture/) section of the Pivot strategy. See also [The Connector Model](/architecture/connector-model), [The Action Model](/architecture/action-model), and [How Intent Matches to Actions](/architecture/intent-matching).

Aileron commits to specific format choices for each kind of artifact. The choices reflect a security-and-clarity preference for declarative source code that drives execution.

| Artifact | Format |
|---|---|
| Action files | Markdown body + TOML frontmatter (`+++` delimited) |
| Connector manifests | Pure TOML |
| Project config | Pure TOML |
| Runtime IPC and internal state | JSON |

**Why TOML over YAML for the structured parts.** YAML's whitespace sensitivity, type coercion (the "Norway problem"), and parser-implementation differences are real footguns for content that drives execution. A misparsed action file isn't a broken build — it's the wrong action running with the wrong arguments, against real systems, with real credentials. TOML's strict, unambiguous syntax eliminates these footguns. Parser attack surface is also smaller; the deserialization-to-arbitrary-objects vulnerability class that has produced repeated YAML CVEs simply doesn't exist in TOML. For security-critical declarative content, TOML's strictness is a structural advantage, not just a stylistic preference.

**Why Markdown for action files.** Action files have three audiences simultaneously: the runtime needs the structured contract, developers reading the project need documentation, and the LLM that surfaces the action as a tool needs a function description. Markdown with structured frontmatter is the established pattern for handling this — used by Anthropic Skills, AGENTS.md, Hugo, Jekyll, GitHub issues, and the broader "structured frontmatter + prose" convention. The Hub renders the Markdown for browsing; developers read the prose; the runtime parses the frontmatter; the LLM consumes the description from the body. One file, four readers, no impedance mismatch.

**The body doubles as the LLM-facing description.** When Aileron augments the agent's tool catalog with installed actions, each function's `description` is drawn from the action file's Markdown body — typically the first paragraph or a designated section. The documentation the author writes for humans IS the description the LLM reads. One source of truth, no separate "LLM hint" field to keep in sync with the prose.

**On YAML preference among action authors.** Some authors will prefer YAML for the frontmatter — particularly those coming from CI config or Anthropic Skills tooling. We're shipping TOML and watching for adoption friction. Format choice is reversible (clean conversion in either direction) and we can add YAML frontmatter support later if community feedback shows TOML is a real barrier. Starting with the safer format is the right default for security-critical content.
