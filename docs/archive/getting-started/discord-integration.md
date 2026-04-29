---
title: "Discord Integration"
description: "Receive and reply to Discord messages from your terminal"
---

Aileron can receive Discord messages in your terminal. The setup mirrors Slack, and both listeners can run simultaneously.

## 1. Create a Discord bot

In the [Discord Developer Portal](https://discord.com/developers/applications):

- Create a new application, then add a **Bot** under the Bot tab
- Copy the **Bot Token**
- Under **Privileged Gateway Intents**, enable **Message Content Intent**
- Generate an invite link under **OAuth2 > URL Generator** with the `bot` scope and these permissions: `Read Messages/View Channels`, `Send Messages`, `Read Message History`
- Invite the bot to your server using the generated URL

## 2. Get channel IDs

In Discord, enable Developer Mode (User Settings > Advanced > Developer Mode). Right-click a channel and **Copy Channel ID**.

## 3. Add the config to aileron.yaml

```yaml
notifications:
  discord:
    bot_token: "your-bot-token"
    channels:
      - name: "1234567890123456789"   # channel ID
        show: all
        auto_draft: true
      - name: "9876543210987654321"
        show: all
        priority: high
    ignore:
      - "1111111111111111111"          # channel ID to ignore
```

## 4. Launch as usual

```sh
aileron launch claude
```

Discord messages appear alongside Slack messages in the same notification bar and overlay.
