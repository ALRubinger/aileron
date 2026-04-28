---
title: "Architecture"
description: "Index of the architectural commitments behind the Aileron pivot — each will be ratified as an ADR"
order: 4
---

> **Section:** part of the Pivot strategy. See [Overview](/pivot/overview) for the one-sentence pitch, [Aileron Runtime](/pivot/runtime) and [Aileron Control](/pivot/control) for the two layers, and [Why This Is Structurally Defensible](/pivot/why-defensible) for how the architecture maps to the competitive landscape.

The architectural decisions below will be ratified as ADRs. They're sketched here so the strategic choices are visible up front: Aileron core ships small, knows nothing about specific systems, and treats every connector as untrusted code with declared, enforced capabilities.

## Why these decisions become ADRs

The connector model, the action model, capability binding UX, dependency resolution, intent matching mechanisms, the user channel and out-of-band approval surfaces, failure-handling policy, project portability and the action-file-as-contract model, manifest format conventions, install consent flow, and sandbox choice are all foundational. Each will get an ADR with the trade-offs, alternatives considered, and decision criteria recorded explicitly. That documentation exists to keep these decisions reviewable and to make changes deliberate, not accidental.

## The architectural commitments

- **[The Connector Model](/pivot/architecture/connector-model)** — sandboxed binaries declaring abstract requirements; Aileron core ships only primitive capability types.
- **[The Capability Model](/pivot/architecture/capability-model)** — types, not paths; users bind concrete resources at install. No capability abstraction layer (deliberate, permanent).
- **[The Action Model](/pivot/architecture/action-model)** — atomic, depends on connectors with explicit version + hash + capability subsets; ShadCN-style copy-on-install.
- **[How Intent Matches to Actions](/pivot/architecture/intent-matching)** — primary tool augmentation via function calling; secondary pre-LLM bypass for clear intents.
- **[Install Consent: One Path](/pivot/architecture/install-consent)** — single binary install/cancel decision; no per-capability denial.
- **[Capability Binding UX](/pivot/architecture/capability-binding)** — one auth path on demand; types at install, bindings at first-use, opt-in pre-binding.
- **[Two Distribution Mechanics](/pivot/architecture/distribution-mechanics)** — connectors are content-addressed binaries; actions are files copied into the project.
- **[Sandboxing and Runtime Enforcement](/pivot/architecture/sandboxing)** — WASM by default, OS-process escalation as an opt-in higher tier.
- **[The User Channel](/pivot/architecture/user-channel)** — streaming output in-band; consent out-of-band via five tiers; agent never in the trust path.
- **[Failure Handling](/pivot/architecture/failure-handling)** — visible by default; LLM fallback opt-in for informational; structured errors; idempotency by default.
- **[Project Portability](/pivot/architecture/project-portability)** — committable action files; personal bindings; `aileron sync`; Control federation for shared credentials.
- **[Manifest Format](/pivot/architecture/manifest-format)** — Markdown body + TOML frontmatter for actions; pure TOML for connectors and project config; JSON for IPC.
