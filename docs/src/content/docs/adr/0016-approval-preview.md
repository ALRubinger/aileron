---
title: "ADR-0016: Approval-Time Preview Fetch"
description: "Action manifests may declare a [approval.preview] block naming a read-only op the runtime calls before showing the approval prompt. The preview response is rendered into the prompt and never returned to the agent, so the user sees an authoritative summary of what they are approving."
order: 16
---


<div class="meta">
<table>
  <tr><th>Status</th><td>Proposed</td></tr>
  <tr><th>Date</th><td>2026-05-12</td></tr>
  <tr><th>Tracking</th><td><a href="https://github.com/ALRubinger/aileron/issues/658">#658</a></td></tr>
</table>
</div>

## Context

Action manifests can gate a connector op behind per-call user approval. Per [ADR-0003](/adr/0003-action-model), the manifest declares this with:

```toml
[approval]
required = true
```

Per [ADR-0008](/adr/0008-intent-matching) and [ADR-0009](/adr/0009-user-channel), when the runtime asks the user to approve, the question is rendered on an out-of-band surface the agent cannot reach. Today that surface has only the action call's **inputs** to render.

For some actions inputs are sufficient. The `send-email` action declares `to`, `subject`, and `body` as inputs, and the approval UI can show them directly. For other actions inputs are not sufficient. The `send-draft` action in `aileron-connector-google` takes a single input, `draft_id`, which is opaque to the user. Approving "send draft `r-OR0123456789`" gives the user no signal about who the mail is going to, what the subject is, or what the body says.

One path that does not work is to let the agent populate the prompt. The `send-draft` manifest currently suggests:

> The approval prompt should surface the draft id (and ideally the agent should pass through a recent `draft_email` response so the user recognizes what they are sending).

The "agent passes through a recent response" path is forgeable. The agent supplies the preview text, so the prompt's content is only as trustworthy as the agent. For a non-reversible op (a dispatched mail cannot be unsent), the approval payload cannot rest on agent honesty. It must come from the same authoritative source the op will write to.

[ADR-0009](/adr/0009-user-channel) makes this constraint structural. The approval surface is the user's escape hatch when the agent is compromised, misaligned, or wrong. Letting the agent populate that surface defeats the purpose.

## Decision

Extend the action manifest's `[approval]` block with an optional `preview` directive. The directive names a read-only op the runtime calls **before** showing the approval prompt. The preview op's response is rendered in the prompt. It is never returned to the agent.

The preview directive controls how *external state fetched at approval time* is shown to the user. It composes with the per-input display metadata defined by [ADR-0003](/adr/0003-action-model/) (`label` and `multiline` on `[[inputs]]`), which controls how *the agent's call-time args* are rendered. An action can declare both: the preview block surfaces the remote object being acted on (the calendar event being updated, the email being drafted against), and the input metadata surfaces what the agent is about to write.

```toml
[approval]
required = true

[approval.preview]
# Connector op to call. Must live on the same connector as the gated
# op, must be declared idempotent in some action manifest, and must
# not itself carry [approval].required = true (no recursion).
op = "get_draft"

# Argument template. Values may interpolate ${args.<name>} from the
# gated action call's inputs, matching the existing interpolation
# convention used in [[execute]] steps.
args = { id = "${args.draft_id}" }

# Fields to render in the approval prompt. Keys are user-facing
# labels; values are dotted paths into the preview op's JSON response.
# Missing paths are omitted with a per-field "n/a" indicator.
render = { To = "message.payload.headers.To",
           Subject = "message.payload.headers.Subject",
           Body = "message.snippet" }

# Labels in `render` whose values are long-form content. The approval
# UI surfaces these as scrollable blockquotes below the inline rows
# rather than as single-line key/value entries, so the user can read
# the whole body of (e.g.) a draft email before approving. Optional;
# omit for prompts whose fields are all short.
multiline = ["Body"]
```

The runtime's approval flow becomes:

1. Agent calls the gated op.
2. Runtime resolves `[approval.preview]`. If present, the runtime validates the preview op against the rules below, invokes it in the same WASM sandbox with the same credential binding the gated op would use, and extracts `render` field paths into a `{label: value}` map. On failure the runtime records the error and falls back to rendering raw inputs with a "preview unavailable: `<reason>`" line.
3. Show the approval prompt with the preview map (or fallback) alongside the gated op's inputs.
4. On approval, invoke the gated op as normal.
5. On denial, skip the gated op as normal.

