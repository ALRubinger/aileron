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

The second is **behavioral determinism**. No LLM runs at Flight Plan runtime by default. An LLM runs only at a single seam that is explicitly marked in the skill and structurally enforced by the runtime. A freeze-time lint rejects any unmarked LLM call. A skill that reaches an LLM outside the marked seam fails freeze and never becomes a Flight Plan. The marked seam is the one place where non-deterministic reasoning is allowed, and everything outside it is deterministic by construction.

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

A Flight Plan runs on a composed execution image, and that image is assembled from rungs.

Rung one pins a whole prebuilt image. The skill names an image, and freeze resolves it to a digest. The operator owns that image.

Rung two declares capability units, and Aileron composes them. The skill declares the units it requires on top of a generic Aileron-provided agent-free minimal base image. Freeze composes the operator-owned capability-unit devcontainer Features onto that base and pins the result.

Rung three is a per-step sealed sibling-image dispatch. Freeze resolves each step's sibling image to a content-addressed digest pinned in the lock. The execution image is agent-free. The base image carries no coding agent. The Flight Plan runs composed steps, not an interactive agent session.

## The lifecycle

A Flight Plan moves through three phases.

1. **Author.** You hand-write a `SKILL.md` with the `requires:` block, the deterministic step graph, the `outputs:` contract, and a per-action trust contract for every action it calls. The [Authoring a Flight Plan](/guides/authoring-a-flight-plan/) guide walks through this.
2. **Freeze.** `aileron skill freeze` seals the skill into a signed, digest-pinned, immutable Flight Plan version. The [Freezing a Flight Plan](/guides/freezing-a-flight-plan/) guide covers the command.
3. **Launch.** `aileron skill launch` verifies the frozen unit, resolves inputs once at the boundary, walks the step graph with no model in the loop, enforces the trust contract, and materializes the declared outputs. The [Launching a Flight Plan](/guides/launching-a-flight-plan/) guide covers the runtime.

## Where to go next

- [Authoring a Flight Plan](/guides/authoring-a-flight-plan/) — write the `SKILL.md` by hand, then hand it off to freeze.
- [Freezing a Flight Plan](/guides/freezing-a-flight-plan/) — seal an installed skill into a signed Flight Plan version.
- [Launching a Flight Plan](/guides/launching-a-flight-plan/) — run a frozen Flight Plan deterministically.
- [ADR-0027: Flight Plan = Sealed Installable Skill](/adr/0027-flight-plan-sealed-installable-skill/) — the design decision behind everything on this page.
- [Flight Plan Manifest Spec](/development/flight-plan-manifest-spec/) — the normative `SKILL.md` frontmatter format, field by field.
