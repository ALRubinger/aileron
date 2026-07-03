---
title: "Authoring a Flight Plan"
description: "Hand-write a Flight Plan SKILL.md: the requires block for actions and execution environments, per-action trust contracts, declared inputs and outputs, the deterministic no-LLM step graph, and the handoff to freeze and sign."
---

This guide walks you from an empty file to a skill ready to freeze. By the end you will have a single `SKILL.md` document that declares the actions it calls, the environment it runs in, the inputs it resolves, the outputs it produces, and a deterministic step graph that wires them together with no language model in the loop.

If you have not read it yet, start with [Flight Plans](/concepts/flight-plans/) for the model. This guide is the *how*; that page is the *what* and *why*. The normative field reference is the [Flight Plan Manifest Spec](/development/flight-plan-manifest-spec/), and the companion guides downstream are [Freezing a Flight Plan](/guides/freezing-a-flight-plan/) and [Launching a Flight Plan](/guides/launching-a-flight-plan/).

This guide covers authoring by hand. The same manifest is also the form an [AI-assisted authoring flow](/development/ai-assisted-authoring-spec/) prompts an operator to fill in, but the artifact is identical either way.

## What you are building

A Flight Plan is a skill before it is sealed. The skill is one `SKILL.md` document in the [agentskills.io](https://agentskills.io) format, extended with a single Aileron-specific block. You author the skill. A freeze step turns it into a Flight Plan by resolving images to digests, locking, binding the execution environment, attaching the trust contract, and signing.

This shapes how you author. You write the contract; you do not write the seal. The `requires:` block, the inputs, the outputs, the step graph, and the per-action trust contract are yours to write. The lockfile, the digest pins, and the signature are produced by freeze, never hand-written.

The Aileron extension lives under one namespaced `aileron` key in the frontmatter. The extension is lossless if stripped. A tool that ignores the Aileron fields still reads a valid skill, and a host that lacks a named action reports a missing requirement rather than refusing to load the document.

## The skeleton

A complete, valid minimum:

```yaml
---
name: weekly-metrics-digest
description: Read a metrics window, summarize it, and file a tracking issue.
aileron:
  schemaVersion: aileron.flightplan.v1
  requires:
    actions:
      - ref: aileron:metrics.read_window
        trustContract:
          credential:
            kind: oauth2
            placement: header
          oauth:
            scopes: ["metrics.read"]
          hosts: ["metrics.example.com:443"]
          effect: read
          idempotency:
            safeToRetry: true
          audit:
            fields: ["network-target", "operation-effect", "response-summary"]
    executionEnvironment:
      rung2CapabilityUnits:
        features:
          - "ghcr.io/aileron/features/jq:1"
  inputs:
    - name: window_days
      type: number
      description: How many days of metrics to read.
      resolution:
        rule: literal
        default: 7
  outputs:
    - name: digest
      mimeType: text/markdown
      encoding: utf-8
      publish:
        target: file
        path: digest.md
  steps:
    - id: read
      kind: action-call
      actionRef: aileron:metrics.read_window
      args:
        days: inputs.window_days
    - id: summarize
      kind: transform
      bindings:
        rows: steps.read.messages
      outputs: ["text"]
      materializesOutput: digest
---

# Weekly Metrics Digest

This skill reads a recent metrics window, writes a short digest, and files a
tracking issue that links to it.
```

Open the frontmatter block at the top; close it; write the body underneath. The `aileron` block is the only Aileron-specific part. The keys it sits beside (`name`, `description`, and the rest) are the agentskills.io skill keys.

## The `requires:` block

The `requires:` block lists the actions the plan calls and the execution environment it runs in. It is the dependency declaration freeze reads when it pins and binds.

### Actions and the per-action trust contract

Each `requires.actions[]` entry names one action by `ref` and carries a per-action `trustContract`. The `ref` is `aileron:<connector>.<action>`, and it attaches to the [action model](/concepts/actions/). An unsatisfied `requires:` entry is a missing-requirement signal the runtime surfaces, never a parse failure.

The trust contract is the security surface for that action. You write it; freeze attaches it; the detached signature is the human attestation that it is correct. Every field below records something the runtime enforces or the user sees.

- `credential` declares the credential `kind` (`none`, `api-key`, `oauth2`, `aws-sigv4`) and its `placement` (`header`, `query`, `cookie`, `body`, `signing`, `session`). It never carries a credential value. An `oauth2` credential is always placed in a `header`; an `aws-sigv4` credential is always placed via `signing`.
- `oauth` is required when `credential.kind` is `oauth2`. It declares the scopes, endpoints, and refresh behavior, never a token.
- `hosts` is the closed list of upstream hosts the action reaches, as `host:port` pairs. The declared host set IS the security boundary. The runtime grants nothing undeclared. `paths` optionally narrows the request paths on those hosts.
- `effect` is one of `read`, `write`, `delete`, `spend`, `external-send`. It drives default approval routing. A `read` runs unattended; the other four raise an approval gate at launch.
- `idempotency.safeToRetry` declares whether re-running with the same inputs produces no additional effect. `idempotency.idempotencyKey` declares whether the operation accepts a client-supplied key. Read ops are typically safe to retry; writes that mutate the world are not.
- `redaction` optionally lists rules applied to response fields before they reach the agent or the author. A `drop` rule removes a field, a `mask` rule replaces its value, and a `hash` rule replaces it with a stable hash.
- `verification` optionally declares a read-only probe the runtime can call to confirm the credential still works.
- `audit.fields` declares which of the closed audit-record fields this operation emits. The closed set is `connector-hash`, `action-manifest-version`, `credential-binding`, `identity-label`, `approved-input`, `approval-decision`, `network-target`, `operation-effect`, `request-summary`, `response-summary`, and `result`.

Declare the minimum each action needs. The contract is verbose by design. A plan with many actions records many such blocks, and that is the cost of a per-action audit story the user can read.

### The execution environment

`requires.executionEnvironment` declares the image the Flight Plan runs in, as one rung. The three rungs are mutually exclusive, so exactly one is declared.

- `rung1Image.ref` names a whole prebuilt operator-owned image. Freeze resolves a tag to an `image@sha256:` digest pin. `rung1Image.ref` is optional. When `rung1Image` declares no ref (write `rung1Image: {}`), freeze resolves the Aileron-provided runner image for the freezing CLI's version and records that concrete ref plus its digest pin in the lock.
- `rung2CapabilityUnits.features` declares the capability-unit devcontainer Features composed onto the Aileron agent-free base image. Freeze composes them and pins the built image by digest.
- `rung3PerStepImages.steps` declares one sibling image per step, each with an optional `id`, `mount`, and `collect`. Freeze resolves each step's image to an `image@sha256:` digest pin recorded in the lock.

The execution image is agent-free. The base image carries no coding agent. The Flight Plan runs composed steps, not an interactive agent session.

## Inputs

Every input the plan depends on is declared in `inputs[]`, each with a resolution rule. A value that varies by use case, such as a time window, is a declared input rather than a constant baked into the composition, so one composition serves many operators.

- `name` is the input name, unique within the manifest.
- `type` is one of `string`, `number`, `boolean`, `timestamp`, `object`, `array`.
- `description` is optional human-readable semantics.
- `resolution` is the resolution rule, discriminated by its `rule` field.

There are three resolution rules.

- `literal` takes a value passed at launch. An optional `default` applies when no value is passed.
- `dynamic` resolves a launch-relative value once at launch. Its `value` is `now` (the launch timestamp) or `today` (the launch date).
- `source` reads from a live source. Its `source.actionRef` names the action whose result resolves the input, with an optional `source.select`.

Inputs resolve once, at the launch boundary, into a concrete resolved-input set. Two steps that read the same dynamic input see one value. A `source` read happens in the resolution phase, before the step graph walks, so it is not a graph edge.

## Outputs

The `outputs[]` block is the declared contract for what the plan produces. It is kept distinct from the transport so the transport can change later without changing the contract.

- `name` is the artifact name, unique within the manifest. The audit references artifacts by this name.
- `mimeType` is the artifact media type.
- `encoding` is `utf-8` or `base64`. v1 implements `utf-8` only. `base64` is reserved.
- `publish.target` is `file` or `none`. When `target` is `file`, `publish.path` names the output file.

A step result reaches a declared output through the step's `materializesOutput` field, which names a declared `outputs[].name`.

## The deterministic step graph

The `steps[]` block is the composition. It wires declared actions and deterministic transforms into a directed acyclic graph. The block is optional. A skill with no `steps` block is instruction-only and still a valid manifest.

Every step carries an `id` unique within the graph and a `kind` from a closed enum. The `kind` is what makes the no-LLM guarantee structurally checkable.

| `kind` | Reaches an LLM | Semantics |
|---|---|---|
| `action-call` | No | Invokes a declared action. Its `actionRef` names the action and its `args` bind the action's arguments. |
| `transform` | No | Runs deterministic no-LLM logic over data already in the graph. It has no host, network, or credential surface. |
| `llm-seam` | Yes | The single marked non-deterministic seam. The only kind that reaches an LLM. |

Wire steps with bindings, never values. A binding is a reference. An `action-call` step binds its `args`; a `transform` and an `llm-seam` step bind their `bindings`. A binding takes one of two forms.

- `inputs.<name>` references a declared input resolved once at the launch boundary.
- `steps.<stepId>.<outputName>` references a named output of a prior step.

The references make the wiring a graph. The runtime executes it in deterministic topological order, breaking ties by declaration order.

Three rules a JSON Schema cannot express, that the freeze lint checks and the runtime enforces:

- The graph is acyclic.
- An `action-call` step's `actionRef` must match a declared `requires.actions[].ref`.
- An `llm-seam` is the only kind that may reach an LLM.

No step field may hold a secret. Every step kind is a closed object, and the `args` and `bindings` grammar is closed to `inputs.<name>` and `steps.<id>.<output>`, so a literal secret cannot be embedded in the wiring.

### Wiring the no-LLM seam

Behavioral determinism is the whole point. No LLM runs at Flight Plan runtime by default. If your plan needs LLM reasoning at one point, mark exactly one step `kind: llm-seam` and route all of it through that step. Everything else is `action-call` and `transform`, both of which hold no reference to any language-model type.

In v1 the seam is unwired by default. An `llm-seam` step with no configured provider is a hard error at launch, so a default launch reaches no model at all. A skill that reaches an LLM outside the marked seam fails the freeze lint and never becomes a Flight Plan. Keep the seam single. A plan that needs reasoning in more than one place routes all of it through the one marked seam or restructures to fit.

## The Markdown body

Everything after the closing frontmatter is the body. It is the agentskills.io skill body. Write what the skill does, the inputs it expects, and the output it produces. The body is the prose a reader and a skill-aware host consume, and it stays a valid skill even with the Aileron block stripped.

## Handoff to freeze and sign

When the skill validates and behaves the way you want, hand it to freeze. Freeze is the boundary between a skill and a Flight Plan.

```sh
aileron skill freeze weekly-metrics-digest \
  --signing-key ~/.aileron/keys/author.pem \
  --version 1.0.0
```

Freeze parses and lints the manifest, rejecting any step that could reach an LLM outside the marked `llm-seam`. It resolves the execution environment to image digests. It builds the lockfile. It content-addresses the unit and signs it with a detached ed25519 signature. It writes the frozen version into the store as an immutable directory.

The signing key is a PEM-encoded ed25519 private key, read for signing only and never copied into the artifact. The detached signature is your attestation that the per-action trust contract is correct. See [Freezing a Flight Plan](/guides/freezing-a-flight-plan/) for the full command and the reproducibility and signature-verification guarantees.

After freeze, [launch](/guides/launching-a-flight-plan/) runs the frozen Flight Plan deterministically.

## Common authoring mistakes

- **A literal secret in the wiring.** No step field holds a value. `args` and `bindings` are references (`inputs.<name>` or `steps.<id>.<output>`), and the credential lives in the trust contract as a `kind`/`placement` declaration, never a token.
- **An unmarked LLM call.** Any reach to a model outside a `kind: llm-seam` step fails the freeze lint. Mark the one seam, or restructure the plan to keep the logic deterministic.
- **More than one seam.** v1 allows exactly one marked seam. Route all reasoning through it.
- **An `actionRef` with no matching `requires.actions[].ref`.** A step can only call an action the `requires:` block declares. The freeze lint catches the mismatch.
- **Hand-writing the lock.** The `lock` section is produced by freeze. It is absent before freeze, and you never author it.
- **A `base64` output in v1.** The `encoding` field reserves `base64`, but the v1 runtime materializes `utf-8` text only. A binary artifact waits on the deferred rung-three run-and-collect launch boundary.

## Where to go next

- [Flight Plans](/concepts/flight-plans/) — the model behind everything in this guide.
- [Flight Plan Manifest Spec](/development/flight-plan-manifest-spec/) — the normative field reference, with the full schema and a worked example.
- [Freezing a Flight Plan](/guides/freezing-a-flight-plan/) — seal the skill into a signed Flight Plan version.
- [Launching a Flight Plan](/guides/launching-a-flight-plan/) — run the frozen Flight Plan deterministically.
- [ADR-0027: Flight Plan = Sealed Installable Skill](/adr/0027-flight-plan-sealed-installable-skill/) — the design constraints behind this layer.
