---
title: "The Connector Model"
description: "Sandboxed binaries that declare abstract requirements; Aileron core ships only primitive capability types"
order: 1
---

> **Architecture:** part of the [Architecture](/architecture/) section of the Pivot strategy. See also [The Capability Model](/architecture/capability-model), [The Action Model](/architecture/action-model), and [Two Distribution Mechanics](/architecture/distribution-mechanics).

Aileron core ships only the *primitive capability types* — "network host:port," "vault credential of kind X," "host function Y" — not specific connectors. Gmail, Slack, GitHub, Stripe — these are connectors that arrive as separately distributed code, signed by their publishers, declaring what they need. Aileron core never has built-in knowledge of "Gmail" or "Slack."

Each connector ships a manifest declaring its needs:

```toml
[connector]
name = "gmail"
version = "1.2.3"
publisher = "acme.dev"
provenance_hash = "sha256:abc123..."

[capabilities.network]
hosts = ["gmail.googleapis.com:443", "oauth2.googleapis.com:443"]

[capabilities.credential]
kind = "oauth2"
scope = "https://www.googleapis.com/auth/gmail.send"

[capabilities.runtime]
imports = ["wasi:http/outgoing-handler", "wasi:cli/stdout"]

[provides]
intents = ["send_email", "draft_email"]
```

The manifest is a *request*. The runtime grants nothing not declared in it.
