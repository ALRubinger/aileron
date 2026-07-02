---
title: "ADR-0027: Flight Plan = Sealed Installable Skill"
description: "A Flight Plan and an Aileron skill are the same construct seen at two points in one lifecycle. A skill is authored in the agentskills.io SKILL.md format extended with a requires block, and it becomes a Flight Plan at a freeze step that resolves image references to digests, produces a lockfile, binds the execution environment, attaches a per-action trust contract, and signs the result. The execution image is agent-free, and an LLM runs only at a single structurally-enforced seam."
order: 27
---

<div class="meta">
<table>
  <tr><th>Status</th><td>Accepted</td></tr>
  <tr><th>Date</th><td>2026-06-23</td></tr>
  <tr><th>Tracking</th><td><a href="https://github.com/ALRubinger/aileron/issues/1506">#1506</a>, <a href="https://github.com/ALRubinger/aileron/issues/1514">#1514</a></td></tr>
</table>
</div>

## Context

Aileron carried two framings for the same idea. One framing called the unit a Flight Plan, a named composition that runs above actions as a repeatable, audited unit. The other framing called it a skill, the agentskills.io `SKILL.md` document extended with an Aileron-specific dependency declaration. These two framings described one construct viewed at two points in its lifecycle. This ADR reconciles them under one vocabulary and one format.

A Flight Plan composes primitives that other ADRs already define. It does not redefine them. It calls actions as defined in [ADR-0003](/adr/0003-action-model), where each action is an atomic unit with an explicit version, hash, and capability subset. It runs inside the sandbox of [ADR-0005](/adr/0005-sandbox-choice), where the runtime mediates credential use so the composed code never holds a raw credential. Its per-action effects surface to the user through the out-of-band approval channel of [ADR-0009](/adr/0009-user-channel), where the agent is structurally never in the approval trust path. Its credentialed calls flow through the data-plane injection of [ADR-0019](/adr/0019-v4-https-data-plane), where credentials are injected at the proxy boundary. This ADR composes those four primitives. It does not supersede any of them.

