---
title: "Google"
description: "Twenty-one actions for the user's Google account: Gmail, Calendar, Contacts, Drive, and Docs."
order: 1
---

The Google suite gives your agent twenty-one capabilities against the user's authenticated Google account, spanning five surfaces: Gmail, Calendar, Contacts, Drive, and Docs. Read paths run without prompting because they touch only the user's own data. Write paths split: irreversible writes (`send-email`, `send-draft`) and third-party-observable writes (`move-file`) are gated by per-call user approval; reversible writes (`draft-email`, `create-calendar-event`, `create-doc`, `update-doc`, `upload-file`, `rename-file`) run without a runtime prompt because the API itself provides the safety net (Drafts folder, Docs revision history, Drive trash).

The suite is published from the [`aileron-connector-google`](https://github.com/ALRubinger/aileron-connector-google) repo. The connector runs as a sandboxed WASM module talking only to `gmail.googleapis.com`, `www.googleapis.com`, `people.googleapis.com`, and `docs.googleapis.com`; the user's OAuth token lives in the Aileron vault and is injected host-side at the network boundary — the connector code never sees the token bytes.

## Install

### Whole suite (recommended)

```sh
aileron action add-suite github://ALRubinger/aileron-connector-google/suite.toml@latest
```

### Individual actions

Pick whichever actions you actually want exposed to the agent. Grouped by surface:

```sh
# Gmail
aileron action add github://ALRubinger/aileron-connector-google/actions/list-recent-emails@latest
aileron action add github://ALRubinger/aileron-connector-google/actions/get-email@latest
aileron action add github://ALRubinger/aileron-connector-google/actions/list-drafts@latest
aileron action add github://ALRubinger/aileron-connector-google/actions/get-draft@latest
aileron action add github://ALRubinger/aileron-connector-google/actions/draft-email@latest
aileron action add github://ALRubinger/aileron-connector-google/actions/send-email@latest
aileron action add github://ALRubinger/aileron-connector-google/actions/send-draft@latest
```

```sh
# Calendar
aileron action add github://ALRubinger/aileron-connector-google/actions/list-upcoming-events@latest
aileron action add github://ALRubinger/aileron-connector-google/actions/create-calendar-event@latest
```

```sh
# Contacts
aileron action add github://ALRubinger/aileron-connector-google/actions/search-contacts@latest
aileron action add github://ALRubinger/aileron-connector-google/actions/get-contact@latest
aileron action add github://ALRubinger/aileron-connector-google/actions/list-contacts@latest
```

```sh
# Drive
aileron action add github://ALRubinger/aileron-connector-google/actions/search-files@latest
aileron action add github://ALRubinger/aileron-connector-google/actions/get-file-metadata@latest
aileron action add github://ALRubinger/aileron-connector-google/actions/get-file-content@latest
aileron action add github://ALRubinger/aileron-connector-google/actions/upload-file@latest
aileron action add github://ALRubinger/aileron-connector-google/actions/rename-file@latest
aileron action add github://ALRubinger/aileron-connector-google/actions/move-file@latest
```

```sh
# Docs
aileron action add github://ALRubinger/aileron-connector-google/actions/create-doc@latest
aileron action add github://ALRubinger/aileron-connector-google/actions/get-doc-structure@latest
aileron action add github://ALRubinger/aileron-connector-google/actions/update-doc@latest
```

## Gmail

### `list-recent-emails`

Lists recent Gmail messages, ordered most-recent first. Returns the raw `users.messages.list` response: `id`/`threadId` pairs plus paging metadata. The agent typically resolves bodies by following up with `get-email`.

| Input | Required | Description |
|---|---|---|
| `query` | no | Gmail search query, e.g. `is:unread` or `from:alice@example.com`. Empty fetches the most recent without filtering. |
| `max_results` | no | Page size. Default 10, max 100. |

Idempotent. Read-only. No approval prompt.

### `get-email`

Fetches headers and a body snippet for a single Gmail message. Uses `format=metadata` so the call returns Subject / From / To / Date / labelIds / snippet without pulling the full MIME body — a fast call cost for "what does this email say at a glance" agent flows.

| Input | Required | Description |
|---|---|---|
| `id` | yes | Gmail message id, as returned by `list-recent-emails`. |

Idempotent. Read-only. No approval prompt.

### `list-drafts`

Lists Gmail drafts, ordered most-recent first. Returns the raw `users.drafts.list` response: `{id, message: {id, threadId}}` entries with paging metadata. The list shape carries ids only; pair with `get-draft` to resolve headers and a snippet, then `send-draft` to dispatch.

| Input | Required | Description |
|---|---|---|
| `query` | no | Gmail search query, e.g. `subject:invoice` or `to:alice@example.com`. |
| `max_results` | no | Page size. Default 10, max 100. |
| `page_token` | no | Continuation token from a prior call's `nextPageToken`. |

Idempotent. Read-only. No approval prompt.

### `get-draft`

Fetches headers and a body snippet for a single Gmail draft. Returns the raw `users.drafts.get?format=metadata` response. Useful immediately before `send-draft` so the user sees what is about to dispatch — and that's exactly how `send-draft`'s approval preview is wired.

| Input | Required | Description |
|---|---|---|
| `id` | yes | Gmail draft id (as returned by `draft-email` in the response's `id` field — distinct from message ids). |

Idempotent. Read-only. No approval prompt.

### `draft-email`

Creates an email draft in Gmail. The draft lands in the user's Drafts folder, where they review and send manually from Gmail's UI or via `send-draft`. This is the safer write path because it inserts a human review step naturally.

| Input | Required | Description |
|---|---|---|
| `to` | yes | Comma-separated recipient addresses. |
| `subject` | yes | Email subject line. |
| `body` | yes | Plain-text body. |
| `cc` | no | Comma-separated Cc addresses. |
| `bcc` | no | Comma-separated Bcc addresses. |

**Not idempotent**: invoking twice creates two drafts. No runtime-level approval prompt — the natural Gmail review step (or `send-draft`'s approval) is what gates send.

### `send-email`

Sends an email from the user's Gmail account. Unlike `draft-email`, the message leaves the outbox immediately.

| Input | Required | Description |
|---|---|---|
| `to` | yes | Comma-separated recipient addresses. |
| `subject` | yes | Email subject line. |
| `body` | yes | Plain-text body. The user will be asked to approve this exact text before send. |
| `cc` | no | Comma-separated Cc addresses. |
| `bcc` | no | Comma-separated Bcc addresses. |

**Approval-gated** ([ADR-0009](/adr/0009-user-channel)). The runtime asks the user via the launch-comms channel before Gmail is contacted; on denial nothing is sent and no quota is burned. **Not idempotent** — the runtime's retry layer is configured to honor that and will not double-send on transient failure.

`draft-email` is the safer default for unattended flows; reach for `send-email` only when skipping the manual click is worth the approval prompt.

### `send-draft`

Dispatches an existing draft from the user's Gmail Drafts folder. The draft's recipients, subject, and body are already set in Gmail — this action takes only the draft id and tells Gmail to send. Pairs naturally with `draft-email`: the agent (or the user) drafts first, reviews, then sends without reconstructing the body.

| Input | Required | Description |
|---|---|---|
| `draft_id` | yes | Gmail draft id to dispatch. |

**Approval-gated** with an authoritative preview ([ADR-0016](/adr/0016-approval-preview)). Before showing the prompt the runtime calls `get-draft` against the supplied id and renders To, Subject, and the body snippet pulled straight from Gmail — not from the agent. **Not idempotent**: a second invocation against the same `draft_id` will fail with 404 (the draft no longer exists after a successful send).

## Calendar

### `list-upcoming-events`

Lists upcoming events on a Google Calendar, chronological. Recurring events are expanded; `timeMin` is set to "now" so past events don't surface.

| Input | Required | Description |
|---|---|---|
| `calendar_id` | no | Calendar id. Default `primary`. |
| `max_results` | no | Page size. Default 10, max 100. |

Idempotent. Read-only. No approval prompt.

### `create-calendar-event`

Inserts a new event into a Google Calendar.

| Input | Required | Description |
|---|---|---|
| `title` | yes | Event title (Calendar's "summary" field). |
| `start_time` | yes | RFC3339 timestamp, e.g. `2026-05-04T15:00:00-07:00`. |
| `end_time` | yes | RFC3339 timestamp. |
| `timezone` | no | IANA timezone, e.g. `America/New_York`. |
| `description` | no | Long-form description. |
| `location` | no | Physical or virtual location. |
| `attendees` | no | Array of email addresses, or comma-separated string. |
| `calendar_id` | no | Calendar id. Default `primary`. |

**Approval-gated**. **Not idempotent** — invoking twice creates two events.

## Contacts

### `search-contacts`

Searches the authenticated user's Google Contacts via the People API's `people:searchContacts` endpoint. Match is fuzzy across names, email addresses, phone numbers, organizations, and other searchable fields — "alice", "alice@example.com", and "Acme Corp" are all valid queries.

| Input | Required | Description |
|---|---|---|
| `query` | yes | Search string. Empty queries are rejected before dispatch. |
| `read_mask` | no | Comma-separated People API person fields to return on each match. Default `names,emailAddresses,phoneNumbers,birthdays`. |
| `max_results` | no | Default 10; clamped to 30 (the People API's documented per-request cap). |

Returns the raw `people:searchContacts` response. Each result's `person.resourceName` (e.g. `people/c123456789`) feeds `get-contact` if a wider field set is needed.

Idempotent. Read-only. No approval prompt.

### `get-contact`

Fetches a single contact by `resource_name` and returns the wide field set requested in `person_fields`. Pair with `search-contacts` or `list-contacts` when the lean field shape returned by those is not enough.

| Input | Required | Description |
|---|---|---|
| `resource_name` | yes | Google-issued resource name of the form `people/<id>`. |
| `person_fields` | no | Comma-separated People API person fields. Default covers names, emails, phone numbers, birthdays, addresses, organizations, biographies, urls. |

Idempotent. Read-only. No approval prompt.

### `list-contacts`

Enumerates the authenticated user's Google Contacts via `people/me/connections`. Returns the raw `people.connections.list` response. Use when the agent doesn't yet know who it's looking for (e.g. "show me everyone whose birthday is this month") and filtering happens client-side after the fetch. For keyword discovery prefer `search-contacts`.

| Input | Required | Description |
|---|---|---|
| `person_fields` | no | Comma-separated People API person fields. Default `names,emailAddresses,phoneNumbers`. |
| `max_results` | no | Page size. Default 100, max 100. Use `page_token` to walk past the cap. |
| `page_token` | no | Continuation token from a prior call's `nextPageToken`. |
| `sort_order` | no | One of `LAST_MODIFIED_ASCENDING`, `LAST_MODIFIED_DESCENDING`, `FIRST_NAME_ASCENDING`, `LAST_NAME_ASCENDING`. |

Idempotent. Read-only. No approval prompt.

## Drive

### `search-files`

Searches Google Drive via the v3 `files.list` endpoint. The `query` argument is Drive's full query language passed through unchanged — name matching, content matching, mimeType filtering, folder scoping, date ranges. Examples: `name contains 'budget'`, `mimeType='application/vnd.google-apps.document'`, `'<folderId>' in parents`, `trashed=false`.

| Input | Required | Description |
|---|---|---|
| `query` | no | Drive query expression. Omit for an unfiltered list. |
| `max_results` | no | Page size. Default 25, max 100. |
| `order_by` | no | Sort key, e.g. `modifiedTime desc`, `name`. |
| `page_token` | no | Continuation token from a prior call's `nextPageToken`. |
| `fields` | no | Drive partial-response field mask. Default covers id, name, mimeType, modifiedTime, parents, webViewLink. |

Returns the raw `files.list` response. Each `files[].id` feeds `get-file-content`, `get-file-metadata`, `get-doc-structure`, `update-doc`, `rename-file`, or `move-file`.

Idempotent. Read-only. No approval prompt.

### `get-file-metadata`

Fetches a single Drive file's properties without downloading its content. Returns the raw `files.get` response. This action also powers the authoritative approval preview on `move-file`.

| Input | Required | Description |
|---|---|---|
| `file_id` | yes | Drive file id. |
| `fields` | no | Drive partial-response field mask. Default covers id, name, mimeType, parents, modifiedTime, owners, webViewLink. |

Idempotent. Read-only. No approval prompt.

### `get-file-content`

Reads the text content of a Drive file. Handles two paths transparently: Google native files (Docs / Sheets / Slides) are exported via Drive's `/export` endpoint (default targets: text/plain for Docs and Slides, text/csv for Sheets). Non-native files with text-shaped mimeTypes (text/*, application/json, application/xml, application/yaml, etc.) are downloaded directly via `alt=media`.

| Input | Required | Description |
|---|---|---|
| `file_id` | yes | Drive file id. |
| `export_mime_type` | no | Override the default text export type for native files. Examples: `text/html`, `application/rtf`. Ignored for non-native files. |

Returns `{name, mimeType, exportedAs, content}` where `content` is the file's text as a UTF-8 string. **v1 scope: text content only** — binary files (PDFs, images, archives) are rejected with a clear error. Native types with no text export (Drawings, Forms, Folders) also error unless `export_mime_type` names a text-compatible target.

Idempotent. Read-only. No approval prompt.

### `upload-file`

Uploads a new text file to Drive via the v3 multipart upload endpoint. The file lands in the user's Drive — by default in My Drive root, optionally in one or more specified folders.

| Input | Required | Description |
|---|---|---|
| `name` | yes | File name (the Drive display name). |
| `content` | yes | File content as UTF-8 text. v1 supports text content only — markdown, code, plain text, JSON / XML / YAML / TOML payloads. |
| `mime_type` | no | Content mimeType. Default `text/plain`. Examples: `text/markdown`, `text/csv`, `application/json`. |
| `parents` | no | Comma-separated parent folder id(s). Omit to land in My Drive root. |

For creating a native Google Doc, use `create-doc` instead — it returns a proper Docs document_id that `update-doc` and `get-doc-structure` can target.

**Not idempotent** — invoking twice creates two files. No approval gate: uploads are reversible (trash from Drive) and private by default.

### `rename-file`

Renames a Drive file (or Google Doc — Docs use the same id space as Drive). Only the display name changes; the file's id, owners, parents, content, and sharing remain untouched.

| Input | Required | Description |
|---|---|---|
| `file_id` | yes | Drive file id (or Docs document id). |
| `new_name` | yes | The new display name. |

Idempotent in effect (renaming to the current name is a no-op). No approval gate: renames are reversible by reissuing with the previous name.

### `move-file`

Moves a Drive file between folders by changing its parent(s). Pass `add_parents` for the destination and `remove_parents` for the source. Omitting `remove_parents` makes the file a multi-parent child of both folders rather than moving it.

| Input | Required | Description |
|---|---|---|
| `file_id` | yes | Drive file id to move. |
| `add_parents` | yes | Comma-separated parent folder id(s) to add. |
| `remove_parents` | no | Comma-separated parent folder id(s) to remove — typically the file's current parent. |

**Approval-gated** with an authoritative preview ([ADR-0016](/adr/0016-approval-preview)). Before showing the prompt the runtime calls `get-file-metadata` against the supplied id and renders the file name, mimeType, and current parents — pulled straight from Drive, not the agent.

Why approval (when other Drive write actions aren't gated): moving a file between folders changes its inherited permissions. A file in a folder shared with team-A picks up team-A's access; moving it to team-B's folder revokes team-A's access and grants team-B's. Even though the move is reversible in principle, the access change is third-party-observable — collaborators may have already opened, copied, or linked from the file during the window. The decision matrix:

| Action | Reversible? | Third-party-observable? | Approval? |
|---|---|---|---|
| `update-doc` | yes (revisions) | no (private edit) | no |
| `rename-file` | yes (rename back) | no | no |
| `move-file` | yes (move back) | yes (permissions) | yes |
| `send-email` | no | yes | yes |

Idempotent in effect — re-running with the same args leaves Drive in the same state.

## Docs

### `create-doc`

Creates a new Google Doc and (optionally) writes initial body content. The new doc lands in My Drive root, owned by the user, private by default.

| Input | Required | Description |
|---|---|---|
| `title` | yes | Document title. Shown in the Drive picker and Docs window header. |
| `body` | no | Optional initial body content (plain text). |

When `body` is supplied the connector issues `documents.create` followed by `documents.batchUpdate` with `insertText` and refetches the doc so the response carries the inserted text — letting the agent compose follow-up `update-doc` requests against known indices without an extra `get-doc-structure` call.

**Not idempotent** — invoking twice creates two docs. No approval gate: doc creation is reversible (trash from Drive) and private by default.

### `get-doc-structure`

Returns the full structured representation of a Google Doc as the Docs API exposes it: body content (paragraphs, tables, section breaks, table of contents), headers and footers, lists, ranges, named styles, and the index positions of every structural element. Pair with `update-doc` — the indices returned here are the indices `update-doc` operations target.

| Input | Required | Description |
|---|---|---|
| `document_id` | yes | Google Docs document id (the long alphanumeric in the doc URL). |
| `suggestions_view_mode` | no | One of `DEFAULT_FOR_CURRENT_ACCESS`, `SUGGESTIONS_INLINE`, `PREVIEW_SUGGESTIONS_ACCEPTED`, `PREVIEW_WITHOUT_SUGGESTIONS`. Use `SUGGESTIONS_INLINE` when an `update-doc` batchUpdate will run against a document carrying user-made suggestions. |

This is separate from `get-file-content`: that action returns exported plain text (useful for reading content for context, but discards index space), while this returns the Docs API's structured JSON the agent can target via `update-doc`.

Idempotent. Read-only. No approval prompt.

### `update-doc`

Applies a batch of structured edits to a Google Doc via `documents.batchUpdate`. The `requests` array is the Docs API's own Request union — `insertText`, `replaceAllText`, `deleteContentRange`, `updateTextStyle`, `updateParagraphStyle`, `insertTable`, `insertInlineImage`, `createNamedRange`, and the rest. The agent constructs them; the connector passes them through.

| Input | Required | Description |
|---|---|---|
| `document_id` | yes | Google Docs document id. |
| `requests` | yes | Array of Docs API Request objects to apply in order. Pair with `get-doc-structure` for accurate indices before constructing range-based requests. |

Order-sensitivity: the Docs API processes requests in array order and computes index shifts between them. The conventional safe pattern is to construct requests in descending end-index order so earlier requests do not shift the indices later requests target.

**Not idempotent** — re-running the same `insertText` inserts the text twice. No approval gate: Docs revision history makes every edit reversible. [ADR-0009](/adr/0009-user-channel) reserves runtime-level approval for irreversible writes (sending mail) and third-party-observable writes (`move-file`'s permission inheritance change); structured doc edits are neither.

## Scopes the consent screen asks for

Google's OAuth consent screen names the connector publisher (`ALRubinger`) and the requested scope set:

- **Gmail** (restricted tier): read messages and drafts; create, send, and dispatch drafts (`gmail.metadata`, `gmail.compose`).
- **Calendar** (sensitive tier): read events and create events.
- **Contacts** (sensitive tier): read the user's contacts (`contacts.readonly`).
- **Drive**: read, write, and organize files (`drive`).
- **Docs**: read and write document structure (`documents`).

Per [ADR-0002](/adr/0002-connector-model)'s OAuth section, the consent screen is a contract between the user and the entity identified on it. The user is granting these scopes to the connector publisher's OAuth app, not to Aileron itself.

## See also

- [Installing an Action](/guides/installing-an-action/) — the general install flow.
- [aileron-connector-google](https://github.com/ALRubinger/aileron-connector-google) — connector source.
- [ADR-0002: Connector Model](/adr/0002-connector-model)
- [ADR-0009: User Channel and Approval Surfaces](/adr/0009-user-channel)
- [ADR-0016: Approval-Time Preview Fetch](/adr/0016-approval-preview)
