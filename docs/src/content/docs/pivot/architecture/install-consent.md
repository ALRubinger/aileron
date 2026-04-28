---
title: "Install Consent: One Path"
description: "Single binary install/cancel decision; no per-capability denial; the install moment as a readable summary"
order: 5
---

> **Architecture:** part of the [Architecture](/pivot/architecture/) section of the Pivot strategy. See also [The Connector Model](/pivot/architecture/connector-model), [The Capability Model](/pivot/architecture/capability-model), and [Capability Binding UX](/pivot/architecture/capability-binding).

## Install consent: one path

There is no tiered installation flow. Every connector and every action installs through the same path. The user sees the publisher identity, the signature status, and the full capability declaration; the user either clicks Install or Cancel. There is no option to selectively deny capabilities — the contract installs whole or not at all.

This is a deliberate simplification. Partial-install state — where a connector is installed but missing some of the capabilities it declared — is a source of ambiguity: the connector might or might not work, the action might or might not run, errors become confusing. By removing the option to selectively deny, Aileron preserves a clear invariant: an installed connector has all of its declared capabilities; an installed action has all of its declared connector dependencies; if not, it isn't installed.

Publisher identity and signature verification still matter — they appear in the install consent UI so the user can decide whether to install at all. A connector signed by a known publisher whose key Aileron has verified shows that signature; an unsigned connector from an unknown source shows "UNSIGNED" prominently. Verification is information shown to the user, not a different UX flow.

The Hub may use signing and verification status to organize and rank entries (verified publishers may appear higher; unsigned entries may carry a warning badge in browse views). But once the user chooses to install, the consent flow is the same regardless: show what you're getting, get explicit user approval, install in full or not at all.

## The install moment

At install, Aileron presents a single readable summary:

```
Installing: gmail v1.2.3
Publisher: acme.dev  (signature verified)

This connector requests:

  Network
    • gmail.googleapis.com:443
    • oauth2.googleapis.com:443

  Credentials (bound at first-use)
    • An OAuth2 token (scope: gmail.send)

  Capabilities
    • Outbound HTTP
    • No filesystem access
    • No environment access

  Provides actions: send_email, draft_email

[Install]  [Cancel]
```

The user installs the contract as declared, or cancels. There is no partial install; there is no per-capability customization. This preserves the invariant that an installed artifact carries all of its declared capabilities — no ambiguity, no half-state.
