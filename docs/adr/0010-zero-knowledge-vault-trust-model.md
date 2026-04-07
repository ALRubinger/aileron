# ADR-0010: Zero-Knowledge Vault with Hybrid TEE + Client-Side Key Custody

**Status:** Accepted
**Date:** 2026-04-06

## Context

ADR-0009 established Aileron as a deterministic execution plane that owns user credentials (OAuth tokens for Gmail, Google Calendar, payment instruments) and executes irreversible actions on behalf of agents. The credential vault is a core component — agents never see credentials, only structured results.

However, ADR-0009 left the vault's trust model unspecified. The initial implementation used plaintext in-memory storage (`MemVault`). In production, this means Aileron operators, database administrators, and hosting providers could access user secrets. This contradicts the foundational promise: if Aileron asks users to trust it with their Gmail token, calendar credentials, and payment instruments, the business must be architecturally unable to access those secrets — not just operationally committed to not looking.

This is not a compliance checkbox. It is core to the value proposition and creates a competitive moat that cannot be easily retrofitted by competitors.

### Options Considered

1. **Session-Unlocked Vault (BYOK + Ephemerality)** — User provides a passphrase to unlock secrets at session start. Server decrypts in memory, uses, discards. Zero-knowledge at rest, but the server sees plaintext at runtime. Trust is operational, not cryptographic.

2. **Confidential Computing (TEEs / Secure Enclaves)** — Connector execution inside a Trusted Execution Environment (AWS Nitro Enclaves). Even host operators cannot inspect enclave memory. Attestation proves code integrity. Trust is hardware-attested.

3. **Client-Side Execution (Proxy Model)** — Aileron constructs the API request, the user's device injects credentials and fires it. True zero-knowledge, but requires the user to be online. Breaks async/scheduled actions.

4. **Hybrid TEE + Client-Side Key Custody** — Combines options 2 and 3. Users derive a Key Encryption Key (KEK) from a passphrase (never stored server-side). All vault secrets are encrypted with the user's KEK. Connector execution happens inside a TEE that receives the KEK only after the client verifies enclave attestation. For async actions, time-limited key escrow inside the enclave.

## Decision

Aileron adopts **Option 4: Hybrid TEE + Client-Side Key Custody** as the foundational trust model for the credential vault, implemented in two stages:

### Stage 1: Encrypted-at-Rest Vault (Phases 1-3) — Implemented

Zero-knowledge storage with user-derived keys. This delivers immediate value without requiring TEE infrastructure.

**Phase 1: Crypto Primitives + Encrypted Vault Decorator**

- `core/crypto/` package with three modules:
  - `kek.go` — Argon2id key derivation (`DeriveKEK`, `GenerateSalt`). Parameters: time=3, memory=64MB, threads=4, keyLen=32. Configurable for testing.
  - `envelope.go` — AES-256-GCM envelope encryption (`Encrypt`, `Decrypt`, `WrapKey`, `UnwrapKey`). Random 96-bit nonce prepended to ciphertext.
  - `ecdh.go` — P-256 ECDH key exchange (`GenerateKeyPair`, `DeriveSharedSecret`) with SHA-256 key derivation. For future TEE session key exchange.
- `core/vault/encrypted.go` — `EncryptedVault` decorator wrapping any `vault.Vault` implementation. Encrypts values on `Put`, decrypts on `Get`. Metadata stored unencrypted.
- Refactored `apiServer.vault` field from `*vault.MemVault` to `vault.Vault` interface to enable the decorator pattern.

**Phase 2: User Key Material + Passphrase Flow**

- `core/model/UserKeyMaterial` — stores Argon2id salt and KEK verification blob per user. The KEK itself is **never stored server-side**.
- `core/store/UserKeyMaterialStore` interface with in-memory and PostgreSQL implementations.
- Database table `user_key_materials` (user_id PK, salt, kek_verification, timestamps).
- API endpoints:
  - `POST /v1/users/me/passphrase` — set or rotate passphrase. Derives KEK, stores salt + encrypted verification constant, returns salt.
  - `POST /v1/users/me/passphrase/verify` — re-derives KEK, verifies against stored blob, caches KEK in session.
  - `GET /v1/users/me/passphrase/salt` — returns salt and whether passphrase is set (for client-side KEK derivation).
- Passphrase verification uses a known constant (`aileron-kek-verification-ok`) encrypted with the KEK. On verify, the server re-derives the KEK and attempts decryption. If it succeeds and the plaintext matches, the passphrase is correct — without ever storing the KEK.

**Phase 3: Connected Account Encryption + Session KEK Cache**

- `core/auth/KEKSessionCache` — in-memory cache keyed by user ID with configurable TTL (default 30 minutes). Copy-on-set/get semantics. Zeros KEK bytes on eviction or clear.
- `core/vault/UserScopedVault` — decorator that applies per-user encryption. When a KEK is provided, `Put` encrypts and tags secrets with `"encrypted": "true"` metadata label. When KEK is nil, operations pass through unchanged.
- `VerifyPassphrase` handler caches the derived KEK on successful verification.
- `RunExecution` handler detects `"encrypted": "true"` secrets, retrieves the KEK from the session cache, and decrypts before injecting into the connector. Returns HTTP 423 Locked if no KEK session is active.
- `ConnectAccountCallback` handler wraps the vault with `UserScopedVault` when a KEK session exists, so OAuth tokens are encrypted at storage time.
- `GoogleService.WithVault()` enables per-request vault scoping without modifying the account service SPI.
- New environment variable: `AILERON_KEK_SESSION_TTL` (default `30m`).

