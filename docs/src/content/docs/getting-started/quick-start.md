---
title: "Quick Start"
description: "Launch your first Aileron session"
---

## Launch your agent

```sh
aileron launch claude
```

Aileron spawns the agent as a child process with a policy-enforced shell. Every command the agent runs flows through `aileron-sh` and the policy engine before reaching the real shell. Aileron handles agent-specific quirks (shell validation, command wrapping) so the policy rules stay clean.

This works with zero configuration. Built-in defaults allow common toolchain commands (Go, Node, Python, Rust, Ruby, Elixir, Java) and deny dangerous operations (recursive delete, push to main). Every command flows through `aileron-sh` and the policy engine. Aileron is the single approval layer, replacing the agent's native tool approval.

## Policy evaluates every command

Safe commands (tests, builds, reads) auto-approve silently. Dangerous commands (force push, recursive delete) are hard-denied. Ambiguous commands (commit, push, deploy) prompt you once with context:

```
  ⏸ aileron: agent wants to run `git push origin feature/auth`
    matched rule: ask (git push)
    [y] allow  [n] deny  [a] allow always  [s] show details
```

## Scaffold a project policy

Optionally, create a project-specific policy:

```sh
aileron init
```

This creates a minimal `aileron.yaml` with project-specific deny rules and env scrubbing. Language toolchain and OS rules are built in, so you don't need to list them.

## See your config at a glance

```sh
aileron status              # show everything
aileron status policy       # merged policy: defaults + project + user
aileron status env          # scrubbed and passthrough vars
aileron status notifications # Slack/Discord channels and token status
aileron status vault        # stored secrets (names only)
```
