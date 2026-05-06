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

## 3. Add the config to `~/.aileron/config.yaml`

The notifications block lives in the user-scoped `~/.aileron/config.yaml` per [ADR-0012](/adr/0012-local-daemon-architecture) — the daemon owns the Discord listener across launches.

Store the bot token in the vault and reference it; plaintext tokens are rejected.

```sh
aileron secret set discord_bot_token
```

```yaml
notifications:
  discord:
    bot_token: vault:discord_bot_token
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

## 4. Unlock the vault, launch as usual

```sh
aileron daemon start         # if not already running
aileron launch claude
```

Listener startup is deferred until the vault is unlocked. Discord messages then appear alongside Slack messages in the same notification surface.
