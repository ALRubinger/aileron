# ADR-0014: aileron.yaml Policy Schema for Launch Sessions

**Status:** Accepted
**Date:** 2026-04-08
**Relates to:** [ADR-0013](0013-local-policy-enforced-shell.md) (Local Policy-Enforced Shell)

## Context

ADR-0013 established that `aileron launch` enforces policy through a shell shim (`aileron-sh`) that intercepts every command an agent runs. The shim needs a policy definition to evaluate commands against. This ADR defines the schema for `aileron.yaml` — the policy-as-code file that lives in the repo, is reviewable in PRs, and is shared with the team.

The schema must be:
- **Simple to author.** A developer should be able to write a working policy in under a minute. Short-form string rules for common cases, long-form maps for advanced matching.
- **Composable.** OS and language profiles provide sensible defaults. The user's `aileron.yaml` layers on top. Rules from all layers combine predictably.
- **Translatable to the existing engine.** The core `RuleEngine` already evaluates conditions against flat field maps. The schema must map cleanly to `PolicyRule` and `PolicyCondition` types without requiring engine changes beyond glob matching.

## Decision

### Top-level structure

```yaml
version: 1

profiles:
  - "lang/go"
  - "./team-policy.yaml"

default: ask          # allow | deny | ask — when no rule matches

settings:
  ask_mode: terminal  # terminal | ui
  audit_log: .aileron/audit.jsonl
  timeout: 30         # seconds before auto-deny on ask

env:
  scrub:
    - "AWS_*"
    - "*_SECRET"
  passthrough:
    - "HOME"
    - "PATH"

allow:
  - "go test ./..."
  - command: "git diff *"
    description: "read-only git diffs"

deny:
  - command: "rm -rf *"
    description: "block recursive force delete"

ask:
  - "git push *"
```

### Three buckets

Every rule lives in one of three buckets that determine its effect:

| Bucket | Engine effect | Default priority | Meaning |
|--------|--------------|-----------------|---------|
| `allow` | `allow` | 50 | Auto-approve silently |
| `ask` | `require_approval` | 100 | Prompt the developer |
| `deny` | `deny` | 200 | Hard block, no override |

When a command matches rules in multiple buckets, priority determines the winner. With defaults: deny > ask > allow.

### Rule syntax

Rules support two forms:

**Short form** — a string treated as a glob pattern against the full command:
```yaml
allow:
  - "go test ./..."
  - "git status"
```

**Long form** — a map with explicit match fields (all fields AND together):
```yaml
deny:
  - id: "deny-rm-rf"
    command: "rm -rf *"
    working_dir: "/production/*"
    description: "block rm -rf in production dirs"
    priority: 250
```

### Match fields

| Field | Engine field | Operator | Description |
|-------|-------------|----------|-------------|
| `command` | `shell.command` | `matches` (glob) | Full command string |
| `binary` | `shell.binary` | `matches` | argv[0] only |
| `args_contain` | `shell.args` | `contains` | Substring match on arguments |
| `working_dir` | `shell.working_dir` | `matches` | `$PWD` when command runs |

All specified fields must match (implicit AND). Short-form strings are equivalent to `command: "<string>"`.

### Rule metadata fields

| Field | Type | Description |
|-------|------|-------------|
| `id` | string | Stable identifier for override and audit |
| `description` | string | Human-readable explanation, shown on deny/ask |
| `priority` | int | Override the bucket's default priority |
| `override` | string | Cancel a rule from a lower-layer profile by ID |

### Profile composition

The full composition order is:

```
Built-in structural deny rules (always active, non-overridable)
  → OS profile (auto-detected from runtime.GOOS)        [future]
    → Language profile (auto-detected from project files)  [future]
      → Explicit profiles listed in aileron.yaml
        → Project aileron.yaml
          → User ~/.aileron/settings.yaml (personal)       [future]
```

