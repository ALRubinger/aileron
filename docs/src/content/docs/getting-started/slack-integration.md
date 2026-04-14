---
title: "Slack Integration"
description: "Receive and reply to Slack messages from your terminal"
---

Aileron can receive Slack messages in your terminal while you work. Incoming messages appear in the status bar; press **Ctrl-]** to open the full notification overlay.

## 1. Create a Slack app

Create a Slack app with [Socket Mode](https://api.slack.com/apis/socket-mode) enabled. You need:

- An **App-Level Token** (`xapp-...`) with `connections:write` scope
- A **Bot Token** (`xoxb-...`) with `channels:history`, `channels:read`, `users:read` scopes for receiving messages
- (Optional) A **User OAuth Token** (`xoxp-...`) with `chat:write` scope for sending messages as yourself instead of the bot
- Subscribe to the `message.channels` event
- Invite the bot to channels it should listen on (`/invite @your-bot`)

If you only provide a bot token, replies are sent as the bot. Add a user token so replies come from your identity.

## 2. Store tokens in the encrypted vault

Tokens must be stored in the vault, not in plaintext config:

```sh
aileron secret set slack_app_token    # prompts for passphrase + token
aileron secret set slack_bot_token
aileron secret set slack_user_token   # optional, for sending as yourself
aileron secret list                   # shows stored names
```

## 3. Reference them in aileron.yaml

```yaml
notifications:
  slack:
    app_token: vault:slack_app_token
    bot_token: vault:slack_bot_token
    user_token: vault:slack_user_token   # optional, omit to send as bot
    channels:
      - name: "#backend"
        show: all
        auto_draft: true
      - name: "#incidents"
        show: all
        priority: high
    ignore:
      - "#random"
```

Token fields **must** use `vault:` references. Aileron rejects plaintext tokens to prevent secrets from being committed to version control.

## 4. Launch as usual

```sh
aileron launch claude
```

Aileron prompts for the vault passphrase, then starts the Slack listener.

## Using the notification overlay

Messages from configured channels appear in the notification bar. Press **Ctrl-]** to view the full queue.

| Key | Action |
|-----|--------|
| **j/k** or arrows | Navigate messages |
| **r** | Type a reply directly |
| **a** | Ask the agent to draft a reply using codebase context |
| **d** | Dismiss |
| **Escape** | Return |

When a draft is ready:

| Key | Action |
|-----|--------|
| **y** | Send |
| **e** | Edit |
| **c** | Revise with feedback |
| **n** | Discard |

On channels with `auto_draft: true`, messages are flagged for drafting. Open the overlay and press **a** to trigger the agent.
