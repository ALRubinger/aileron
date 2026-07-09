---
title: "Flight Plans"
description: "A Flight Plan is a sealed Aileron skill: an authored SKILL.md that a freeze step pins to digests, locks, binds to an execution environment, attaches a per-action trust contract to, and signs into a reproducible, behaviorally deterministic unit."
order: 7
---

A **Flight Plan** is a sealed Aileron skill. A skill is the authoring artifact. A Flight Plan is the same artifact after a freeze step seals it. One format spans both states, and the freeze step is the boundary between them.

If [actions](/concepts/actions/) are the atomic unit of agent capability, a Flight Plan is the named composition above them. It calls actions, runs inside the [sandbox](/concepts/deterministic-agentic-execution/), and surfaces its per-action effects through the out-of-band approval channel. A Flight Plan composes those primitives. It does not redefine them.

## A Flight Plan is a sealed skill

"Flight Plan" is the user-facing product noun. "Skill" is the [agentskills.io](https://agentskills.io) `SKILL.md` format, extended with one Aileron-specific block. Every Flight Plan is a skill. Not every skill is a Flight Plan.

A skill becomes a Flight Plan only after it is frozen. Before freeze, the document is an authoring artifact with no determinism guarantee. After freeze, the document is a sealed unit with every guarantee below.

The extension is lossless if stripped. A tool that ignores the Aileron fields still reads a valid skill. That is why the same `SKILL.md` spans authoring and sealing.

## The freeze boundary

Freeze is the step that turns a skill into a Flight Plan. It runs a fixed sequence over one `SKILL.md` document.

- It resolves every image reference to a content-addressed digest.
- It produces a lockfile that pins those digests and the resolved capability set.
- It binds the execution environment the Flight Plan runs in.
- It attaches the per-action trust contract.
- It signs the result with a detached signature.

Before freeze, none of these are present. After freeze, all of them are present and immutable for that version. The signature is a human attestation that the trust contract is correct, and it is the act that makes the unit trusted. The [Freezing a Flight Plan](/guides/freezing-a-flight-plan/) guide walks through running freeze and the reproducibility it guarantees.

## Two determinisms

A Flight Plan carries two distinct determinism guarantees.

The first is **environmental reproducibility**. Every image reference is pinned to a digest at freeze. The same Flight Plan resolves the same images on every run. The lockfile is the record of those pins.

The second is **behavioral determinism**. No LLM runs at Flight Plan runtime by default. An LLM runs only at explicitly marked seams that are structurally enforced by the runtime. A plan may declare one or more marked seams. A freeze-time lint rejects any unmarked LLM call. A skill that reaches an LLM outside a marked seam fails freeze and never becomes a Flight Plan. The marked seams are the only places where non-deterministic reasoning is allowed, and everything outside them is deterministic by construction.

## Inputs, resolution, and the audit boundary

Behavioral determinism is a property of the function, not of the output. A Flight Plan is a pinned function over its declared inputs. Given the same resolved inputs, it produces the same output.

The point of running a Flight Plan again is that its inputs, or the data in the systems it reads, have moved. Results therefore vary across runs, and that variance is expected rather than a determinism violation. The line is between the logic, which is sealed and fixed, and the inputs, which are declared, resolved, and recorded.

Inputs are declared, each with a resolution rule. A resolution rule is a literal value passed at launch, a dynamic value such as the launch time or launch date, or a read from a live source. A value that varies by use case, such as a time window, is a declared input rather than a constant baked into the composition, so one composition serves many operators.

Inputs resolve once, at the launch boundary. The runtime resolves all declared inputs one time into a concrete resolved-input set, and the composition runs against that set. Two steps that read the same moving value such as the wall clock see one resolved value. The resolved-input set is the single recorded binding for the run.

The audit records resolved inputs and outputs. A scalar input is recorded by value, so a launch-time clock input is recorded as the concrete timestamp it resolved to. A data read is recorded by its resolved binding, which is the parameters, the query, and a result or snapshot identifier with a summary, rather than the full dataset inline. The recorded binding is what makes a past run explainable without reconstructing it from a moving source.

## The per-action trust contract

Each action a Flight Plan calls carries a per-action trust contract. The contract is the field set freeze attaches, and the detached signature is the human attestation that it is correct.

The contract records the credential kind and its placement, never a credential value. It records the OAuth scopes, endpoints, and refresh behavior for an OAuth credential. It records the expected network hosts and paths the action reaches. It records the operation effect as one of `read`, `write`, `delete`, `spend`, or `external-send`. It records whether the operation is idempotent. It records the redaction rules for the operation's inputs and outputs. It records a verification probe the runtime can call to confirm the operation's result. It records the structure of the audit record the operation emits.

The operation effect feeds the approval surface. A `read` runs unattended. A `write`, `delete`, `spend`, or `external-send` raises an approval gate and blocks on the decision. The user sees the recorded effect when an approval is requested.

## The execution environment

A Flight Plan runs inside one container. The `environment` block declares it, and freeze composes and pins exactly one image for the whole plan.

The block declares `tools`, an `image`, or both. `tools` names curated-catalog tools, each as `<name>@<version>`, that freeze resolves to devcontainer Features and composes onto the Aileron-provided runner base. `image` names a custom base image, the escape hatch for tooling the catalog does not carry, and declared tools compose onto it when both are present. Freeze resolves the declared environment to a single content-addressed digest, and launch boots that one image and runs the whole plan inside it. A tool step runs a declared tool as a deterministic subprocess in that container, and its declared network reach is sealed at freeze and enforced at launch. The execution container is agent-free. The image carries no coding agent. The Flight Plan runs composed steps, not an interactive agent session.

## Parameterized reach

A sealed reach can be a template rather than a fixed literal. A sealed tool command or a sealed host may embed a token that names a declared input, and that input carries a constraint that bounds what the token can resolve to. The signer attests the template together with its constraint, so the signature covers the shape the reach can take rather than one frozen endpoint.

This lets one frozen and published plan retarget across the endpoints the signer admits without re-freezing. A region-parameterized plan points at a new region the constraint allows, and launch instantiates the concrete endpoint from the resolved input. The proxy still matches the instantiated endpoint by exact string, so the plan reaches only an endpoint the signer's constraint admits. The field mechanics live in the [Flight Plan Manifest Spec](/development/flight-plan-manifest-spec/) and the [Authoring a Flight Plan](/guides/authoring-a-flight-plan/) guide.

## Multi-identity

The credential binding lives on the operator's machine, not in the plan. The identical sealed artifact launches under different identities with different authorizations, and the audit trail records who. The image and the booted container never contain a secret in any form. The daemon forward proxy injects or re-signs with the vault credential at the egress boundary, so one frozen version serves many operators without ever carrying one operator's identity.

## The lifecycle

A Flight Plan moves through three phases.

1. **Author.** You hand-write a `SKILL.md` with the `requires:` block, the deterministic step graph, the `outputs:` contract, and a per-action trust contract for every action it calls. The [Authoring a Flight Plan](/guides/authoring-a-flight-plan/) guide walks through this.
2. **Freeze.** `aileron skill freeze` seals the skill into a signed, digest-pinned, immutable Flight Plan version. The [Freezing a Flight Plan](/guides/freezing-a-flight-plan/) guide covers the command.
3. **Launch.** `aileron skill launch` verifies the frozen unit, resolves inputs once at the boundary, walks the step graph with no model in the loop, enforces the trust contract, and materializes the declared outputs. The [Launching a Flight Plan](/guides/launching-a-flight-plan/) guide covers the runtime.

## Where to go next

- [Authoring a Flight Plan](/guides/authoring-a-flight-plan/). Write the `SKILL.md` by hand, then hand it off to freeze.
- [Freezing a Flight Plan](/guides/freezing-a-flight-plan/). Seal an installed skill into a signed Flight Plan version.
- [Launching a Flight Plan](/guides/launching-a-flight-plan/). Run a frozen Flight Plan deterministically.
- [ADR-0027: Flight Plan = Sealed Installable Skill](/adr/0027-flight-plan-sealed-installable-skill/). The design decision behind everything on this page.
- [Flight Plan Manifest Spec](/development/flight-plan-manifest-spec/). The normative `SKILL.md` frontmatter format, field by field.
