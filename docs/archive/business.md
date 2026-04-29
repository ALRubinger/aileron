---
title: "The Business"
description: "Revenue surfaces atop the open-source distribution layer: Control, Hub, Certification, Studio, Insights, Enterprise"
order: 7
---

> **Section:** part of the Pivot strategy. See [Overview](/) for the two-layer summary, [The Customer](/customer) for who this monetizes, and [Why This Is Structurally Defensible](/why-defensible) for what makes the surfaces compounding.

The core is open source. That is the distribution layer. Free is the right price because distribution is the asset.

- **Aileron Runtime** — MIT, self-hosted, `brew install aileron`. Includes the action engine, the connector sandbox, and the capability enforcement layer.
- **`ACTIONS.md` and the connector manifest format** — open primitives; community-authored actions and connectors available freely.
- **Aileron Control basics** — local vault, basic policy, single-user audit.

Revenue compounds across these surfaces:

## 1. Aileron Control — governance for production agents

Sold to teams shipping production agents that take real-world actions. The full Control surface: multi-user vault with scoped credentials, policy authoring and enforcement, inline approval workflows, full audit retention, RBAC, SSO.

- **Team tier:** $50/seat/mo.
- **Enterprise tier:** custom, with on-prem option, SLA, and dedicated support.

The expansion vector is architectural: every Runtime install is a Control candidate the moment the agent takes its first real-world action.

## 2. Aileron Hub — the unified marketplace surface

The Hub is one developer-facing browse experience covering both connectors and actions. Internally there are two distinct distribution mechanics — connectors are sandboxed binaries with signing, version pinning, and capability declaration; actions are template manifests copied into the developer's project on install. Externally these appear as one unified surface where developers browse for capabilities ("I want Slack things") and find both connectors and actions related to that domain.

Three tiers across both content types:

- **Free tier:** community-published connectors and actions, unsigned, best-effort.
- **Verified tier:** signed by Aileron, security-reviewed, SLA-backed.
- **Enterprise tier:** built and supported by Aileron for SAP, Salesforce, Workday, ServiceNow, and other high-value vertical integrations.

Aileron sits in the middle with a take rate. Pattern shape: Docker Hub, npm, GitHub Marketplace.

## 3. Aileron Connector Certification — vendor-funded trust

SaaS vendors pay Aileron to certify their official connector. Their logo gets the "Aileron Verified" badge. Customers get an audited supply chain. Vendor-funded, not customer-funded.

## 4. Aileron Connector Studio — for organizations building their own

Tooling for organizations building connectors against their own internal systems — sandbox primitives, capability declaration helpers, signing flow, CI integration, testing harness. Per-seat pricing.

## 5. Aileron Insights — observability and cost analytics

A first-party observability surface populated by every Runtime install:

- **Determinism rate** per agent — what fraction of requests are substituted by deterministic actions vs. left to the LLM.
- **Cost analytics** — savings from action substitution (every action that runs is an LLM call that didn't).
- **Connector observability** — which connectors run hot, which fail, which capabilities are exercised.
- **Action observability** — which actions are most-used, which fail, which capability surfaces are growing.

Sold per seat or as part of higher Control tiers.

## 6. Aileron Enterprise — the high-touch top

The traditional enterprise tier: on-premises deployment, SSO, RBAC, audit retention beyond cloud limits, dedicated support, SLAs, security reviews, custom action and connector development. Six-figure annual contracts. Detailed treatment in the enterprise document.

## The compounding logic

Every Aileron Runtime install is a candidate for all surfaces. Control monetizes production governance. Hub monetizes the platform itself. Certification monetizes vendor reputation. Studio monetizes custom integration. Insights monetizes ongoing operations. Enterprise monetizes the long tail of high-value organizations. Each makes the others more valuable. All ride on a free OSS distribution layer.
