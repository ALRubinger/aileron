---
title: "ADR-0023: v4 Vault-Centric Encryption Schema"
description: "v4 stores credentials under vault keys that are wrapped per member, allowing personal and future shared vaults to use the same storage model."
order: 23
---

<div class="meta">
<table>
  <tr><th>Status</th><td>Proposed</td></tr>
  <tr><th>Date</th><td>2026-06-01</td></tr>
  <tr><th>Tracking</th><td><a href="https://github.com/ALRubinger/aileron/issues/827">#827</a>, <a href="https://github.com/ALRubinger/aileron/issues/747">#747</a></td></tr>
</table>
</div>

## Context

[ADR-0011](/adr/0011-local-credential-vault) defines the current local credential vault. The v4/v4.x/v5 roadmap needs a storage shape that can support personal vaults, future shared vaults, BYOC deployment, and eventually multi-tenant SaaS without rewriting credential encryption.

## Decision

v4 credential storage moves toward a vault-centric schema:

- Each vault has a random vault key.
- Credentials in that vault are encrypted with the vault key.
- The vault key is wrapped once per authorized member.
- A personal vault is just a one-member vault.
- Granting a member adds a new wrapping for the same vault key.
- Revoking a member rotates the vault key, re-encrypts credentials, and re-wraps the new key for remaining members.

The v4 user feature set does not need shared-vault UX immediately. The storage and encryption model should still avoid baking in a personal-only shape.

## Consequences

The runtime decrypt path becomes two-step: unwrap the vault key, then decrypt the credential. The vault key may be cached in memory for the session under the same safety constraints as today's unlocked vault state.

State externalization in #827 must route vault metadata, key wrappings, and encrypted credentials through backend interfaces rather than hardcoded `~/.aileron` paths.

The HTTPS data plane uses credential material only inside the trusted runtime boundary and never exposes raw credentials to the agent container.

## Alternatives Considered

**One user key encrypts every credential directly.** Simpler but does not extend cleanly to shared vaults.

**Build shared vaults later with a separate schema.** Rejected because migration would touch the most sensitive part of the system.

## References

- [Issue #827](https://github.com/ALRubinger/aileron/issues/827) — state externalization
- [ADR-0011](/adr/0011-local-credential-vault) — current local vault
