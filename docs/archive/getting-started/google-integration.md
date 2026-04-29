---
title: "Google Integration"
description: "Connect Gmail, Google Calendar, and Google Drive for context in AI-drafted replies"
---

Connect your Google account so the LLM can search emails, check your calendar, and read Google Docs when drafting replies. When someone asks about a meeting or references an email thread, the draft can pull in real context instead of guessing.

One Google OAuth app covers all three services: **Gmail**, **Google Calendar**, and **Google Drive**.

## What this enables

Google tools become available for the LLM during draft generation:

| Tool | Service | Description |
|------|---------|-------------|
| `gmail_search` | Gmail | Search emails by sender, subject, date range |
| `gmail_get_thread` | Gmail | Read a full email thread |
| `drive_search` | Drive | Search Google Drive files by name or content |
| `drive_get_doc` | Drive | Get the text content of a Google Doc |
| `calendar_events` | Calendar | Get events in a date range with attendees and locations |
| `calendar_free_busy` | Calendar | Check free/busy status for a time range |

## Example

Without Google context:
> "Let me check and get back to you on timing."

With Google context (LLM checks calendar, finds the thread):
> "Thursday works — you're free 2-4pm and the budget thread from Sarah confirms the numbers are finalized."

## 1. Create a Google OAuth App

> **Separate from sign-in.** If you already use Google OAuth for Aileron sign-in (`GOOGLE_SIGNIN_*`), you need a **second** OAuth client for connected accounts. Sign-in uses narrow scopes (`openid`, `email`, `profile`); the connector needs access to Gmail, Calendar, and Drive.

1. Go to the [Google Cloud Console](https://console.cloud.google.com/apis/credentials)
2. Select your project (or create one)
3. Click **Create Credentials** → **OAuth client ID**
4. Select **Web application** as the application type
5. Give it a name (e.g. `Aileron Connected Accounts`)

### Authorized redirect URIs

Add these redirect URIs:

```
https://api.yourdomain.com/v1/connect/gmail/callback
https://api.yourdomain.com/v1/connect/google_calendar/callback
```

Replace `api.yourdomain.com` with your actual Aileron API domain (e.g. `api.withaileron.ai`).

### Note the credentials

After creating the client:

- **Client ID** → `GOOGLE_CONNECTOR_CLIENT_ID`
- **Client secret** → `GOOGLE_CONNECTOR_CLIENT_SECRET`

### Enable the APIs

In the [API Library](https://console.cloud.google.com/apis/library), enable:

- **Gmail API**
- **Google Calendar API**
- **Google Drive API**

All three must be enabled in the same project as your OAuth client.

### Configure data access scopes

In the [Google Auth Platform](https://console.cloud.google.com/auth/scopes), go to **Data Access** and add these scopes:

- `https://www.googleapis.com/auth/gmail.readonly`
- `https://www.googleapis.com/auth/gmail.send`
- `https://www.googleapis.com/auth/gmail.compose`
- `https://www.googleapis.com/auth/drive.readonly`
- `https://www.googleapis.com/auth/calendar`
- `https://www.googleapis.com/auth/calendar.events`
- `https://www.googleapis.com/auth/userinfo.email`

### Configure the audience

If you haven't already, configure the OAuth consent screen under **Google Auth Platform** → **Audience**:

1. Select **External** (or **Internal** for Google Workspace orgs)
2. Fill in the required fields (app name, support email, developer contact)
3. Add your email as a **test user** (required while the app is in "Testing" status)

> **Publishing status:** While in "Testing", only users listed as test users can authorize. Move to "Production" when ready to remove this restriction. Google may require a verification review for sensitive scopes.

## 2. Configure environment variables

Set these on your Aileron cloud server:

| Variable | Value |
|----------|-------|
| `GOOGLE_CONNECTOR_CLIENT_ID` | Client ID from the OAuth client |
| `GOOGLE_CONNECTOR_CLIENT_SECRET` | Client secret from the OAuth client |

> **Note:** These are separate from `GOOGLE_SIGNIN_CLIENT_ID` / `SECRET` used for Aileron login. Using the same credentials for both will cause scope and callback URL conflicts.

Restart the server. Verify the logs show:

```
enabled Google connected accounts and source connectors (Gmail, Calendar)
```

If you don't see this line, the environment variables are missing or empty.

## 3. Connect Gmail

Open in browser (must be logged into Aileron):

```
https://your-domain/v1/connect/gmail
```

Google's OAuth consent screen appears. Authorize the requested scopes (email, Gmail, Drive).

## 4. Connect Google Calendar

Open in browser (must be logged into Aileron):

```
https://your-domain/v1/connect/google_calendar
```

Authorize the Calendar scopes.

> Gmail and Calendar are connected separately because they request different scopes and store independent tokens. You can connect one without the other.

## 5. Verify

```sh
curl -H "Authorization: Bearer $TOKEN" \
  https://your-domain/v1/connected-accounts
```

Should show entries for `gmail` and/or `google_calendar` with `status: active`.

You can also verify in the Aileron UI under **Settings** → **Connected Accounts**.

## Troubleshooting

| Symptom | Likely cause |
|---------|-------------|
| "unsupported provider: google_calendar" | `GOOGLE_CONNECTOR_CLIENT_ID` / `SECRET` not set or empty — the server didn't register the Google provider |
| OAuth consent screen shows wrong scopes | Wrong OAuth client — make sure you're using the connector client, not the sign-in client |
| "Access blocked: app has not been verified" | App is in Testing mode and your account is not listed as a test user |
| "No refresh token returned" | User already authorized this app before. Revoke access at [myaccount.google.com/permissions](https://myaccount.google.com/permissions), then reconnect |
| 403 from Gmail/Calendar/Drive API | API not enabled in the Google Cloud project. Enable it in the [API Library](https://console.cloud.google.com/apis/library) |
| "redirect_uri_mismatch" | The callback URL doesn't match what's configured in Google Cloud Console. Check for typos, trailing slashes, and http vs https |

## Security

- **Read-only for Gmail and Drive:** The LLM reads emails, threads, and documents via tools. Gmail send/compose scopes are present for future approved-action support but are not used by the LLM.
- **Read-write for Calendar:** Calendar scopes allow event creation/modification — these will be gated by the approval system (ADR-0019) when calendar execution is implemented.
- **Your permissions:** Tools use your OAuth token, so the LLM can only see data you have access to.
- **Token storage:** OAuth refresh tokens are stored in the vault, encrypted at rest when TEE mode is enabled.
