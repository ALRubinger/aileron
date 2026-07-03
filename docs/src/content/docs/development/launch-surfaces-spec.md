---
title: "Launch-a-Flight Surfaces Spec"
description: "The distribution surfaces over a frozen, signed Flight Plan. How anyone in an org Launches a Flight without operating an agent, the per-surface invocation contract, preview and approval routing, and result delivery including file-artifact outputs."
order: 12
---

A Launch invokes a frozen, signed Flight Plan version through the deterministic Launch runtime ([Launching a Flight Plan guide](/guides/launching-a-flight-plan/), [ADR-0027](/adr/0027-flight-plan-sealed-installable-skill)). A surface is the front door to that runtime. A surface is not a new runtime. This page specifies the distribution surfaces over a frozen Flight Plan: the four internal surfaces, the four external surfaces, and per surface the invocation contract, the preview and approval routing, and the result delivery.

Anyone in an org can Launch a Flight against a Flight Plan, and no one operates an agent. The Flight Plan is exposed for launching through one or more surfaces. The person who Launches a Flight states an outcome and receives a result. The deterministic Launch runtime does the work, and the org's connectors perform the credentialed calls.

## Scope and non-goals

This spec defines surfaces. It does not redefine the freeze boundary, the Launch runtime, the per-action trust contract, or the [ADR-0009](/adr/0009-user-channel) out-of-band approval. Those are specified elsewhere and reused here.

A surface introduces no new approval mechanism. Approval routing is the effect-driven out-of-band approval already specified by the [Flight Plan Manifest Spec](/development/flight-plan-manifest-spec/) trust contract and [ADR-0009](/adr/0009-user-channel). A surface binds to that routing and never replaces it.

File-artifact outputs are in scope for result delivery. Binary materialization stays deferred. v1 materializes `utf-8` artifacts only, and `base64` or binary output is blocked on the host-ABI binary-body field and the tool step's mount or collect boundary ([#1510](https://github.com/ALRubinger/aileron/issues/1510)).

