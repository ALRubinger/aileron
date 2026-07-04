---
title: "ADR-0027: Flight Plan = Sealed Installable Skill"
description: "A Flight Plan and an Aileron skill are the same construct seen at two points in one lifecycle. A skill is authored in the agentskills.io SKILL.md format extended with a requires block, and it becomes a Flight Plan at a freeze step that resolves image references to digests, produces a lockfile, binds the execution environment, attaches a per-action trust contract, and signs the result. The execution image is agent-free, and an LLM runs only at a single structurally-enforced seam."
order: 27
---

<div class="meta">
<table>
  <tr><th>Status</th><td>Accepted</td></tr>
  <tr><th>Date</th><td>2026-06-23</td></tr>
  <tr><th>Tracking</th><td><a href="https://github.com/ALRubinger/aileron/issues/1506">#1506</a>, <a href="https://github.com/ALRubinger/aileron/issues/1514">#1514</a>, <a href="https://github.com/ALRubinger/aileron/issues/1898">#1898</a></td></tr>
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

### The execution environment

A Flight Plan runs inside one container. The `environment` block declares it, and freeze composes and pins exactly one image for the whole plan.

The block declares `tools`, or an `image`, or both. `tools` names entries from a closed curated catalog, each as `<name>@<version>` (for example `aws-cli@2.x`). Freeze resolves each entry to its catalog devcontainer Feature recipe and composes one image on the Aileron-provided runner base. An unknown tool name is rejected at validation, before freeze. `image` names a custom base image, the escape hatch for tooling the curated catalog does not carry. Declaring both composes the declared tools onto the custom base. A present `environment` block must declare at least one of the two, so an empty `environment: {}` is rejected. A skill that needs no environment omits the block, and freeze pins nothing.

Freeze resolves the declared environment to one content-addressed digest and records that single pin in the lock. Launch boots that exact pinned image from the verified lock and runs the whole plan inside it, so the container entered corresponds to the lock's signed image assertion rather than any re-resolved tag. A composed-tools pin converges with the image-only path. Freeze builds the composed image locally and stays hermetic and offline, so freeze itself pushes nothing. A separate, explicit `skill publish` verb pushes the composed image to an OCI registry and pins it there by a verifiable registry digest, the same `ref@digest` shape the image-only and custom-base paths already carry. Launch resolves that registry digest from the verified lock, pulls the image, and verifies it against the signed lock before entering it. A pulled image whose digest does not match the signed pin, or an image the registry cannot serve, refuses to boot rather than entering an unverified environment. Because the pin is a registry digest rather than a local-daemon tag, a machine that never ran freeze can pull and verify the exact composed image a second operator published. The original ADR recorded this case as local-only, where the attested digest was a local image Id and a sealed local-daemon tag was the bootable identity; that local-only limitation is superseded as of 2026-07-04 by the OCI-unified distribution model tracked under [#1898](https://github.com/ALRubinger/aileron/issues/1898). A Flight Plan that declares no environment has an empty resolved-image set, so its launch runs the step graph in-process instead of booting an image. On the container boot path in v1, approvals and audit are re-established in-container and the resulting AuditIDs are not threaded back to the host `RunResult`, so full `RunResult` and AuditID threading across the container boundary lands in a later sub-issue rather than being complete now.

A `tool` step runs a declared environment tool as a deterministic subprocess inside that one booted container. It carries an argv `command` (no shell interpretation), optional `mount` and `collect` file-I/O boundaries, and an optional per-step trust contract whose `hosts` declare that step's network reach. Freeze seals the declared reach into a lock `stepTrust` section keyed by the tool-step id, so the reach is covered by the content hash and the signature and cannot be re-supplied at launch. Launch enforces it. Before a tool step runs, the daemon mints a step-scoped, TTL-bounded proxy credential restricted to exactly the sealed hosts. A scoped CONNECT to an undeclared host is refused with a 403 and a `sandbox.proxy.trust_denied` audit record (reason `step_scope_host_denied`), before the TLS interception handshake, so no credential bytes ever enter the container. A contracted tool step in a plan with no verified sealed entry, and a tool-step plan launched with no pinned environment, both fail closed. This enforcement is a proxy-credential guarantee, not a network-layer egress guarantee. An `enforced: true` reach record means the step ran under a step-scoped proxy credential restricted to its sealed reach; it does not attest that the subprocess was physically unable to reach any other host. A non-cooperative subprocess that ignores `HTTPS_PROXY` and dials a raw socket directly is out of scope for this layer.

The execution container is agent-free. The image carries no coding agent. The Flight Plan runs composed steps, not an interactive agent session.

### Multi-identity

The credential binding lives on the operator's machine, not in the plan. The identical sealed artifact launches under different identities with different authorizations, and the audit trail records who. The image and the booted container never contain a secret in any form. The daemon forward proxy injects or re-signs with the vault credential at the egress boundary, so the same frozen version serves many operators without ever carrying one operator's identity.

### Distribution across machines

A frozen Flight Plan is portable across machines. Freeze stays local, hermetic, and offline, and a separate `skill publish` verb performs the network-touching act of pushing the frozen version to an OCI registry. Publish pushes one OCI reference that carries both the composed image and the signed artifact, where the signed artifact bundles the `SKILL.md`, the `aileron.lock`, the detached signature, and the publisher's public signing key as an OCI referrers manifest. A second operator runs `skill install <oci-ref>` to pull that reference and verify it, and no re-freeze happens on the second machine. The composed image is pinned by a verifiable registry digest, so the install-by-reference path resolves and verifies the exact image the publisher sealed.

