---
title: "iMessage via BlueBubbles"
description: "Three actions for reading iMessage conversations and sending new ones through a local BlueBubbles bridge on macOS."
order: 2
---

The iMessage via BlueBubbles suite gives your agent three capabilities against the user's iMessage history: list recent conversations, read a single conversation, and send a new message. Send is gated by per-call user approval; reads run without prompting.

Aileron does not touch Messages.app directly. The connector talks to a local [BlueBubbles Server](https://bluebubbles.app/) running on the user's Mac, which holds the macOS Full Disk Access and Automation permissions and translates HTTP requests into reads of the Messages database and AppleScript-driven sends. The decision rationale lives at [issue #514](https://github.com/ALRubinger/aileron/issues/514).

The connector runs as a sandboxed WASM module talking only to `localhost:1234`; the BlueBubbles server password lives in the Aileron vault and is injected host-side at the network boundary — the connector code never sees the password bytes.

## Prerequisites

- **Install and configure BlueBubbles Server** per the [setup guide](/guides/setting-up-bluebubbles/). About five minutes, one-time per Mac. Without BlueBubbles running, every action returns a `bridge_unreachable` error with setup pointers.
- **Bind the BlueBubbles password** to your Aileron vault. The install flow below runs `aileron binding setup` to prompt for it once.

## Install

### Whole suite (recommended)

```sh
aileron keyring trust github://ALRubinger/aileron-connector-bluebubbles
aileron action add-suite github://ALRubinger/aileron-connector-bluebubbles/suite.toml@latest
aileron binding setup github://ALRubinger/aileron-connector-bluebubbles
```

The first command trusts the publisher's signing key (once per machine). The second pulls the suite manifest at the latest release and installs all three actions in declaration order, sharing the install consent prompt across the set. The third binds the BlueBubbles password to your vault.

### Individual actions

```sh
aileron keyring trust github://ALRubinger/aileron-connector-bluebubbles

aileron action add github://ALRubinger/aileron-connector-bluebubbles/actions/list-recent-chats@latest
aileron action add github://ALRubinger/aileron-connector-bluebubbles/actions/read-chat@latest
aileron action add github://ALRubinger/aileron-connector-bluebubbles/actions/send-message@latest

aileron binding setup github://ALRubinger/aileron-connector-bluebubbles
```

Pick whichever actions you actually want exposed to the agent. The connector, the trust grant, and the password binding are shared across all of them.

## Actions

### `list-recent-chats`

Lists iMessage chats from the user's Mac, ordered most-recent activity first. Each chat carries a `guid` you can pass to `read-chat` to fetch its messages. Returns the BlueBubbles `/api/v1/chat/query` envelope.

| Input | Required | Description |
|---|---|---|
| `limit` | no | How many chats to return. Default 25, max 100. |
| `offset` | no | Pagination offset from the most-recent end. Default 0. |

Idempotent. Read-only. No approval prompt.

### `read-chat`

Returns recent messages from a single iMessage chat, most-recent first. Use `list-recent-chats` first to discover the `chat_guid`. Returns the BlueBubbles `/api/v1/chat/{guid}/message` envelope.

| Input | Required | Description |
|---|---|---|
| `chat_guid` | yes | The chat GUID, as returned by `list-recent-chats`. Example: `iMessage;-;+15551234567`. |
| `limit` | no | How many messages to return, most-recent first. Default 50, max 200. |
| `offset` | no | Pagination offset. Default 0. |

Idempotent. Read-only. No approval prompt.

### `send-message`

Sends a text iMessage to an existing chat. The user is prompted to approve the recipient and exact message body before BlueBubbles dispatches.

| Input | Required | Description |
|---|---|---|
| `chat_guid` | yes | The target chat GUID. |
| `message` | yes | The message body to send. The user will be asked to approve this exact text before delivery. |

**Approval-gated** ([ADR-0009](/adr/0009-user-channel)). The runtime asks the user via the launch-comms channel before BlueBubbles is contacted; on denial nothing is sent. **Not idempotent** — the runtime's retry layer is configured to honor that and will not double-send on transient failure. Dispatched iMessages are not recoverable (no Drafts-folder analog), so approve-before-dispatch is the only safe model for write paths.

## Error classes

The connector emits structured errors per [ADR-0010](/adr/0010-failure-handling):

| Class | When | What the user sees |
|---|---|---|
| `bridge_unreachable` | BlueBubbles Server is not running on `localhost:1234`. | "Can't reach BlueBubbles Server. Open Applications and relaunch BlueBubbles Server. If you haven't installed it yet, follow the setup guide." |
| `unauthorized` | BlueBubbles returned 401 or 403. | The bound password doesn't match. Re-run `aileron binding setup github://ALRubinger/aileron-connector-bluebubbles`. |
| `external_api_error` | BlueBubbles returned a non-2xx that isn't 401/403. | Body is included for the agent and the audit log. |
| `connector_runtime_error` | Malformed input, unparseable response, or a missing required arg. | Generic; check the action's required inputs. |

## What this does not do

- **Not a phone bridge.** BlueBubbles' own iOS/Android client features (Firebase, etc.) are unrelated to Aileron's use of it. Skip them during BlueBubbles' setup wizard.
- **Not for received-message webhooks.** v1 is pull-only. Reacting to new incoming messages in real time is a v3 conversation.
- **Not a Mac-less option.** BlueBubbles needs the Mac to be running and signed in to iMessage.

## See also

- [Setting up BlueBubbles for Aileron](/guides/setting-up-bluebubbles/) — the prereq install + permissions walkthrough.
- [aileron-connector-bluebubbles](https://github.com/ALRubinger/aileron-connector-bluebubbles) — connector source.
- [BlueBubbles documentation](https://bluebubbles.app/docs/) — upstream project docs.
- [ADR-0002: Connector Model](/adr/0002-connector-model)
- [ADR-0009: User Channel and Approval Surfaces](/adr/0009-user-channel)
- [Issue #514](https://github.com/ALRubinger/aileron/issues/514) — the decision to use BlueBubbles rather than direct Messages.app access.
