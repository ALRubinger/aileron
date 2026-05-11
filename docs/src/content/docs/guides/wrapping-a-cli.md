---
title: "Wrapping a CLI"
description: "Turn a local CLI into an Aileron action surface with one command. No WASM build, no signing key, no publish cycle."
---

If you have a CLI installed on your machine, you can give your agent access to it under Aileron's capability bounds without writing or building a connector. The `aileron action wrap` command does the work: it generates a manifest, lands it in the daemon's connector store, writes action files under `~/.aileron/actions/`, and the wrapped subcommands become tool calls the agent can invoke.

This is the spawn primitive's payoff (per [ADR-0002](/adr/0002-connector-model)'s spawn-primitive section). Where [Authoring a Connector](/guides/authoring-a-connector/) is the path for new services that need their own WASM, this guide is the path for the long tail of existing CLIs (`git`, `gh`, `slackdump`, `imsg`, anything POSIX) that already do the work you want the agent to compose with.

## What you get

A connector that the daemon recognizes and the agent can call. Behind the scenes:

- The **manifest** describes which subcommands the agent may invoke, which arguments they take, which env vars get forwarded, and which filesystem scopes the CLI may touch.
- The **embedded forwarder** is a tiny WASM binary that ships inside the Aileron daemon. It reads the agent's tool call, looks up the matching subcommand in your manifest, substitutes placeholders, and invokes `aileron_host.spawn_op`. You never write WASM.
- The **runtime** enforces the manifest on every invocation. An argv shape the manifest does not declare is denied with a structured `capability_denied` error; the sandbox confines the subprocess (per [ADR-0014](/adr/0014-spawn-sandbox-technology)) to the declared filesystem and env scopes.

The result: a wrapped CLI is a manifest, signed and trusted by you locally. No third-party publisher, no WASM toolchain.

## The 30-second path

You have `gh` installed. You want the agent to use it.

```sh
aileron action wrap --config=gh.yaml --install
```

With this `gh.yaml`:

```yaml
connector:
  name: github://you/local-gh
  version: 0.0.1
  publisher: you

program:
  path: /opt/homebrew/bin/gh

env_passthrough:
  - GH_TOKEN

credential_env_keys:
  - GH_TOKEN

credential:
  kind: api_key
  scope: GitHub personal access token

subcommands:
  - name: pr-view
    description: Show a pull request by number
    argv: gh pr view {pr_number}
    params:
      - name: pr_number
        type: string
        description: The PR number to view
        required: true
  - name: pr-list
    description: List open pull requests for the current repo
    argv: gh pr list --state open
```

That's the whole connector. The agent now has two new tool calls (`pr-view`, `pr-list`); invoking either runs `gh` under the manifest's bounds.

## The two modes

`aileron action wrap` has two ways to describe what you're wrapping.

### YAML mode (recommended)

The example above. A `--config=<file>` flag points at a YAML spec you hand-write. The schema mirrors the connector manifest field by field; you control everything precisely.

Use this when:

- You know which subcommands and flags you want the agent to see.
- You need to declare credentials, filesystem scopes, or env passthroughs.
- You plan to commit the YAML to a repo for reuse across machines.

### `--help`-parsing mode (scaffold)

```sh
aileron action wrap --name=github://you/local-gh --install /opt/homebrew/bin/gh
```

The tool invokes `gh --help`, parses the output, and emits a manifest with one operation per detected subcommand. The shape is a scaffold; you almost always want to refine it (the heuristic does not pick up per-subcommand flags, parameter types, or filesystem scopes).

Use this for a fast first pass when you do not yet know the CLI's surface. Pair with `--install` if you want to try it immediately, or omit `--install` and edit the emitted YAML before running again.

## Default output versus `--install`

Without `--install`, the tool writes a slim source tree under `--out` (default `./aileron-connector`):

```
aileron-connector/
  connector/
    manifest.toml
  actions/
    pr-view.md
    pr-list.md
```

You can inspect the manifest, edit it, and re-run with `--install`. Or you can commit the directory to a repo, sign the manifest, and distribute it (see [Publishing a Connector](/guides/publishing-a-connector/) for the publishing path).

With `--install`, no source tree lands on disk. The manifest goes directly into the daemon's connector store at `~/.aileron/store/connectors/sha256/<hash>/manifest.toml`, and the action files land in `~/.aileron/actions/<connector-leaf>-<op>.md`. The wrap is live immediately; the agent sees the new tool calls the next time it queries.

## The YAML schema

| Field | Required | Description |
|---|---|---|
| `connector.name` | yes | FQN of the connector. `github://you/local-<cli>` is the convention for local wraps. Determines the connector's identity. |
| `connector.version` | yes | Strict SemVer. Local wraps usually stay at `0.0.x` until the surface stabilizes. |
| `connector.publisher` | no | Human-readable publisher name surfaced in install prompts. |
| `program.path` | yes | Absolute or `~/`-anchored path of the binary. The runtime verifies the path matches before every spawn. |
| `program.hash` | no | Optional `sha256:` content hash. When set, the runtime additionally verifies the binary's bytes; a mismatch fails the spawn loudly. |
| `subcommands[].name` | yes | The operation name the agent sees. Lowercase, alphanumeric plus underscore and hyphen, must begin with a letter. |
| `subcommands[].description` | no | One-line summary the agent's tool surface displays. |
| `subcommands[].argv` | yes | Templated argv shape. `{name}` placeholders bind to the agent's args at call time. |
| `subcommands[].params[]` | no | Parameter declarations (name, type, description, required). One per `{name}` placeholder in the argv. |
| `env_passthrough` | no | Closed list of env keys the runtime is allowed to forward to the subprocess. POSIX-shape names only. |
| `credential` | no | When the CLI needs an API key or OAuth credential. `kind` is `api_key` or `oauth2`; `scope` is human-readable prose for the consent surface. |
| `credential_env_keys` | no | Subset of `env_passthrough` into which the runtime injects the resolved credential value. Each key must also appear in `env_passthrough`. |
| `fs_read` | no | Filesystem read scopes (absolute or `~/`-anchored paths). The sandbox restricts subprocess reads to these. |
| `fs_write` | no | Filesystem write scopes. Same shape as `fs_read`. |
| `cwd` | no | Optional working-directory policy. When set, must be within `fs_read`. |