Publisher trust extends the connector keyring. The keyring-v2 model of [ADR-0013](/adr/0013-connector-hub-and-trust-distribution) already ratifies per-publisher owner-level grants alongside per-repo static-ed25519 grants for connectors, and the same keyring covers Flight Plan publishers. One `keyring trust <publisher>` authorizes a publisher's signing key for connectors and plans alike. An untrusted publisher, or an artifact whose signature or content hash does not verify, fails closed rather than installing or launching.

Discovery stays deferred, mirroring the "the Hub points; it does not host" framing of [ADR-0013](/adr/0013-connector-hub-and-trust-distribution). Install-by-reference works with no Hub, exactly as connector install-by-FQN does, so a known OCI reference is enough to install and launch. A git-backed Hub listing that indexes published plans folds into a later concern.

Multi-identity is preserved, not rebuilt. The per-operator vault, the proxy credential injection at the egress boundary, and the operator-attributed audit of [ADR-0019](/adr/0019-v4-https-data-plane) already distinguish two operators running the identical sealed artifact. The second operator wires their own vault credential and runs as themselves, and the audit trail records who ran it.

### Manifest and signing

The manifest is the `SKILL.md` frontmatter, extended in one format. The extension is lossless if stripped, so a tool that ignores the Aileron fields still reads a valid skill.

The frontmatter gains a `requires:` block and an `environment:` block. The `requires:` block lists the actions the Flight Plan calls. The sibling `environment:` block declares the single container the plan runs in. Each listed action attaches to the action model of [ADR-0003](/adr/0003-action-model).

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
- A Flight Plan is multi-identity. The sealed artifact carries no credential binding, so one frozen version launches under different vault-bound identities with different authorizations, and the audit trail records who ran it. The image and the booted container never hold a secret, because the daemon forward proxy injects or re-signs with the vault credential at the egress boundary.
- A Flight Plan is portable across machines. A frozen plan is published once to an OCI registry with a separate `skill publish` verb, and a second operator installs it by reference and launches it under their own identity without re-freezing. The composed image is pinned by a verifiable registry digest, so install-by-reference pulls and verifies the exact image the publisher sealed, and an untrusted publisher or a tampered image fails closed.

### Negative

- Freeze is rigid. A frozen Flight Plan pins one resolved environment, and changing the environment requires a new freeze and a new version.
- The single-seam constraint is strict. A Flight Plan that needs LLM reasoning in more than one place must route all of it through the one marked seam or restructure to fit.
- The trust contract is verbose. Every action carries a full contract block, and authoring a Flight Plan with many actions records many such blocks.

## Deferred

The following are out of scope for this ADR and this layer's MVP.

- Container audit threading. Per-step tool-egress reach is now enforced, not deferred: a tool step's declared reach is sealed into the lock `stepTrust` at freeze, and launch runs the step under a step-scoped, TTL-bounded proxy credential restricted to exactly the sealed hosts, refusing an undeclared host at the daemon proxy before the TLS handshake. What remains deferred is the container-boundary audit threading noted above: on the container boot path in v1, in-container AuditIDs are not threaded back to the host `RunResult`, so full `RunResult` and AuditID threading across the container boundary is the follow-up.
- Binary outputs. The `outputs:` contract reserves the `base64` encoding, but the v1 runtime materializes `utf-8` artifacts only. Binary output is blocked on a host-ABI binary-body field, because the current JSON-string result body coerces arbitrary bytes to valid UTF-8 and corrupts them. A tool step's mount and run-and-collect boundary (#1510) is the escape hatch for large or binary artifacts when a consumer arrives.
- Flight Plan discovery. Install and launch work from a known OCI reference with no Hub, exactly as connector install-by-FQN does. A git-backed Hub listing that indexes published Flight Plans folds into a later concern, mirroring the deferred discovery framing of [ADR-0013](/adr/0013-connector-hub-and-trust-distribution).
- STS and SSO. Short-lived token services and single sign-on integration are not specified here.
- Specific connectors. No named connector is specified by this ADR. The trust-contract format applies to any action, and individual connector contracts are authored against this format elsewhere.

## References

- [Issue #1506](https://github.com/ALRubinger/aileron/issues/1506). The Flight Plan layer umbrella and implementation home
- [Issue #1514](https://github.com/ALRubinger/aileron/issues/1514). This ADR's tracking sub-issue
- [Issue #1519](https://github.com/ALRubinger/aileron/issues/1519). The output-contract reservation that text is the v1 implementation and binary is a deferred follow-up
- [The Flight Plan manifest specification](/development/flight-plan-manifest-spec). The `outputs:` contract shape and the file-map transport
- [The Launch-a-Flight Surfaces Spec](/development/launch-surfaces-spec). The distribution surfaces over a frozen Flight Plan
- [Issue #1898](https://github.com/ALRubinger/aileron/issues/1898). The shareable-frozen-plan umbrella that records the OCI-unified distribution model
- [ADR-0003](/adr/0003-action-model). The action model the per-action trust contract attaches to
- [ADR-0005](/adr/0005-sandbox-choice). The sandbox and credential mediation a Flight Plan runs inside
- [ADR-0013](/adr/0013-connector-hub-and-trust-distribution). The connector Hub and keyring-v2 trust model the Flight Plan distribution model mirrors
- [ADR-0009](/adr/0009-user-channel). The out-of-band approval channel the per-action effect feeds
- [ADR-0019](/adr/0019-v4-https-data-plane). The data-plane injection a Flight Plan's credentialed calls flow through
- [ADR-0026](/adr/0026-cli-capability-units). The capability-unit shape the curated-catalog tool Features follow when freeze composes the environment image
- The Flight Plan specs (#925, #927, #928) are absorbed and quoted here, not depended on.
- The deterministic-unit work (#720) is absorbed and quoted here, not depended on.
</content>
</invoke>
