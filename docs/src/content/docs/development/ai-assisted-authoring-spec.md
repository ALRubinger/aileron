---
title: "AI-Assisted Authoring UX Spec"
description: "The AI-assisted authoring experience that takes a non-technical operator from describing an outcome to a signed Flight Plan, with the AI owning composition and the human owning the trust contract."
order: 11
---

This page specifies the AI-assisted authoring experience for a Flight Plan. A Flight Plan is a sealed Aileron skill ([ADR-0027](/adr/0027-flight-plan-sealed-installable-skill)). This spec defines how a non-technical operator, with AI assistance, goes from describing an outcome in plain language to an attested pre-freeze manifest ready to hand to freeze. It is a spec deliverable that defines a UX. It ships no code. The fields the editor surfaces are exactly the trust-contract fields specified by the [Flight Plan Manifest Spec](/development/flight-plan-manifest-spec/), surfaced in business language and never invented here.

The spine of this experience is a division of labor. The AI owns composition combinatorics, which actions to call, in what order, with what argument bindings. The human owns the trust contract, the effect labels, the approval routing, and the redaction rules. Neither does the other's job. The AI scaffolds the composition and proposes contract values. Only the human's attestation, carried into freeze as the detached signature, says that the trust contract is correct, mirroring the [ADR-0027](/adr/0027-flight-plan-sealed-installable-skill) decision that the detached signature is the human attestation that the trust contract is correct.

This page ends at the freeze handoff. It specifies what the signing step feeds freeze. It does not specify freeze's internals. Freeze is tracked in [#1509](https://github.com/ALRubinger/aileron/issues/1509) and is referenced here, not depended on.

## The authoring lifecycle

Authoring is four ordered phases. Each phase has one job and hands a defined artifact to the next.

| Phase | Name | The operator's job | What it produces |
|---|---|---|---|
| 1 | Describe the outcome | State the desired outcome in natural language and confirm scope. | A confirmed outcome statement. |
| 2 | Scaffold from connected actions | Review the AI's proposed composition over connected actions. | A reviewed draft step graph. |
| 3 | Sign the trust contract | Confirm each human-owned trust-contract field and attest it with a signature. | An attested pre-freeze manifest. |
| 4 | Freeze | Hand the attested manifest to freeze. | A sealed, signed Flight Plan. |