Currently implemented: built-in rules → explicit profiles → project `aileron.yaml`. OS/language auto-detection and user settings overlay are planned for Phase 5 (#63).

Merge semantics at each layer:

- **Rules:** Each bucket's lists are concatenated across layers. All deny rules from all layers apply.
- **Settings/default:** Last-writer-wins (later layer overrides earlier).
- **Env scrub:** Union across layers. Passthrough at any layer beats scrub for the same pattern.
- **Override mechanism:** A rule with `override: "rule-id"` cancels a specific rule from a lower layer. Only works on `allow` and `ask` rules — deny rules cannot be overridden (security invariant).
- **User settings** (`~/.aileron/settings.yaml`): Same schema as `aileron.yaml`, personal to the developer, not checked into any repo. Users can add personal allow/ask rules and override notification preferences. Users cannot override project deny rules or built-in structural deny rules.

### Built-in structural deny rules

Always active, non-overridable, priority 300 (above user deny). They catch dangerous structural patterns regardless of user policy:

- Pipe to shell execution (`| bash`, `| sh`, `| exec`)
- `eval` (arbitrary code execution)
- Base64 decode piped to execution
- Remote fetch into command substitution (`$(curl ...)`, `$(wget ...)`)
- Inline script execution (`python -c`, `python3 -c`, `node -e`, `ruby -e`, `perl -e`)

### Default disposition

When no rule matches a command, the `default` field determines the outcome. If `default` is unset, it defaults to `ask` — the safe choice that prompts the developer rather than silently allowing or blocking.

### Glob matching

The `matches` operator uses multi-wildcard glob matching where `*` matches any substring (including empty). This handles shell command patterns naturally:

- `"go test *"` matches `"go test ./..."`
- `"git * --force"` matches `"git push --force"` and `"git push origin main --force"`
- `"*.sh"` matches `"script.sh"` and `"path/to/script.sh"`

No regex support. Glob patterns are simpler to read, write, and audit in policy files.

### Translation to engine types

Each YAML rule becomes an `api.PolicyRule` with:
- `action.type` condition set to `"shell.exec"` (all launch rules are shell commands)
- Match field conditions mapped to `shell.*` engine fields
- Effect and priority set by the bucket (or explicit `priority` field)

The translated rules are loaded into the existing `RuleEngine` via an in-memory `PolicyStore`. No engine changes were needed beyond enhancing `globMatch` for multi-wildcard patterns.

## Consequences

- `aileron.yaml` is the primary user-facing artifact for the `aileron launch` experience.
- The schema is intentionally minimal — three buckets, glob patterns, no regex. Complexity is handled by profile composition, not by individual rule expressiveness.
- Community profiles (`lang/go.yaml`, `os/darwin.yaml`) are data files that compose with the same schema. Adding support for a new language or OS is a single YAML file.
- The `override` mechanism allows users to relax profile rules without forking the profile, while maintaining the invariant that deny rules cannot be overridden.
- The `env` section provides credential hygiene by default — scrubbing sensitive environment variables before they reach the agent.

## Examples

### Minimal policy (Go project)

```yaml
version: 1
profiles: ["lang/go"]
default: ask
```

### Full policy

```yaml
version: 1
profiles: ["lang/go"]
default: ask

settings:
  audit_log: .aileron/audit.jsonl
  timeout: 30

env:
  scrub:
    - "AWS_*"
    - "GITHUB_TOKEN"
    - "*_SECRET"
    - "OPENAI_*"
  passthrough:
    - "HOME"
    - "PATH"
    - "GOPATH"

allow:
  - "git status"
  - "git diff *"
  - "git log *"
  - "cat *"
  - "ls *"
  - "task *"

deny:
  - command: "git push origin main"
    description: "no direct push to main"
  - command: "rm -rf /*"
    description: "no recursive delete outside project"

ask:
  - "git add *"
  - "git commit *"
  - "git push *"
  - "docker *"
  - "curl *"
```