### Stage 2: Confidential Computing (Phases 4-6) — Planned

Hardware-attested execution isolation. Tracked in a separate GitHub issue.

**Phase 4: Enclave Execution Runtime** — Separate Go binary (`cmd/aileron-enclave/`) running inside AWS Nitro Enclaves. Listens on vsock, receives encrypted credentials, decrypts inside the enclave, executes connector logic, returns structured results. Ephemeral ECDH key exchange for session establishment. `LocalClient` for dev/test (in-process, no actual enclave).

**Phase 5: Remote Attestation Verification** — Server-side and client-side (browser) verification of Nitro attestation documents (COSE Sign1 + PCR validation). Client-side KEK derivation in WebAssembly (Argon2id). Attestation flow: client verifies enclave integrity before releasing KEK, encrypted with the enclave's attested public key.

**Phase 6: Async/Scheduled Action Key Escrow** — Time-limited, scope-limited credential escrow inside the enclave for actions that execute when the user is offline. Entries indexed by grant ID with action type scope, time window, and max uses. Credentials exist only in enclave memory; zeroed on expiry.

## Key Hierarchy

```
User Passphrase
  │
  │  Argon2id (salt stored server-side)
  │
  ▼
Key Encryption Key (KEK)  ──── never stored server-side
  │
  │  AES-256-GCM
  │
  ▼
Encrypted Vault Secrets  ──── stored in database (ciphertext only)
  │
  │  Decrypted at execution time:
  │    Stage 1: in server memory (session KEK cache)
  │    Stage 2: inside TEE (enclave memory only)
  │
  ▼
Connector Execution  ──── raw credential used for API call, then discarded
```

## Threat Model

| Threat | Stage 1 Mitigation | Stage 2 Mitigation |
|--------|-------------------|-------------------|
| Database compromise | Secrets encrypted with user KEK; attacker gets ciphertext only | Same |
| Host memory dump | KEK held only during active session (30min TTL) | KEK + plaintext exist only inside TEE |
| Aileron operator | Cannot read vault at rest | Cannot access enclave memory |
| Hosting provider | Cannot read vault at rest | Nitro Enclave hardware isolation |
| Compromised server binary | KEK visible in process memory during session | Attestation PCR mismatch; client refuses to release KEK |
| User offline (scheduled actions) | Not supported without KEK session | Time-limited escrow in enclave, scoped to pre-approved grants |
| Passphrase loss | No recovery (future work: recovery codes) | Same |

## Consequences

- The `vault.Vault` SPI is unchanged. All encryption is layered via decorators (`EncryptedVault`, `UserScopedVault`). Existing code that accepts `vault.Vault` works without modification.
- `MemVault` remains the default for development and testing. Production deployments use `EncryptedVault` or `UserScopedVault` wrapping a persistent vault.
- Users who have not set a passphrase continue to use plaintext storage (passthrough mode). Encryption is opt-in per user until a migration flow is implemented.
- The `apiServer.vault` field is now `vault.Vault` (interface) instead of `*vault.MemVault` (concrete type).
- New database table: `user_key_materials`.
- New API endpoints: `/v1/users/me/passphrase`, `/v1/users/me/passphrase/verify`, `/v1/users/me/passphrase/salt`.
- New environment variable: `AILERON_KEK_SESSION_TTL`.
- Stage 2 (Phases 4-6) will introduce infrastructure requirements: AWS Nitro Enclaves (or equivalent TEE), vsock communication, and attestation verification in both server and client.
- Stage 2 will add new API endpoints: `/v1/enclave/attestation`, `/v1/enclave/session`, `/v1/enclave/status`.
- The zero-knowledge model is a competitive moat. Competitors who built on plaintext credential storage would need to substantially rearchitect to retrofit this model.

## Files

### New (Stage 1)
- `core/crypto/kek.go`, `envelope.go`, `ecdh.go` — cryptographic primitives
- `core/vault/encrypted.go` — EncryptedVault decorator
- `core/vault/user_vault.go` — UserScopedVault decorator
- `core/auth/kek_session.go` — KEK session cache
- `core/model/user_key_material.go` — user key material model
- `core/store/mem/user_key_material.go` — in-memory store
- `core/store/postgres/user_key_material.go` — PostgreSQL store
- `core/app/handlers_passphrase.go` — passphrase API handlers

### Modified (Stage 1)
- `core/app/handlers.go` — `vault` field type change; RunExecution encrypted credential handling
- `core/app/handlers_connected_accounts.go` — UserScopedVault in OAuth callback
- `core/app/app.go` — KEK cache wiring
- `core/account/service.go` — `WithVault()` method
- `core/config/auth.go` — `KEKSessionTTL()`
- `core/api/openapi.yaml` — passphrase endpoints
- `core/schema/schema.hcl` — `user_key_materials` table
- `core/store/store.go` — `UserKeyMaterialStore` interface

### Planned (Stage 2)
- `core/enclave/` — protocol types, client interface, attestation verification
- `cmd/aileron-enclave/` — enclave binary (handler, escrow, attestation)
- `ui/src/lib/crypto/` — client-side attestation, KEK derivation, ECDH
