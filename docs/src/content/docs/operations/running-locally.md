---
title: "Running Locally"
description: "Run the full Aileron stack with Docker Compose"
---

For the full server/UI stack:

```sh
task up
```

This starts the API server, management UI, API documentation, and PostgreSQL. The API is available at `http://localhost:8080`, the UI at `http://localhost:3000`.

On first run, `task up` copies `deploy/.env.example` to `deploy/.env` with safe local defaults (including `AILERON_JWT_SIGNING_KEY`). No manual setup needed.

Email verification is disabled by default locally (`AILERON_AUTO_VERIFY_EMAIL=true`), so new accounts are activated immediately after signup.

## Customizing the local environment

Edit `deploy/.env` (gitignored) to customize. For example, to enable OAuth providers locally:

```sh
# deploy/.env
AILERON_JWT_SIGNING_KEY=local-dev-signing-key-not-for-production
GOOGLE_SIGNIN_CLIENT_ID=your-google-signin-client-id
GOOGLE_SIGNIN_CLIENT_SECRET=your-google-signin-client-secret
GOOGLE_CONNECTOR_CLIENT_ID=your-google-connector-client-id
GOOGLE_CONNECTOR_CLIENT_SECRET=your-google-connector-client-secret
GITHUB_SIGNIN_CLIENT_ID=your-github-signin-client-id
GITHUB_SIGNIN_CLIENT_SECRET=your-github-signin-client-secret
GITHUB_CONNECTOR_CLIENT_ID=your-github-connector-client-id
GITHUB_CONNECTOR_CLIENT_SECRET=your-github-connector-client-secret
```

Verification codes for email/password signup are printed to the server log when `RESEND_API_KEY` is not set (dev mode). To send real emails, add `RESEND_API_KEY` (and optionally `MAIL_FROM`) to your `.env`.

## Stopping

```sh
task down
```

## Auth environment variables

Docker Compose connects to PostgreSQL, which enables auth and requires a JWT signing key. Locally, `task up` handles this automatically. In CI, the workflow sets its own values directly. For other environments, create `deploy/.env` with at minimum:

```
AILERON_JWT_SIGNING_KEY=<any 32+ character string>
```
