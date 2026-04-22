---
title: "Connecting Your Slack Account"
description: "Connect your Slack account to Aileron and start drafting"
---

This guide is for **users** who want to use Aileron in Slack. A workspace admin must have already [installed the Aileron Slack app](/getting-started/slack-app-install) to your workspace.

For an overview of what the integration does, see [Slack Cloud Integration](/getting-started/slack-cloud-integration).

## Connect your account

Open this URL in your browser (you must be logged into Aileron):

```
https://your-domain/v1/connect/slack
```

This starts a user-level OAuth flow. Slack asks you to authorize Aileron to read your messages and send on your behalf. This does **not** install the bot — your admin already did that.

Verify the connection:

```sh
curl -H "Authorization: Bearer $TOKEN" \
  https://your-domain/v1/connected-accounts
```

Should show your Slack account with `status: active`.

## Using Aileron in Slack

### Message shortcut

1. Find any message in a channel
2. Hover → `⋯` → **Draft reply with Aileron**
3. A modal opens showing "Researching..." → then the draft appears in an editable text area
4. Edit the draft or add refinement instructions → click **Send**
5. The reply appears in the channel from your account

### Agent DM

1. Open the Aileron app in Slack (sidebar → Apps → Aileron)
2. See suggested prompts: "Draft a reply", "Write a message", "What needs attention?"
3. Click a prompt or type a message
4. Watch the thinking status and streamed response
5. Iterate: "Make it more concise" → refined draft streams back

### `/aileron` slash command

1. In any channel, type: `/aileron Draft me a weekly status update`
2. A modal opens with the generated draft
3. Edit and click **Send**

Or ask a question: `/aileron How many hours on calls today?` → ephemeral answer appears.

## Troubleshooting

| Symptom | Likely cause |
|---------|-------------|
| "Connect" page shows an error | Aileron server may be down, or public distribution not enabled — ask your admin |
| Connected but no response from Aileron | Bot not installed to workspace — ask your admin to [install the app](/getting-started/slack-app-install) |
| Message shortcut not in ⋯ menu | Shortcut not registered — ask your admin |
| `/aileron` command not found | Slash command not registered — ask your admin |
| "Could not find your Aileron account" | You haven't connected yet — visit the connect URL above |
| "Your vault is locked" | Unlock your vault in Aileron before using Slack features |
| Duplicate key on reconnect | Disconnect first (`DELETE /v1/connected-accounts/{id}`), then reconnect |
