---
title: "Authoring an Action"
description: "Write an action from scratch: file layout, the TOML frontmatter contract, the LLM-facing Markdown body, inputs and execution chains, capability subsetting, and the iteration loop."
---

This guide walks you from an empty file to a working, installable action. By the end you will have a single Markdown file the LLM can discover, the runtime can execute deterministically, and any user can install with one CLI command.

If you have not read it yet, start with [Actions](/concepts/actions/) for the model. This guide is the *how*; that page is the *what* and *why*. The companion guide is [Authoring a Connector](/guides/authoring-a-connector/) — actions exist to call connectors, so the two surfaces are designed in parallel.

## What you are building

An action is a single Markdown file. There is no compiled artifact, no separate manifest, no bundled binary. The whole contract — what the action does, which connectors it touches, which capabilities it exercises, what arguments it accepts — lives in TOML frontmatter at the top of the file. The Markdown body is documentation that doubles as the description the LLM reads when deciding whether to invoke.

Once installed, the file lives at `~/.aileron/actions/<name>.md` and **the user owns it**. They can edit the trigger phrases, swap a connector version, retitle the action, rewrite the body — without anyone's permission. You are not shipping a runtime dependency; you are shipping a starting-point template that gets copied into the user's home directory and forked from there.

This shapes how you author. The file is the artifact. There is nothing else.

## Project skeleton

For an action template you are publishing, the file lives inside a connector repo per the layout in [Publishing a connector](/guides/publishing-a-connector/):

```
aileron-connector-slack/
└── actions/
    └── ship-update/
        └── action.md
```

The file is named `action.md`; the directory name becomes the action handle (`ship-update` here). When a user runs `aileron action add github://acme/aileron-connector-slack/actions/ship-update@1.0.0`, the runtime fetches the tarball, verifies it, and writes the body to `~/.aileron/actions/ship-update.md`.

For an action you are writing for yourself — no template, no publishing — just create the file directly under `~/.aileron/actions/`. Same shape, same rules.

## The skeleton

A complete, valid minimum:

```markdown
+++
name = "ship-update"
version = "1.0.0"
source = "hub://aileron/ship-update@1.0.0"

[[requires.connectors]]
name = "github://aileron/slack"
version = "1.2.0"
hash = "sha256:abc123..."
capabilities = ["chat:write"]

[match]
intent = "tell team I shipped"

[[inputs]]
name = "channel"
type = "string"
description = "Slack channel to post to (e.g. '#engineering')."

[[inputs]]
name = "summary"
type = "string"
description = "What you shipped, in one or two sentences."

[[execute]]
id = "post"
connector = "github://aileron/slack"
op = "post_message"

[execute.inputs]
channel = "${args.channel}"
text = "${args.summary}"
+++

# Ship Update

Posts a brief "shipped" announcement to a Slack channel.

## When it fires

- "tell the team I shipped the migration"
- "post a ship update to #engineering"
- "let everyone know I merged the PR"
```

That is everything. Open one of those `+++` blocks at the top; close it; write the body underneath.

## Frontmatter, field by field

Every field in the frontmatter is enforced. There is no "ignored if empty" — unknown keys are a parse error and missing required keys are a validation error. The schema is closed in v1; better one specific message than silent acceptance of a typo.

### Identity

```toml
name    = "ship-update"
version = "1.0.0"
source  = "hub://aileron/ship-update@1.0.0"
```

- `name` is the bare local handle. Lowercase, starts with a letter or digit, dashes/dots/underscores allowed (`^[a-z0-9][a-z0-9._-]*$`). It becomes the filename and the tool name the LLM sees.
- `version` is strict SemVer 2.0 — no `v` prefix. Pre-MVP convention is to stay at `0.x.y` until the action's surface is stable.
- `source` is the FQN+version the file was copied from. It is provenance only — the runtime never re-fetches it. If the user edits the file post-install, `source` still points at the upstream origin so `aileron action update` knows where to fetch the new template.

The recognized FQN schemes are `github://`, `gitlab://`, and `hub://`. Anything else is a validation error.

### Connector dependencies

```toml
[[requires.connectors]]
name         = "github://aileron/slack"
version      = "1.2.0"
hash         = "sha256:abc123..."
capabilities = ["chat:write", "channels:read"]
```

This block does several jobs at once:

- **Pins the connector.** `name` + `version` + `hash` together identify exactly one binary. The runtime refuses to execute an action whose pinned hash does not match what is in the content-addressed store. A connector publisher cannot ship a malicious update to your installed action — the action references a specific content hash, period.
- **Declares the capability subset.** `capabilities` is what the action actually uses, not what the connector is capable of. The Slack connector might support `chat:write`, `chat:read`, `users:read`, `files:upload`; if your action only posts messages, you declare `chat:write` and that is the entire surface area.
- **Drives the audit story.** A reader of the file can tell at a glance what surface the action touches. If a future template update adds a new capability, it shows up as a TOML diff against the user's local copy and they can accept or reject it.

You can declare multiple connectors. Compose them in your execute chain — one step pulls the recent merge from `git`, the next posts the announcement to `slack`.

### Intent matching

```toml
[match]
intent = "tell team I shipped"
```

`intent` is a canonical natural-language phrase the runtime uses to surface this action to the agent. The shape will grow as [ADR-0008](/adr/0008-intent-matching/) matures; in v1 it is one required string.

The intent string is *not* the only signal the LLM sees. The Markdown body's "When it fires" section (or whatever shape your prose takes) is what actually drives selection. Treat `intent` as the canonical short form and the body as the elaboration.

### Inputs