### Manifest validation rules

The runtime rejects an `[approval.preview]` block at manifest-load time if any of the following hold:

- `op` does not exist on the same connector as the gated op.
- The `op` named is not declared `idempotent = true` in at least one bundled action manifest.
- The `op` named appears in any action manifest with `[approval].required = true`. This prevents preview-of-preview recursion.
- `args` references an `${args.X}` that the gated action does not declare as an input.
- `render` is empty.
- An entry in `multiline` does not appear as a key in `render`.

These checks happen at suite load and at release-time signing, so a malformed preview directive fails fast.

### Runtime semantics

**Same sandbox.** The preview op runs through the same `aileron_host.http_request` boundary, with the same `[capabilities.network]` enforcement and the same OAuth token injection. Per [ADR-0005](/adr/0005-sandbox-choice), the connector still never sees the token.

**Same credential.** The preview op uses the connector's bound credential. There is no scope expansion or re-verification. If the gated op is allowed to dispatch, the preview is allowed to read the same draft.

**Agent isolation.** The preview response is rendered only to the approval surface. It is not returned to the agent's context. Otherwise preview becomes a side-channel. An agent could request approval purely to learn the contents of a draft it has no business reading.

**Quota cost.** The preview op consumes one API call against the upstream provider per approval. For Gmail's `users.drafts.get?format=metadata` this is a single quota unit, cheap relative to the dispatch.

**Timeout.** The runtime enforces a short bound on the preview call (suggested: 5s) and falls back to "preview unavailable" on timeout rather than blocking the user indefinitely.

**Caching.** Preview responses are not cached. If the user is re-prompted for the same approval (for example after a denied retry), the runtime re-fetches. This keeps the preview faithful to current state. A draft the user edited in Gmail between prompts shows the edited version.

### Field-path resolution

The `render` values are dotted JSON paths into the preview response. For Gmail's draft shape, headers live in an array (`message.payload.headers: [{name, value}, ...]`), so the path `message.payload.headers.Subject` is shorthand. It means "find the entry in `message.payload.headers` whose `name` equals `Subject`, return its `value`". The runtime implements this shorthand for header-style arrays. For other shapes the path is a plain object/array walk.

### Multiline fields

A field whose value is long-form content (an email body, a commit message, an HTTP response payload) reads poorly as a single `key: value` row. The optional `multiline` list names labels in `render` that the approval surface should treat as block content rather than inline key/value entries.

The semantic guarantee is purely user-visible: the runtime resolves each label's value the same way regardless of whether the label appears in `multiline`. The flag is a render hint the approval prompt honors. The webapp renders multiline labels as scrollable blockquotes below the inline rows; CLI surfaces are free to render them with a header line and an indented block, or any equivalent affordance.

`multiline` is optional. A manifest that omits it produces the prior all-inline rendering. A label that appears in `multiline` but not in `render` is rejected at manifest-load time, matching the validation discipline applied to `args` interpolation. The flag does not change the failure-mode contract: a multiline label whose path resolves missing surfaces as "n/a" the same as any other field.

### Failure modes and fallbacks

| Failure | Behavior |
|---|---|
| Preview op HTTP non-2xx | Render fallback ("Preview unavailable: Gmail returned 404"). Approval still proceeds. |
| Preview op timeout | Render fallback ("Preview unavailable: timeout"). Approval still proceeds. |
| `render` path missing in response | Omit that field; surface a per-field "n/a" indicator. Approval still proceeds. |
| Preview op denied at sandbox boundary | Manifest validation should have caught this. If it slips through, render fallback and log. |
| Preview op crashes (WASM trap) | Render fallback. |

In every failure mode the approval still proceeds. The user is asked to approve based on raw inputs plus a clear "preview unavailable" note. The runtime never silently degrades to "approved without preview". The user sees that preview failed and can decline.

### Security model

The connector is hash-pinned per [ADR-0002](/adr/0002-connector-model). A connector could lie to the preview as it could to any op. That is the existing trust model. The audit trail ([ADR-0010](/adr/0010-failure-handling)) catches divergence after the fact, and the user can deny if the rendered preview looks wrong.

Preview output is never returned to the agent, so it cannot be used as a data-exfil channel.

The preview op runs with the same network and credential capabilities as the gated op, with no expansion. A connector cannot use the preview slot to reach a new host.

