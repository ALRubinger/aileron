---
title: "Sandboxing and Runtime Enforcement"
description: "WASM by default; OS-process escalation as an opt-in higher tier; capability denials at both connector and action boundaries"
order: 8
---

> **Architecture:** part of the [Architecture](/pivot/architecture/) section of the Pivot strategy. See also [The Connector Model](/pivot/architecture/connector-model), [The Action Model](/pivot/architecture/action-model), and [The Capability Model](/pivot/architecture/capability-model).

Connectors run in a WASM sandbox by default — capability-based isolation, language-agnostic, fast startup, deterministic execution. Aileron's vault sits in the host process; credentials are issued to connectors as short-lived scoped tokens per call, never as long-term keys.

The sandbox guarantees:

- Connector A cannot read Connector B's memory.
- Connector A cannot dial network hosts not in its grant.
- Connector A cannot access vault entries outside its bound credentials.
- Capability requests at runtime not in the install grant are denied at the WASM boundary.
- Action requests at runtime not in the action's `requires` declaration are denied at the action boundary.

For ultra-sensitive credentials (banking, healthcare), connectors can be escalated to OS-process isolation as a higher tier — slower, stronger boundary. Default is WASM; escalation is opt-in.
