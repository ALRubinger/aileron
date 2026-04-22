---
title: "Installing the Aileron Slack App"
description: "Workspace admin guide: create, configure, and install the Aileron Slack app"
---

This guide is for **workspace admins** (or the developer deploying Aileron). It covers creating the Slack app, configuring the Aileron server, and installing the bot to your workspace.

Once complete, users can [connect their Slack accounts](/getting-started/slack-connect) to start using Aileron.

For an overview of what the integration does, see [Slack Cloud Integration](/getting-started/slack-cloud-integration).

## 1. Create a Slack App

Go to [api.slack.com/apps](https://api.slack.com/apps) and create a new app from scratch.

### Enable Agents & AI Apps

Sidebar → **Agents & AI Apps** → toggle ON. This enables the agent DM experience with suggested prompts, thinking indicators, and streaming responses.

### OAuth & Permissions

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

The first handles user OAuth (when individual users connect their accounts). The second handles bot installation (this guide).

### App Credentials

From **Basic Information**, note:

- **Client ID** → `SLACK_CLIENT_ID`
- **Client Secret** → `SLACK_CLIENT_SECRET`
- **Signing Secret** → `SLACK_SIGNING_SECRET`

### Enable public distribution

By default, a Slack app can only be installed in the workspace where it was created. To allow users from any workspace to connect:

1. Sidebar → **Manage Distribution**
2. Under "Share Your App with Other Workspaces", ensure all checklist items are complete
3. Click **Activate Public Distribution**

Without this step, users outside the development workspace will see `invalid_team_for_non_distributed_app` when trying to connect.

> **Do NOT configure Event Subscriptions, Interactivity, or Slash Commands yet.** The Aileron server must be running first — see step 3.

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
enabled Slack Events API webhook, interaction, command, and install endpoints
```

## 3. Enable Event Subscriptions, Interactivity, and Slash Commands

The server must be running before this step — Slack sends verification challenges immediately.

### Event Subscriptions

1. Sidebar → **Event Subscriptions** → toggle ON
2. **Request URL:** `https://your-domain/v1/webhooks/slack/events`
3. Wait for the green checkmark ✓
4. Under **Subscribe to bot events**, add:
   - `assistant_thread_started`
   - `assistant_thread_context_changed`
   - `message.im`
5. Click **Save Changes**

### Interactivity & Shortcuts

1. Sidebar → **Interactivity & Shortcuts** → toggle ON
2. **Request URL:** `https://your-domain/v1/webhooks/slack/interactions`
3. Under **Shortcuts**, create a **Message Shortcut**:
   - **Name:** Draft reply with Aileron
   - **Short Description:** Generate an AI-drafted reply to this message
   - **Callback ID:** `draft_reply`
4. Click **Save Changes**

### Slash Commands

1. Sidebar → **Slash Commands** → **Create New Command**
2. **Command:** `/aileron`
3. **Request URL:** `https://your-domain/v1/webhooks/slack/commands`
4. **Short Description:** Draft messages or ask questions with AI
5. **Usage Hint:** `[draft a reply | ask a question]`
6. Click **Save**

## 4. Install to workspace

Install the bot to the workspace. This grants the bot token (`xoxb-...`) needed for Aileron to respond in Slack.

- **Sidebar → Install App → Install to Workspace** (or **Reinstall** if updating an existing install)

When you authorize the app, Slack redirects to `https://your-domain/v1/slack/install/callback`. Aileron exchanges the code for a bot token and stores it in the [system vault](/getting-started/credential-vault#system-vault) (encrypted at rest, keyed by workspace). You'll see a success page and can close the tab.

Users do **not** need to repeat this step — they only [connect their own accounts](/getting-started/slack-connect).

## Troubleshooting

| Symptom | Likely cause |
|---------|-------------|
| `invalid_team_for_non_distributed_app` | Public distribution not enabled |
| url_verification fails | Server not running, wrong URL, signing secret mismatch |
| No events arriving | App not installed, event subscriptions not saved, Agents feature not enabled |
| Agent DM shows no suggested prompts | `assistant_thread_started` event not subscribed, or bot token missing from system vault |
| Message shortcut missing from ⋯ menu | Shortcut not registered in Slack app settings, or callback_id mismatch |
| `/aileron` command not found | Slash command not registered, wrong request URL |
| Draft generated but modal doesn't update | `trigger_id` expired (>3s), check server logs for modal update errors |
| Interactivity buttons don't work | Interactivity not enabled, wrong Request URL |