```toml
[[inputs]]
name        = "channel"
type        = "string"
description = "Slack channel to post to (e.g. '#engineering')."

[[inputs]]
name        = "summary"
type        = "string"
description = "What you shipped, in one or two sentences."
required    = false  # default is true; set false to mark optional
```

Inputs become the JSON Schema `parameters` object the LLM sees when Aileron exposes the action as a tool. The LLM uses `description` to decide what to pass. Write descriptions for the LLM — terse, concrete, with one example if format matters.

- `name` is `^[a-z][a-z0-9_]*$` (snake_case, starts with a letter). It maps to a JSON Schema property name.
- `type` is one of `string`, `integer`, `number`, `boolean`. Object and array types are post-MVP.
- `required` defaults to `true`. Set `required = false` for optional inputs.
- `description` is required. This is what the LLM reads — write it accordingly.

### Execution chain

```toml
[[execute]]
id        = "recent_merge"
connector = "github://aileron/git"
op        = "read_recent_merge"

[[execute]]
id        = "post"
connector = "github://aileron/slack"
op        = "post_message"

[execute.inputs]
channel = "${args.channel}"
text    = "${args.summary}"
```

Steps run in declared order. Each step calls one connector op with one set of inputs. The runtime's contract:

- **First failure terminates.** If step 2 fails, step 3 does not run. Per [ADR-0010](/adr/0010-failure-handling/), the action returns a Result whose `Failure` is the failing step's structured error.
- **No auto-rollback.** Successful prior steps are not undone. If `recent_merge` succeeded and `post` failed, the merge fact is unchanged in the world (it was a read anyway, but the rule holds for writes too). If you need compensating actions, the agent composes them in conversation, not the action file.
- **Each step ID is unique.** Step IDs are how prior outputs will be referenced once interpolation lands (see below).
- **Each `connector` must appear in `[[requires.connectors]]`.** You cannot reference a connector you did not declare — validation fails before the action is installable.

`[execute.inputs]` is a keyed map of arguments the runtime passes into the connector's op. Values can be literals or `${args.<name>}` interpolations referencing the call-time inputs you declared in `[[inputs]]`. The validator confirms every `${args.X}` reference matches a declared input.

**v1 caveat on interpolation:** `${args.X}` works. `${step_id.field}` (referencing a prior step's output) is post-MVP. If your action genuinely needs to feed step 1's output into step 2's input, factor it into the connector for now — write a connector op that does both halves — rather than trying to chain steps in the action file.

### Approval (optional)

```toml
[approval]
required = true
```

When `required = true`, the action does not run until the user explicitly approves the invocation in the Aileron webapp. The HTTP response from `POST /v1/actions/<name>/run` holds open while the approval is queued; on approve, the action runs and returns its normal result; on deny, the runtime returns a structured error with class `approval_denied`.

Use approval for actions that touch the world in ways the user wants to confirm — sending an email, charging a card, deleting something. Skip it for read-only actions and low-stakes writes.

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

- `channel` — Slack channel to post to.
- `summary` — One or two sentences about what shipped.
```

Write the body for the LLM first. Trigger phrases and a tight one-paragraph description matter more than ASCII art. Avoid hedging language ("might", "sometimes", "depending on") — the LLM uses prose to predict the function's behavior, and uncertain prose makes it less likely to invoke when it should.

## The execute model in practice

A few non-obvious behaviors worth knowing:

- **Inputs from `[[inputs]]` are merged with `[execute.inputs]`.** v1 passes both through to the connector op. The connector receives the call-time args plus whatever the step declared.
- **Connector ops get the JSON envelope shape from [Authoring a Connector](/guides/authoring-a-connector/)** — `{op: "post_message", args: {channel: "#engineering", text: "..."}}`. The action's job is to produce that shape; the connector's job is to dispatch on it.
- **Step output becomes the action result.** When all steps succeed, the runtime aggregates them into the final result. v1 returns the last successful step's output as the primary content.
- **Capability denial is sticky.** If a step tries a network call to a host outside the connector's manifest, or outside the action's declared capability subset, the call is denied at the sandbox boundary and the step fails. The runtime does not silently retry or substitute.

## Defense in depth: the two capability gates

This is worth understanding because it shapes what `capabilities` should contain.

When a step runs:

1. **Connector boundary.** The runtime checks the call against the connector's manifest. If the connector declared `chat:write` and the call is `chat:write`, fine. If the connector did not declare it, the call is denied.
2. **Action boundary.** The runtime *also* checks the call against the action's declared capability subset. If the action declared `["chat:write"]` and the connector tries something else, the call is denied — even though the connector itself permits it.

Both gates fire on every call. Either denies and the step fails. The action's `capabilities` list is the second gate; declare the minimum subset you need so adding a new capability later is a visible TOML diff for the user, not a silent change in behavior.

## Iterating on an action

The fastest loop is to install a real template, edit the local file, and run it through the agent.

```sh
# Install a template you'll modify
$ aileron action add hub://aileron/ship-update@1.0.0
✓ Action file written to ~/.aileron/actions/ship-update.md

# Edit the file
$ $EDITOR ~/.aileron/actions/ship-update.md

# Validation runs on every load — invalid frontmatter surfaces immediately
$ aileron action list
ship-update  1.0.0  hub://aileron/ship-update@1.0.0
```

When you have written something the runtime does not like — missing field, malformed FQN, unknown frontmatter key — `aileron action list` and `aileron action show ship-update` surface the error with the file and (when known) the line. Fix it in place and try again.

To exercise the action end-to-end, drive it through `aileron launch` — the agent picks it up automatically once it is in the actions directory.

## Common authoring mistakes

- **Forgetting `hash`.** The validator requires `sha256:<hex>` on every `[[requires.connectors]]` entry. Get it from the connector's release tarball or from `aileron connector show <FQN>` after install.
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
