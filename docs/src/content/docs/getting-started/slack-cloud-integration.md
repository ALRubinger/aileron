---
title: "Slack Cloud Integration"
description: "Always-on Slack agent with streaming drafts, message shortcuts, and slash commands"
---

The Slack cloud integration turns Aileron into a Slack agent. You can draft replies, ask questions, and write messages — all from within Slack. Always-on, no `aileron launch` required.

This is separate from the [local Slack integration](/getting-started/slack-integration), which uses Socket Mode and requires an active terminal session. Both can coexist.

## How it works

There are three ways to interact with Aileron in Slack:

### Message shortcut

Hover over any message → click `⋯` → **"Draft reply with Aileron"**. A modal opens in your current channel with the AI-generated draft. Edit it, add refinement instructions, and click **Send** — the reply is posted as you.

### Agent DM

Open the Aileron app in Slack and start a conversation. Aileron shows suggested prompts and streams responses in real time. You can iterate conversationally ("Make it shorter", "Add context about the deadline") and click **Send** when satisfied.

### `/aileron` slash command

Type `/aileron Draft me a weekly status update` in any channel. A modal opens with the generated draft. Or ask a question — `/aileron How many hours on calls today?` — and get an ephemeral answer.

| Entry point | Best for | Response surface |
|---|---|---|
| Message shortcut (⋯ menu) | Replying to a specific message | Modal in current channel |
| Agent DM | Free-form writing and conversation | Streaming DM thread |
| `/aileron` command | Quick drafts or questions in context | Modal (drafts) or ephemeral (questions) |

In all cases, replies are sent as **you** (via your user token), not as the bot.

## Setup

Setup has two parts, performed by different people:

1. **[Installing the Aileron Slack App](/getting-started/slack-app-install)** — A workspace admin creates and configures the Slack app, sets up the Aileron server, and installs the bot to the workspace. Done once per workspace.

2. **[Connecting your Slack account](/getting-started/slack-connect)** — Each user connects their own Slack account to Aileron via OAuth. Takes under a minute.

## Architecture

```
Slack workspace
    │
    ├── Message shortcut (⋯ → "Draft reply")
    ├── Agent DM (message.im)
    ├── /aileron slash command
    │
    ▼
Aileron Cloud
    │
    ├─ Verify HMAC-SHA256 signature
    ├─ Deduplicate by event_id
    ├─ Route by event type:
    │   ├─ assistant_thread_started → suggested prompts + title
    │   ├─ message.im → agent handler (streaming draft)
    │   ├─ message_action → open modal, generate draft
    │   └─ /aileron command → modal (draft) or ephemeral (question)
    │
    ▼
Draft Generation Pipeline
    │
    ├─ Round 1: Research — LLM gathers context via tools
    │   ├─ LLM may call tools (e.g. slack_channel_history)
    │   ├─ Aileron executes tools with user's OAuth token
    │   └─ Output: structured context summary
    │
    ├─ Round 2: Ghostwrite — LLM composes the reply
    │   ├─ Streaming: text deltas flow to Slack in real time
    │   └─ Output: the draft
    │
    ▼
Delivery
    ├─ Agent DM: streamed via chat.startStream/appendStream/stopStream
    ├─ Modal: views.update with editable draft + Send button
    └─ Slash command question: ephemeral response via response_url
    │
    ▼
User clicks Send → Aileron posts reply as user via xoxp- token
```

## Context retrieval tools

The LLM can call these tools during draft generation:

| Tool | Description |
|------|-------------|
| `slack_channel_history` | Recent messages in a channel |
| `slack_thread_replies` | Replies in a thread |
| `slack_search_messages` | Search messages across channels |

## Draft lifecycle API

Drafts are also available via REST:

| Endpoint | Description |
|----------|-------------|
| `GET /v1/drafts?status=pending` | List pending drafts |
| `POST /v1/drafts/{id}/approve` | Approve and send |
| `POST /v1/drafts/{id}/edit` | Edit body and send |
| `POST /v1/drafts/{id}/discard` | Discard |

## Security

- **Signature verification:** HMAC-SHA256 with the signing secret. Invalid or stale (>5min) signatures rejected.
- **No JWT auth on webhooks:** The webhook endpoints are excluded from Aileron's JWT middleware — Slack calls them directly. Signature verification provides authentication.
- **Event deduplication:** In-memory TTL map by `event_id` (5 minutes).
- **Token storage:** User OAuth tokens stored in the user vault (encrypted with per-user KEK). Bot tokens in the system vault (encrypted with system key).
- **Read/write boundary (ADR-0019):** The LLM reads via tools. Aileron owns all writes (sending messages). User approval required via Send button.