Several earlier specs explored pieces of this layer and are absorbed here. The Flight Plan specs (#925, #927, #928) explored the composition-above-actions construct, the reproducibility guarantee, and the trust-contract surface. The deterministic-unit work (#720) explored the same composition seen as a sealed, format-level skill. Their resolved decisions are quoted as context in this ADR. This ADR does not depend on those open issues at runtime, and it adds no dependency edge to them. The strategy roadmap in #929 remains the roadmap. This ADR does not supersede it.

The gap this decision closes is the absence of a single recorded boundary between an instruction-only skill and a sealed, signed, reproducible Flight Plan. Without that boundary, the determinism guarantee had no defined point at which it attaches, and the trust contract had no defined point at which it is frozen.

## Decision

A Flight Plan is a sealed Aileron skill. The skill is the authoring artifact. The Flight Plan is the same artifact after a freeze step seals it. One format spans both states, and a freeze step is the boundary between them.

### Vocabulary: a Flight Plan is a sealed skill

A Flight Plan and a skill are the same construct. "Flight Plan" is the user-facing product noun. "Skill" is the `SKILL.md` format from agentskills.io, extended with a `requires:` block. Every Flight Plan is a skill. Not every skill is a Flight Plan. A skill becomes a Flight Plan only after it is frozen. Before freeze, the document is an authoring artifact with no determinism guarantee. After freeze, the document is a sealed unit with the guarantees recorded below.

### The freeze boundary

Freeze is the step that turns a skill into a Flight Plan. Freeze resolves every image reference to a content-addressed digest. Freeze produces a lockfile that pins those digests and the resolved capability set. Freeze binds the execution environment the Flight Plan runs in. Freeze attaches the per-action trust contract described below. Freeze signs the result, which is a human attestation that the trust contract is correct. The signature is the human trust-contract attestation, and it is the act that makes the unit trusted. Before freeze, none of these are present. After freeze, all of them are present and immutable for that version.

### Two determinisms

A Flight Plan carries two distinct determinism guarantees.

The first is environmental reproducibility. Every image reference is pinned to a digest at freeze. The same Flight Plan resolves the same images on every run. The lockfile is the record of those pins. Launch boots the pinned image from the verified lock, so the environment the plan runs in is the one the signature attested.

The second is behavioral determinism. No LLM runs at Flight Plan runtime by default. An LLM runs only at a single seam that is explicitly marked in the skill and structurally enforced by the runtime. A freeze-time lint rejects any unmarked LLM call. A skill that reaches an LLM outside the marked seam fails freeze and never becomes a Flight Plan. The marked seam is the one place where non-deterministic reasoning is allowed, and everything outside it is deterministic by construction.

### Inputs, resolution, and the audit boundary

Behavioral determinism is a property of the function, not of the output. A Flight Plan is a pinned, agent-invariant function over its declared inputs. Given the same resolved inputs, a Flight Plan produces the same output, and that is the property a freeze verification checks: pin the inputs as fixtures and the output is identical. A Flight Plan that depended on no inputs and always produced the same output would be a constant, and a constant is cached rather than run. The point of running a Flight Plan again is that its inputs, or the data in the systems it reads, have moved. Results therefore vary across runs, and that variance is expected rather than a determinism violation.

Inputs are declared. The manifest declares every input the Flight Plan depends on, and each input carries a resolution rule. A resolution rule is a literal value passed at launch, a dynamic value such as the launch time or launch date, or a read from a live source. A value that varies by use case, such as a time window, is a declared input rather than a constant baked into the composition, so one composition serves many operators rather than overfitting to one.

Inputs resolve once, at a boundary. At launch the runtime resolves all declared inputs one time into a concrete resolved-input set, and the composition runs against that set. Resolving once gives internal consistency, because two steps that read the same moving value such as the wall clock see one resolved value rather than two readings taken moments apart. The resolved-input set is the single recorded binding for the run.

The audit records resolved inputs and outputs. Each launch records the resolved value of every input and the outputs the run produced. A scalar input is recorded by value, so a launch-time clock input is recorded as the concrete timestamp it resolved to. A data read is recorded by its resolved binding, which is the parameters, the query, and a result or snapshot identifier with a summary, rather than the full dataset inline. The dataset is the run's recorded output, and the audit references it rather than duplicating it. The recorded binding is what makes a past run explainable without reconstructing it from a moving source.

Outputs are a declared contract, separate from their transport. The manifest declares an `outputs:` block that names each artifact, its media type, and its encoding, and the runtime materializes those artifacts deterministically through a typed file-map transport. Keeping the contract distinct from the transport lets the transport change later without changing what the plan promises. The `encoding` field admits `utf-8` and `base64`, and the v1 runtime implements `utf-8` only. Text is the v1 implementation, never the declared interface, so the contract reserves the binary shape without committing to a binary mechanism now. The shape and the field layout live in the [Flight Plan manifest specification](/development/flight-plan-manifest-spec).

The no-LLM-at-runtime rule seals the agent and the LLM out of the function, not the data out of the inputs. An LLM in the runtime loop is forbidden because it injects non-determinism into the function itself. A live or time-relative data read is an input, and an input is allowed. The line is between the logic, which is sealed and fixed, and the inputs, which are declared, resolved, and recorded.

### Execution rungs

A Flight Plan runs on a composed execution image, and that image is assembled from rungs.

Rung one pins a whole prebuilt image. The skill names an image, and freeze resolves it to a digest. The operator owns that image.

Rung two declares capability units, and Aileron composes them. The skill declares the units it requires on top of a generic Aileron-provided agent-free minimal base image. Freeze composes the operator-owned capability-unit devcontainer Features onto that base and pins the result. The capability-unit shape is the one defined in [ADR-0026](/adr/0026-cli-capability-units).

Rung three is a per-step sealed sibling-image dispatch with mount and run-and-collect I/O. Each step names a sibling image, and freeze resolves each named image to a content-addressed digest. Freeze records one digest pin per step in the lock alongside the rung-one and rung-two pins. Each rung-three pin also carries the id of the step that dispatches to it, so a step associates to its pin by id rather than by the mutable image reference. Two steps that share an image tag therefore pin distinctly by id, with no association ambiguity. The per-step image is the operator-owned tool the step dispatches to. A manifest may declare the `rung3PerStepImages` rung alone, and freeze pins one digest per step so a rung-three Flight Plan seals to a signed image assertion like every other rung. Each rung-three step may also declare a per-step trust contract, reusing the per-action trust-contract shape, whose `hosts` declare that step's network reach. Freeze records the declared reach on the step's lock pin alongside the digest, so the content hash and signature cover it. A step's declared reach is therefore sealed at freeze and cannot be re-supplied at launch. This issue declares and seals the reach; consuming it by stamping it onto the launch host allow-list is a separate follow-up.

The digest pin is load-bearing at launch, not only at freeze. When the verified lock carries a resolved rung-one or rung-two image digest, launch boots that exact pinned image and runs the whole plan inside it. The digest booted comes from the verified lock, so the image entered corresponds to the lock's signed image assertion rather than any re-resolved tag. A Flight Plan that declares no execution rung has an empty resolved-image set, so its launch runs the step graph in-process instead of booting an image. On the container boot path in v1, approvals and audit are re-established in-container and the resulting AuditIDs are not threaded back to the host `RunResult`, so full `RunResult` and AuditID threading across the container boundary lands in the later sub-issue rather than being complete now.

Rung three is distinct: its dispatch is per step, not whole-plan. The plan orchestration stays on the host and walks the step graph in-process, and only an individual rung-three step shells out to its pinned sibling tool image. That step mounts its resolved input into the image, runs the tool, and reads back the collected output as the step's named output, which then flows into downstream steps through the ordinary dataflow. A step that dispatches a sibling image enters it by its signed digest rather than a re-resolved tag. A rung-three step whose image is not pinned by digest in the verified lock is refused at launch, so no tag-shaped or floating reference is ever dispatched.

The execution image is agent-free. The base image carries no coding agent. The Flight Plan runs composed steps, not an interactive agent session.

### Manifest and signing

The manifest is the `SKILL.md` frontmatter, extended in one format. The extension is lossless if stripped, so a tool that ignores the Aileron fields still reads a valid skill.

The frontmatter gains a `requires:` block. The `requires:` block lists the actions the Flight Plan calls and the execution environments it runs in. Each listed action attaches to the action model of [ADR-0003](/adr/0003-action-model).

Each listed action carries a per-action trust contract block. The trust contract block records the credential kind and its placement. It records the OAuth scopes, endpoints, and refresh behavior for an OAuth credential. It records the expected network hosts and paths the action reaches. It records the operation effect as one of read, write, delete, spend, or external-send. It records whether the operation is idempotent. It records the redaction rules for the operation's inputs and outputs. It records a verification probe the runtime can call to confirm the operation's result. It records the structure of the audit record the operation emits. The operation effect feeds the approval surface of [ADR-0009](/adr/0009-user-channel), so the user sees the recorded effect when an approval is requested.

Freeze adds a lock and digest section. That section pins the resolved image digests and the resolved capability set. That section is produced by freeze and is absent before freeze.

The signature is detached. The signature covers the content-addressed manifest plus the lockfile. The signature is the human attestation that the trust contract is correct.

The version is the content hash plus a semver label. The content hash identifies the exact frozen bytes. The semver label is the human-facing version name.

### Layer split at freeze

The architectural split sits at the freeze step. There are two sub-layers.

The format and install sub-layer holds skills before freeze. This sub-layer covers instruction-only skills and credentialed skills. This sub-layer carries no determinism guarantee. A skill in this sub-layer can be installed and run, and it has no reproducibility or behavioral-determinism promise.

The Flight-Plan-core sub-layer holds skills after freeze. This sub-layer carries the freeze step, the execution-environment binding, the no-LLM-at-runtime rule, and the signing. A unit in this sub-layer is a Flight Plan with all the guarantees recorded above. The distribution surfaces over a frozen Flight Plan are specified in the [Launch-a-Flight Surfaces Spec](/development/launch-surfaces-spec).

### Layer boundary

The diagram below shows the boundary. A skill crosses the freeze step and becomes a Flight Plan. Both states sit above the actions and connectors they compose.

```
  format / install sub-layer        Flight-Plan-core sub-layer
  ---------------------------       ----------------------------
        SKILL.md                          Flight Plan
   (instruction-only or          (digests pinned, exec-env bound,
    credentialed skill)    freeze  no LLM at runtime, signed,
   no determinism guarantee  -->   per-action trust contract)
        |                                     |
        |                                     |
        +------------------+------------------+
                           |
                   composes primitives
                           |
        +------------------+------------------+
        |                  |                  |
     actions          sandbox +          approval
   (ADR-0003)     credential mediation    channel
                    (ADR-0005)          (ADR-0009)
                           |
                   data-plane injection
                      (ADR-0019)
```

## Consequences

### Positive

- A Flight Plan is reproducible. Every image reference is pinned to a digest at freeze, so the same plan resolves the same environment on every run. Launch boots that pinned image from the verified lock, so the environment the plan runs in matches the signed pin.
- A Flight Plan is auditable. The per-action trust contract records the credential, the network reach, the effect, and the audit-record structure for every action the plan calls.
- A Flight Plan is behaviorally deterministic. No LLM runs at runtime outside the single marked seam, and the freeze-time lint rejects any unmarked LLM call before the plan is sealed.
- A Flight Plan is deterministic given its resolved inputs. Inputs are declared with resolution rules, resolved once at launch, and recorded with the outputs. Results vary only with declared, resolved inputs, so every run is explainable from its recorded binding rather than reconstructed from a moving source.
- One format spans authoring and sealing. A skill and a Flight Plan share the `SKILL.md` format, and the extension is lossless if stripped.
- The trust contract is human-attested. The detached signature over the manifest plus lockfile records a human's confirmation that the contract is correct.
- The layer composes existing primitives. It reuses the action model, the sandbox, the approval channel, and the data plane rather than redefining them.

### Negative

- Freeze is rigid. A frozen Flight Plan pins one resolved environment, and changing the environment requires a new freeze and a new version.
- The single-seam constraint is strict. A Flight Plan that needs LLM reasoning in more than one place must route all of it through the one marked seam or restructure to fit.
- The trust contract is verbose. Every action carries a full contract block, and authoring a Flight Plan with many actions records many such blocks.

## Deferred

The following are out of scope for this ADR and this layer's MVP.

- Rung-three dispatch hardening. Freeze pins the rung-three per-step sibling images by digest and the launch-time runtime now mounts each image, runs it, and collects its output as the step's dataflow. A rung-three step's network reach is now declared in its per-step trust contract and sealed onto its lock pin, so the reach is covered by the signature and not re-suppliable at launch. The launch-time runtime now also routes a rung-three tool container's HTTPS egress through the [ADR-0019](/adr/0019-v4-https-data-plane) daemon forward proxy via env-based CA trust ([#1769](https://github.com/ALRubinger/aileron/issues/1769)), so a matched host binding injects the operator's vault-bound credential at the boundary with no credential bytes in the tool image, environment, mounts, or arguments. That wiring attaches the credential on the way out only. It does not enforce the declared reach at the tool-container network boundary: per-step egress enforcement is descoped and audit-only ([#1783](https://github.com/ALRubinger/aileron/issues/1783)), surfacing the declared reach in the audit trail is a separate follow-up ([#1784](https://github.com/ALRubinger/aileron/issues/1784)), the fail-closed behavior when a step declares no reach is a separate follow-up ([#1758](https://github.com/ALRubinger/aileron/issues/1758)), and the per-step egress firewall that would enforce the reach at the network boundary is its own follow-up ([#1736](https://github.com/ALRubinger/aileron/issues/1736)).
- Binary outputs. The `outputs:` contract reserves the `base64` encoding, but the v1 runtime materializes `utf-8` artifacts only. Binary output is blocked on a host-ABI binary-body field, because the current JSON-string result body coerces arbitrary bytes to valid UTF-8 and corrupts them. The mount and run-and-collect boundary of rung three (#1510) is the escape hatch for large or binary artifacts when a consumer arrives.
- STS and SSO. Short-lived token services and single sign-on integration are not specified here.
- Specific connectors. No named connector is specified by this ADR. The trust-contract format applies to any action, and individual connector contracts are authored against this format elsewhere.

## References

- [Issue #1506](https://github.com/ALRubinger/aileron/issues/1506). The Flight Plan layer umbrella and implementation home
- [Issue #1514](https://github.com/ALRubinger/aileron/issues/1514). This ADR's tracking sub-issue
- [Issue #1519](https://github.com/ALRubinger/aileron/issues/1519). The output-contract reservation that text is the v1 implementation and binary is a deferred follow-up
- [The Flight Plan manifest specification](/development/flight-plan-manifest-spec). The `outputs:` contract shape and the file-map transport
- [The Launch-a-Flight Surfaces Spec](/development/launch-surfaces-spec). The distribution surfaces over a frozen Flight Plan
- [ADR-0003](/adr/0003-action-model). The action model the per-action trust contract attaches to
- [ADR-0005](/adr/0005-sandbox-choice). The sandbox and credential mediation a Flight Plan runs inside
- [ADR-0009](/adr/0009-user-channel). The out-of-band approval channel the per-action effect feeds
- [ADR-0019](/adr/0019-v4-https-data-plane). The data-plane injection a Flight Plan's credentialed calls flow through
- [ADR-0026](/adr/0026-cli-capability-units). The capability-unit shape composed at rung two
- The Flight Plan specs (#925, #927, #928) are absorbed and quoted here, not depended on.
- The deterministic-unit work (#720) is absorbed and quoted here, not depended on.
</content>
</invoke>