Manifest validation happens at signing time. A release that declares a preview op pointing to a non-idempotent or approval-gated op fails to publish.

## Worked example: `send-draft`

Connector `ALRubinger/aileron-connector-google` ships a new read-only op `get_draft` (calling `users.drafts.get?format=metadata`) and a matching `actions/get-draft` manifest with `idempotent = true` and no `[approval]` block.

The `send-draft` action manifest is updated:

```toml
[approval]
required = true

[approval.preview]
op = "get_draft"
args = { id = "${args.draft_id}" }
render = { To = "message.payload.headers.To",
           Subject = "message.payload.headers.Subject",
           Body = "message.snippet" }
multiline = ["Body"]
```

When the agent calls `send_draft` with `draft_id = "r-12345"`:

1. Runtime sees `[approval.preview]` and calls `get_draft({id: "r-12345"})`.
2. Gmail returns the draft. Runtime extracts To, Subject, and snippet.
3. The approval prompt renders:

   ```
   Approve send-draft?
     Draft id: r-12345
     To:       alice@example.com
     Subject:  Weekly recap
     Body:
       > Here's the recap from this week's standup...
       > <full body rendered as a scrollable block per the
       >  manifest's `multiline = ["Body"]` directive>
   [Approve] [Deny]
   ```

4. The user approves, and `send_draft` dispatches.
5. The user denies, and `send_draft` returns the standard denial error. No message is sent. The audit log records the deny.

If `get_draft` returns 404 (the agent passed a message id instead of a draft id, the exact bug that motivated this ADR), the prompt renders:

```
Approve send-draft?
  Draft id: 19df4136f28569d2
  Preview unavailable: Gmail API returned 404
[Approve] [Deny]
```

The user sees that something is off before approving, instead of silently approving a doomed call.

## Alternatives considered

**Agent-supplied preview inputs.** Add optional `to_preview`, `subject_preview`, `body_preview` inputs to `send-draft`. Cheap, works today. Rejected as the load-bearing solution. The values are forgeable, and the entire point of approval is to not trust the agent on the question of "what am I about to do." This path could still be allowed as a *fallback hint* when no preview op is declared.

**Make `send-draft` take the draft contents instead of an id.** Defeats the purpose of the draft-then-send split. The agent reconstructing the RFC 2822 payload is what `draft-email` already avoids.

**Runtime-side Gmail integration.** The runtime fetches the draft directly without going through the connector. Rejected. It special-cases a single provider, breaks the connector boundary, and would need to be re-implemented per provider.

## Open questions

**Cross-connector previews.** Should a preview op be allowed to live on a *different* connector than the gated op? The argument for is composability. The argument against is trust-boundary muddiness. Initial proposal says no. Revisit if a real use case appears.

**Preview re-render on edit.** If the user edits the draft in Gmail between the agent's `send_draft` call and the approval prompt, the runtime sees the edited version. Good. If they edit *after* the prompt renders and before they click approve, the prompt is stale. Should the prompt poll? Initial proposal: no. The latency window is short and polling adds quota cost.

**Preview output retention.** Are preview responses logged in the audit trail? The argument for is completeness. The audit shows what the user saw. The argument against is PII/content leakage into logs. Initial proposal: log a hash of the preview output, not the output itself.

## Migration

Existing `[approval]` blocks without `preview` continue to work unchanged.

Connectors opt in by adding a read-only op and updating the gated action manifest. The opt-in is per-action.

`aileron-connector-google` ships `get_draft` and the `get-draft` action manifest in the same release as the runtime support. The `send-draft` manifest gets updated to reference the preview op once the runtime version enforcing the new schema is the minimum supported.

## References

- [Issue #658](https://github.com/ALRubinger/aileron/issues/658) — this proposal
- [ADR-0002](/adr/0002-connector-model) — Connector Model (hash-pinning, capability boundary)
- [ADR-0003](/adr/0003-action-model) — Action Model (`[[inputs]]`, `${args.X}` interpolation, `[approval]` block)
- [ADR-0005](/adr/0005-sandbox-choice) — Sandbox Choice (credential mediation)
- [ADR-0008](/adr/0008-intent-matching) — Action Exposure to Agents (single execution path through `POST /v1/actions/{name}/run`)
- [ADR-0009](/adr/0009-user-channel) — User Channel and OOB Approval Surfaces (agent never in trust path)
- [ADR-0010](/adr/0010-failure-handling) — Failure-Handling Policy (audit store)
