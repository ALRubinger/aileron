---
title: "Flight Plan Manifest Spec"
description: "The SKILL.md frontmatter extension that declares a Flight Plan's requires block, per-action trust contract, inputs, outputs, deterministic step graph, and frozen lock section."
order: 9
---

The Flight Plan manifest is the machine-readable contract that both the authoring tool prompts an operator to fill in and the runtime enforces. A Flight Plan is a sealed Aileron skill ([ADR-0027](/adr/0027-flight-plan-sealed-installable-skill)). The skill is the authoring artifact and the Flight Plan is the same artifact after a freeze step seals it. One format spans both states. This page specifies that format field by field.

The manifest is the YAML frontmatter of a `SKILL.md` document in the [agentskills.io](https://agentskills.io) format, extended with a single Aileron-specific block. Every Aileron field lives under one namespaced top-level key so the extension is lossless if stripped. A host without Aileron reads a valid skill and treats the Aileron block as inert.

The deterministic step-graph composition format is specified on this single page rather than a separate sibling page. It is one coherent format covered by one drift-guard test, amended in place under the [Composition and the step graph](#composition-and-the-step-graph) section below. The existing "Flight Plan Manifest Spec" navigation entry stands and no new navigation entry is added.

This page is the normative format spec. The Go parser and validator ship under [#1508](https://github.com/ALRubinger/aileron/issues/1508). Freeze ships under [#1509](https://github.com/ALRubinger/aileron/issues/1509): `aileron skill freeze` resolves image references to digests, produces the lockfile, content-addresses the unit, and signs it. Runtime enforcement is tracked in [#1511](https://github.com/ALRubinger/aileron/issues/1511). The `lock` section below is freeze's target, and freeze now populates it; the schema on this page stays the authoritative shape.

## Schema

The normative schema is JSON Schema draft 2020-12. It lives outside the docs collection so the site never renders it as a page:

```text
docs/schema/flight-plan-manifest.schema.json
```

A worked, multi-action example that validates against the schema lives beside it:

```text
docs/schema/flight-plan-manifest.example.skill.md
```

A committed Go drift-guard test (`internal/flightplan/spec`) keeps the example and the schema from drifting apart. The Aileron block is the only part the schema constrains. The skill keys it sits beside (`name`, `description`, and so on) are governed by the agentskills.io format. The Aileron block is named `aileron` at the top level of the frontmatter:

```yaml
---
name: weekly-metrics-digest
description: Read a metrics window, summarize it, and file a tracking issue.
aileron:
  schemaVersion: aileron.flightplan.v1
  requires:
    actions: []          # the actions the plan calls, each with a trust contract
    executionEnvironment: {}  # the rung-1 or rung-2 execution image
  inputs: []             # declared inputs with resolution rules
  outputs: []            # declared output artifacts
  steps: []              # the deterministic step graph, optional
  # lock: produced by freeze, absent before freeze
---
```

## Field reference

The tables below give every field's type, required-ness, and semantics. Field names align with [ADR-0003](/adr/0003-action-model) (effect, idempotency, approval routing) and [ADR-0019](/adr/0019-v4-https-data-plane) (the credential-placement scheme set). The `idempotency`, `credential`, `hosts`, and `audit` vocabulary reuses the connector-spec names from the [Sandbox Connector Specs](/development/sandbox-connector-specs/) page rather than inventing parallel ones.

### `aileron` (top-level block)

| Field | Type | Required | Semantics |
|---|---|---|---|
| `schemaVersion` | string | No | When present, must be `aileron.flightplan.v1`. Identifies the block contract this manifest was authored against. |
| `requires` | object | Yes | The action and execution-environment dependencies. |
| `inputs` | array | Yes | The declared inputs, each with a resolution rule. |
| `outputs` | array | Yes | The declared output artifacts. |
| `steps` | array | No | The deterministic step graph. The composition that wires actions and transforms into a directed acyclic graph. Absent when the skill is instruction-only. |
| `lock` | object | No | The frozen lock and digest section. Absent before freeze. |

### `requires`

| Field | Type | Required | Semantics |
|---|---|---|---|
| `actions` | array | Yes | The actions the plan calls. At least one entry. Each entry carries a per-action trust contract. |
| `executionEnvironment` | object | No | The execution image the plan runs in, declared as one rung. |

Each `actions[]` entry:

| Field | Type | Required | Semantics |
|---|---|---|---|
| `ref` | string | Yes | The action reference in the form `aileron:<connector>.<action>`. Attaches to the action model of [ADR-0003](/adr/0003-action-model). |
| `trustContract` | object | Yes | The per-action trust contract for this action. |

An unsatisfied `requires:` entry is a missing-requirement signal the runtime surfaces. It is never a parse failure. A manifest that names an action the host has not installed still parses as a valid skill.

### `requires.executionEnvironment`

The execution image is assembled from rungs ([ADR-0027](/adr/0027-flight-plan-sealed-installable-skill) execution rungs). Exactly one of `rung1Image`, `rung2CapabilityUnits`, or `rung3PerStepImages` is declared when `executionEnvironment` is present. The three rungs are mutually exclusive. Freeze resolves the declared rung to content-addressed digest pins recorded in the lock section.

| Field | Type | Required | Semantics |
|---|---|---|---|
| `rung1Image` | object | No | Rung one. Names a whole prebuilt operator-owned image. Freeze resolves it to a digest. |
| `rung1Image.ref` | string | Yes within `rung1Image` | An OCI image reference. Freeze resolves a tag to an `image@sha256:` digest pin. |
| `rung2CapabilityUnits` | object | No | Rung two. Declares capability units composed onto the Aileron agent-free base image. |
| `rung2CapabilityUnits.features` | array | Yes within `rung2CapabilityUnits` | The capability-unit devcontainer Feature references ([ADR-0026](/adr/0026-cli-capability-units)). |
| `rung3PerStepImages` | object | No | Rung three. Per-step sealed sibling-image dispatch with mount and run-and-collect I/O. Freeze resolves each step's sibling image to a digest pin recorded in the lock. |
| `rung3PerStepImages.steps` | array | Yes within `rung3PerStepImages` | The per-step sibling images. Non-empty; freeze pins one digest per step. |
| `rung3PerStepImages.steps[].image` | string | Yes within a step | An OCI image reference for the per-step sibling tool. Freeze resolves a tag to an `image@sha256:` digest pin. |
| `rung3PerStepImages.steps[].id` | string | No | An optional step identifier, unique within the rung-three step set. |
| `rung3PerStepImages.steps[].mount` | object | No | An optional mount declaration for the step's input I/O. `mount.path` is the mount path inside the image. |
| `rung3PerStepImages.steps[].collect` | object | No | An optional run-and-collect declaration for the step's output I/O. `collect.path` is the path whose contents are collected as the step output. |
| `rung3PerStepImages.steps[].trustContract` | object | No | An optional per-step trust contract reusing the per-action shape. Its `hosts` declare the step's network reach. Freeze seals the declared reach onto the step's lock pin so the signature covers it. A declared contract must be non-empty. |

### `requires.actions[].trustContract`

The trust contract is the field set absorbed from #925, quoted here as context rather than depended on. Freeze attaches this contract, and the detached signature is the human attestation that it is correct.

| Field | Type | Required | Semantics |
|---|---|---|---|
| `credential` | object | Yes | The credential kind and placement. Never a credential value. |
| `oauth` | object | Required when `credential.kind` is `oauth2` | OAuth scopes, endpoints, and refresh behavior. |
| `hosts` | array | Yes | The expected upstream hosts. The access-scope declaration IS the security boundary. The runtime grants nothing undeclared. |
| `paths` | array | No | The expected request paths on the declared hosts. Part of the access-scope declaration. |
| `effect` | string | Yes | One of `read`, `write`, `delete`, `spend`, `external-send`. Aligned to [ADR-0003](/adr/0003-action-model). Drives default approval routing through [ADR-0009](/adr/0009-user-channel). |
| `idempotency` | object | Yes | Whether the operation is safe to retry and whether it supports an idempotency key. |
| `redaction` | array | No | Rules applied to response fields before they reach the agent or author. |
| `verification` | object | No | A read-only probe the runtime can call to confirm the credential still works. |
| `audit` | object | Yes | The structure of the audit record this operation emits. |

The `credential` block:

| Field | Type | Required | Semantics |
|---|---|---|---|
| `kind` | string | Yes | One of `none`, `api-key`, `oauth2`, `aws-sigv4`. |
| `placement` | string | Yes unless `kind` is `none` | One of `header`, `query`, `cookie`, `body`, `signing`, `session`. Maps 1:1 to the closed [ADR-0019](/adr/0019-v4-https-data-plane) injection-scheme set. A `none` credential has no wire placement. |
| `identityLabel` | string | No | A subject or account identity label for multi-tenant binding. A non-secret label. |

An `oauth2` credential must declare the `oauth` block. An `aws-sigv4` credential is always placed via `signing`, and an `oauth2` credential is always placed in a `header`.

The placement values map to the [ADR-0019](/adr/0019-v4-https-data-plane) wire mechanisms. `header` is a bearer or templated header (`bearer` or `header-template`). `query` is `query-param`. `signing` is AWS SigV4, which maps to `sigv4-resign` and is sealed by [#1501](https://github.com/ALRubinger/aileron/issues/1501). `session` is the cookie or session-token wire form for a session-authenticated upstream. `body` carries the credential in the request body. The exact header or wire shape is per service, not per placement alone.

The `idempotency` block:

| Field | Type | Required | Semantics |
|---|---|---|---|
| `safeToRetry` | boolean | Yes | True when re-running with the same inputs produces no additional effect. |
| `idempotencyKey` | boolean | No | True when the operation accepts a client-supplied idempotency key. Defaults to false. |

The `audit` block:

| Field | Type | Required | Semantics |
|---|---|---|---|
| `fields` | array | Yes | The closed set of audit-record fields this operation emits. |
| `sink` | string | No | A reference to the customer-owned audit sink. A non-secret reference. |

The closed `audit.fields` set is `connector-hash`, `action-manifest-version`, `credential-binding`, `identity-label`, `approved-input`, `approval-decision`, `network-target`, `operation-effect`, `request-summary`, `response-summary`, and `result`.

### `inputs[]`

| Field | Type | Required | Semantics |
|---|---|---|---|
| `name` | string | Yes | The input name, unique within the manifest. |
| `type` | string | Yes | One of `string`, `number`, `boolean`, `timestamp`, `object`, `array`. |
| `description` | string | No | Human-readable semantics. |
| `resolution` | object | Yes | The resolution rule. One of `literal`, `dynamic`, or `source`. |

### `outputs[]`

| Field | Type | Required | Semantics |
|---|---|---|---|
| `name` | string | Yes | The artifact name, unique within the manifest. The audit references artifacts by this name. |
| `mimeType` | string | Yes | The artifact media type. |
| `encoding` | string | Yes | One of `utf-8`, `base64`. `base64` is reserved. v1 implements `utf-8` only. |
| `publish` | object | Yes | The publish target. `target` is `file` or `none`; `path` is required and names the output file when `target` is `file`. |

### `steps[]`

Every step carries an `id` unique within the graph and a `kind`. The remaining fields depend on the kind.

| Field | Type | Required | Semantics |
|---|---|---|---|
| `id` | string | Yes | The step id, unique within the step graph. Later steps reference this step's outputs as `steps.<id>.<outputName>`. |
| `kind` | string | Yes | One of `action-call`, `transform`, `llm-seam`. A closed enum. |
| `actionRef` | string | Yes for `action-call` | The action this step invokes, in the form `aileron:<connector>.<action>`. Must match a declared `requires.actions[].ref`. |
| `args` | object | No (`action-call` only) | The action arguments. Each value is a binding reference, never a value. |
| `bindings` | object | No (`transform`, `llm-seam`) | The named inputs to the step. Each value is a binding reference. |
| `outputs` | array | Yes for `transform` and `llm-seam`, No for `action-call` | The named results the step produces. |
| `materializesOutput` | string | No | Names a declared `outputs[].name` artifact this step's result materializes into. |

## Composition and the step graph

The `steps` block is the deterministic composition ([ADR-0027](/adr/0027-flight-plan-sealed-installable-skill)). It wires declared actions and deterministic transforms into a directed acyclic graph. The block is optional. A skill with no `steps` block is instruction-only and still a valid manifest.

There are three step kinds.

| `kind` | Reaches an LLM | Semantics |
|---|---|---|
| `action-call` | No | Invokes a declared action. Its `actionRef` names the action and its `args` bind the action's arguments. |
| `transform` | No | Runs deterministic no-LLM logic over data already in the graph. It has no host, network, or credential surface. |
| `llm-seam` | Yes | The single marked non-deterministic seam. The only kind that reaches an LLM. |

The `kind` enum is closed. By construction no kind other than `llm-seam` can reach an LLM, so the no-LLM guarantee is structurally checkable. The `kind: llm-seam` value is the first-class mark the freeze and lint step ([#1509](https://github.com/ALRubinger/aileron/issues/1509)) checks, and the runtime ([#1511](https://github.com/ALRubinger/aileron/issues/1511)) enforces that only that one seam reaches an LLM. A `transform` is the structural guarantee that intermediate logic stays deterministic.

A step reads its data through bindings. A binding is a reference, never a value. An `action-call` step binds its `args`; a `transform` and an `llm-seam` step bind their `bindings`. A binding takes one of two forms. `inputs.<name>` references a declared input resolved once at the launch boundary ([#1523](https://github.com/ALRubinger/aileron/issues/1523)). `steps.<stepId>.<outputName>` references a named output of a prior step. The references make the wiring a graph.

The step graph is a directed acyclic graph. The runtime executes it in deterministic topological order. A JSON Schema cannot express acyclicity, so the schema does not enforce it. The acyclicity rule and the topological-order rule are stated here as invariants, the freeze and lint step ([#1509](https://github.com/ALRubinger/aileron/issues/1509)) checks them, and the runtime ([#1511](https://github.com/ALRubinger/aileron/issues/1511)) enforces them. The drift-guard test asserts the worked example's references resolve and are acyclic.

An `action-call` step's `actionRef` must match a declared `requires.actions[].ref`. A JSON Schema cannot express cross-array membership, so this too is a stated invariant the lint step checks and the drift-guard asserts on the example.

A step result wires to a declared output artifact through `materializesOutput`. The value names a declared `outputs[].name`. This is how a step result reaches the declared outputs contract that the runtime materializes through the file-map transport. The split between the declared outputs contract and the file-map transport is unchanged. See the [Outputs contract versus file-map transport](#outputs-contract-versus-file-map-transport) section. v1 materializes `utf-8` artifacts only and `base64` stays reserved.

No step field may hold a secret. This extends the credential-sealing invariant. A step binds references, never values. Every step kind is a closed object, so an `additionalProperties` key carrying a value is rejected. The `args` and `bindings` values are binding references whose grammar is closed to `inputs.<name>` and `steps.<id>.<output>`, so a literal secret cannot be embedded in the wiring.

The composition lives under the `aileron` block, so it is lossless if stripped. Removing the block removes the `steps` graph with it and leaves a valid instruction-only skill.

## Lossless if stripped

The extension is lossless if stripped. A tool that ignores the Aileron fields still reads a valid skill. The schema encodes this guarantee two ways. The `aileron` block is optional at the top level, so a manifest with the block removed is not rejected. Every Aileron-specific field is nested under that one key, so removing it leaves the agentskills.io skill keys untouched.

The same worked example with its Aileron block removed is still a valid skill:

```yaml
---
name: weekly-metrics-digest
description: Read a metrics window, summarize it, and file a tracking issue.
license: Apache-2.0
---

# Weekly Metrics Digest

This skill reads a recent metrics window, writes a short digest, and files a
tracking issue that links to it.
```

A host without Aileron parses the document above and runs it as an instruction-only skill. An unsatisfied `requires:` is a missing-requirement signal, never a parse failure, so a host that does read the Aileron block but lacks a named action reports a missing requirement rather than refusing to load the document.

## Credential sealing invariant

The manifest declares credential kind and placement. It never declares a credential value. This is load-bearing and follows [ADR-0005](/adr/0005-sandbox-choice) and [ADR-0019](/adr/0019-v4-https-data-plane). The runtime mediates credential use at the data-plane proxy boundary so the composed code never holds a raw credential.

No field in the schema can hold a secret. The `credential` block declares `kind`, `placement`, and a non-secret `identityLabel`. The `oauth` block declares scopes, endpoint URLs, and refresh behavior, never a token. The `audit` structure records a credential binding reference, never the credential bytes. The frozen artifact and the audit records carry bindings and references, never secret material.

## Inputs and resolution

A Flight Plan is a pinned, agent-invariant function over its declared inputs ([ADR-0027](/adr/0027-flight-plan-sealed-installable-skill), durable record in [#1523](https://github.com/ALRubinger/aileron/issues/1523)). It is deterministic given its resolved inputs. Results legitimately vary as inputs and externally-connected data vary. The crisp line is that an LLM in the runtime loop is forbidden because it injects non-determinism into the logic, while a live or time-relative data read is an allowed input that varies the data, not the logic.

Every input the plan depends on is declared, each with a resolution rule. A value that varies by use case, such as a time window, is a declared input rather than a constant baked into the composition, so one composition serves many operators. Inputs resolve once, at the launch boundary, into a concrete resolved-input set. Two steps that read the same moving value such as the wall clock see one resolved value.

There are three resolution rules, discriminated by the `rule` field.

| `rule` | Required fields | Semantics |
|---|---|---|
| `literal` | none beyond `rule` | A value passed at launch. An optional `default` applies when no value is passed. |
| `dynamic` | `value` | A launch-relative value resolved once at launch. `value` is `now` (the launch timestamp) or `today` (the launch date). |
| `source` | `source` | A read from a live source. `source.actionRef` names the action whose result resolves the input, with an optional `source.select`. |

## Outputs contract versus file-map transport

The `outputs:` block is the declared interface. The file-map is the transport. They are kept distinct so the transport can change later without changing the declared contract ([#1519](https://github.com/ALRubinger/aileron/issues/1519)).

The `outputs:` block names each artifact, its `mimeType`, its `encoding`, and its publish target. This is the contract the runtime materializes and distribution publishes, and the audit references artifacts by name. The transport is a typed JSON file-map. A step returns an array of `{path, mimeType, encoding, content}` entries that the runtime materializes into files. The file-map generalizes the existing `get-file-content` shape. The `outputs:` block is the contract that names what those files are.

Both the file-entry and the `outputs:` contract carry `mimeType` and `encoding`. The `encoding` enum is `utf-8` and `base64`. v1 implements `utf-8` only. Text-only is the v1 implementation, never the declared interface. Binary outputs are a deferred follow-up blocked on a host-ABI binary-body field, and the mount or run-and-collect boundary ([#1510](https://github.com/ALRubinger/aileron/issues/1510)) is the escape hatch for large or binary artifacts later.

## Audit record structure

The audit records resolved inputs to outputs ([ADR-0027](/adr/0027-flight-plan-sealed-installable-skill) audit boundary, [#1523](https://github.com/ALRubinger/aileron/issues/1523)). A scalar input is recorded by value, so a `current_timestamp` input that resolved to a concrete `as_of` is written down as that timestamp. A data read is recorded by its resolved binding, which is the parameters, the query, and a result or snapshot identifier with a summary, never the full dataset inline. The dataset is the run's recorded output, and the audit references it rather than duplicating it.

Each operation's `audit.fields` declares which of the closed record fields it emits. The closed set is `connector-hash`, `action-manifest-version`, `credential-binding`, `identity-label`, `approved-input`, `approval-decision`, `network-target`, `operation-effect`, `request-summary`, `response-summary`, and `result`. The sink is customer-owned. The recorded binding is what makes a past run explainable without reconstructing it from a moving source.

## Freeze and the lock section

Freeze turns a skill into a Flight Plan ([ADR-0027](/adr/0027-flight-plan-sealed-installable-skill) freeze boundary). Freeze resolves every image reference to a content-addressed digest, produces a lockfile that pins those digests and the resolved capability set, binds the execution environment, attaches the per-action trust contract, and signs the result. The `lock` section is the record of those pins.

The `lock` section is absent before freeze. Freeze populates it and writes an immutable, signed version into the canonical skill store. See the [Freezing a Flight Plan](/guides/freezing-a-flight-plan) guide for the command and the reproducibility and signature-verification guarantees. See the [Launching a Flight Plan](/guides/launching-a-flight-plan) guide for how the runtime ([#1511](https://github.com/ALRubinger/aileron/issues/1511)) verifies a frozen unit, resolves inputs once at the launch boundary, walks the step graph with no model in the loop, enforces the per-action trust contract, and materializes the declared outputs.

| Field | Type | Required | Semantics |
|---|---|---|---|
| `resolvedImages` | array | No | The resolved image digest pins. Each entry pairs a pre-freeze `ref` with its resolved `digest`. A rung-three per-step pin also carries the dispatching step's `id` and, when the step declared a trust contract, the sealed `hosts` reach. |
| `resolvedCapabilitySet` | array | No | The resolved capability set the freeze pinned. |
| `contentHash` | string | No | The content hash identifying the exact frozen manifest bytes. |
| `version` | string | No | The human-facing semver label for this frozen version. |

The version is the content hash plus a semver label. The content hash identifies the exact frozen bytes. The semver label is the human-facing version name.

## Execution rungs

A Flight Plan runs on a composed execution image assembled from rungs ([ADR-0027](/adr/0027-flight-plan-sealed-installable-skill) execution rungs). Rung one pins a whole prebuilt operator-owned image, declared as `rung1Image`. Rung two declares capability units that Aileron composes onto a generic agent-free minimal base image, declared as `rung2CapabilityUnits` whose Features follow [ADR-0026](/adr/0026-cli-capability-units).

Rung three is a per-step sealed sibling-image dispatch with mount and run-and-collect I/O carried in the `rung3PerStepImages` rung. Freeze resolves each step's sibling image to a content-addressed digest pinned in the lock. The execution image is agent-free. The base image carries no coding agent. The Flight Plan runs composed steps, not an interactive agent session.