The author-to-freeze boundary is where this spec ends. Phases 1 through 3 are the authoring surface. Phase 4 is the handoff. The authoring tool prompts the operator through phases 1 through 3 and produces the attested manifest that freeze ([ADR-0027](/adr/0027-flight-plan-sealed-installable-skill), [#1514](https://github.com/ALRubinger/aileron/issues/1514)) consumes. The AI owns composition across phases 1 and 2. The human owns the trust contract in phase 3. The operator's attestation in phase 3 is the gate that no AI proposal can pass on its own.

## Phase 1: Describe the outcome

The operator opens the authoring tool and describes the outcome they want in natural language. The opening prompt asks for the result, not the steps. An example of the copy intent is "What outcome do you want this Flight Plan to produce?" rather than "Which actions should run?". The operator answers in their own words, such as "every Monday, summarize last week's support tickets and file a tracking issue with the summary".

The AI reads the outcome and asks clarifying questions. The questions resolve ambiguity that changes the composition or the trust contract. The AI asks about the time window when the outcome says "last week", about which destination when the outcome says "file an issue", and about who should be asked before anything is written or sent. The questions are in plain language and never expose a field name. The operator answers each one or accepts the AI's suggested default.

Phase 1 ends with a scope confirmation. The AI restates the outcome and the resolved ambiguities in one summary, and the operator confirms it before any scaffolding begins. The confirmation is an explicit decision point. The operator can edit the outcome statement and re-confirm. Scaffolding does not start until the operator confirms the scope. The confirmed outcome statement is the input to phase 2.

## Phase 2: Scaffold from connected actions

Phase 2 is the action-picker. The AI scaffolds a composition from the actions the operator has connected. The operator reviews the composition. The AI proposes. The human reviews.

The tool surfaces the connected actions as a catalog the operator can see. Each action is shown in business language, named by what it does rather than by its `aileron:<connector>.<action>` reference. The AI selects from this catalog the actions the outcome needs and orders them into the deterministic step graph specified by the [Flight Plan Manifest Spec](/development/flight-plan-manifest-spec/). The operator sees the proposed ordering as a readable sequence of steps, each labeled by its outcome contribution.

The AI binds each step's arguments. A binding is a reference, never a value, consistent with the manifest spec's binding grammar. A binding takes one of two forms. An `inputs.<name>` binding references a declared input the operator supplies at launch, such as a time window. A `steps.<id>.<output>` binding references a named output of a prior step, such as the summary text produced by an earlier step. The authoring tool presents bindings in business language, showing "the summary from the previous step" rather than the raw `steps.<id>.<output>` reference, while the artifact records the reference form.

The operator reviews the proposed composition. The review is a decision point. The operator can ask the AI to add a step, remove a step, reorder steps, or change a binding, and the AI re-proposes. The operator does not hand-write the step graph. The AI owns the composition combinatorics and the operator confirms the result. The reviewed draft step graph is the input to phase 3.

## Phase 3: Sign the trust contract

Phase 3 has two steps. The operator edits the trust contract in business language, then attests it with a signature. Both steps are the human's job. The trust-contract editor comes first and signing closes the phase.

### The trust-contract editor

The trust-contract editor is the core of this experience. Every field it surfaces is a trust-contract field specified by the [Flight Plan Manifest Spec](/development/flight-plan-manifest-spec/). The editor invents no fields. It presents each field as a business-language affordance, a question or control an operator answers without reading a schema.

The mapping below is the editor's contract. Each row is one trust-contract field from the manifest spec and its operator-facing affordance.

| Trust-contract field | Editor affordance (operator-facing) |
|---|---|
| `effect` | "What does this step do to the outside world?" presented as a plain-English consequence: it reads data, it writes data, it deletes data, it spends money, or it sends something out. The operator picks the consequence, not the enum value. |
| approval routing (driven by `effect` via [ADR-0009](/adr/0009-user-channel)) | "Who gets asked before this runs?" The routing default follows the effect, so a write, delete, spend, or external-send proposes asking the operator, and a read proposes asking no one. The operator sees who will be asked and can widen it. |
| `redaction` | "What should be hidden from the agent and from this Flight Plan's author?" The operator names the response fields to hide, such as customer emails or ticket bodies, in plain terms. |
| `credential` (kind and placement) | "How does this step sign in, and is that an API key, an OAuth login, an AWS-signed request, or nothing?" The operator confirms the sign-in kind. The editor never asks for the secret itself, only its kind and where it goes on the wire. |
| `oauth` | "Which permissions does this OAuth login grant?" Shown as the scope list the connector declares, in readable terms, when the sign-in kind is an OAuth login. |
| `hosts` and `paths` | "Which services and endpoints may this step reach?" The operator confirms the upstream hosts and paths. The editor states that this access-scope declaration IS the security boundary and that nothing undeclared is granted. |
| `idempotency` | "Is this step safe to run again if it is retried?" The operator confirms whether re-running with the same inputs produces no additional effect. |
| `verification` | "May Aileron run a read-only check to confirm the sign-in still works?" The operator confirms the optional read-only probe. |
| `audit` | "What gets written to the audit record when this step runs?" The operator sees the audit fields this step emits and the customer-owned sink, in readable terms. |

The editor enforces one invariant. The AI may pre-fill proposed values for these fields, but the human-owned fields require explicit human confirmation. The editor never lets the AI silently set a human-owned field. A proposed `effect`, approval route, or `redaction` rule is shown as a proposal the operator must confirm before it counts. The operator can accept the AI's proposal or change it, and the act of confirming is the human's, not the AI's. This is the authoring-time expression of the invariant that the AI cannot bypass the trust contract. The AI scaffolds composition and proposes contract values, and only the human's attestation, recorded by the signing step that follows, attests them.

### Signing

Signing closes phase 3. The operator reviews the assembled contract one last time and signs. The signature is the human attestation that the trust contract is correct ([ADR-0027](/adr/0027-flight-plan-sealed-installable-skill)).

The attestation is specific. By signing, the operator confirms that every effect label is right, that every approval route asks the right people, that every redaction rule hides the right fields, and that every declared host and path is an access scope they intend to grant. The signature does not attest the AI's composition for its own sake. It attests the trust contract the composition runs under.

The signing step records the operator's attestation against the authored manifest. The authoring step produces the attested manifest, which is freeze's input. Freeze produces the detached signature over the content-addressed manifest plus the lockfile, per the [ADR-0027](/adr/0027-flight-plan-sealed-installable-skill) "Manifest and signing" decision, because the lockfile and the digest pins it references are produced by freeze. The version that signature is recorded against is a content hash plus a semver label, matching the manifest spec's [lock section](/development/flight-plan-manifest-spec/#freeze-and-the-lock-section). The content hash identifies the exact frozen bytes and the semver label is the human-facing version name. The authoring tool carries the operator's attestation to freeze, and freeze is where the attestation becomes the detached signature over the frozen bytes.

## Phase 4: Freeze (the handoff boundary)

This page ends at the attested manifest. The authoring tool hands freeze an attested pre-freeze manifest carrying the operator's confirmation that the trust contract is correct. Freeze ([#1509](https://github.com/ALRubinger/aileron/issues/1509)) resolves every image reference to a content-addressed digest, produces the lockfile that pins those digests and the resolved capability set, binds the execution environment, attaches the per-action trust contract, and produces the sealed Flight Plan with the detached signature over the manifest plus the lockfile. None of those freeze internals are specified here. They are specified by [#1509](https://github.com/ALRubinger/aileron/issues/1509) and the [Flight Plan Manifest Spec](/development/flight-plan-manifest-spec/). The boundary is the attested manifest, and this spec stops at it.

## Absorbed prior art

The authoring model is absorbed from [#927](https://github.com/ALRubinger/aileron/issues/927), quoted here as context per the [ADR-0027](/adr/0027-flight-plan-sealed-installable-skill) absorb-and-quote convention. The #927 model framed authoring as a describe-then-assemble flow where a non-technical operator states intent and the tool assembles a runnable composition. This spec absorbs that framing and makes the division of labor precise. The AI owns the assembly and the human owns the trust contract. There is no dependency edge to #927. Its authoring model is context this spec carries forward, not a unit this spec waits on.
