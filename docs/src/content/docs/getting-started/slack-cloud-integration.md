---
title: "Slack Cloud Integration"
description: "Always-on Slack event ingestion and AI-drafted replies via cloud webhook"
---

The Slack cloud integration receives Slack messages at Aileron's cloud endpoint, generates AI-drafted replies with source context, and presents them for review as ephemeral messages in Slack. Always-on — no `aileron launch` required.

This is separate from the [local Slack integration](/getting-started/slack-integration), which uses Socket Mode and requires an active terminal session. Both can coexist.

## How it works

1. You connect your Slack account to Aileron via OAuth
2. Slack sends message events to Aileron's webhook endpoint
3. Aileron generates a context-aware draft reply using the LLM and source connector tools
4. An ephemeral message appears in the Slack channel (visible only to you) with the draft and Approve/Edit/Discard buttons
5. You click Approve — the reply is sent as you, not as a bot

## 1. Create a Slack App

Go to [api.slack.com/apps](https://api.slack.com/apps) and create a new app from scratch.

### OAuth & Permissions

Under **Bot Token Scopes**, add:

- `chat:write` — for posting ephemeral draft previews (visible only to you)

Under **User Token Scopes**, add:

- `channels:history` — read messages in public channels
- `channels:read` — list channels and their info
- `chat:write` — send messages as the user
- `search:read` — search message history for context
- `users:read` — look up user names

Under **Redirect URLs**, add both:

```
https://your-domain/v1/connect/slack/callback
https://your-domain/v1/slack/install/callback
```

The first handles user OAuth (connecting individual accounts). The second handles bot installation (when a workspace admin installs the app from the Slack App Directory).

### App Credentials

From **Basic Information**, note:

- **Client ID** → `SLACK_CLIENT_ID`
- **Client Secret** → `SLACK_CLIENT_SECRET`
- **Signing Secret** → `SLACK_SIGNING_SECRET`

### Install to workspace (admin, once per workspace)

A workspace admin installs the bot once. This grants the bot token (`xoxb-...`) needed for Aileron to respond in Slack. The admin can install via:

- **Slack App Directory** — if public distribution is enabled
- **Sidebar → Install App → Install to Workspace** — for the development workspace

When the admin authorizes the app, Slack redirects to `https://your-domain/v1/slack/install/callback`. Aileron exchanges the code for a bot token and stores it in the [system vault](/getting-started/credential-vault#system-vault) (encrypted at rest, keyed by workspace). The admin sees a success page and can close the tab.

Individual users do **not** need to install the app — they only connect their own account (step 4), which requests user-level scopes without repeating the bot install prompt.

### Enable public distribution

By default, a Slack app can only be installed in the workspace where it was created. To allow users from any workspace to connect:

1. Sidebar → **Manage Distribution**
2. Under "Share Your App with Other Workspaces", ensure all checklist items are complete (redirect URLs, bot user, app description)
3. Click **Activate Public Distribution**

Without this step, users outside the development workspace will see `invalid_team_for_non_distributed_app` when trying to connect.

> **Do NOT configure Event Subscriptions or Interactivity yet.** The Aileron server must be running first. See step 3.

## 2. Configure environment variables

Set these on your Aileron cloud server:

```sh
SLACK_CLIENT_ID=your-client-id
SLACK_CLIENT_SECRET=your-client-secret
SLACK_SIGNING_SECRET=your-signing-secret

# For AI-powered draft generation:
ANTHROPIC_API_KEY=sk-ant-your-key

# Optional — these are the defaults:
AILERON_LLM_MODEL_RESEARCH=claude-haiku-4-5-20251001   # fast model for tool-call decisions
AILERON_LLM_MODEL_SYNTHESIS=claude-sonnet-4-6           # capable model for composing the reply
```

The draft pipeline uses two models: a fast model gathers context via tool calls (research), and a capable model composes the reply in your voice (synthesis). This keeps latency low without sacrificing quality.

You also need `AILERON_SYSTEM_VAULT_KEY` configured — the Slack bot token (from workspace installation) is stored in the [system vault](/getting-started/credential-vault#system-vault), which requires this key for at-rest encryption.

Verify the server logs show:

```
enabled Slack connected accounts and source connector
enabled cloud draft generation  research_model=claude-haiku-4-5-20251001  synthesis_model=claude-sonnet-4-6
enabled Slack Events API webhook and interaction endpoints
```

## 3. Enable Event Subscriptions and Interactivity

The server must be running before this step — Slack sends verification challenges immediately.

### Event Subscriptions

1. Sidebar → **Event Subscriptions** → toggle ON
2. **Request URL:** `https://your-domain/v1/webhooks/slack/events`
3. Wait for the green checkmark ✓
4. Under **Subscribe to events on behalf of users**, add: `message.channels`
5. Click **Save Changes**

### Interactivity

1. Sidebar → **Interactivity & Shortcuts** → toggle ON
2. **Request URL:** `https://your-domain/v1/webhooks/slack/interactions`
3. Click **Save Changes**

### Reinstall

Sidebar → **Install App** → **Reinstall to Workspace** if prompted.

## 4. Connect your Slack account

Each user connects their own Slack account. This is a user-level OAuth flow — it only asks for user consent (the scopes listed under "User Token Scopes" above). It does **not** repeat the bot installation.

Open in browser (must be logged into Aileron):

```
https://your-domain/v1/connect/slack
```

Verify:

```sh
curl -H "Authorization: Bearer $TOKEN" \
  https://your-domain/v1/connected-accounts
```

Should show your Slack account with `status: active`.

## 5. Invite the bot to channels

The bot must be a member of channels where you want to receive draft previews:

```
/invite @Aileron
```

The bot is silent — it only posts ephemeral messages (visible to you, invisible to teammates). Replies are sent via your user token.

## 6. Test it

Have someone send a message in a channel. You should see:

1. Server logs: `draft pending review` and `ephemeral draft delivered`
2. An ephemeral message in Slack with the draft and Approve/Edit/Discard buttons
3. Click **Approve** → reply appears from your account

## Context retrieval tools

The LLM can call these tools during draft generation:

| Tool | Description |
|------|-------------|
| `slack_channel_history` | Recent messages in a channel |
| `slack_thread_replies` | Replies in a thread |
| `slack_search_messages` | Search messages across channels |

## Draft lifecycle API

Drafts are also available via REST (fallback when ephemeral delivery fails):

| Endpoint | Description |
|----------|-------------|
| `GET /v1/drafts?status=pending` | List pending drafts |
| `POST /v1/drafts/{id}/approve` | Approve and send |
| `POST /v1/drafts/{id}/edit` | Edit body and send |
| `POST /v1/drafts/{id}/discard` | Discard |

## Architecture

```
Slack workspace
    │
    │  Events API (HTTP POST)
    ▼
Aileron Cloud (/v1/webhooks/slack/events)
    │
    ├─ Verify HMAC-SHA256 signature
    ├─ Deduplicate by event_id
    ├─ Look up (team_id, user_id) → ConnectedAccount
    │
    ▼
Draft Generation Pipeline
    │
    ├─ Build system prompt + user instructions
    ├─ Resolve available tools from connected accounts
    ├─ Call LLM with tools
    │   ├─ LLM may call tools (e.g. slack_channel_history)
    │   ├─ Aileron executes tools with user's OAuth token
    │   └─ LLM generates draft from assembled context
    │
    ▼
Ephemeral message in Slack (Approve / Edit / Discard)
    │
    ▼
User approves → Aileron sends reply as user
```

## Security

- **Signature verification:** HMAC-SHA256 with the signing secret. Invalid or stale (>5min) signatures rejected.
- **No JWT auth on webhooks:** The webhook endpoints are excluded from Aileron's JWT middleware — Slack calls them directly. Signature verification provides authentication.
- **Event deduplication:** In-memory TTL map by `event_id` (5 minutes).
- **Token storage:** OAuth tokens stored in the vault (Postgres-backed, encrypted at rest in Phase 2).
- **Read/write boundary (ADR-0019):** The LLM reads via tools. Aileron owns all writes (sending messages). User approval required.

## Troubleshooting

| Symptom | Likely cause |
|---------|-------------|
| `invalid_team_for_non_distributed_app` | Public distribution not enabled — see "Enable public distribution" above |
| url_verification fails | Server not running, wrong URL, signing secret mismatch |
| No events arriving | App not installed, event subscriptions not saved, channel is private |
| Events arrive but no draft | `ANTHROPIC_API_KEY` not set, check logs for `draft generation failed` |
| Draft generated but no ephemeral | Bot not installed in workspace (admin must install first), bot not in channel, check `ephemeral:` errors |
| Buttons don't work | Interactivity not enabled, wrong Request URL |
| Duplicate key on reconnect | Disconnect first, then reconnect |
