---
title: "Launching a Flight Plan"
description: "How aileron skill launch runs a frozen Flight Plan deterministically: a no-LLM step graph with trust-contract enforcement, single-boundary input resolution, and materialized outputs."
---

Launch is the runtime step that runs a frozen Flight Plan ([ADR-0027](/adr/0027-flight-plan-sealed-installable-skill)). Freeze seals a skill into a signed, digest-pinned version. Launch loads that version, verifies it, and runs its declared step graph deterministically. The runtime walks the steps as plain code. No language model sits in the execution loop by default. This guide explains the command and the guarantees it produces.

The Flight Plan Launch runtime is a `skill` subcommand. It is distinct from the top-level `aileron launch` command, which starts an interactive agent sandbox. The two are different subsystems. This guide covers `aileron skill launch` only.

## Running a launch

Launch a frozen skill by name.

```sh
aileron skill launch weekly-metrics-digest --out-dir ./run
```

The runtime loads the most recent frozen version of that skill from the canonical store. Pass `--version` to launch a specific version id. Pass `--input name=value` once per literal input you want to override. File-target outputs are written under `--out-dir`.

When you run an interactive launch from a terminal and a required literal input has no `--input` override and no declared default, the launch prompts you for its value instead of failing. The prompt shows the input name, its declared description, and any enum or pattern constraint so you know the accepted shape. The value you type is validated by the same constraint check as an `--input` override, so an out-of-constraint answer still fails the launch closed. A non-interactive launch, one with piped stdin or a launch run in CI, never prompts. There a missing required literal fails fast with the same error as before, so a scripted launch never blocks waiting on input.

The launch prints a result summary: the version, the content hash, the written artifacts, and the audit-record count. The resolved inputs are listed by name with a compact `<type, size>` summary rather than their full values, so a plan that passes a large input (such as an inlined document) does not bury the result under a wall of text. Pass `--verbose` (or `-v`) to print the full resolved-input values.

```sh
aileron skill launch weekly-metrics-digest \
  --version 5f2a9c1e4b7d8a30 \
  --input window_days=30 \
  --out-dir ./run
```

## The verification gate

Launch refuses to run a frozen unit it cannot verify. Before any step runs, the runtime reconstructs the exact canonical bytes freeze signed, recomputes the content hash, and compares it to the recorded hash. It then verifies the detached signature against the stored public key. A flipped manifest byte fails the hash check. A flipped signature byte fails the signature check. A recorded hash that disagrees with the bytes fails the comparison. Any failure aborts the launch before a single step dispatches.

## The no-LLM guarantee

A Flight Plan is a pinned function over its declared inputs. It is deterministic given those resolved inputs. The runtime guarantees this structurally. Every step declares a `kind` from a closed set: `action-call`, `transform`, `tool`, or `llm-seam`. The executor has exactly four branches, one per kind. The `action-call`, `transform`, and `tool` branches hold no reference to any language-model type. No deterministic step can reach a model by construction.

A `tool` step runs a declared environment tool as a deterministic subprocess inside the one container the plan booted. It executes an argv `command` with no shell interpretation. Its sealed network reach is enforced at launch: before the step runs, the daemon mints a step-scoped, TTL-bounded proxy credential restricted to exactly the sealed hosts. A scoped call to an undeclared host is refused with a 403 at the daemon proxy before the TLS handshake, and no credential bytes ever enter the container. A contracted tool step with no verified sealed reach, and a tool-step plan launched with no pinned environment, both fail closed.

The single marked seam is the `llm-seam` step. It is the only kind that may reach a model. In v1 the seam is unwired by default. An `llm-seam` step with no configured provider is a hard error. A default launch therefore reaches no model at all, so the no-LLM property holds by default and not only by construction.

## Input resolution at the launch boundary

Every declared input resolves once, at the launch boundary, into a concrete value. The resolution rule decides how. A `literal` input takes its launch override, then its declared default. A required literal with neither is prompted for interactively when the launch runs from a terminal, and is an error otherwise. A `dynamic` input reads the launch clock once. `now` resolves to the launch timestamp and `today` resolves to the launch date. The runtime reads the clock a single time and reuses that value for every dynamic input, so two steps that read the same dynamic input always see one value. A `source` input is read live from a declared action through the sealed boundary. The runtime records the read by its resolved binding, the action ref and the selector, never the dataset inline.

A `source` input that reads from the same action a step calls is not a step and not a graph edge. The acyclicity check covers `steps.*` edges only. The source read happens in the resolution phase, before the step graph walks.

## Trust-contract enforcement

Each `action-call` dispatches through the sealed action boundary. The runtime enforces the per-action trust contract on every call. The operation effect routes approval. A `read` runs unattended. A `write`, `delete`, `spend`, or `external-send` raises an approval gate and blocks on the decision. A denied action aborts its step and the launch records the denial. Idempotency governs retries. An action declared not safe to retry is never silently re-issued. An action that declares an idempotency key threads a stable key so the upstream deduplicates a retried write. Redaction runs on the dispatch result before it enters the graph or any audit summary. A `drop` rule removes a field, a `mask` rule replaces its value, and a `hash` rule replaces it with a stable hash. Credentials never appear in step arguments. A binding is a reference, never a value, and the closed binding grammar enforces this at decode time.

## Output materialization

A step that declares `materializesOutput` produces a typed file-map. The runtime materializes the entry into a file at the declared output path. v1 materializes `utf-8` text outputs only. A `base64` or binary output is refused with a clear error that points at the deferred mount and run-and-collect boundary ([#1510](https://github.com/ALRubinger/aileron/issues/1510)). The materialized file's mime type must match the declared output's mime type. Materialization is pure deterministic code. No model interprets the file-map.

## The audit boundary

Launch emits a customer-owned audit record per action and one per launch. Each record carries exactly the audit fields the action declares. Scalars are recorded by value. Data reads are recorded by their resolved binding, a result or snapshot summary, never the full dataset inline. The audit sink is the customer-owned store the runtime is wired to.

## Multi-identity

The credential binding lives on the launching operator's machine, not in the frozen plan. Two operators launch the identical sealed version, each under a different vault-bound identity with different authorizations, and each launch's audit trail records who ran it. The image and the booted container never receive the vault credential bytes, because the daemon forward proxy injects or re-signs with the vault credential at the egress boundary. One artifact, two operators, two identities, and the audit records who.

## Determinism

Given the same resolved inputs, a launch produces the same output. The topological walk order is deterministic. Ties break by declaration order. The transform vocabulary is pure and closed. The only non-deterministic seam is the marked `llm-seam`, which is unwired by default. This is the behavioral guarantee that makes a Flight Plan a function you can pin, sign, and re-run.