A complete reference manifest is in [ADR-0002](/adr/0002-connector-model)'s "Implementation details" subsection.

## Credential injection

For CLIs that need a token (`gh`, `slackdump`, anything that reads a secret from an env var), declare both `credential` and `credential_env_keys`. The runtime resolves the credential from your vault at spawn time and sets the env var on the subprocess. The connector and the agent never see the credential bytes.

```yaml
credential:
  kind: api_key
  scope: GitHub personal access token

env_passthrough:
  - GH_TOKEN

credential_env_keys:
  - GH_TOKEN
```

On first call, the agent gets a `binding_required` error if no credential is bound for this connector. Run `aileron binding setup <connector-fqn>` to bind a vault entry, then retry.

## Argv patterns and placeholders

Placeholders bind argv tokens to agent args. The rules are simple:

- A whole-token placeholder is `{name}`. Matches any value.
- An embedded placeholder is `--flag={name}`. The literal prefix and suffix match exactly; the placeholder consumes the middle.
- A placeholder name appears as a param the agent must supply. Calling the op without supplying `{name}` is denied at substitution time.

Example argv patterns:

| Pattern | Matches |
|---|---|
| `git log --since={since}` | `git log --since=2026-01-01`, `git log --since=yesterday`, etc. |
| `git log --since={since} --author={author}` | The same with an `--author=...` token appended. |
| `gh pr view {pr_number}` | `gh pr view 123`, `gh pr view 9999`. |
| `slackdump export -c {channel}` | `slackdump export -c general`. |

What does **not** match: extra flags the agent invents (`git log --since=X --pretty=oneline` against a pattern that does not declare `--pretty`), reordered tokens, or argvs of different length. The gate is strict, intentionally.

## Filesystem and env scopes

If your CLI reads or writes files, declare the scopes the manifest permits:

```yaml
fs_read:
  - ~/code/

fs_write:
  - ~/.cache/aileron/gitcrawl/

cwd: ~/code/
```

The platform sandbox (per [ADR-0014](/adr/0014-spawn-sandbox-technology)) enforces these. A subprocess that attempts to read outside `fs_read` fails at the kernel level on Linux (via namespaces plus Landlock), the `sandbox-exec` profile on macOS, and a restricted token plus job object on Windows. The connector's own manifest gate refuses envelopes whose `cwd` falls outside `fs_read`.

Environment is the same shape:

```yaml
env_passthrough:
  - GIT_AUTHOR_NAME
  - GIT_AUTHOR_EMAIL
```

The runtime reads each declared key from its own environment (the daemon's, which is the user's shell env at daemon start) and sets only those on the subprocess. Nothing else leaks. The connector cannot pass env keys outside this list.

## Common shapes

### Wrap a single binary with a fixed set of subcommands

The `gh` example above is the canonical shape. One `program`, one `env_passthrough` entry for the credential, one operation per subcommand the agent should see.

### Wrap a CLI that reads from your filesystem

```yaml
connector:
  name: github://you/local-gitcrawl
  version: 0.0.1
program:
  path: /usr/bin/git
fs_read:
  - ~/code/
cwd: ~/code/
subcommands:
  - name: log
    description: List commits since a date in the working repo
    argv: git log --since={since} --format=%H%x09%s
    params:
      - name: since
        type: string
        description: ISO-8601 date
        required: true
  - name: status
    description: Show working-tree status
    argv: git status --short
```

The cwd policy fixes where the agent operates. The agent cannot trick the connector into running `git log` against another repo because `cwd` is bound to `~/code/`.

### Wrap a CLI that emits structured output

If the CLI produces JSON (or another structured shape), the action can declare what the agent should expect. The agent sees the subprocess's stdout as a string; parsing happens on the agent side.

For now, the wrapped output is always `{exit, stdout, stderr}` as strings. A future enhancement may surface typed outputs in the action.md; for v1, the agent's prompt instructs it on how to interpret the CLI's output format.

## Inspecting what got installed

```sh
ls ~/.aileron/actions/                            # the agent-visible action files
ls ~/.aileron/store/connectors/sha256/            # the manifest entries
aileron connector list                            # the daemon's view
```

Each action file is a `+++`-delimited TOML manifest plus a Markdown body. The body is what the agent's tool surface shows.

## Removing a wrap

```sh
rm ~/.aileron/actions/<connector-leaf>-<op>.md   # remove an individual action
rm -rf ~/.aileron/store/connectors/sha256/<hash> # remove the manifest entry
```

The daemon rebuilds its index on next start. A more polished `aileron connector remove` is a future ergonomic improvement; for now, manual file removal is the path.

## See also

- [ADR-0002: Connector Model](/adr/0002-connector-model) — the model the spawn primitive sits in.
- [ADR-0014: Spawn Sandbox Technology](/adr/0014-spawn-sandbox-technology) — what kernel mechanism confines the subprocess on each platform.
- [Authoring a Connector](/guides/authoring-a-connector/) — for services that need their own WASM rather than a CLI wrap.
- [Publishing a Connector](/guides/publishing-a-connector/) — if you want to publish your manifest under a stable FQN for others to install.
