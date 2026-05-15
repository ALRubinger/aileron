---
title: "Authoring an Action"
description: "Write an action from scratch: file layout, the TOML frontmatter contract, the LLM-facing Markdown body, inputs and execution chains, capability subsetting, and the iteration loop."
---

This guide walks you from an empty file to a working, installable action. By the end you will have a single Markdown file the LLM can discover, the runtime can execute deterministically, and any user can install with one CLI command.

If you have not read it yet, start with [Actions](/concepts/actions/) for the model. This guide is the *how*; that page is the *what* and *why*. The companion guide is [Authoring a Connector](/guides/authoring-a-connector/) — actions exist to call connectors, so the two surfaces are designed in parallel.

The reference templates throughout this guide are the [`actions/*/action.md`](https://github.com/ALRubinger/aileron-connector-google/tree/main/actions) files in `aileron-connector-google` — six real action templates spanning idempotent reads, non-idempotent writes, and per-call user approval.

## What you are building

An action is a single Markdown file. There is no compiled artifact, no separate manifest, no bundled binary. The whole contract — what the action does, which connectors it touches, which capabilities it exercises, what arguments it accepts — lives in TOML frontmatter at the top of the file. The Markdown body is documentation that doubles as the description the LLM reads when deciding whether to invoke.

Once installed, the file lives at `~/.aileron/actions/<name>.md` and **the user owns it**. They can edit the trigger phrases, swap a connector version, retitle the action, rewrite the body — without anyone's permission. You are not shipping a runtime dependency; you are shipping a starting-point template that gets copied into the user's home directory and forked from there.

This shapes how you author. The file is the artifact. There is nothing else.

## Project skeleton

For an action template you are publishing, the file lives inside a connector repo per the layout in [Publishing a Connector](/guides/publishing-a-connector/):

```
aileron-connector-google/
└── actions/
    └── list-recent-emails/
        └── action.md
```

The file is named `action.md`; the directory name becomes the action handle (`list-recent-emails` here). When a user runs `aileron action add github://ALRubinger/aileron-connector-google/actions/list-recent-emails@<version>`, the runtime fetches the tarball, verifies it, and writes the body to `~/.aileron/actions/list-recent-emails.md`.

For an action you are writing for yourself — no template, no publishing — just create the file directly under `~/.aileron/actions/`. Same shape, same rules.

## The skeleton

A complete, valid minimum, modeled on `list-recent-emails/action.md` from the reference repo:

```markdown
+++
name = "list-recent-emails"
version = "0.0.0-dev"
source = "github://ALRubinger/aileron-connector-google/actions/list-recent-emails@0.0.0-dev"

[[requires.connectors]]
name = "github://ALRubinger/aileron-connector-google"
version = "0.0.0-dev"
hash = "sha256:bound-at-release"
capabilities = ["list_recent_emails"]

[match]
intent = "list my recent Gmail messages"

[[execute]]
id = "list"
connector = "github://ALRubinger/aileron-connector-google"
op = "list_recent_emails"
idempotent = true

[[inputs]]
name = "query"
type = "string"
description = "Optional Gmail search query, e.g. \"is:unread\" or \"from:alice@example.com\"."
required = false

[[inputs]]
name = "max_results"
type = "integer"
description = "Maximum number of messages to return. Defaults to 10; capped at 100."
required = false
+++

# List Recent Gmail Messages

Fetches a list of recent Gmail messages for the authenticated user. Returns
the raw `users.messages.list` response (a paginated list of `{id, threadId}`
pairs); the agent or a downstream action resolves message bodies as needed.

When it fires:
- "summarize my unread emails from this week"
- "show me messages from alice@example.com"
- "what's in my inbox right now"
```

That is everything. Open one of those `+++` blocks at the top; close it; write the body underneath.

The `0.0.0-dev` and `sha256:bound-at-release` placeholders are *intentional* in source. The release workflow substitutes the real values at tag-push time. That pattern is covered in the [Identity](#identity) and [Connector dependencies](#connector-dependencies) sections below, and in detail in [Publishing a Connector](/guides/publishing-a-connector/).

## Frontmatter, field by field

Every field in the frontmatter is enforced. There is no "ignored if empty" — unknown keys are a parse error and missing required keys are a validation error. The schema is closed in v1; better one specific message than silent acceptance of a typo.

### Identity

```toml
name    = "list-recent-emails"
version = "0.0.0-dev"
source  = "github://ALRubinger/aileron-connector-google/actions/list-recent-emails@0.0.0-dev"
```

- `name` is the bare local handle. Lowercase, starts with a letter or digit, dashes/dots/underscores allowed (`^[a-z0-9][a-z0-9._-]*$`). It becomes the filename and the tool name the LLM sees.
- `version` is strict SemVer 2.0 — no `v` prefix. Pre-MVP convention is to stay at `0.x.y` until the action's surface is stable.
- `source` is the FQN+version the file was copied from. It is provenance only — the runtime never re-fetches it. If the user edits the file post-install, `source` still points at the upstream origin so `aileron action update` knows where to fetch the new template.

The recognized FQN schemes are `github://`, `gitlab://`, and `hub://`. Anything else is a validation error.

**Source-template convention:** in a published action template, both `version` and the version suffix in `source` are typically `0.0.0-dev`. The release workflow substitutes the real version (extracted from the pushed tag) into a build copy of the file before signing. The committed source intentionally stays a template, so the publisher pushes a tag and never edits version fields by hand. The shipped tarball — the file the user actually installs — carries the real values.

### Connector dependencies

```toml
[[requires.connectors]]
name         = "github://ALRubinger/aileron-connector-google"
version      = "0.0.0-dev"
hash         = "sha256:bound-at-release"
capabilities = ["list_recent_emails"]
```

This block does several jobs at once:

- **Pins the connector.** `name` + `version` + `hash` together identify exactly one binary. The runtime refuses to execute an action whose pinned hash does not match what is in the content-addressed store. A connector publisher cannot ship a malicious update to your installed action — the action references a specific content hash, period.
- **Declares the capability subset.** `capabilities` is what the action actually uses, not what the connector is capable of. The Google connector exposes `list_recent_emails`, `get_email`, `send_email`, `create_calendar_event`, etc.; an action that only lists messages declares `["list_recent_emails"]` and that is the entire surface area. The capability strings are typically the connector's op names, since each op corresponds to one external operation.
- **Drives the audit story.** A reader of the file can tell at a glance what surface the action touches. If a future template update adds a new capability, it shows up as a TOML diff against the user's local copy and they can accept or reject it.

You can declare multiple connectors. Compose them in your execute chain — one step pulls a list of messages from `gmail`, the next creates a calendar event in `calendar`.

**Placeholder convention:** as with `version`, the `0.0.0-dev` and `sha256:bound-at-release` placeholders are how a published template stays template-shaped. The release workflow substitutes both at tag-push time:

- `0.0.0-dev` → the real version (taken from the pushed tag).
- `sha256:bound-at-release` → the real connector content hash (computed after the connector tarball is built in the same workflow run).

The committed source carries the placeholders; the published tarball carries the real values. This means every action in a connector repo automatically pins to the same connector hash, in the same release cohort, with no per-action commits per release.

### Intent matching

```toml
[match]
intent = "list my recent Gmail messages"
```

`intent` is a canonical natural-language phrase that documents what this action does. It does not gate selection — the LLM picks among the MCP tools the agent host advertises (per [ADR-0008](/adr/0008-intent-matching/)) using the action's Markdown body as the description. In v1 it is one required string.

The intent string is *not* the only signal the LLM sees. The Markdown body's "When it fires" section (or whatever shape your prose takes) is what actually drives selection. Treat `intent` as the canonical short form and the body as the elaboration.

### Inputs

```toml
[[inputs]]
name        = "query"
type        = "string"
description = "Optional Gmail search query, e.g. \"is:unread\" or \"from:alice@example.com\"."
required    = false  # default is true; set false to mark optional

[[inputs]]
name        = "max_results"
type        = "integer"
description = "Maximum number of messages to return. Defaults to 10; capped at 100."
required    = false
```

Inputs become the JSON Schema `parameters` object the LLM sees when Aileron exposes the action as a tool. The LLM uses `description` to decide what to pass. Write descriptions for the LLM — terse, concrete, with one example if format matters.

- `name` is `^[a-z][a-z0-9_]*$` (snake_case, starts with a letter). It maps to a JSON Schema property name.
- `type` is one of `string`, `integer`, `number`, `boolean`, `array`, `object`. Scalar types map to the corresponding JSON Schema primitive. Structured types (`array`, `object`) pass through to the LLM-facing schema as `{"type":"array"}` / `{"type":"object"}` with no per-item or per-property constraints — document the expected shape in the input's `description` so the LLM produces a well-formed value. The connector receives the decoded JSON as `[]any` / `map[string]any` and validates semantic shape at op time. See [Structured inputs](#structured-inputs) below.
- `required` defaults to `true`. Set `required = false` for optional inputs.
- `description` is required. This is what the LLM reads — write it accordingly.

#### How inputs appear on approval surfaces

For approval-gated actions, the Aileron approval card renders every input as a labeled row so the user reads `To: alr@example.com / Subject: … / Body: …` instead of a JSON dump. Two optional keys shape that surface:

- `label` — the user-facing label the row uses. Defaults to `name` when omitted. Has no effect on the LLM-visible JSON Schema.
- `multiline` — when `true`, the value renders as a scrollable blockquote with embedded newlines preserved as real line breaks. Only valid when `type = "string"` (a `multiline = true` on an integer/number/boolean is rejected at manifest-load time). Defaults to `false`.

Concrete example: an approval-gated `send-email` action that wants pretty labels and a scrollable body block declares its inputs like this:

```toml
[[inputs]]
name        = "to"
type        = "string"
description = "Comma-separated recipient addresses."
label       = "To"

[[inputs]]
name        = "subject"
type        = "string"
description = "Email subject line."
label       = "Subject"

[[inputs]]
name        = "body"
type        = "string"
description = "Plain-text body."
label       = "Body"
multiline   = true
```

This composes with the `[approval.preview]` directive from [ADR-0016](/adr/0016-approval-preview/). `[approval.preview]` controls how *external state the runtime fetches before approval* is rendered (e.g. the calendar event being updated); the per-input `label` and `multiline` keys control how *the agent's call-time args* are rendered (e.g. the email the agent wants to send). An action can declare both.

#### Structured inputs

Some upstream APIs take structured arguments — a list of objects, a nested configuration, a tagged-union of commands. Google Docs `documents.batchUpdate` is a typical case: its `requests` field is an array of `Request` union objects (`insertText`, `replaceAllText`, `deleteContentRange`, …) and there is no natural scalar decomposition. Declare those as `array` or `object`:

```toml
[[inputs]]
name        = "requests"
type        = "array"
description = """
A list of Docs API request objects. Each element is one of {insertText,
replaceAllText, deleteContentRange, updateTextStyle, ...}. See the Google
Docs API reference for the full union. Example:
[
  { "insertText": { "location": { "index": 1 }, "text": "hello" } },
  { "replaceAllText": { "containsText": { "text": "{{NAME}}" }, "replaceText": "Ada" } }
]
"""
```

A few constraints:

- **No per-item or per-property typing.** v1 declares the type as bare `array` / `object`; there is no `items.type` or `properties` map in the manifest. The LLM-visible schema is correspondingly bare. Authors who want item-level typing document it in `description` (and the LLM uses that to produce well-formed values).
- **The connector validates semantic shape.** The runtime hands the connector whatever the LLM produced, decoded from JSON. Missing required fields or off-spec types surface as `connector_runtime_error` at op time, not at manifest-load time.
- **`multiline = true` is rejected on non-string declarations.** Structured values get the scrollable affordance automatically — the approval card renders them as pretty-printed JSON blocks under their label, so you do not (and cannot) declare `multiline` for them.

### Execution chain

```toml
[[execute]]
id         = "send"
connector  = "github://ALRubinger/aileron-connector-google"
op         = "send_email"
idempotent = false

[execute.inputs]
to      = "${args.to}"
subject = "${args.subject}"
body    = "${args.body}"
```

Steps run in declared order. Each step calls one connector op with one set of inputs. The runtime's contract:

- **First failure terminates.** If step 2 fails, step 3 does not run. Per [ADR-0010](/adr/0010-failure-handling/), the action returns a Result whose `Failure` is the failing step's structured error.
- **No auto-rollback.** Successful prior steps are not undone. If you need compensating actions, the agent composes them in conversation, not the action file.
- **Each step ID is unique.** Step IDs are how prior outputs will be referenced once interpolation lands (see below).
- **Each `connector` must appear in `[[requires.connectors]]`.** You cannot reference a connector you did not declare — validation fails before the action is installable.

`[execute.inputs]` is a keyed map of arguments the runtime passes into the connector's op. Values can be literals or `${args.<name>}` interpolations referencing the call-time inputs you declared in `[[inputs]]`. The validator confirms every `${args.X}` reference matches a declared input.

**`idempotent`** declares whether the gateway's retry layer ([ADR-0010](/adr/0010-failure-handling/)) is allowed to re-invoke this step on transient failure. Read ops (GETs) are typically `true`. Write ops that mutate the world (sending an email, creating a calendar event) MUST be `false` — repeating them produces duplicates. The connector's documentation tells you which ops are repeatable; the action manifest is where you record that decision.

**v1 caveat on interpolation:** `${args.X}` works. `${step_id.field}` (referencing a prior step's output) is post-MVP. If your action genuinely needs to feed step 1's output into step 2's input, factor it into the connector for now — write a connector op that does both halves — rather than trying to chain steps in the action file.

### Approval (optional)

```toml
[approval]
required = true
```

When `required = true`, the action does not run until the user explicitly approves the invocation in the Aileron webapp. The HTTP response from `POST /v1/actions/<name>/run` holds open while the approval is queued; on approve, the action runs and returns its normal result; on deny, the runtime returns a structured error with class `approval_denied`.

Use approval for actions that touch the world in ways the user wants to confirm. The reference repo's three write actions illustrate the gating axis:

- **`draft-email` — un-gated.** Drafts land in Gmail's Drafts folder and are fully reversible. The user already has a built-in human-in-the-loop step (clicking Send in Gmail), so a runtime prompt would duplicate that review without adding safety.
- **`send-email` — gated.** Dispatched mail is not reversible. The approval step moves from Gmail's UI to Aileron's prompt; it does not disappear.
- **`create-calendar-event` — gated.** Calendar's `events.insert` dispatches invitation emails to attendees as a side effect, and those notifications don't retract cleanly when the event is later deleted.

Reversibility is the test. If the action's effect is recoverable by a normal user action (delete a draft, edit a doc), skip approval. If it isn't (mail dispatched, money moved, irreversible API call), gate it.

The block is optional. Absent block ⇒ no approval needed.

## The Markdown body

Everything after the closing `+++` is the body. Four readers consume it:

- The **developer** browsing the file or its source repo.
- The **Hub** rendering it for catalog listings.
- The **LLM** deciding whether to invoke the action when the user types something.
- The **user** reading the audit log after the fact.

You write one piece of prose. There is no separate "LLM hint" field to keep in sync. This is structurally important — the documentation and the LLM's understanding of the action cannot drift apart, because they are the same string.

A workable shape:

```markdown
# Action Title

One-paragraph description of what the action does. Lead with the verb and
the affected system. The LLM uses this as the function description.

## When it fires

- Trigger phrase one.
- Trigger phrase two.
- Trigger phrase three.

## What it does

Brief prose explanation of the steps. Mention any non-obvious behavior
(e.g. "posts to the first channel matching the input, case-insensitive").

## Inputs

- `query` — Optional Gmail search query.
- `max_results` — Page size cap (default 10).
```

Write the body for the LLM first. Trigger phrases and a tight one-paragraph description matter more than ASCII art. Avoid hedging language ("might", "sometimes", "depending on") — the LLM uses prose to predict the function's behavior, and uncertain prose makes it less likely to invoke when it should.

## The execute model in practice

A few non-obvious behaviors worth knowing:

- **Inputs from `[[inputs]]` are merged with `[execute.inputs]`.** v1 passes both through to the connector op. The connector receives the call-time args plus whatever the step declared.
- **Connector ops get the JSON envelope shape from [Authoring a Connector](/guides/authoring-a-connector/)** — `{op: "list_recent_emails", args: {query: "is:unread", max_results: 10}}`. The action's job is to produce that shape; the connector's job is to dispatch on it.
- **Step output becomes the action result.** When all steps succeed, the runtime aggregates them into the final result. v1 returns the last successful step's output as the primary content.
- **Capability denial is sticky.** If a step tries a network call to a host outside the connector's manifest, or outside the action's declared capability subset, the call is denied at the sandbox boundary and the step fails. The runtime does not silently retry or substitute.

## Defense in depth: the two capability gates

This is worth understanding because it shapes what `capabilities` should contain.

When a step runs:

1. **Connector boundary.** The runtime checks the call against the connector's manifest. If the connector declared `gmail.googleapis.com:443` in `[capabilities.network].hosts` and the call dials that host, fine. If the connector tries to dial anywhere else, the call is denied.
2. **Action boundary.** The runtime *also* checks the call against the action's declared capability subset. If the action declared `["list_recent_emails"]` and the step tries to invoke `send_email` instead, the call is denied — even though the connector itself permits it.

Both gates fire on every call. Either denies and the step fails. The action's `capabilities` list is the second gate; declare the minimum subset you need so adding a new capability later is a visible TOML diff for the user, not a silent change in behavior.

## Iterating on an action

The fastest loop is to install a real template, edit the local file, and run it through the agent.

```sh
# Install a template you'll modify (replace <version> with a real tag)
$ aileron action add github://ALRubinger/aileron-connector-google/actions/list-recent-emails@<version>
✓ Action file written to ~/.aileron/actions/list-recent-emails.md

# Edit the file
$ $EDITOR ~/.aileron/actions/list-recent-emails.md

# Validation runs on every load — invalid frontmatter surfaces immediately
$ aileron action list
list-recent-emails  0.0.1  github://ALRubinger/aileron-connector-google/actions/list-recent-emails@0.0.1
```

When you have written something the runtime does not like — missing field, malformed FQN, unknown frontmatter key — `aileron action list` and `aileron action show list-recent-emails` surface the error with the file and (when known) the line. Fix it in place and try again.

To exercise the action end-to-end, drive it through `aileron launch` — the agent picks it up automatically once it is in the actions directory.

## Common authoring mistakes

- **Forgetting `hash`.** The validator requires `sha256:<hex>` on every `[[requires.connectors]]` entry. In a published template, the placeholder `sha256:bound-at-release` is what the source carries; CI substitutes the real hash at release time. If you are hand-writing an action against a connector you've already installed, get the hash from `aileron connector show <FQN>`.
- **Listing more capabilities than you use.** The action boundary enforces the subset you declare; declaring more weakens the audit story. Strip capabilities aggressively.
- **A `${args.X}` reference that does not match an input.** Validation catches this — the error names the offending step, key, and missing input.
- **Multi-paragraph descriptions on inputs.** The LLM reads `description` as a single line. Keep it tight.
- **Putting business logic in the body.** The body is documentation; the contract is in the frontmatter. If the body says "posts only to channels starting with `#eng-`" but the execute step does no filtering, the runtime will not enforce it. Either filter in a connector op or do not promise the behavior.
- **Reusing an existing `id` across steps.** Step IDs must be unique within the action.

## Where to go next

- [Authoring a Connector](/guides/authoring-a-connector/) — the connector your action depends on. Action capability subsets are meaningful only against a connector's declared capability set.
- [Publishing a connector](/guides/publishing-a-connector/) — once you have an action template you want others to install, the same repo and signing flow applies. Action tarballs ship alongside the connector.
- [ADR-0001: Manifest Format](/adr/0001-manifest-format/) — why TOML, why `+++`, why the frontmatter shape.
- [ADR-0003: Action Model](/adr/0003-action-model/) — the design constraints behind everything in this guide.
- [ADR-0010: Failure Handling](/adr/0010-failure-handling/) — first-failure-terminates, no auto-rollback, structured error envelopes.
