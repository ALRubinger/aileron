---
title: "ADR-0020: System Vault for Infrastructure Secrets"
---


<div class="meta">
<table>
  <tr><th>Status</th><td>Proposed</td></tr>
  <tr><th>Date</th><td>2026-04-21</td></tr>
  <tr><th>References</th><td><a href="/adr/0010-zero-knowledge-vault-trust-model">ADR-0010</a></td></tr>
</table>
</div>

## Context

ADR-0010 established a zero-knowledge vault for user credentials. Users derive a Key Encryption Key (KEK) from a passphrase; the KEK encrypts their OAuth tokens and API keys; Aileron cannot read those secrets without the user's participation. This is the correct model for user secrets — it is the trust foundation of the product.

However, Aileron now needs to store secrets that do not belong to any user.

The immediate driver is Slack bot tokens. When a workspace admin installs the Aileron Slack app from the Slack App Directory, the installation produces a bot token (`xoxb-...`) scoped to that workspace. Aileron needs this token to call Slack APIs — posting agent responses, setting thinking indicators, streaming messages. The bot token:

- Has no associated Aileron user. The workspace admin who installs the app may not have an Aileron account.
- Must be readable without human intervention. When a Slack event arrives at the webhook, the server must immediately retrieve the bot token and respond. There is no passphrase prompt, no session unlock, no user in the loop.
- Is scoped to a workspace, not a user. Multiple Aileron users in the same workspace share one bot token.
- Will not be the only secret of this kind. Future integrations (Discord bots, webhook signing keys, service account credentials for scheduled actions) will follow the same pattern: infrastructure secrets that the server must access autonomously.

The zero-knowledge user vault cannot store these secrets. Zero-knowledge means the server cannot read secrets without a user-derived KEK. Infrastructure secrets must be readable by the server at any time. These are fundamentally different access patterns.

### Options considered

1. **Store infrastructure secrets in the user vault under a service account.** Create a synthetic "system user" with a known passphrase. The server derives the KEK at startup and caches it. This preserves a single vault but undermines the zero-knowledge claim — a known passphrase is not zero-knowledge. It also creates a confused ownership model where workspace-level secrets are tied to a fake user.

2. **Store infrastructure secrets as environment variables.** Simple, no new infrastructure. But env vars do not scale to per-workspace secrets. A deployment serving 100 Slack workspaces would need 100 env vars. Env vars also lack metadata, audit trails, and lifecycle management.

3. **Store infrastructure secrets in a plain database column.** Direct and queryable, but secrets would be stored in cleartext (or with application-level encryption that amounts to a key in an env var). No separation of concerns between the secret store and the application database.

4. **Introduce a system vault — a separate vault for infrastructure secrets, encrypted at rest with a server-managed key, implementing the same `vault.Vault` SPI.** Clear separation from the user vault. Encrypted at rest. Scales to any number of secrets. Same interface, different trust model.

## Decision

Aileron introduces a **system vault** for infrastructure secrets, implementing the existing `vault.Vault` SPI with a server-managed encryption key. The system vault is encrypted at rest but is **not** zero-knowledge — the server can read secrets autonomously.

### Two vaults, two trust models

| | User Vault (ADR-0010) | System Vault |
|---|---|---|
| **Purpose** | User's personal credentials | Infrastructure secrets |
| **Examples** | Slack user token (`xoxp-`), Gmail OAuth token | Slack bot token (`xoxb-`), webhook signing keys |
| **Encryption** | User's KEK (Argon2id from passphrase) | Server-managed key (AES-256-GCM) |
| **Zero-knowledge** | Yes — server cannot read without user | No — server reads autonomously |
| **Key source** | User's passphrase (never stored) | `AILERON_SYSTEM_VAULT_KEY` env var |
| **Access pattern** | User unlocks session, server caches KEK temporarily | Server decrypts at any time |
| **Ownership** | Per-user, scoped by user ID | Per-resource (e.g. per-workspace), no user association |

### What belongs where

**System vault:**
- Slack bot tokens (per workspace)
- Discord bot tokens (per server, future)
- Webhook signing keys for third-party integrations
- Service account credentials for scheduled/autonomous actions
- Any secret the server must access without a user session

**User vault (unchanged):**
- OAuth user tokens (Slack `xoxp-`, Gmail, Google Calendar, etc.)
- Personal API keys
- Any secret that should be inaccessible to Aileron operators

The rule: if a human must approve access, it belongs in the user vault. If the server must act autonomously, it belongs in the system vault.

### Encryption model

```
AILERON_SYSTEM_VAULT_KEY (env var, 32 bytes)
  |
  |  AES-256-GCM (random 96-bit nonce per secret)
  |
  v
Encrypted system vault secrets (stored in database)
  |
  |  Decrypted by server process at runtime
  |
  v
Infrastructure operation (Slack API call, webhook verification, etc.)
```

The system vault reuses the `core/crypto/envelope.go` primitives (AES-256-GCM with random nonce) already built for the user vault. The only difference is the key source: an env var instead of a user-derived KEK.

### Storage

A new PostgreSQL table, separate from user vault storage:

```sql
CREATE TABLE system_vault_secrets (
    path        TEXT PRIMARY KEY,
    value       BYTEA NOT NULL,
    metadata    JSONB NOT NULL DEFAULT '{}',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

Path conventions:
- `slack-workspaces/{team_id}/bot-token` — Slack bot token for a workspace
- `discord-servers/{guild_id}/bot-token` — Discord bot token (future)

### Interface

The system vault is not a new type — it reuses the same building blocks as the user vault:

```go
// PostgresVault is parameterized by table name. Both vaults use the same code.
userStore  := vault.NewPostgresVault(pool)                            // → vault_secrets
sysStore   := vault.NewPostgresVaultForTable(pool, "system_vault_secrets") // → system_vault_secrets

