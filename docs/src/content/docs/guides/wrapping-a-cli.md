---
title: "Wrapping a CLI"
description: "Turn a local CLI into an Aileron action surface with one command. No WASM build, no signing key, no publish cycle."
---

If you have a CLI installed on your machine, you can give your agent access to it under Aileron's capability bounds without writing or building a connector. The `aileron action wrap` command does the work: it generates a manifest, lands it in the daemon's connector store, writes action files under `~/.aileron/actions/`, and the wrapped subcommands become tool calls the agent can invoke.

This is the spawn primitive's payoff (per [ADR-0002](/adr/0002-connector-model)'s spawn-primitive section). Where [Authoring a Connector](/guides/authoring-a-connector/) is the path for new services that need their own WASM, this guide is the path for the long tail of existing CLIs (`git`, `gh`, `slackdump`, anything POSIX) that already do the work you want the agent to compose with.

> **Not for iMessage.** Apple's data model is locked down enough that there is no usable CLI to wrap. The iMessage connector takes a different path: it talks over HTTP to a local [BlueBubbles](https://bluebubbles.app/) bridge that holds the platform permissions instead. See [Setting up BlueBubbles for Aileron](/guides/setting-up-bluebubbles/).

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

## Looking ahead: v3 will make this one command

[Milestone v3](https://github.com/ALRubinger/aileron/issues/584) introduces a one-command flow for catalog-listed CLIs ([#580](https://github.com/ALRubinger/aileron/issues/580)). The shape:

```sh
aileron cli add linear
# resolves through a registered tap, runs `go install`, introspects --help,
# generates the manifest, prompts for credentials. Done.
```

A **tap** is a catalog (`registry.json`) of installable CLIs. [PrintingPress](https://printingpress.dev/) ships as the bundled default tap, covering its ~55 Go-installable CLIs out of the box. Other taps can be added with `aileron tap add <name> <url>`. The catalog pins by commit hash for reviewable provenance.

The manual YAML path on this page stays useful in v3 for two cases:

1. **CLIs not covered by any tap.** The `slackdump` and `gitcrawl` examples below are this case — they're not in PrintingPress, and they may never be.
2. **Customizing a tap-generated manifest.** The `--help` introspection in v3 produces a scaffold. If you want narrower argv patterns, filesystem scopes, or non-default credential injection, you edit the YAML.

Read the rest of this page as the underlying mechanism. v3's BYOCLI flow is a one-command wrapper around it for catalog CLIs.

## Worked example: `slackdump`

[`rusq/slackdump`](https://github.com/rusq/slackdump) reads a Slack workspace's channels, threads, and search results into a local archive. It's the canonical "read recent team conversation" CLI; if your agent needs to pull what people said while you were heads-down, this is the wrap.

### The manifest

```yaml
connector:
  name: github://you/local-slackdump
  version: 0.0.1
  publisher: you

program:
  path: /opt/homebrew/bin/slackdump

env_passthrough:
  - SLACK_TOKEN
  - SLACK_COOKIE

credential_env_keys:
  - SLACK_TOKEN
  - SLACK_COOKIE

credential:
  kind: api_key
  scope: Slack workspace session — XOXC token plus the `d` cookie, extracted from your browser

fs_write:
  - ~/.aileron/cache/slackdump/

cwd: ~/.aileron/cache/slackdump/

subcommands:
  - name: dump-channel
    description: Dump a channel's recent messages to local JSON
    argv: slackdump dump --format=json {channel}
    params:
      - name: channel
        type: string
        description: Channel name (e.g. general) or ID (e.g. C01ABCDEF)
        required: true
  - name: search-messages
    description: Search the workspace for a phrase
    argv: slackdump search messages {query}
    params:
      - name: query
        type: string
        description: The search phrase
        required: true
  - name: list-channels
    description: List channels accessible to the authenticated user
    argv: slackdump list channels
```

Run with `aileron action wrap --config=slackdump.yaml --install` and the agent gains three tool calls. Adapt the `argv` patterns if your installed `slackdump` version uses different subcommand syntax — `slackdump --help` is the source of truth for the binary you have.

### Why each piece

- **Credential is `api_key`, not `oauth2`.** Slack's XOXC token comes from a browser session, not an OAuth dance. The runtime resolves the bound credential from your vault at spawn time and injects it as `SLACK_TOKEN` (plus `SLACK_COOKIE` for the `d` cookie that slackdump needs alongside the token). The connector and the agent never see the credential bytes.
- **`fs_write` and `cwd` are bound.** slackdump writes its archive to disk; the sandbox confines those writes to `~/.aileron/cache/slackdump/` and runs the subprocess with that directory as cwd. The agent can't trick the connector into dumping into your home directory.
- **Narrow argv patterns.** Each subcommand declares the exact argv shape the agent can produce. An invented flag (`slackdump dump --secret=...`) is denied at the manifest gate — the agent never reaches the binary with an undeclared argument.

### XOXC token caveat

The XOXC + `d` cookie pair is a *browser session*, not a Slack-issued API key. Workspace admins can disable third-party exports in Slack's admin console; if that happens for your workspace, slackdump (and this wrap) stop working until you obtain a fresh session against a workspace that permits it. This is a real risk for org-managed workspaces, less so for personal ones. There's no Aileron-side workaround — Slack controls the export surface.

For the send side (posting messages from your agent), use [`aileron-connector-slack`](https://github.com/ALRubinger/aileron-connector-slack) instead. That's a native HTTP connector with a proper OAuth flow.

## Wrapping the Steipete CLIs

[@steipete](https://github.com/steipete) publishes a growing set of CLIs designed to be agent-ready: small, focused, env-var-authenticated, predictable surface. They wrap well. We highlight one below as the worked example; the pattern is reusable for others he (or anyone in that shape) ships.

### `gitcrawl`

[`gitcrawl`](https://gitcrawl.sh/) archives your GitHub activity — mentions, PR reviews, comments, the whole stream — into a local searchable corpus. Where the GitHub REST API is rate-limited and shallow, `gitcrawl`'s archive gives the agent a "what's happened on GitHub this week" surface it can query without burning the API budget.

```yaml
connector:
  name: github://you/local-gitcrawl
  version: 0.0.1
  publisher: you

program:
  path: /opt/homebrew/bin/gitcrawl

env_passthrough:
  - GITHUB_TOKEN

credential_env_keys:
  - GITHUB_TOKEN

credential:
  kind: api_key
  scope: GitHub personal access token (repo + read:user scopes for gitcrawl's archive)

subcommands:
  - name: search
    description: Full-text query across your archived GitHub activity
    argv: gitcrawl search {query}
    params:
      - name: query
        type: string
        description: Search phrase
        required: true
  - name: mentions
    description: Surface @-mentions of you across watched repos
    argv: gitcrawl mentions
  - name: since
    description: List GitHub activity since a date
    argv: gitcrawl since {date}
    params:
      - name: date
        type: string
        description: ISO-8601 date (e.g. 2026-05-01)
        required: true
```

Install with `aileron action wrap --config=gitcrawl.yaml --install`.

`gitcrawl` is read-only — no filesystem write scopes needed, no approval gates, the agent can call it freely under the credential bound to the connector. That's the simplest end of the wrap spectrum: token in, structured output out. If you find a Steipete CLI that wants to write somewhere, declare an `fs_write` scope the same way `slackdump` does above.

## The two modes

`aileron action wrap` has two ways to describe what you're wrapping.

### YAML mode (recommended)

A `--config=<file>` flag points at a YAML spec you hand-write. The schema mirrors the connector manifest field by field; you control everything precisely. Every worked example on this page uses YAML mode.

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
| `slackdump dump --format=json {channel}` | `slackdump dump --format=json general`. |

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
| `cwd` | no | Optional working-directory policy. When set, must be within `fs_read` or `fs_write`. |
| `network.hosts` | no | Outbound network allowlist as `host:port` pairs. The runtime injects `HTTPS_PROXY` and `HTTP_PROXY` and tunnels CONNECTs through a daemon-mediated proxy that enforces the list (per [ADR-0014](/adr/0014-spawn-sandbox-technology)'s network-confinement section). Omit to deny all outbound. |
| `limits.max_stdout_bytes` | no | Byte cap on captured subprocess stdout. Defaults to 1 MiB. Output past the cap is truncated with a structured marker. |
| `limits.max_stderr_bytes` | no | Byte cap on captured subprocess stderr. Defaults to 256 KiB. Same truncation semantics. |

A complete reference manifest is in [ADR-0002](/adr/0002-connector-model)'s "Implementation details" subsection.

## Notes on structured output

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

## Network access for spawned CLIs

A wrapped CLI that needs to make outbound HTTPS calls (Linear, Sentry, the GitHub API, anything that hits a remote service) declares the hosts it talks to. The runtime injects standard `HTTPS_PROXY` and `HTTP_PROXY` env vars on the subprocess and points them at a local proxy the daemon runs for the duration of the call. The proxy enforces the host:port allowlist; everything outside the list is denied. Direct egress that bypasses the proxy is denied at the kernel level by the platform sandbox.

```yaml
network:
  hosts:
    - api.linear.app:443
    - api.github.com:443
```

The shape mirrors `[capabilities.network]` for WASM connectors. Hosts are pinned to `host:port` pairs; no wildcards. Omit the block to deny all outbound network from the subprocess.

What the proxy sees and audits: the requested host:port per CONNECT request, and the bytes tunneled through. TLS is end-to-end between the subprocess and the remote, so the proxy does not see request URLs, headers, or bodies. Audit granularity is per host:port per request, correlated with the spawn invocation's audit id.

What the CLI must do: honor `HTTPS_PROXY` / `HTTP_PROXY`. Most Go CLIs and anything built on `curl` or `requests` do this by default. CLIs that build their own HTTP clients and ignore standard proxy env vars cannot reach the network through Aileron because the sandbox denies direct egress. The failure surfaces as a structured `network_unreachable` error on the first outbound call.

See [ADR-0014](/adr/0014-spawn-sandbox-technology)'s "Network confinement: daemon-mediated proxy" section for the per-platform mechanism.

## Output caps

The runtime caps the bytes it captures from a subprocess's stdout and stderr. Defaults are 1 MiB stdout and 256 KiB stderr. Output past the cap is truncated and a structured marker (`[aileron: output truncated, N bytes dropped]`) is appended to the captured slice the connector reads back.

Override the defaults per CLI when you need more or less headroom:

```yaml
limits:
  max_stdout_bytes: 4194304  # 4 MiB
  max_stderr_bytes: 131072   # 128 KiB
```

The caps cannot be disabled. Unbounded output is a load-bearing problem rather than a convenience one: a wrapped CLI's stdout flows back into the connector and ultimately into the agent's LLM context, and the agent treats it as data to consider. A subprocess that emits an arbitrary volume of attacker-chosen bytes is an attempt to overwhelm the agent's prompt. The cap bounds that side channel's blast radius. The schema rejects values past 64 MiB or values of zero (omit the field to use the default).

## What `aileron cli add` shows you

When [Milestone v3](https://github.com/ALRubinger/aileron/issues/584) ships the BYOCLI flow ([#580](https://github.com/ALRubinger/aileron/issues/580)), the install command will show the full inferred capability block before fetching the binary. The user makes two trust decisions at once: that the catalog and the upstream module are OK to install, and that the binary should have access to the listed scopes.

The shape the runtime presents at install time:

```sh
$ aileron cli add linear

Tap:          printingpress (registry pinned at sha256:b3d4e2…)
Module:       github.com/printing-press/linear-cli@v1.4.0
Install:      go install github.com/printing-press/linear-cli@v1.4.0

This connector will be granted:
  Filesystem read:   ~/.config/linear/                  [scope-default]
  Filesystem write:  ~/.cache/linear/                   [scope-default]
  Network hosts:     api.linear.app:443                 [from --help]
  Environment vars:  LINEAR_API_TOKEN                   [credential-injected]
  Output caps:       1 MiB stdout, 256 KiB stderr        [defaults]

Proceed?  [y] yes  [e] edit  [n] no  [d] dry-run
```

The display surfaces every grant the runtime will give the wrapped CLI. Two flags refine the flow:

- `aileron cli add --dry-run <cli>` prints the manifest the introspector would emit and exits without fetching the binary. Useful for reviewing what the introspector inferred from `--help` before any install side effects occur.
- `aileron cli add --edit <cli>` opens the inferred manifest in `$EDITOR` so you can narrow scopes or tweak operations before confirming. The runtime re-validates the edited manifest before continuing.

Broad scopes (filesystem scope of `$HOME`, unrestricted network) are highlighted in the install-time display. The introspector defaults to the narrowest possible scope (`$XDG_CONFIG_HOME/<cli>` for FS, the hosts mentioned in `--help` for network); when the heuristic can't infer a narrow scope, it prompts rather than expanding silently.

This UX spec is the contract; the implementation lands with [#580](https://github.com/ALRubinger/aileron/issues/580).

## Threat model

What BYOCLI defends against, and what it does not. Reading this honestly is part of using it well.

### Compared to running the CLI bare in your shell

Running a CLI bare from your shell:

- The CLI runs with your user's full filesystem access. It can read SSH keys, browser profiles, `.env` files in any project, shell history.
- The CLI inherits your shell's environment. Any credential set in your shell (`AWS_ACCESS_KEY_ID`, ambient `GITHUB_TOKEN`, IDE-injected secrets) is visible to it.
- The CLI's network egress is unrestricted. It can reach any host on the public internet.
- The CLI's stdout / stderr ends up wherever you piped it. Nothing limits the volume or scrutinizes the content.

Wrapping the same CLI through Aileron's spawn primitive:

- The CLI's filesystem reach is bound by `fs_read` and `fs_write`. The kernel enforces the bound (per [ADR-0014](/adr/0014-spawn-sandbox-technology)) via Linux namespaces plus Landlock, macOS `sandbox-exec`, or Windows job objects plus restricted tokens. Files outside the declared scopes are not visible to the subprocess.
- The CLI sees only the env keys in `env_passthrough`. Credentials it needs are injected from the vault at spawn time and never live in your shell. The connector itself does not see the credential bytes either.
- The CLI's network egress is denied except to the hosts in `network.hosts`, enforced by the daemon-mediated proxy. Hosts outside the allowlist are refused with an audit row, not just a network error.
- The CLI's stdout / stderr is captured against a byte cap. Output past the cap is truncated with a marker; the agent never sees an unbounded volume of subprocess-chosen bytes.
- Every spawn invocation emits an audit event: connector identity, argv shape, exit code, output hashes, network decisions.

The agent calling the wrapped CLI through Aileron gets less surface than calling the same CLI through your shell. Wrapping is a strict narrowing.

### What wrapping does not defend against

- **What's in the binary.** Aileron does not analyze the CLI's syscalls, infer behavior, or otherwise vet the binary's contents. The trust anchor is "you installed this binary yourself" (or "the catalog you tapped vouches for it"). A malicious binary can do anything the declared scopes permit. The kernel-enforced sandbox bounds *what* it can touch, not *whether* what it touches is what you wanted.
- **The LLM provider sees prompts and tool results.** A wrapped CLI's stdout flows back to the agent and into the LLM's context. The credential bytes do not leave your machine, but the data the CLI returns does. Wrap CLIs whose output you would be comfortable sharing with your LLM provider.
- **stdout as a side channel.** A wrapped CLI can, within its filesystem scope, read declared files and write their contents to stdout. That stdout becomes LLM context. A malicious CLI granted read access to `~/code/` could exfiltrate code via its stdout response to an unrelated query. The output cap bounds the volume; it does not prevent the channel. Treat the stdout cap as a blast-radius bound, not a confidentiality guarantee.
- **CLIs that bypass `HTTPS_PROXY`.** The network confinement relies on the CLI honoring standard proxy env vars. CLIs that don't honor them cannot reach the network at all (the sandbox denies direct egress); they fail rather than bypass. But the failure mode is "the CLI doesn't work," not "the CLI works and leaks." When a wrapped CLI fails with `network_unreachable` on first call, suspect a proxy-ignoring HTTP client and either patch the CLI or use a different wrap.
- **Binary integrity after install.** The manifest records the binary's SHA-256 hash at install time and the runtime rechecks on every spawn. This defends against a later `go install` of a malicious version of the same module: the hash mismatches and the spawn refuses. It does not defend against compromise *at the moment of original install*. If the upstream was malicious when you ran `cli add`, you wrapped a malicious binary and pinned its hash.
- **Unsupported platforms.** The kernel-enforced sandbox runs on Linux, macOS, and Windows only. On BSDs, illumos, and other Unix-likes, spawn-using connectors refuse to install with a structured `spawn_sandbox_unavailable` error. The CLI cannot be wrapped on those platforms in v3; there is no fallback to "wrap unconfined." Failure is closed.

### Disposition for sensitive workflows

Use wrapping when:

- You want an agent to compose with a CLI's specific operations under bounds you control.
- You are comfortable with the CLI's stdout being readable by your LLM provider.
- The CLI honors standard proxy env vars (or it has no network needs at all).

Reach for something other than wrapping when:

- The CLI emits secrets in stdout that should not flow into the LLM context. Wrapping won't filter those.
- The CLI's behavior is poorly understood and the threat model assumptions ("it only reads its config dir") cannot be validated. Spend time understanding the binary first.
- The use case wants stronger boundaries than file-system-and-network scoping. The connector model (authored WASM, [Authoring a Connector](/guides/authoring-a-connector/)) gives finer-grained host-function-level control at the cost of writing code rather than declaring a manifest.

## See also

- [ADR-0002: Connector Model](/adr/0002-connector-model) — the model the spawn primitive sits in.
- [ADR-0014: Spawn Sandbox Technology](/adr/0014-spawn-sandbox-technology) — what kernel mechanism confines the subprocess on each platform.
- [Authoring a Connector](/guides/authoring-a-connector/) — for services that need their own WASM rather than a CLI wrap.
- [Publishing a Connector](/guides/publishing-a-connector/) — if you want to publish your manifest under a stable FQN for others to install.
- [Setting up BlueBubbles for Aileron](/guides/setting-up-bluebubbles/) — the iMessage path, which uses HTTP-to-a-local-bridge instead of a CLI wrap.
