---
title: "Getting Started"
description: "Build Aileron from source, install the Google connector, bind a Google account via OAuth, and launch Claude Code with Gmail and Calendar actions wired in."
order: 2
---

This guide walks the full end-to-end flow on a fresh machine: build the binaries, trust a publisher, install the [`aileron-connector-google`](https://github.com/ALRubinger/aileron-connector-google) reference connector, bind a real Google account, and run [Claude Code](https://docs.claude.com/en/docs/claude-code) with Gmail and Calendar actions exposed via Aileron's MCP server.

By the end you will have asked Claude Code "summarize my five most recent emails", watched it pick a Gmail tool Aileron published, and seen Aileron execute it inside the WASM sandbox against the real Google API — all without Claude ever holding your OAuth token.

## Prerequisites

- **Go 1.25 or newer.** Aileron's modules require it. `go version` should report at least `go1.25.0`.
- **[Task](https://taskfile.dev/installation/).** All build commands go through `task`. `brew install go-task` on macOS.
- **[Claude Code](https://docs.claude.com/en/docs/claude-code/setup) installed and configured** with an Anthropic API key in `ANTHROPIC_API_KEY`. Aileron's launcher routes Claude Code's LLM calls through the local Aileron daemon, but it does not supply or replace your API key.
- **A Google account.** Gmail and Calendar must be enabled. The OAuth dance opens in your default browser.
- **`jq` and `openssl`** for the one-liner that trusts the connector's signing key.

There is no `brew install aileron` yet — for now you build from source. That is the path this guide follows.

## 1. Build the binaries

```sh
git clone https://github.com/ALRubinger/aileron.git
cd aileron
task build:cli build:server build:mcp build:sh
```

This produces four binaries in `build/`:

| Binary | Purpose |
|---|---|
| `aileron` | The CLI you use for install, binding, status, audit, launch, and `aileron daemon start/stop/status`. |
| `server` | The user-scoped local daemon (per [ADR-0012](/adr/0012-local-daemon-architecture)). Auto-spawned by the CLI on first need; you don't run it directly during normal use. |
| `aileron-mcp` | The MCP server that exposes installed actions to the agent. `aileron launch` resolves it as a sibling of the `aileron` binary. |
| `aileron-sh` | The policy-enforced shell shim used by `aileron launch`. |

Put the build directory on your `PATH` so the launcher can find `aileron-mcp` next to `aileron`:

```sh
export PATH="$PWD/build:$PATH"
```

Verify:

```sh
aileron version
```

## 2. The Local Daemon

Per [ADR-0012](/adr/0012-local-daemon-architecture), the first `aileron <anything>` you run auto-spawns the local daemon — there is no `aileron serve` step. The daemon binds an ephemeral port on `127.0.0.1` and advertises itself in `~/.aileron/daemon.json` so every subsequent CLI command and every `aileron launch` finds it without you typing a port number.

You can manage the daemon explicitly when you want to:

```sh
aileron daemon start    # idempotent — prints the URL whether or not it was already running
aileron daemon status   # URL + PID + locked/unlocked vault state
aileron daemon stop     # SIGTERM, drops the unlocked vault key from memory
aileron stop            # alias for `aileron daemon stop`
```

On first launch you will be prompted to create a vault and choose a passphrase. The vault is encrypted at rest with [Argon2id](https://datatracker.ietf.org/doc/html/rfc9106); see [The Vault](/concepts/the-vault/) for what it protects you from. The vault file lives at `~/.aileron/secrets.json`. **You unlock the vault once per daemon lifetime** — every subsequent CLI command and every `aileron launch` reuses the unlocked state until you `aileron stop` or reboot.

## 3. Trust the connector's publisher key

Every connector install verifies an ed25519 signature against keys you have explicitly trusted (per [ADR-0002](/adr/0002-connector-model)). Without a trusted key for the publisher's authority, install fails closed before fetching anything.

Download the `aileron-connector-google` publisher key and trust it:

```sh
curl -fsSLO https://raw.githubusercontent.com/ALRubinger/aileron-connector-google/main/keys/publisher.pub
aileron keyring trust github://ALRubinger/aileron-connector-google publisher.pub
```

You should see:

```
✓ Trusted publisher github://ALRubinger/aileron-connector-google
  Fingerprint: sha256:...
  Keyring: /Users/you/.aileron/keyring.json
```

Confirm it landed:

```sh
aileron keyring list
```

## 4. Install an action

Pick the latest release tag from [the connector's releases page](https://github.com/ALRubinger/aileron-connector-google/releases) and install one of its action templates. Versions in this guide use `0.0.6` — substitute the current tag.

```sh
aileron action add github://ALRubinger/aileron-connector-google/actions/list-recent-emails@0.0.6
```

Aileron resolves the action's connector dependency, fetches the connector tarball from the GitHub release, verifies the signature against your trusted key, computes the content hash, and walks you through a [consent prompt](/adr/0007-install-consent) for each new connector and the action itself. Confirm each step.

When the install completes, the action file is at `~/.aileron/actions/list-recent-emails.md` (you own it — edit it freely) and the connector is in the content-addressed store at `~/.aileron/store/`.

Add a couple more so the agent has room to compose:

```sh
aileron action add github://ALRubinger/aileron-connector-google/actions/get-email@0.0.6
aileron action add github://ALRubinger/aileron-connector-google/actions/list-upcoming-events@0.0.6
aileron action add github://ALRubinger/aileron-connector-google/actions/draft-email@0.0.6
```

The connector is fetched once; subsequent action installs reuse it.

## 5. Bind your Google account

Action templates declare *what* credential they need; binding is how you tell Aileron *which* one of your accounts to use. The Google connector declares an OAuth2 capability with read+compose scopes for Gmail and read+events scopes for Calendar.

If `aileron action add` already prompted you to bind during install, skip ahead. Otherwise:

```sh
aileron binding setup github://ALRubinger/aileron-connector-google
```

You'll be asked for an identity (e.g. `personal`, `work`) — this is just a label that disambiguates multiple accounts you might bind under the same connector. Aileron then opens your browser to Google's OAuth consent screen. Approve, and Google redirects to a loopback URL Aileron is listening on. The CLI captures the code, exchanges it for a refresh token server-side, and stores the result in your vault.

The connector's publisher already shipped a Desktop OAuth `client_id` and `client_secret` bound at release time, so there is no Google Cloud Console setup on your end.

Confirm the binding landed:

```sh
aileron binding list
```

You should see something like:

```
oauth2/aileron-connector-google/personal  oauth2  google  ...
```

The token bytes are encrypted at rest. Aileron refreshes them transparently when they near expiry; the connector never sees the credential. See [The Vault](/concepts/the-vault/) for how the credential travels at execution time.

## 6. Launch Claude Code through Aileron

```sh
aileron launch claude
```

You'll see a banner like:

```
✈️  Aileron — webapp http://127.0.0.1:54321 — session 01HK6... — log ~/.aileron/logs/...
```

If the vault happens to be locked at this moment, a follow-up line points you at the unlock surface:

```
✈️  Vault locked — open http://127.0.0.1:54321 and enter your passphrase to unlock.
```

What happened under the hood:

- The CLI resolved the daemon URL via `~/.aileron/daemon.json` (auto-spawning the daemon on first need).
- It registered the launch session by `POST /v1/sessions`; the daemon minted a [ULID](https://github.com/ulid/spec) session ID and stamped its start time.
- It set `ANTHROPIC_BASE_URL` to the daemon URL so Claude Code's LLM calls flow through Aileron.
- It registered `aileron-mcp` as an MCP server for the session. On startup, `aileron-mcp` queries the daemon's `/v1/actions` and generates one MCP tool per installed action — `list_recent_emails`, `get_email`, `list_upcoming_events`, `draft_email`.
- The vault binding you set up in step 5 is in scope because the daemon is the same process the CLI used to register the binding.

The daemon URL is **stable across launches** — bookmark it once and it stays reachable until you `aileron stop`. Claude Code is now a normal Claude Code session, with a tool catalog that includes the actions Aileron published.

## 7. Drive the actions from the agent

In the Claude Code prompt, try:

> Summarize my five most recent emails.

Claude picks `list_recent_emails` (it sees one tool per installed action thanks to MCP discovery), Aileron executes it inside the WASM sandbox against `gmail.googleapis.com`, returns `{id, threadId}` pairs, Claude fans out parallel `get_email` calls for the metadata, and you get a summary.

A few more prompts to exercise the rest of the surface:

> What's on my calendar this week?

Routes to `list_upcoming_events`.

> Draft a reply to the last email from Andrew thanking him and confirming Tuesday.

Routes to `get_email` (to read context) then `draft_email` — which lands a draft in your Gmail Drafts folder. Drafts are reversible, so this action is not gated; the existing manual "click Send in Gmail" step is the human review.

`send-email` and `create-calendar-event` are gated for per-call approval because their effects are not reversible (see [the connector's README](https://github.com/ALRubinger/aileron-connector-google#what-it-ships) for why). When Claude proposes one of those, the approval lands on the daemon's `/approvals` page (the same URL printed in the launch banner above), and Claude's tool call blocks until you click Approve or Deny.

## 8. Inspect what happened

In a separate terminal — the daemon is still running, so any CLI call connects to the same process — see what the runtime knows:

```sh
aileron status               # what's configured: action/connector/binding counts, policy, env, vault state
aileron daemon status        # what's running: daemon URL, PID, version, started_at, locked/unlocked vault
aileron sessions list        # every aileron launch, with status (running / ended / orphaned) and exit code
aileron action audit         # every installed action and the capabilities it can exercise
aileron binding list         # bound credentials with their last-used timestamp
aileron audit list           # action-execution audit log: every call, with inputs and outcome
```

`aileron status` is configuration-shaped: what you've installed, what your policy says, where credentials live. `aileron daemon status` is process-shaped: which daemon process the CLI is talking to and whether its vault is unlocked. They overlap on vault state because both surface it for different reasons; the rest is disjoint.

The audit log is the receipt for everything the agent did on your behalf. Each entry names the action, the connector version, the binding identity, the duration, and the disposition — recorded under `~/.aileron/audit/audit-YYYY-MM-DD.jsonl` (daily-rotated, user-scope per [ADR-0012](/adr/0012-local-daemon-architecture)). Combined with the install consent log, you can answer two questions for any action: "did I authorize this capability?" and "what did the agent actually do with it?"

For end-to-end timing or correlation with the rest of your agent stack, Aileron also emits OpenTelemetry traces — opt-in via `AILERON_OTEL_ENABLED=true AILERON_OTEL_EXPORTER=stdout`. See [Observability](/guides/observability/) for what gets emitted, the span attribute schema, and the env vars that control both the audit and tracing surfaces.

## What you've built

You ran a real LLM through a local proxy that augmented its tool catalog with installed actions, executed those actions inside a WASM sandbox, mediated OAuth tokens at the network boundary so the LLM never held a credential, and recorded every decision and execution in an audit log.

This is the loop the rest of the docs builds on. From here:

- [Concepts → Deterministic Agentic Execution](/concepts/deterministic-agentic-execution/) — why this is the architecturally load-bearing seam.
- [Concepts → Actions](/concepts/actions/) and [Connectors](/concepts/connectors/) — the model behind what you just installed.
- [Guides → Authoring an Action](/guides/authoring-an-action/) — write your own action template against an existing connector.
- [Guides → Authoring a Connector](/guides/authoring-a-connector/) — ship a connector for a service Aileron does not yet have.
- [CLI Reference](/cli/) — every command grouped by concern, each linked to the ADR that ratifies its behavior.
