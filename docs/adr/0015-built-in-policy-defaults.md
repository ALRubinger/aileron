# ADR-0015: Built-in Policy Defaults — Convention over Configuration

**Status:** Accepted
**Date:** 2026-04-09
**Supersedes:** [ADR-0014](0014-aileron-yaml-policy-schema.md) (profile composition model only; the rest of ADR-0014 remains in effect)

## Context

ADR-0014 defined a profile composition model where OS and language rules ship as separate YAML files (`profiles/lang/go.yaml`, `profiles/os/darwin.yaml`) loaded and merged at runtime. `aileron init` detects the project language and generates a `aileron.yaml` pre-populated with language-specific allow rules.

In practice this creates several problems:

1. **Indirection.** Rules come from files the developer didn't write and may not know about. "Where did this rule come from?" requires understanding the merge order.
2. **Unnecessary project config.** Language allow rules (go test, npm run, cargo build) are universal. Every Go project has the same set. Baking them into `aileron.yaml` adds boilerplate that every project repeats.
3. **OS rules don't belong in project config.** Different engineers on the same project have different OSes. OS-specific deny rules (darwin Keychain paths, linux `/etc/shadow`) are per-platform, not per-project.
4. **Complexity without value.** A profile loader with merge precedence, a `profiles/` directory to ship and maintain, auto-detection logic — all to produce the same result as compiling the rules into the binary.

The goal is convention over configuration: Aileron works with zero config. The project file should only contain what's specific to *this* project.

## Decision

### Three-layer policy model

```
Built-in defaults (compiled into the Aileron binary)
  → Project aileron.yaml (checked into repo, optional)
    → User ~/.aileron/settings.yaml (personal, optional)
```

### Built-in defaults

Compiled into the binary. No external files. Always active.

**Language allow rules** — all supported languages in a single set:
- Go: `go test *`, `go build *`, `go run *`, `go mod *`, `go vet *`, `golangci-lint *`
- Node: `npm *`, `pnpm *`, `yarn *`, `npx *`, `node *`
- Python: `python *`, `python3 *`, `pip *`, `pip3 *`, `pytest *`, `uv *`
- Rust: `cargo *`, `rustc *`, `clippy *`, `rustfmt *`
- Ruby: `bundle *`, `gem *`, `rake *`, `ruby *`
- Elixir: `mix *`, `iex *`
- Java: `mvn *`, `gradle *`, `java *`, `javac *`
- Common tools: `git status`, `git diff *`, `git log *`, `ls *`, `cat *`, `head *`, `tail *`, `find *`, `grep *`, `wc *`, `task *`, `make *`

Including all languages means a Go developer has `pip install` in their defaults but never triggers it. No harm, no detection logic needed.

**OS-specific deny rules** — activated by `runtime.GOOS`:
- darwin: Keychain access, `~/Library` system dirs, `defaults write` system prefs
- linux: `/etc/shadow`, `/etc/passwd` writes, systemd unit manipulation
- windows: registry writes, credential store access, AppData system dirs

**Structural deny rules** — always active, non-overridable (unchanged from ADR-0014):
- Pipe to shell execution (`| bash`, `| sh`)
- `eval`, base64-to-exec, remote fetch into command substitution
- Inline script execution (`python -c`, `node -e`, etc.) defaults to `ask`

### Project `aileron.yaml`

Minimal. Only project-specific configuration:

```yaml
version: 1
default: ask

deny:
  - command: "git push origin main"
    description: "no direct push to main"

env:
  scrub:
    - "AWS_*"
    - "*_SECRET"

notifications:
  slack:
    app_token: vault:slack_app_token
    bot_token: vault:slack_bot_token
    channels:
      - name: "#backend"
        show: all
```

No language allow rules. No OS rules. No profiles field. `aileron init` generates this minimal file. Aileron works without it entirely — the built-in defaults cover the common case.

### User `~/.aileron/settings.yaml`

Personal preferences. Same schema as `aileron.yaml`:

```yaml
allow:
  - "cat /tmp/*"

notifications:
  slack:
    channels:
      - name: "#backend"
        show: mentions
        auto_draft: false
```

### Merge semantics

Unchanged from ADR-0014:
- **Rules:** Each bucket's lists concatenated across layers.
- **Settings/default:** Last-writer-wins.
- **Env:** Union. Passthrough beats scrub.
- **Security invariant:** User/project cannot override built-in deny rules.

### What changes from ADR-0014

| ADR-0014 | ADR-0015 |
|----------|----------|
| `profiles:` field in YAML references external files | Removed. No profiles field. |
| Profile YAML files in `profiles/` directory | Removed. Rules compiled into binary. |
| Runtime profile loading and merge | Removed. `DefaultPolicy()` returns built-in rules. |
| Language auto-detection at init/launch | Removed. All languages included by default. |
| OS auto-detection loads profile file | OS detected via `runtime.GOOS`, rules compiled in. |
| `aileron init` generates language-specific rules | `aileron init` generates minimal project config. |
| `override: "profile:rule-id"` cancels profile rules | Override mechanism remains but targets built-in rule IDs. |

## Consequences

- **Zero-config experience.** `aileron launch claude` works without any `aileron.yaml`. Built-in defaults handle the common case.
- **`aileron init` is optional.** Useful for scaffolding project-specific deny rules and notifications, but not required.
- **Simpler codebase.** No profile loader, no profile directory, no merge chain beyond three fixed layers.
- **All languages always active.** A polyglot repo works out of the box. No detection, no missed languages.
- **Community contributions target the binary.** Adding rules for a new language or OS is a code change to `DefaultPolicy()`, not a YAML file. This is a trade-off: higher bar to contribute, but no runtime file loading complexity.
- **The `profiles` field in `aileron.yaml` is deprecated.** Existing files with `profiles:` will log a warning but otherwise function (the field is ignored).
