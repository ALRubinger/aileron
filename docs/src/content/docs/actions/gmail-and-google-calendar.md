---
title: "Gmail and Google Calendar"
description: "Six actions for reading and writing Gmail and Google Calendar via the user's authenticated account."
order: 1
---

The Gmail and Google Calendar suite gives your agent six capabilities against the user's authenticated Google account: list and read email, draft and send new mail, list upcoming events, and create new events. Write paths (`send-email`, `create-calendar-event`) are gated by per-call user approval. Read paths run without prompting because they touch only the user's own data.

The suite is published from the [`aileron-connector-google`](https://github.com/ALRubinger/aileron-connector-google) repo. The connector runs as a sandboxed WASM module talking to `gmail.googleapis.com` and `www.googleapis.com`; the user's OAuth token lives in the Aileron vault and is injected host-side at the network boundary — the connector code never sees the token bytes.

## Install

### Whole suite (recommended)

```sh
aileron action add-suite github://ALRubinger/aileron-connector-google/suite.toml@latest
```

### Individual actions

Pick whichever actions you actually want exposed to the agent.

```sh
aileron action add github://ALRubinger/aileron-connector-google/actions/list-recent-emails@latest
```

```sh
aileron action add github://ALRubinger/aileron-connector-google/actions/get-email@latest
```

```sh
aileron action add github://ALRubinger/aileron-connector-google/actions/list-upcoming-events@latest
```

```sh
aileron action add github://ALRubinger/aileron-connector-google/actions/draft-email@latest
```

```sh
aileron action add github://ALRubinger/aileron-connector-google/actions/send-email@latest
```

```sh
aileron action add github://ALRubinger/aileron-connector-google/actions/create-calendar-event@latest
```

## Actions

### `list-recent-emails`

Lists recent Gmail messages, ordered most-recent first. Returns the raw `users.messages.list` response: `id`/`threadId` pairs plus paging metadata. The agent typically resolves bodies by following up with `get-email`.

| Input | Required | Description |
|---|---|---|
| `query` | no | Gmail search query, e.g. `is:unread` or `from:alice@example.com`. Empty fetches the most recent without filtering. |
| `max_results` | no | Page size. Default 10, max 100. |

Idempotent. Read-only. No approval prompt.

### `get-email`

Fetches headers and a body snippet for a single Gmail message. Uses `format=metadata` so the call returns Subject / From / To / Date / labelIds / snippet without pulling the full MIME body — a fast call cost for "what does this email say at a glance" agent flows.

| Input | Required | Description |
|---|---|---|
| `id` | yes | Gmail message id, as returned by `list-recent-emails`. |

Idempotent. Read-only. No approval prompt.

### `list-upcoming-events`

Lists upcoming events on a Google Calendar, chronological. Recurring events are expanded; `timeMin` is set to "now" so past events don't surface.

| Input | Required | Description |
|---|---|---|
| `calendar_id` | no | Calendar id. Default `primary`. |
| `max_results` | no | Page size. Default 10, max 100. |

Idempotent. Read-only. No approval prompt.

### `draft-email`

Creates an email draft in Gmail. The draft lands in the user's Drafts folder, where they review and send manually from Gmail's UI. This is the safer write path because it inserts a human review step naturally.

| Input | Required | Description |
|---|---|---|
| `to` | yes | Comma-separated recipient addresses. |
| `subject` | yes | Email subject line. |
| `body` | yes | Plain-text body. |
| `cc` | no | Comma-separated Cc addresses. |
| `bcc` | no | Comma-separated Bcc addresses. |

**Not idempotent**: invoking twice creates two drafts. No runtime-level approval prompt — the natural Gmail review step is what gates send.

### `send-email`

Sends an email from the user's Gmail account. Unlike `draft-email`, the message leaves the outbox immediately.

| Input | Required | Description |
|---|---|---|
| `to` | yes | Comma-separated recipient addresses. |
| `subject` | yes | Email subject line. |
| `body` | yes | Plain-text body. The user will be asked to approve this exact text before send. |
| `cc` | no | Comma-separated Cc addresses. |
| `bcc` | no | Comma-separated Bcc addresses. |

**Approval-gated** ([ADR-0009](/adr/0009-user-channel)). The runtime asks the user via the launch-comms channel before Gmail is contacted; on denial nothing is sent and no quota is burned. **Not idempotent** — the runtime's retry layer is configured to honor that and will not double-send on transient failure.

`draft-email` is the safer default for unattended flows; reach for `send-email` only when skipping the manual click is worth the approval prompt.

### `create-calendar-event`

Inserts a new event into a Google Calendar.

| Input | Required | Description |
|---|---|---|
| `title` | yes | Event title (Calendar's "summary" field). |
| `start_time` | yes | RFC3339 timestamp, e.g. `2026-05-04T15:00:00-07:00`. |
| `end_time` | yes | RFC3339 timestamp. |
| `timezone` | no | IANA timezone, e.g. `America/New_York`. |
| `description` | no | Long-form description. |
| `location` | no | Physical or virtual location. |
| `attendees` | no | Array of email addresses, or comma-separated string. |
| `calendar_id` | no | Calendar id. Default `primary`. |

**Approval-gated**. **Not idempotent** — invoking twice creates two events.

## Scopes the consent screen asks for

Google's OAuth consent screen names the connector publisher (`ALRubinger`) and the requested scope set:

- Gmail (restricted tier): read messages and create drafts.
- Calendar (sensitive tier): read events and create events.

Per [ADR-0002](/adr/0002-connector-model)'s OAuth section, the consent screen is a contract between the user and the entity identified on it. The user is granting these scopes to the connector publisher's OAuth app, not to Aileron itself.

## See also

- [Installing an Action](/guides/installing-an-action/) — the general install flow.
- [aileron-connector-google](https://github.com/ALRubinger/aileron-connector-google) — connector source.
- [ADR-0002: Connector Model](/adr/0002-connector-model)
- [ADR-0009: User Channel and Approval Surfaces](/adr/0009-user-channel)