This spec ships no code. It is a normative format and contract spec in the same register as the [Flight Plan Manifest Spec](/development/flight-plan-manifest-spec/) and the [AI-Assisted Authoring UX Spec](/development/ai-assisted-authoring-spec/). The runtime it surfaces is tracked in [#1511](https://github.com/ALRubinger/aileron/issues/1511). The surface set is absorbed and quoted from [#928](https://github.com/ALRubinger/aileron/issues/928), not depended on.

## The common Launch contract

Every surface shares one Launch contract. The per-surface dimensions specialize how a surface gathers a payload and renders a result, never the underlying guarantees.

Every Launch binds a frozen Flight Plan version. The version is the content hash plus the semver label from the manifest `lock` section ([Flight Plan Manifest Spec](/development/flight-plan-manifest-spec/) freeze and lock section). A surface invokes the deterministic Launch runtime against a digest-pinned, signed version. A surface never invokes an unfrozen skill.

Booting the pinned environment image runs the aileron binary baked into that image against the host CLI that started the Launch ([ADR-0027](/adr/0027-flight-plan-sealed-installable-skill)). The launcher reads the image's `ai.aileron.cli.version` label and compares it to the host CLI version. A mismatch prints a stderr warning naming both versions. The Launch never fails on version skew. An image that carries no such label boots silently, because a custom image's contract is the operator's responsibility under ADR-0027.

Every Launch resolves its declared inputs once, at the launch boundary ([#1523](https://github.com/ALRubinger/aileron/issues/1523)). A surface passes literal-input overrides into that single resolution step. A surface never resolves inputs itself and never resolves them more than once.

Every Launch carries an idempotency key. The key identifies one Launch attempt so a retried submission from the same surface does not double-run a Flight. The key is supplied by the surface and recorded with the run.

Every Launch ties to an `identityLabel` for audit. The label is the same non-secret subject or account identity label in the manifest `credential` block ([Flight Plan Manifest Spec](/development/flight-plan-manifest-spec/) credential block). The label binds the Launch to an identity for audit ([ADR-0015](/adr/0015-launch-audit-scope/)). The label is never a credential value.

Three per-surface dimensions specialize this contract.

| Dimension | What it specializes |
|---|---|
| Invocation contract | The payload shape, the identity binding, and the idempotency key for this surface. |
| Preview and approval routing | How an effect-driven approval reaches the approver through the out-of-band channel for this surface. |
| Result delivery | How message-shaped and file-artifact outputs reach the person who Launched. |

Approval routing reuses the effect-driven gate. The trust contract declares an `effect` for each action, one of `read`, `write`, `delete`, `spend`, or `external-send` ([Flight Plan Manifest Spec](/development/flight-plan-manifest-spec/) trust contract). A `read` Launch runs unattended. A `write`, `delete`, `spend`, or `external-send` Launch raises an out-of-band approval gate and blocks on the decision ([ADR-0009](/adr/0009-user-channel)). The approval-time preview is fetched at decision time ([ADR-0016](/adr/0016-approval-preview/)). A surface routes the approval to the approver through its channel and surfaces the recorded decision in result delivery. A surface never confirms an effect inline in place of the out-of-band gate.

## Internal surfaces

The internal surfaces expose a Flight Plan to people inside the org. The four internal surfaces are the button in the org's own portal, the form, the Slack command, and integrations into the org's own tools.

### Portal button

The portal button is an embeddable widget the org places in its own portal. The button design is specified in [Embeddable widget designs](#embeddable-widget-designs), and its embedding mechanism is the script-tag-loaded web component with an iframe fallback in [Embedding mechanism](#embedding-mechanism).

The invocation contract is a button click that emits the embeddable invocation payload. The payload names the Flight Plan, the version, the resolved-input overrides, the idempotency key, and the identity label. The identity binding is the portal session identity carried into the `identityLabel`. The idempotency key is minted per click so a double click does not double-run.

The approval routing is the effect-driven out-of-band gate. A `read` Launch runs on click. A `write`, `delete`, `spend`, or `external-send` Launch raises the [ADR-0009](/adr/0009-user-channel) gate, and the button reflects a pending-approval state until the decision returns.

The result delivery is the result-render contract of [Embeddable widget designs](#embeddable-widget-designs). The widget renders a message-shaped result inline and renders each file artifact through the file-artifact rules in [Result delivery and file artifacts](#result-delivery-and-file-artifacts).

### Form

The form collects literal-input overrides as form fields and Launches on submit. The form is an embeddable widget, and its embedding mechanism is the script-tag-loaded web component with an iframe fallback in [Embedding mechanism](#embedding-mechanism).

The invocation contract is a form submission that maps each declared literal input to a form field. The submission emits the same invocation payload as the portal button. The identity binding is the form session identity carried into the `identityLabel`. The idempotency key is minted per submission so a resubmitted form does not double-run.

The approval routing is the effect-driven out-of-band gate, unchanged from the common contract.

The result delivery shows the run outcome on the form's result view. A message-shaped result renders inline. A file artifact renders through the file-artifact rules.

### Slack command

The Slack command is an embeddable command the org installs in its own workspace. The command design is specified in [Embeddable widget designs](#embeddable-widget-designs), and its embedding mechanism is slash-command registration plus an interactive-message callback in [Embedding mechanism](#embedding-mechanism).

The invocation contract is a slash-command invocation that emits the invocation payload. Command arguments map to declared literal inputs. The identity binding is the Slack user identity carried into the `identityLabel`. The idempotency key is minted per invocation.

The approval routing is the effect-driven out-of-band gate. A gated Launch routes the approval to the approver and posts the recorded decision back into the command's thread.

The result delivery posts the result into the command's Slack thread. A message-shaped result posts inline. A file artifact posts through the file-artifact rules, by reference when the artifact is large or binary.

### Integrations into the org's own tools

This surface lets the org Launch a Flight from its own internal tools through a programmatic trigger.

The invocation contract is a programmatic call that supplies the invocation payload. The identity binding is the calling system's identity carried into the `identityLabel`. The idempotency key is supplied by the calling system so a retried call does not double-run.

The approval routing is the effect-driven out-of-band gate. A gated Launch blocks on the [ADR-0009](/adr/0009-user-channel) decision before the run proceeds.

The result delivery returns the run outcome to the calling system. A message-shaped result returns inline. A file artifact returns through the file-artifact rules, by reference for large or binary artifacts.

## External surfaces

The external surfaces expose a Flight Plan to people outside the org. The four external surfaces are the customer portal button, the self-serve form, the public Slack or email channel, and the email reply parser.

### External auth model

The person Launching never operates an agent and never authenticates beyond the org's own surface. A customer interacts with the org's surface alone. The org's connectors perform the credentialed work behind that surface. Identity binding ties each Launch to an identity label for audit ([ADR-0015](/adr/0015-launch-audit-scope/)). The customer never holds a credential, never sees a credential, and never reaches the connectors directly. The org's frozen Flight Plan and its trust contract are the only thing the customer's request flows through.

### Customer portal button

The customer portal button is the embeddable button placed in a customer-facing portal.

The invocation contract is a button click that emits the invocation payload. The identity binding is the customer identity carried into the `identityLabel`. The idempotency key is minted per click.

The approval routing is the effect-driven out-of-band gate. The approver is in the org, not the customer. A gated Launch routes the approval to the org's approver through the [ADR-0009](/adr/0009-user-channel) channel, and the customer sees a pending state until the decision returns.

The result delivery renders the result in the customer portal. A message-shaped result renders inline. A file artifact renders through the file-artifact rules, including a hosted URL or iframe target for a surface that cannot execute JavaScript.

### Self-serve form

The self-serve form lets a customer Launch a Flight by filling in literal inputs.

The invocation contract is a form submission that maps declared literal inputs to fields and emits the invocation payload. The identity binding is the customer identity carried into the `identityLabel`. The idempotency key is minted per submission.

The approval routing is the effect-driven out-of-band gate, with the org as the approver.

The result delivery shows the outcome on the form's result view. A file artifact renders through the file-artifact rules, by reference for large or binary artifacts.

### Public Slack or email channel

This surface lets a customer Launch a Flight by posting into a public Slack channel or sending to an org email address.

The invocation contract parses the inbound message into the invocation payload. Message content maps to declared literal inputs. The identity binding is the sender identity carried into the `identityLabel`. The idempotency key is derived from the inbound message identity so a redelivered message does not double-run.

The approval routing is the effect-driven out-of-band gate, with the org as the approver.

The result delivery replies in the same channel. A message-shaped result replies inline. A file artifact replies through the file-artifact rules. An email reply that cannot render inline content receives a hosted URL.

### Email reply parser

The email reply parser Launches a Flight from a structured reply to an org email.

The invocation contract parses the reply body into the invocation payload. The reply structure maps to declared literal inputs. The identity binding is the replying sender identity carried into the `identityLabel`. The idempotency key is derived from the reply message identity.

The approval routing is the effect-driven out-of-band gate, with the org as the approver.

The result delivery sends a reply email. A message-shaped result is the reply body. A file artifact is delivered through the file-artifact rules. A large or binary artifact is delivered by reference as a hosted URL rather than an attachment.

## Result delivery and file artifacts

Result delivery covers named file-artifact outputs, not only message-shaped results. The artifacts are the ones declared in the Flight Plan `outputs:` contract ([Flight Plan Manifest Spec](/development/flight-plan-manifest-spec/) outputs contract). Three rules govern file-artifact delivery across every surface.

A named file artifact is published to its declared publish target. The `outputs:` contract names each artifact and its publish target. The runtime publishes each artifact to that target, whether an object store, an embedded portal surface, or email. The audit references each published artifact by its declared name, reusing the `outputs:` contract rule that the audit references artifacts by name ([Flight Plan Manifest Spec](/development/flight-plan-manifest-spec/) outputs contract).

A surface that cannot execute JavaScript receives a hosted URL or an iframe target rather than inline content. An email reply and a static surface cannot render an interactive result inline. The surface receives a hosted URL or an iframe target that points at the published artifact instead.

A large or binary artifact is published by reference, never inlined. A small text artifact may render inline. A large artifact or a binary artifact is published to its target and delivered as a reference to that target. This is consistent with v1 `utf-8`-only materialization. Binary and `base64` materialization is the deferred follow-up blocked on the host-ABI binary-body field and the tool step's mount or collect escape hatch ([#1510](https://github.com/ALRubinger/aileron/issues/1510)).

## Embeddable widget designs

The portal button, the form, and the Slack command are embeddable. Each is implementable from one invocation payload shape, one result-render contract, and one concrete embedding mechanism. The payload shape and the result-render contract are below; the per-widget mechanism is in [Embedding mechanism](#embedding-mechanism).

The invocation payload is the shape a widget emits to Launch a Flight. It names the Flight Plan, the frozen version, the resolved-input overrides, the idempotency key, and the identity label. The payload is a binding of values into the common Launch contract, never a step or a credential.

| Field | Required | Semantics |
|---|---|---|
| `flightPlan` | Yes | The Flight Plan name the widget Launches. |
| `version` | No | The frozen version id, the content hash plus the semver label. Absent selects the most recent frozen version. |
| `inputs` | No | The resolved-input overrides, one entry per declared literal input the widget collected. |
| `idempotencyKey` | Yes | The per-invocation idempotency key the widget mints so a repeated submission does not double-run. |
| `identityLabel` | Yes | The non-secret identity label carried for audit ([ADR-0015](/adr/0015-launch-audit-scope/)). |

The result-render contract is the shape a widget renders after a Launch. It carries the run identity, the approval decision when the Launch was gated, the message-shaped result, and the published file artifacts.

| Field | Required | Semantics |
|---|---|---|
| `runId` | Yes | The run identity the audit references. |
| `approvalDecision` | No | The recorded out-of-band approval decision, present when the Launch was gated ([ADR-0009](/adr/0009-user-channel)). |
| `message` | No | The message-shaped result rendered inline. |
| `artifacts` | No | The published file artifacts, each named and referenced by its declared `outputs:` name, delivered by reference when large or binary. |

A widget renders a message-shaped result inline. A widget renders each file artifact through the file-artifact rules in [Result delivery and file artifacts](#result-delivery-and-file-artifacts). A widget on a surface that cannot execute JavaScript renders the hosted URL or iframe target the runtime publishes.

### Embedding mechanism

Each embeddable widget commits to a concrete embedding mechanism so engineering implements against the mechanism, not only against the field and contract shapes above. The mechanism is fixed per widget. The invocation payload and the result-render contract are the same across mechanisms.

The portal button and the form embed as a **script-tag-loaded web component**. The org places a single `<script>` tag that registers a custom element (`<aileron-launch-button>` for the portal button, `<aileron-launch-form>` for the form). The org then drops the custom element into its own portal markup and configures it with the `flightPlan` and, optionally, the `version` from the invocation payload. The custom element collects the resolved-input overrides, mints the per-invocation idempotency key, binds the surface session identity into the `identityLabel`, emits the invocation payload, and renders the result-render contract inline through its shadow DOM. The custom element is the primary mechanism because it composes into the host portal's own DOM and styling without a nested document.

The portal button and the form fall back to an **iframe** for a surface that cannot execute the script tag or the custom element. The org embeds an `<iframe>` whose `src` is the hosted widget for that Flight Plan. The hosted widget inside the iframe emits the same invocation payload and renders the same result-render contract. The iframe fallback is the same hosted-URL-or-iframe target the [Result delivery and file artifacts](#result-delivery-and-file-artifacts) rules name for a surface that cannot execute JavaScript inline. The web component is primary and the iframe is the fallback, never the reverse.

The Slack command embeds as a **slash-command registration plus an interactive-message callback**. The org installs a Slack app that registers the slash command in the org's own workspace. A slash-command invocation maps the command arguments to declared literal inputs, mints the per-invocation idempotency key, binds the Slack user identity into the `identityLabel`, and emits the invocation payload. The interactive-message callback is the registered request URL that receives the effect-driven approval decision and the result-render contract. The callback posts the recorded approval decision and the message-shaped result back into the command's thread and posts each file artifact through the file-artifact rules, by reference when the artifact is large or binary. Slack does not load the web component or the iframe; the slash-command registration and the interactive-message callback are its embedding mechanism end to end.

## References

- [ADR-0027](/adr/0027-flight-plan-sealed-installable-skill). The Flight Plan as a sealed installable skill, the freeze boundary, and the layer split this spec distributes over
- [The Flight Plan manifest specification](/development/flight-plan-manifest-spec/). The trust contract, the `outputs:` contract, and the `credential` block this spec binds to
- [Launching a Flight Plan](/guides/launching-a-flight-plan/). The deterministic Launch runtime a surface invokes
- [ADR-0009](/adr/0009-user-channel/). The out-of-band approval channel the per-action effect routes through
- [ADR-0016](/adr/0016-approval-preview/). The approval-time preview fetch the gated routing surfaces
- [ADR-0015](/adr/0015-launch-audit-scope/). The launch audit scope the identity label and the artifact names are recorded under
- [Issue #1511](https://github.com/ALRubinger/aileron/issues/1511). The deterministic Launch runtime a surface invokes
- [Issue #1519](https://github.com/ALRubinger/aileron/issues/1519). The output-contract scope that brings named file-artifact outputs into result delivery
- The distribution-surfaces spec ([#928](https://github.com/ALRubinger/aileron/issues/928)) is absorbed and quoted here, not depended on.
