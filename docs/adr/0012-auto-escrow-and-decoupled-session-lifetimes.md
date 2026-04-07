# ADR-0012: Auto-Escrow on Passphrase Verification and Decoupled Session Lifetimes

**Status:** Accepted
**Date:** 2026-04-07

## Context

ADR-0010 established the zero-knowledge vault with a 30-minute default KEK session TTL. ADR-0011 introduced the TEE escrow mechanism (Phase 6) for offline/scheduled execution. However, neither addressed the UX problem: autonomous agents running on behalf of users are interrupted every 30 minutes by HTTP 423 Locked responses, requiring the user to re-enter their passphrase.

For the product promise of autonomous agent execution (email triage, meeting scheduling, payment processing) to work, users cannot be asked to unlock the vault every 30 minutes. At the same time, the KEK session lifetime and the escrow lifetime serve fundamentally different purposes with different security profiles.

### Problem

1. **30-minute interruption cycle** breaks autonomous execution. Agents receive 423 Locked when the KEK session expires.
2. **No automatic escrow** — credentials must be manually escrowed per-grant. Phase 6 (ADR-0011) defined the mechanism but not the trigger.
3. **Single TTL** — the KEK session TTL controlled both interactive UI operations and autonomous execution lifetime, conflating two concerns.

## Decision

### 1. Auto-escrow on passphrase verification

When a user successfully verifies their passphrase and a TEE session is active, Aileron automatically escrows all active connected account credentials into the enclave:

1. List all connected accounts for the user (filter to active status).
2. For each account, read the KEK-encrypted credential from the vault.
3. Call `EscrowStore` on the enclave — the enclave already holds the user's KEK (transmitted during verification) and decrypts internally.
4. Store the returned escrow ID in a transient server-side index (vault path to escrow ID).
5. During execution, the TEE path checks the escrow index and uses the escrow ID instead of sending the encrypted credential.

This means the user unlocks once, and all their approved agents run independently until the escrow expires.

### 2. Decoupled TTLs

The KEK session and the escrow serve different purposes and have different security profiles:

| Concern | KEK Session | Escrow |
|---------|------------|--------|
| **Purpose** | UI operations: viewing credentials, connecting accounts, triggering escrow batch | Autonomous agent execution without user present |
| **Where it lives** | Go process memory (or enclave via TransmitKEK) | TEE hardware-isolated memory only |
| **Security exposure** | Process memory — vulnerable to heap dumps | Hardware-isolated — requires TEE compromise |
| **Default lifetime** | 24 hours (`AILERON_KEK_SESSION_TTL`) | 7 days (`AILERON_ESCROW_TTL`) |
| **User can revoke** | Re-verify resets; expires naturally | Disconnect account or revoke grant |

**`AILERON_KEK_SESSION_TTL`** (default: `24h`, was `30m`) — How long the derived KEK stays in process memory. Controls interactive vault operations. A user who logs into the Aileron UI daily enters their passphrase once per day.

**`AILERON_ESCROW_TTL`** (default: `168h` = 7 days) — How long escrowed credentials persist in TEE memory. Controls autonomous execution. A user who verifies their passphrase once gets 7 days of uninterrupted agent execution.

Re-verifying the passphrase refreshes the escrow with a new 7-day window. A user who logs in daily effectively gets indefinite autonomous execution.

### 3. Passphrase verification UI

A passphrase modal in the management UI intercepts 423 Locked responses:
- Opens automatically when any API call returns 423.
- After successful verification, retries the original operation transparently.
- Shows vault session status (unlocked + time remaining) in the navigation bar.

## Consequences

### What changes

- **KEK session default is 24 hours**, up from 30 minutes. This is a UX improvement with minimal security impact — with auto-escrow, the session is primarily for interactive operations, not autonomous execution.
- **Escrow lifetime is independent of the KEK session.** Even after the KEK session expires, escrowed credentials remain available in TEE memory for agent execution.
- **New endpoint** `GET /v1/users/me/passphrase/session` returns session status for UI display.
- **`VerifyPassphraseResponse`** enriched with `session_expires_at` and `escrowed_count`.
- **Enclave client** TTLs are configurable via options instead of hardcoded constants.
- **Execution path** checks the escrow index before sending encrypted credentials to the enclave.

### What stays the same

- The vault trust model (ADR-0010) is unchanged — KEK is never stored at rest.
- The escrow mechanism (ADR-0011 Phase 6) is unchanged — same `EscrowStore`/`EscrowRevoke` protocol.
- Both TTLs remain configurable via environment variables for deployments wanting different trade-offs.
- Non-TEE deployments continue to use the KEK session for credential decryption (no escrow without TEE).

### Security analysis

**Why 24-hour KEK session is acceptable:**
- With auto-escrow, the KEK session is for UI convenience, not autonomous execution.
- The threat model where 30 minutes vs 24 hours matters (transient one-time memory access) is narrow — if an attacker has process memory access, they likely have persistent access.
- The `AILERON_KEK_SESSION_TTL` env var is available for security-conscious deployments.

**Why 7-day escrow is acceptable:**
- Escrowed credentials live in TEE hardware-isolated memory, not process memory.
- Usage is gated by execution grants and approval policies.
- Users can revoke at any time (disconnect account or revoke grant).
- The credentials already exist in the external service — the risk isn't credential exposure but unauthorized use via Aileron, which the policy engine controls.
- TEE compromise is a catastrophic scenario regardless of escrow duration.

## References

- [ADR-0010: Zero-Knowledge Vault Trust Model](0010-zero-knowledge-vault-trust-model.md)
- [ADR-0011: TEE Provider SPI and Confidential Space](0011-tee-provider-spi-and-confidential-space.md)
- [Issue #55: Passphrase verification UX and auto-escrow for autonomous agents](https://github.com/ALRubinger/aileron/issues/55)
