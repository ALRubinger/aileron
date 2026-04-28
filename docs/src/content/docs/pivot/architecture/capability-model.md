---
title: "The Capability Model"
description: "Types, not paths — and why Aileron does not include a capability abstraction layer"
order: 2
---

> **Architecture:** part of the [Architecture](/pivot/architecture/) section of the Pivot strategy. See also [The Connector Model](/pivot/architecture/connector-model) and [The Action Model](/pivot/architecture/action-model).

## Types, not paths

The connector cannot name a vault path. It declares an abstract requirement ("an OAuth2 credential with this scope"), and the user binds the requirement to a concrete vault entry at install time:

```
Connector "gmail" requests an OAuth2 credential with scope:
  https://www.googleapis.com/auth/gmail.send

You have:
  ▸ gmail/work        (alr@workplace.com)
  ▸ gmail/personal    (alr@home.com)
  ▸ Add new account…

Bind to: [gmail/work]
```

This is structurally important. A malicious connector cannot even *name* a key it shouldn't reach. It declares an abstract need; the user binds the specific resource. The Stripe connector requesting an OAuth2 scope of `gmail.send` would either fail (no matching credential exists) or be visibly wrong to the user.

This pattern matches Android's `ContentProvider` model, iOS privacy entitlements, and macOS TCC. Well-trodden ground.

## Connectors, not capabilities

Actions bind to specific connectors (`slack@1.2.0`), not to abstract capabilities (`messaging:post_to_channel`). This is a deliberate simplification, not a deferred decision. Capability abstraction would add standardization governance (who defines `messaging:post_to_channel`?), parser complexity, a second trust layer (trust the spec *and* trust any implementation), and UX ambiguity ("I want messaging" vs "I want Slack") in exchange for marginal substitutability benefit.

Aileron does not include capability abstraction. Action authors name the connector they want; if they need to swap implementations, they edit the action file. The ShadCN distribution model makes this trivial — the action file is theirs to evolve.
