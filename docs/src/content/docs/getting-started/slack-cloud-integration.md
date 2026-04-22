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

---

## Admin setup

These steps are performed once by a **workspace admin** or the developer deploying Aileron. Regular users skip to [User setup](#user-setup) below.

### 1. Create a Slack App

Go to [api.slack.com/apps](https://api.slack.com/apps) and create a new app from scratch.

#### Enable Agents & AI Apps

Sidebar → **Agents & AI Apps** → toggle ON. This enables the agent DM experience with suggested prompts, thinking indicators, and streaming responses.

#### OAuth & Permissions

Under **Bot Token Scopes**, add:

- `assistant:write` — agent thread interactions (auto-added when Agents feature is enabled)
- `chat:write` — post messages and stream responses
- `im:history` — receive DM messages from users
- `commands` — register slash commands

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

The first handles user OAuth (connecting individual accounts). The second handles bot installation (this step).

#### App Credentials

From **Basic Information**, note:

- **Client ID** → `SLACK_CLIENT_ID`
- **Client Secret** → `SLACK_CLIENT_SECRET`
- **Signing Secret** → `SLACK_SIGNING_SECRET`

#### Enable public distribution

By default, a Slack app can only be installed in the workspace where it was created. To allow users from any workspace to connect:

1. Sidebar → **Manage Distribution**
2. Under "Share Your App with Other Workspaces", ensure all checklist items are complete (redirect URLs, bot user, app description)
3. Click **Activate Public Distribution**

Without this step, users outside the development workspace will see `invalid_team_for_non_distributed_app` when trying to connect.

> **Do NOT configure Event Subscriptions, Interactivity, or Slash Commands yet.** The Aileron server must be running first. See step 3.

### 2. Configure environment variables

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
enabled Slack Events API webhook, interaction, command, and install endpoints
```

### 3. Enable Event Subscriptions, Interactivity, and Slash Commands

The server must be running before this step — Slack sends verification challenges immediately.

#### Event Subscriptions

1. Sidebar → **Event Subscriptions** → toggle ON
2. **Request URL:** `https://your-domain/v1/webhooks/slack/events`
3. Wait for the green checkmark ✓
4. Under **Subscribe to bot events**, add:
   - `assistant_thread_started`
   - `assistant_thread_context_changed`
   - `message.im`
5. Click **Save Changes**

#### Interactivity & Shortcuts

1. Sidebar → **Interactivity & Shortcuts** → toggle ON
2. **Request URL:** `https://your-domain/v1/webhooks/slack/interactions`
3. Under **Shortcuts**, create a **Message Shortcut**:
   - **Name:** Draft reply with Aileron
   - **Short Description:** Generate an AI-drafted reply to this message
   - **Callback ID:** `draft_reply`
4. Click **Save Changes**

#### Slash Commands

1. Sidebar → **Slash Commands** → **Create New Command**
2. **Command:** `/aileron`
3. **Request URL:** `https://your-domain/v1/webhooks/slack/commands`
4. **Short Description:** Draft messages or ask questions with AI
5. **Usage Hint:** `[draft a reply | ask a question]`
6. Click **Save**

### 4. Install to workspace

Install the bot to the workspace. This grants the bot token (`xoxb-...`) needed for Aileron to respond in Slack.

- **Sidebar → Install App → Install to Workspace** (or **Reinstall** if updating an existing install)

When you authorize the app, Slack redirects to `https://your-domain/v1/slack/install/callback`. Aileron exchanges the code for a bot token and stores it in the [system vault](/getting-started/credential-vault#system-vault) (encrypted at rest, keyed by workspace). You'll see a success page and can close the tab.

Users do **not** need to repeat this step — they only connect their own account (see User setup below).

---

## User setup

These steps are performed by **each user** who wants to use Aileron in Slack. The workspace admin must have completed the [Admin setup](#admin-setup) first.

### Connect your Slack account

Open in browser (must be logged into Aileron):

```
https://your-domain/v1/connect/slack
```

This is a user-level OAuth flow — it asks for your consent to the user token scopes listed above. It does **not** repeat the bot installation.

Verify:

```sh
curl -H "Authorization: Bearer $TOKEN" \
  https://your-domain/v1/connected-accounts
```

Should show your Slack account with `status: active`.

### Test it

### Message shortcut

1. Find any message in a channel
2. Hover → `⋯` → **Draft reply with Aileron**
3. A modal opens showing "Researching..." → then the draft appears in an editable text area
4. Edit the draft or add instructions → click **Send**
5. The reply appears in the channel from your account

### Agent DM

1. Open the Aileron app in Slack (sidebar → Apps → Aileron)
2. See suggested prompts: "Draft a reply", "Write a message", "What needs attention?"
3. Click a prompt or type a message
4. Watch the thinking status and streamed response
5. Iterate: "Make it more concise" → refined draft streams back

### Slash command

1. In any channel, type: `/aileron Draft me a weekly status update`
2. A modal opens with the generated draft
3. Edit and click **Send**

Or ask a question: `/aileron How many hours on calls today?` → ephemeral answer appears.

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

## Security

- **Signature verification:** HMAC-SHA256 with the signing secret. Invalid or stale (>5min) signatures rejected.
- **No JWT auth on webhooks:** The webhook endpoints are excluded from Aileron's JWT middleware — Slack calls them directly. Signature verification provides authentication.
- **Event deduplication:** In-memory TTL map by `event_id` (5 minutes).
- **Token storage:** User OAuth tokens stored in the user vault (encrypted with per-user KEK). Bot tokens in the system vault (encrypted with system key).
- **Read/write boundary (ADR-0019):** The LLM reads via tools. Aileron owns all writes (sending messages). User approval required via Send button.

## Troubleshooting

| Symptom | Likely cause |
|---------|-------------|
| `invalid_team_for_non_distributed_app` | Public distribution not enabled — see "Enable public distribution" above |
| url_verification fails | Server not running, wrong URL, signing secret mismatch |
| No events arriving | App not installed, event subscriptions not saved, Agents feature not enabled |
| Agent DM shows no suggested prompts | `assistant_thread_started` event not subscribed, or bot token missing from system vault |
| Message shortcut missing from menu | Shortcut not registered in Slack app settings, or callback_id mismatch |
| `/aileron` command not found | Slash command not registered, wrong request URL |
| Draft generated but modal doesn't update | `trigger_id` expired (>3s), check server logs for modal update errors |
| Buttons don't work | Interactivity not enabled, wrong Request URL |
| Duplicate key on reconnect | Disconnect first, then reconnect |