// Both are wrapped with EncryptedVault — same decorator, different keys.
userVault, _ := vault.NewEncryptedVault(userStore, userKEK)     // user-derived KEK
sysVault, _  := vault.NewEncryptedVault(sysStore, systemKey)    // server-managed key
```

A convenience constructor `NewPostgresSystemVault(pool)` wraps the table name detail.

The `apiServer` struct gains a `systemVault vault.Vault` field alongside the existing `vault vault.Vault` (user vault). Code that needs infrastructure secrets reads from `s.systemVault`; code that needs user secrets reads from `s.vault` (or `s.userVault(userID)` for KEK-scoped access). Both fields have the same type — `vault.Vault` — so all existing code that accepts the interface works with either.

### Access control

**Writes:**
- Unauthenticated OAuth callbacks (e.g. Slack app installation redirect — the admin may not have an Aileron account)
- Future: Aileron admin API for manual secret management

**Reads:**
- Server process — webhook handlers, agent response handlers, scheduled tasks
- No user authentication required for reads (the server reads on its own behalf)

**Management (future):**
- Aileron admin role can list, inspect metadata, and revoke infrastructure secrets
- Admin operations do not require the system vault key (metadata is unencrypted); they require an Aileron admin session

### Threat model

| Threat | User Vault (ADR-0010) | System Vault |
|--------|----------------------|--------------|
| Database compromise | Secrets protected (user KEK required) | Secrets protected (system key required) |
| Host memory dump | KEK in memory only during active session | System key in memory for process lifetime |
| Aileron operator | Cannot read (zero-knowledge) | Cannot read at rest; could read if they obtain the system key |
| Hosting provider | Cannot read at rest | Cannot read at rest; could inspect process memory |
| Compromised server binary | KEK visible during session; attestation mitigates in TEE | System key and decrypted secrets visible in process memory |

The system vault provides **encryption at rest** and **key separation** (the system key is not in the database), but it does not provide zero-knowledge guarantees. An attacker who obtains both the database and the system key can decrypt infrastructure secrets. This is an acceptable trade-off: infrastructure secrets must be server-readable by design. The mitigation is operational — protect the system key through env var management, secret managers (AWS Secrets Manager, GCP Secret Manager), or KMS in production.

### Key management

The `AILERON_SYSTEM_VAULT_KEY` env var holds a 32-byte key, hex-encoded (64 characters). In production, this key should come from a secret manager or KMS rather than a static env var.

Key rotation: when the key changes, existing secrets must be re-encrypted. This is a batch operation — decrypt with the old key, re-encrypt with the new key. The rotation mechanism is deferred to a future issue but the design supports it: secrets are individually encrypted with their own nonce, so rotation is per-secret, not all-or-nothing.

## Consequences

- The `vault.Vault` SPI is unchanged. The system vault reuses `PostgresVault` (parameterized by table name) and `EncryptedVault` (same decorator, different key). No new types implement the interface — the difference is configuration, not code.
- `apiServer` gains a `systemVault vault.Vault` field. Handlers choose which vault to use based on whether the secret is user-scoped or infrastructure-scoped.
- New database table: `system_vault_secrets`. Separate from the existing vault storage, with its own lifecycle.
- New env var: `AILERON_SYSTEM_VAULT_KEY`. Required when the system vault is backed by PostgreSQL. Not required for in-memory (test) mode.
- The zero-knowledge claim (ADR-0010) is unaffected. User secrets remain KEK-encrypted. The system vault stores a different class of secrets with different access requirements.
- Infrastructure secrets are a new category in Aileron's security model. Code reviews should verify that user secrets never end up in the system vault and infrastructure secrets never end up in the user vault.
- Future integrations (Discord bots, scheduled action credentials, webhook signing keys) follow the same pattern: store in the system vault, read autonomously.

## Relationship to other ADRs

- **ADR-0010 (Zero-Knowledge Vault):** Referenced, not superseded. ADR-0010 governs user secrets. ADR-0020 governs infrastructure secrets. Both use the same `PostgresVault` implementation and `EncryptedVault` decorator — the difference is the key source (user-derived KEK vs. server-managed key) and the database table (`vault_secrets` vs. `system_vault_secrets`).
- **ADR-0009 (Execution Plane):** Unchanged. The execution plane owns all write operations. Infrastructure secrets (bot tokens) are used by the execution plane to send messages on behalf of users.
- **ADR-0012 (Auto-Escrow):** Unaffected. Auto-escrow applies to user KEKs inside the TEE. The system vault key is a separate concern.

## Files

### New
- `core/vault/system.go` — `NewPostgresSystemVault` convenience constructor
- `core/vault/system_test.go` — unit tests

### Modified
- `core/vault/postgres.go` — `NewPostgresVaultForTable` constructor, parameterized table name
- `core/app/handlers.go` — add `systemVault vault.Vault` field to `apiServer`
- `core/app/app.go` — wire `EncryptedVault(NewPostgresSystemVault(pool), sysKey)` from config
- `core/config/auth.go` — `SystemVaultKey` field, `SystemVaultEnabled()` method
- `core/schema/schema.hcl` — `system_vault_secrets` table definition
