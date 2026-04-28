---
title: "Enterprise — Addressed Later"
description: "Why enterprise is intentionally out of the v1 developer-facing pitch, and what's preserved architecturally for it"
order: 9
---

> **Section:** part of the Pivot strategy. See [The Customer](/pivot/customer) for the v1 wedge, [Aileron Control](/pivot/control) for the governance surface, and [The Business](/pivot/business) for the revenue tiers that productize this.

## Why this isn't in the v1 pitch

The v1 pitch is for the individual developer who can install Aileron in five minutes and feel productive in fifteen. Enterprise concerns — federated vault, audit retention beyond cloud limits, attested execution, signed-connector supply chains, SSO/RBAC, dedicated security review, on-prem deployment, six-figure annual contracts — are real and the architecture supports them. But putting them in the developer-facing pitch dilutes what makes the wedge work: that an indie agent builder, an AI-coding-tool power user, a local-first developer, and a privacy-conscious power user can each install the same Runtime and get something useful in their first hour. Compliance language doesn't help any of those four close the gap.

## What's preserved architecturally for enterprise

The architectural commitments are deliberately designed so the same codebase serves both wedge and enterprise without a rewrite:

- **[The Connector Model](/pivot/architecture/connector-model)** ships sandboxed binaries with publisher signing and content-addressed hashes. The supply-chain story enterprise compliance teams expect is in the manifest by construction.
- **[The Capability Model](/pivot/architecture/capability-model)** binds connectors to abstract requirements, not vault paths. Federated org-shared credentials slot into the same name-binding model individual developers use.
- **[Sandboxing](/pivot/architecture/sandboxing)** runs connectors in WASM by default and offers OS-process escalation for ultra-sensitive credentials — the boundary enterprise audit teams will demand for banking, healthcare, and similar domains.
- **[The User Channel](/pivot/architecture/user-channel)** has approvals on a tamper-resistant out-of-band surface — structurally protected from agent mediation, which no chat-completion-mediated tool layer can match. This is the property regulated industries have been waiting for.
- **[Failure Handling](/pivot/architecture/failure-handling)** refuses LLM fallback for side-effecting actions at install time. Auditability isn't an opt-in; it's structural.
- **[Project Portability](/pivot/architecture/project-portability)** lets bindings federate through Aileron Control for service accounts and shared credentials, without changing how action files are written.

In other words: the wedge architecture *is* the enterprise architecture. The difference is what's productized around it (multi-user vault, RBAC, audit retention, on-prem deployment, certified connectors, vendor SLAs), not the load-bearing design.

## Where the enterprise document lives

A dedicated enterprise document — covering compliance frameworks, deployment topology, federation patterns, supply-chain assurance, audit retention policies, attested execution flows, certification programs, and pricing/packaging for the Enterprise tier — is intentionally separate from this Pivot section. It's a different audience, a different reading order, and a different sales motion. It will be linked here once it lands.

In the meantime, the closest reading material:

- **[Aileron Control](/pivot/control)** — the governance surface enterprise tiers productize.
- **[The Business](/pivot/business)** — Aileron Enterprise (Surface 7), Aileron Connector Certification (Surface 4), Aileron Insights (Surface 6), and Aileron Connector Studio (Surface 5) are the revenue surfaces aimed at this audience.
- **[Why This Is Structurally Defensible](/pivot/why-defensible)** — the competitive comparison includes 1Password Agent Access, Salesforce Agent Fabric, and other adjacent enterprise-leaning offerings; the seam Aileron occupies remains empty for them too.
