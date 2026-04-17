---
title: "GitHub Integration"
description: "Connect GitHub for code context in AI-drafted replies"
---

Connect your GitHub account so the LLM can search code, look up PRs, read diffs, and reference files when drafting replies. When someone asks about a code change in Slack, the draft can reference the actual PR and diff instead of guessing.

## 1. Create a GitHub OAuth App

GitHub OAuth Apps support only one callback URL. If you're already using a GitHub OAuth App for Aileron sign-in (`/auth/github/callback`), create a **separate** app for connected accounts.

1. Go to [github.com/settings/developers](https://github.com/settings/developers) (or your org's developer settings)
2. Click **OAuth Apps** → **New OAuth App**
3. Fill in:
   - **Application name:** `Aileron Connected Accounts` (must differ from any existing app)
   - **Homepage URL:** `https://withaileron.ai`
   - **Authorization callback URL:** `https://your-domain/v1/connect/github_repos/callback`
4. Click **Register application**
5. Note the **Client ID**
6. Click **Generate a new client secret** and note the **Client Secret**

## 2. Configure environment variables

Set on your Aileron cloud server:

| Variable | Value |
|----------|-------|
| `GITHUB_CONNECTOR_CLIENT_ID` | Client ID from the connected accounts OAuth App |
| `GITHUB_CONNECTOR_CLIENT_SECRET` | Client Secret from the connected accounts OAuth App |

> **Note:** These are separate from `GITHUB_SIGNIN_CLIENT_ID` / `SECRET` used for Aileron login. Using the same credentials for both will cause a callback URL conflict.

## 3. Connect your GitHub account

Open in browser (must be logged into Aileron):

```
https://your-domain/v1/connect/github_repos
```

GitHub's OAuth consent screen appears. Authorize with `repo` + `read:org` scopes.

Verify:

```sh
curl -H "Authorization: Bearer $TOKEN" \
  https://your-domain/v1/connected-accounts
```

Should show a `github_repos` account with `status: active`.

## What this enables

GitHub tools become available for the LLM during draft generation:

| Tool | Description | Parameters |
|------|-------------|------------|
| `github_search_code` | Search code across repos | `query` (required), `repo` (optional) |
| `github_search_issues` | Search issues and PRs | `query` (required), `repo` (optional) |
| `github_get_pr` | PR details with changed files | `repo`, `number` (required) |
| `github_get_pr_diff` | Actual diff of a PR | `repo`, `number` (required) |
| `github_get_file` | File contents | `repo`, `path` (required), `ref` (optional) |

## Example

Without GitHub context:
> "I'll look into it and get back to you."

With GitHub context (LLM searches issues, finds PR #247, reads the diff):
> "No, the claims stay the same. PR #247 only moves validation from the handler into middleware — the RolesClaim type in auth/jwt.go is unchanged."

## Security

- **Read-only access:** The LLM reads code and PRs via tools. It cannot push, merge, or modify anything.
- **Your permissions:** Tools use your OAuth token, so the LLM can only see repos you have access to. Private repos require the `repo` scope.
- **ADR-0019:** For enterprise data privacy, run your own LLM (Tier 2/3). Code data stays in your infrastructure.
