---
title: "Policy Configuration"
description: "Configure policy rules for your project"
---

## Three-layer merge

Policy is evaluated in three layers, with later layers winning for the same command pattern:

1. **Built-in defaults** - language toolchain rules (Go, Node, Python, Rust, Ruby, Elixir, Java) and OS-specific protections (macOS Keychain, Linux `/etc`). Compiled into the binary.
2. **User settings** - `~/.aileron/settings.yaml`. Personal preferences that apply across all projects.
3. **Project policy** - `aileron.yaml` in your repo. Project-specific rules, reviewable in PRs, shared with the team.

More specific patterns win regardless of which layer they come from. Your project file only needs what's specific to this repo.

## The three buckets

Every command is evaluated against rules in three categories:

- **allow** - auto-approve silently. The agent doesn't pause.
- **deny** - hard block. The command is rejected.
- **ask** - prompt the developer once with context.

## Learning from use

When prompted, press `[a] allow always` to teach the policy. First session: 8 prompts. Allow-always on 5 patterns. Second session: 3 prompts. At the end of the session, Aileron offers to persist learned patterns to `aileron.yaml`.

## Example aileron.yaml

```yaml
policy:
  allow:
    - "go test *"
    - "npm run *"
  deny:
    - "rm -rf /"
    - "git push --force origin main"
  ask:
    - "git push *"
    - "docker *"

env:
  scrub:
    - "AWS_*"
    - "GITHUB_TOKEN"
```

## Dry-run evaluation

Test how a command would be evaluated without running it:

```sh
aileron policy test "git push origin main"
```

For the full design rationale, see [ADR-0014: aileron.yaml Policy Schema](/archived-adr/0014-aileron-yaml-policy-schema), [ADR-0015: Built-in Policy Defaults](/archived-adr/0015-built-in-policy-defaults), and [ADR-0016: Layer Overrides & Specificity](/archived-adr/0016-layer-overrides-and-specificity).
