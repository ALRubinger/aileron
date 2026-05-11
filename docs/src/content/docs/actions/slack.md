---
title: "Slack"
description: "Three actions for listing Slack channels, searching message history, and posting messages as the authorizing user."
order: 3
---

The Slack suite gives your agent three capabilities against the user's Slack workspace: list channels, search the user's accessible message history, and post a new message. Post is gated by per-call user approval. Reads run without prompting.

The suite is published from the [`aileron-connector-slack`](https://github.com/ALRubinger/aileron-connector-slack) repo. The connector runs as a sandboxed WASM module talking only to `slack.com:443`. The user's OAuth token lives in the Aileron vault and is injected host-side at the network boundary, so the connector code never sees the token bytes.

Posts are made with the authorizing user's token, not as a bot. Messages appear in Slack authored by the user. This is the right shape for the Monday catch-up demo and for power-user automation generally; it is the wrong shape if you want a dedicated app identity for posts.

## Prerequisite

The Aileron Slack app must be installed into your workspace. `aileron action add` (and `add-suite`) handles this for you: the CLI opens Slack's consent screen in your browser, and clicking Allow installs the app and authorizes your user in one step.

If your workspace requires admin approval for third-party apps, Slack shows a "Request to install" button instead of Allow. A workspace admin approves the request, then you re-run the same `aileron action add` command.

The Aileron Slack app is not on the Slack Marketplace, so there's no App Directory page to install from. The CLI is the install path.

## Install

### Whole suite (recommended)

```sh
aileron action add-suite github://ALRubinger/aileron-connector-slack/suite.toml@latest
```

### Individual actions

Pick whichever actions you actually want exposed to the agent.

```sh
aileron action add github://ALRubinger/aileron-connector-slack/actions/list-channels@latest
```

```sh
aileron action add github://ALRubinger/aileron-connector-slack/actions/search-messages@latest
```

```sh
aileron action add github://ALRubinger/aileron-connector-slack/actions/send-message@latest
```

## Actions

### `list-channels`

Lists the Slack channels the user has access to in the workspace. Each entry carries `id`, `name`, `topic`, `purpose`, `is_member`, and the rest of Slack's [`conversations.list`](https://api.slack.com/methods/conversations.list) envelope. By default only public channels are returned.

| Input | Required | Description |
|---|---|---|
| `limit` | no | Page size. Default 100, max 1000. |
| `types` | no | Comma-separated channel types: `public_channel`, `private_channel`, `mpim`, `im`. Default `public_channel`. |
| `cursor` | no | Pagination cursor returned from a previous call's `response_metadata.next_cursor`. |
| `exclude_archived` | no | Skip archived channels. Default `true`. |

Idempotent. Read-only. No approval prompt.

### `search-messages`

Searches the user's Slack message history using Slack's native search syntax. Returns Slack's [`search.messages`](https://api.slack.com/methods/search.messages) envelope; the `messages.matches` array carries the hits with `text`, `user`, `username`, `channel`, `ts`, and `permalink`.

| Input | Required | Description |
|---|---|---|
| `query` | yes | Slack search query. Supports `from:@user`, `in:#channel`, `before:YYYY-MM-DD`, `after:YYYY-MM-DD`, `has:link`, `has:reaction`. Example: `from:@alice deploy in:#release-notes after:2026-04-01`. |
| `count` | no | Results per page. Default 20, max 100. |
| `sort` | no | `score` (relevance, default) or `timestamp` (newest first). |
| `page` | no | 1-based page index. Default 1. |

Idempotent. Read-only. No approval prompt.

`search.messages` is one of Slack's user-token-only APIs. The connector's OAuth dance requests the `search:read` user scope so this action works; if the user denies that scope at consent, the action returns a `missing_scope` error.

### `send-message`

Posts a message to a Slack channel as the authorizing user. The user is prompted to approve the channel and exact message text before Slack receives the post.

| Input | Required | Description |
|---|---|---|
| `channel` | yes | Channel id (e.g. `C0123456`) or channel name (e.g. `#general`). DM ids (`Dxxxx`) and group ids (`Gxxxx`) are also accepted by Slack. |
| `text` | yes | The message body to post. The user will be asked to approve this exact text before Slack receives it. |
| `thread_ts` | no | Parent message timestamp (e.g. `1683750000.000100`) to reply in thread. Omit to post a top-level message. |

**Approval-gated** ([ADR-0009](/adr/0009-user-channel)). The runtime asks the user via the launch-comms channel before Slack is contacted; on denial nothing is sent. **Not idempotent**: invoking twice posts two messages. Slack has a brief edit window but no retraction, and notifications and thread bumps fire on the original post, so approve-before-dispatch is the only safe model.

## Scopes the consent screen asks for

Slack's OAuth consent screen names the connector publisher (`ALRubinger`) and the requested user-token scope set:

| Scope | Used by |
|---|---|
| `chat:write` | `send-message`. Posts appear as the authorizing user. |
| `channels:read` | `list-channels`. |
| `users:read` | `search-messages`. Resolves user ids to display names in hits. |
| `search:read` | `search-messages`. User-token-only; this is what forces the OAuth dance to issue a user token rather than a bot token. |

Per [ADR-0002](/adr/0002-connector-model)'s OAuth section, the consent screen is a contract between the user and the entity identified on it. The user is granting these scopes to the connector publisher's Slack app, not to Aileron itself.

## Error classes

The connector emits structured errors per [ADR-0010](/adr/0010-failure-handling). Slack returns most logical failures as HTTP 200 with `{"ok": false, "error": "<code>"}`, so the connector translates both transport-level and in-body errors into the same class set.

| Class | When | What the user sees |
|---|---|---|
| `unauthorized` | HTTP 401, or `ok=false` with error `not_authed`, `invalid_auth`, `token_revoked`, `account_inactive`, or `token_expired`. | The bound OAuth token is rejected. Re-run `aileron binding setup github://ALRubinger/aileron-connector-slack` to re-consent. |
| `missing_scope` | `ok=false` with error `missing_scope` or `not_allowed_token_type`. | The OAuth grant is missing a scope the action requires. Re-run `aileron binding setup` to grant the missing scope. |
| `rate_limited` | HTTP 429 or `ok=false` with error `ratelimited`. | Slack is throttling. The runtime's retry layer will respect Slack's `Retry-After` header for transient backoff. |
| `external_api_error` | Any other Slack-reported error. | Body is included for the agent and the audit log. |
| `connector_runtime_error` | Malformed input, unparseable response, or a missing required arg. | Generic; check the action's required inputs. |

## See also

- [Installing an Action](/guides/installing-an-action/) for the general install flow.
- [aileron-connector-slack](https://github.com/ALRubinger/aileron-connector-slack) for connector source.
- [Slack API: chat.postMessage](https://api.slack.com/methods/chat.postMessage), [conversations.list](https://api.slack.com/methods/conversations.list), [search.messages](https://api.slack.com/methods/search.messages) for the upstream API surface.
- [ADR-0002: Connector Model](/adr/0002-connector-model)
- [ADR-0009: User Channel and Approval Surfaces](/adr/0009-user-channel)
