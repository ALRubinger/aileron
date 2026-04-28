---
title: "Capability Binding UX"
description: "One auth path, triggered on demand; types at install, bindings at first-use, opt-in pre-binding"
order: 6
---

> **Architecture:** part of the [Architecture](/pivot/architecture/) section of the Pivot strategy. See also [Install Consent: One Path](/pivot/architecture/install-consent), [The Capability Model](/pivot/architecture/capability-model), and [Project Portability](/pivot/architecture/project-portability).

Five architectural commitments shape how users authenticate and bind credentials. The detailed prompt sequences, multi-account UX, and conflict-resolution flows defer to the binding-UX ADR.

**1. Bindings are always explicit user actions.** Aileron never silently associates a credential with a capability. The user confirms at the moment a binding is created or modified.

**2. Capability *types* surface at install (transparency only).** When a connector or action is installed, Aileron shows what capabilities will be requested ("this connector will want OAuth2 access with scope `chat:write`"). No browser opens, no binding happens — this is just transparency about what the user is committing to allow later.

**3. Capability *bindings* surface at first-use.** The actual OAuth dance and credential binding run when the action first needs the credential. This is the same code path that handles credential refresh after expiration or revocation. One auth flow, triggered whenever credentials are needed and missing or expired, regardless of whether it's the first time or the hundredth. Consistency is the win.

**4. Pre-binding is opt-in for users who want it.** `aileron binding setup` (or `aileron sync --bind-now`) runs the auth flow eagerly for everything the project needs. Power users can pre-authenticate everything. Headless and autonomous workflows require this — non-interactive runs cannot complete OAuth flows mid-execution, so credentials must be pre-bound or federated through Control.

**5. Bindings are managed visibly.** `aileron binding list`, `aileron binding inspect`, and `aileron binding rebind` give the user direct control. Bindings are observable, replaceable, and removable on demand.

The detailed UX — exact prompt sequences, what happens when a binding name collides with an existing one, how multi-account workflows look, the precise behavior of `aileron sync --bind-now` in mixed-state projects — defers to the binding-UX ADR. These five commitments set the architectural posture; the ADR fills in the operational details with concrete implementation experience.
