---
title: "Slack Cloud Integration"
description: "Always-on Slack event ingestion via cloud webhook"
---

The Slack cloud integration receives Slack messages at Aileron's cloud endpoint, enabling always-on message handling without requiring `aileron launch` to be running. Messages arrive via the Slack Events API, are routed to the correct Aileron user, and will be available for context-aware draft generation.

This is separate from the [local Slack integration](/getting-started/slack-integration), which uses Socket Mode and requires an active terminal session. Both can coexist — use local for development, cloud for always-on.

## How it works

1. You connect your Slack account to Aileron via OAuth ("Connect Slack")
2. Slack sends message events to `https://your-domain/v1/webhooks/slack/events`
3. Aileron verifies the request signature, identifies your account by Slack team + user ID, and routes the event to your processing pipeline
4. Messages are sent as you (your OAuth user token), not as a bot

There is no bot in your channels. No bot avatar, no bot in the member list. Aileron uses your user token with your permissions.

## 1. Create a Slack app

Go to [api.slack.com/apps](https://api.slack.com/apps) and create a new app.

### OAuth & Permissions

Under **User Token Scopes**, add:

- `channels:history` — read messages in public channels
- `channels:read` — list channels and their info
- `chat:write` — send messages as the user
- `users:read` — look up user names

These are *user* scopes, not bot scopes. Aileron acts as the user, not as a bot.

### Event Subscriptions

Enable **Event Subscriptions** and set the Request URL to:

```
https://your-domain/v1/webhooks/slack/events
```

Slack will send a verification challenge to this URL. Aileron responds automatically once the webhook endpoint is running.

Under **Subscribe to events on behalf of users**, add:

- `message.channels` — messages in public channels

### App Credentials

From **Basic Information**, note:

- **Client ID** — for the OAuth flow
- **Client Secret** — for the OAuth flow
- **Signing Secret** — for webhook signature verification

## 2. Configure environment variables

Set these on your Aileron cloud server (Railway, Docker, etc.):

```sh
SLACK_CLIENT_ID=your-client-id
SLACK_CLIENT_SECRET=your-client-secret
SLACK_SIGNING_SECRET=your-signing-secret
```

All three are required. If any are missing, the Slack cloud integration is disabled.

## 3. Connect your Slack account

Once the server is running with the environment variables set:

1. Navigate to `https://your-domain/v1/connect/slack`
2. Slack's OAuth consent screen appears — authorize with your user scopes
3. Aileron stores your user token in the encrypted vault and creates a connected account record
4. Your Slack team ID and user ID are recorded for event routing

First user in a workspace may need Slack admin approval for the app. Subsequent users in the same workspace self-serve.

## 4. Verify webhook delivery

Once connected, Slack will start sending events to your webhook endpoint. Check the server logs for:

```
slack cloud message received  user_id=usr_xxx channel=C0BACKEND author=U123ABC
```

You can also check Slack's **Event Subscriptions** page for delivery status — it shows recent webhook attempts and response codes.

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
User's processing pipeline
    │
    ▼
Draft generation (future)
```

## Security

- **Signature verification:** Every webhook request is verified using HMAC-SHA256 with the signing secret. Invalid or stale (>5 minutes) signatures are rejected.
- **No JWT auth on webhook:** The webhook endpoint is excluded from Aileron's JWT auth middleware — Slack calls it directly. Signature verification provides authentication.
- **Event deduplication:** Slack may retry events. Aileron deduplicates by `event_id` with a 5-minute TTL.
- **Token storage:** OAuth user tokens are stored in the encrypted vault (see [Credential Vault](/getting-started/credential-vault)).

## Relationship to local Slack integration

| Concern | Local (Socket Mode) | Cloud (Events API) |
|---------|--------------------|--------------------|
| Connection | WebSocket from your machine | HTTP webhook to cloud server |
| Requires | `aileron launch` running | Cloud server running |
| Bot in channels | Yes (must invite bot) | No (user token only) |
| Token type | Bot + App tokens | User OAuth token |
| Always-on | No (active session only) | Yes |
| Configuration | `aileron.yaml` + vault secrets | Environment variables + OAuth |
