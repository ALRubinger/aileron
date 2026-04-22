---
title: "Installing Aileron to Your Slack Workspace"
description: "Workspace admin guide: install the Aileron app from the Slack App Directory"
---

This guide is for **workspace admins** who want to add Aileron to their Slack workspace. It takes about a minute.

Once installed, users in your workspace can [connect their Slack accounts](/getting-started/slack-connect) to start drafting replies, asking questions, and writing messages with AI.

For an overview of what Aileron does in Slack, see [Slack Cloud Integration](/getting-started/slack-cloud-integration).

## Install from the Slack App Directory

[![Add to Slack](https://platform.slack-edge.com/img/add_to_slack.png)](https://api.withaileron.ai/v1/slack/install)

Click the button above, review the requested permissions, and click **Allow**.

That's it. Aileron is now available in your workspace.

## What happens during installation

- Slack grants Aileron a **bot token** for your workspace
- Aileron stores this token in its [system vault](/getting-started/credential-vault#system-vault) (encrypted at rest, keyed by your workspace ID)
- The Aileron bot appears in your workspace's app list
- Users can open a DM with Aileron, use the `/aileron` command, and see the "Draft reply with Aileron" message shortcut

The bot token allows Aileron to post messages, stream responses, and set agent thread status. It does **not** give Aileron access to read messages in channels — that requires each user to [connect their own account](/getting-started/slack-connect) individually.

## Troubleshooting

| Symptom | Likely cause |
|---------|-------------|
| "This app is not available for your workspace" | The app may not be published to the App Directory yet — contact the Aileron team |
| Install succeeds but Aileron doesn't respond | Check that the Aileron server is running and the install callback URL is reachable |
| Users can't find Aileron in Slack | The app is installed but users need to search for "Aileron" in Apps or use `/aileron` |
| `/aileron` command not found | Try reinstalling the app, or check that the command is registered |
