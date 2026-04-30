---
title: "ADR-0011: Local Credential Vault"
description: "Encrypted-at-rest local vault for user credentials. Argon2id KDF from a passphrase derives a key encryption key (KEK); AES-256-GCM envelope encryption protects bindings on disk. KEK held in memory for the runtime session, never persisted. TEE and browser-enclave variants are post-MVP."
order: 11
---


<div class="meta">
<table>
  <tr><th>Status</th><td>Accepted</td></tr>
  <tr><th>Date</th><td>2026-04-29</td></tr>
  <tr><th>Tracking</th><td><a href="https://github.com/ALRubinger/aileron/issues/343">#343</a></td></tr>
</table>
</div>

## Context

[ADR-0006](/adr/0006-capability-binding-ux) ratifies that user credentials live in a local vault and that bindings are user-local artifacts. [ADR-0005](/adr/0005-sandbox-choice) ratifies that the runtime mediates credential use so connectors never hold raw credential bytes. Both ADRs reference "the vault" as the credential-holding boundary; neither specifies what the vault actually is.

This ADR fills that gap. The vault is a security primitive. Its design — encryption scheme, key derivation, on-disk format, when the user is prompted for the passphrase, what the runtime holds in memory and for how long — determines whether the credential-sealing claims of the surrounding ADRs are honest or just policy.

The threat model the vault must address:

- **Disk theft / unauthorized read.** A user's laptop is stolen, or a backup is exfiltrated. The attacker has full read access to the user's home directory. Credentials must be encrypted at rest such that the attacker cannot recover them without the passphrase.
- **Process inspection.** A malicious process running as the same user inspects Aileron's memory. The window during which raw credential bytes are reachable in memory should be as narrow as possible — not zero (impossible without hardware support), but bounded.
- **Operator / hosting compromise.** Not a v1 concern (Aileron runs locally, so there is no hosting operator), but the design should not foreclose future hosted-backend variants where this matters.

The user is the only authenticated principal in v1. There is no multi-user model, no team-shared vault, no federated credential store. The vault belongs to the local user; it lives in the local user's home directory; it is unlocked when the local user is at the keyboard.

## Decision

### The v1 vault is a local encrypted-at-rest file with passphrase-derived encryption

The vault stores user credentials (OAuth tokens, API keys, refresh tokens, signing keys) encrypted on disk. The encryption key is **derived from the user's passphrase**, never persisted to disk, and held in memory only while the Aileron runtime process is running.

The cryptographic chain:

```
User passphrase
  │
  │  Argon2id (salt stored in vault file)
  │
  ▼
Key Encryption Key (KEK)  ──── never persisted
  │
  │  AES-256-GCM
  │
  ▼
Encrypted credentials  ──── stored in ~/.aileron/secrets.json
```

The salt is stored alongside the ciphertext (it is not secret; its purpose is to defeat rainbow-table attacks against the KDF). The KEK is the secret; it exists only in memory.

### Argon2id key derivation

The KEK is derived from the user's passphrase via [Argon2id](https://datatracker.ietf.org/doc/html/rfc9106), the modern password-hashing standard.

Parameters:

| Parameter | Value | Rationale |
|---|---|---|
| Time cost (iterations) | 3 | Strong default per RFC 9106 |
| Memory cost | 64 MiB | High enough to be expensive for GPU attackers; low enough to run on developer hardware |
| Parallelism (threads) | 4 | Standard parallelism level |
| Output length | 32 bytes | 256-bit KEK |
| Salt | 16 bytes, random | Unique per vault file |

These parameters are tunable for testing but fixed in production builds. Slower-on-purpose; the cost is paid once at unlock time.

### AES-256-GCM envelope encryption

Each credential value is encrypted under the KEK using AES-256-GCM, an authenticated encryption mode (RFC 5116). A 96-bit random nonce is generated per-encryption and prepended to the ciphertext. The authentication tag protects against tampering — modifying the ciphertext invalidates decryption.

GCM was chosen over alternatives (AES-CBC, ChaCha20-Poly1305) because:

- **Authenticated:** unlike CBC, decryption fails on tampering rather than producing garbage.
- **Standard:** widely audited, hardware-accelerated on every modern CPU, and supported by every cryptographic library Aileron might ever embed.
- **Aligned with future stages:** the same envelope format works inside a TEE (Stage 2) without changes.

### On-disk vault format

The vault file lives at `~/.aileron/secrets.json`. Format:

```json
{
  "version": 1,
  "salt": "<base64-encoded 16-byte salt>",
  "verification": "<base64-encoded ciphertext of a known constant>",
  "secrets": {
    "oauth2/slack/work": {
      "metadata": {
        "kind": "oauth2",
        "scope": "chat:write,channels:read",
        "encrypted": "true"
      },
      "ciphertext": "<base64-encoded AES-256-GCM(KEK, value)>"
    },
    "api_key/linear/team": {
      "metadata": { "kind": "api_key", "encrypted": "true" },
      "ciphertext": "<base64-encoded AES-256-GCM(KEK, value)>"
    }
  }
}
```

Notable:

- **Salt is per-vault, not per-secret.** Argon2id is expensive; one derivation at unlock time, then the same KEK encrypts all secrets.
- **Verification blob.** A known constant (e.g., `"aileron-kek-verification-ok"`) is encrypted under the KEK and stored. On unlock, the runtime re-derives the KEK from the supplied passphrase, attempts to decrypt the verification blob, and confirms the plaintext matches the constant. This catches wrong-passphrase entry without ever storing the KEK.
- **Metadata is unencrypted.** The kind, scope, and identity of a binding are plaintext (so Aileron can list them via `aileron binding list` without a passphrase). The credential value is the only encrypted field.
- **JSON for the file format.** Per [ADR-0001](/adr/0001-manifest-format), JSON is the right choice for runtime-internal state (machine-authored, machine-read, never edited by hand).

### Passphrase prompt at runtime startup

When the Aileron runtime starts and detects an existing vault file (a passphrase is already set), it prompts the user via the CLI for their passphrase before serving any chat completion requests:

```
$ aileron launch
Vault is encrypted. Enter passphrase to unlock:
> ********

✓ Vault unlocked. Listening on http://localhost:8721/v1
```

The prompt uses the v1 user-channel surface (CLI per [ADR-0009](/adr/0009-user-channel)). After unlock:

- The KEK is held in memory by the runtime process.
- Subsequent chat completions and action invocations resolve credential bindings against the unlocked vault transparently.
- The KEK is **never written to disk**, never logged, never sent over the network, never visible in audit records (audit records contain binding identities, not credential bytes).

If the user's first interaction with Aileron creates the vault (no existing file), a one-time setup flow runs:

```
$ aileron launch
No vault detected. Create one now? [Y/n] Y
Choose a passphrase to protect your credentials:
> ********
Confirm passphrase:
> ********

✓ Vault created at ~/.aileron/secrets.json
✓ Listening on http://localhost:8721/v1
```

The runtime refuses to start if the user declines to create a vault; there is no plaintext fallback. (For ephemeral CI / smoke-test scenarios that genuinely don't need a vault, a future `--no-vault` flag with explicit "actions cannot bind credentials" semantics may exist; not in MVP.)

### KEK session lifetime: while the runtime is running

The KEK lives in memory from the moment the user unlocks the vault until the runtime process exits. There is no separate session timeout in v1.

This is the simpler model: a long-running `aileron launch` keeps the KEK in memory as long as it's running. The user's working session and the KEK's lifetime are the same thing. When the user exits Aileron (Ctrl-C, system shutdown), the KEK is gone and they must re-unlock at next start.

A more elaborate model — a separate session TTL (e.g., 30 minutes of inactivity) — exists in the cloud-shaped vault code (see "Implementation status" below) but is not exercised in v1 user-level mode. Adding TTL'd auto-lock to the local vault is a post-MVP enhancement if real usage shows the runtime stays running for longer than users want their credentials reachable.

### No passphrase recovery in v1

A forgotten passphrase means the vault contents are unrecoverable. The user must rotate every credential at the upstream service (Slack, Linear, Gmail, etc.) and re-bind from scratch.

This is the honest tradeoff for zero-knowledge encryption. Recovery codes, escrow, or any other recovery mechanism would either weaken the trust model (the vault becomes accessible to whoever holds the recovery secret) or duplicate the passphrase problem (the recovery code becomes the new thing to lose).

Recovery codes are a plausible post-MVP feature. v1 ships without them; the documentation will be explicit that passphrase loss = vault loss.

### Stage 2 (TEE) and Stage 3 (browser enclave) are post-MVP

The same cryptographic primitives extend naturally to two future variants:

- **Stage 2 — TEE-backed vault.** Credentials decrypt only inside a Trusted Execution Environment (e.g., Google Confidential Space). The KEK is transmitted to an attested enclave, never visible to the host. This enables Aileron Cloud (the hosted backend introduced in [ADR-0009](/adr/0009-user-channel) Phase 2) and async / scheduled actions where the runtime needs credential access while the user is offline.
- **Stage 3 — Browser enclave / client-side custody.** The user's browser holds the KEK. Aileron sends an encrypted credential request to the browser; the browser decrypts and forwards the API call. True zero-knowledge for hosted Aileron — even Aileron operators cannot access plaintext.

Both variants are deferred from v1. The architectural commitment ratified here is the *direction*: the local vault format is forward-compatible with TEE-backed and browser-enclave-backed variants. The on-disk file format does not change; the difference is *who holds the KEK at decryption time* (local process memory in v1; enclave memory in Stage 2; browser memory in Stage 3).

## Implementation status

The cryptographic primitives and all three stages of the broader design are already implemented in code, though only Stage 1 (the local vault) is wired into the v1 runtime. Stages 2 and 3 exist as ready-to-deploy infrastructure for the hosted backend and are under security review.

**Stage 1 — Local vault (v1 active):**

- `internal/crypto/kek.go` — Argon2id KDF (`DeriveKEK`, `GenerateSalt`)
- `internal/crypto/envelope.go` — AES-256-GCM envelope encryption
- `internal/vault/encrypted.go` — `EncryptedVault` decorator that wraps any vault implementation
- `internal/vault/file.go` — JSON file-backed vault at `~/.aileron/secrets.json`
- `internal/auth/kek_session.go` — KEK session cache (used in cloud mode; v1 uses runtime-lifetime memory holding directly)
- Test coverage: `kek_test.go`, `envelope_test.go`, `encrypted_test.go`, `file_test.go`, `kek_session_test.go`

**Stage 2 — TEE-backed vault (post-MVP, security review in progress):**

- `internal/enclave/` — protocol types, client SPI, attestation verifier interface
- `internal/enclave/local/` — local TEE provider for development (mock attestation)
- `internal/enclave/gcs/` — Google Confidential Space provider with OIDC JWT attestation
- `cmd/aileron-enclave/` — enclave binary (handler, OAuth exchange, escrow, attestation)
- Capability system (`capability.go`, `escrow_key.go`, `request_auth.go`) for fine-grained action authorization within enclave
- Test coverage present across all enclave components

**Stage 3 — Browser enclave / client-side crypto (post-MVP, security review in progress):**

- `ui/src/lib/crypto/argon2.ts` — Browser-side Argon2id (matches server parameters exactly)
- `ui/src/lib/crypto/envelope.ts` — Browser-side AES-256-GCM (matches server format exactly)
- `ui/src/lib/crypto/ecdh.ts` — P-256 ECDH for session establishment with enclave
- `ui/src/lib/crypto/attestation.ts` — Confidential Space OIDC JWT verification client-side
- Test coverage in `vault.test.ts` and `attestation.test.ts`

The cloud-shaped components from the original design — PostgreSQL `user_key_materials` store, HTTP passphrase handlers (`internal/app/handlers_passphrase.go`), `UserScopedVault` decorator with multi-user semantics — exist in the codebase but are dormant in v1 user-level mode. They are wired only when the hosted backend deploys.

The v1 vault implementation reuses the Stage 1 cryptographic primitives directly. The adaptation from the cloud-shaped design is in the storage and prompt layers (file-on-disk replaces database-row; CLI prompt replaces HTTP form), not in the cryptography.

## Alternatives Considered

### Plaintext vault with OS file permissions only (rejected)

The vault is a plaintext file at `~/.aileron/secrets.json` with `chmod 0600`. The user's OS account isolation provides the security boundary.

Rejected because OS-level file permissions are not a strong-enough boundary for credentials of this sensitivity. A backup of the user's home directory, an unencrypted disk image, a stolen laptop without full-disk encryption, or any process running as the same user can read the file. A vault that leaks under any of these scenarios fails the threat model.

The cost of encryption at rest is one Argon2id derivation at startup (~500ms on modern hardware) and a passphrase prompt. That cost is paid once per `aileron launch`; it is small enough that the security gain is unambiguous.

### OS keychain integration (rejected for MVP)

Credentials live in the OS keychain (macOS Keychain, Windows Credential Manager, Linux Secret Service / GNOME Keyring). Aileron never holds them on disk in any format.

Rejected for v1 not because it's a bad idea — it's actually a strong option — but because:

- Cross-platform implementation is non-trivial. Each OS keychain has its own API, idioms, and limits. The pure-Go cross-platform story for keychain integration is patchy.
- The OS keychain doesn't help with the "one passphrase unlocks many credentials" UX. The user would either have to authenticate per-credential (unbearable) or grant Aileron broad keychain access (defeating much of the security benefit).
- Forward-compatibility with Stages 2 and 3 (TEE and browser enclave) is harder when the canonical credential location is OS-specific.

A future hybrid where the OS keychain stores the passphrase (so the user doesn't re-type it) while Aileron continues to manage the encrypted vault file is plausible post-MVP.

### In-memory only, no encryption at rest (rejected)

The vault is in memory only. Each `aileron launch` requires the user to re-bind every credential.

Rejected because re-binding is high-friction. OAuth flows, API key copy-paste, multiple-account selection — doing all of that at every Aileron startup is unworkable. Encryption at rest is the right tradeoff.

### Symmetric key from a hardware token (rejected for MVP)

A YubiKey or similar hardware token holds the KEK directly; Aileron requests it via PIV / FIDO2 protocol.

Rejected for v1 because hardware tokens are not universally available in the wedge audience (developers using AI coding tools; many do not own a hardware token). It's also not forward-compatible with the TEE story without significant additional design.

A `--hardware-token` mode is plausible post-MVP for users who want it.

### Different cipher (ChaCha20-Poly1305 instead of AES-256-GCM) (rejected — modest preference for AES)

ChaCha20-Poly1305 is a strong, modern AEAD with no dependence on AES hardware acceleration. On platforms without AES-NI (some embedded environments), ChaCha20 is faster.

Not chosen primarily for forward compatibility: the TEE provider (Google Confidential Space) and the browser-enclave story both align more naturally with AES-256-GCM. AES-NI is universal on the developer hardware Aileron targets. The performance difference on modern hardware is negligible.

ChaCha20-Poly1305 remains a credible alternative if a future Aileron deployment runs on AES-NI-less hardware. The decorator pattern in the existing code makes the cipher swappable.

## Consequences

### For users

- Every Aileron startup prompts for the vault passphrase. The KEK lives only in the running process; the vault is locked when Aileron isn't running.
- Choosing a strong passphrase matters. Argon2id makes brute-forcing slow but not impossible against weak passphrases.
- A forgotten passphrase means re-binding every credential at the upstream services. There is no recovery in v1.
- Users who want zero-friction startup can pair Aileron with their OS keychain manually (export the passphrase to an env var that Aileron reads, with the trade-off that the env var is now the weakest link).

### For action and connector authors

- No change. The vault is invisible to actions and connectors; they receive resolved credential capability handles per [ADR-0005](/adr/0005-sandbox-choice). The encryption layer is between the runtime and disk, not between the runtime and connectors.

### For Aileron runtime

- The runtime embeds the `internal/crypto/` and `internal/vault/` packages. Wazero (the WASM engine) is the only larger external dependency; the vault adds Argon2id (`golang.org/x/crypto/argon2`) and uses the standard library's `crypto/aes` and `crypto/cipher`.
- Vault unlock happens at startup, before the chat completion endpoint accepts requests. A locked vault means the runtime is not yet serving.
- The KEK is held in a single in-memory location, zeroed on process exit (best-effort; Go's garbage collector limits how strict this can be).

### For audit and security

- The audit log (per [ADR-0010](/adr/0010-failure-handling)) never records credential bytes. Binding identities (`oauth2/slack/work`) and capability use (which scope was exercised) are recorded; the credential value itself is not.
- Vault-unlock events are auditable: each successful unlock records timestamp, source (CLI passphrase entry), and outcome. Failed unlock attempts are also recorded (without the wrong passphrase, of course).
- The runtime refuses to start with a corrupted or tampered vault file. The verification blob's authentication tag catches modification; on failure, the runtime exits with a clear error rather than fall back to a plaintext mode.

### For Phase 2 (hosted backend)

- The Stage 1 file format and crypto primitives are forward-compatible. Upgrading to Stage 2 (TEE-backed) means changing *who holds the KEK*, not the on-disk format.
- The KEK transmission protocol (client-side derivation → ECDH session → attested enclave) is implemented (see Implementation status); enabling it for production is a security-review and deployment concern, not a fresh design effort.

### Open implementation questions (deferred)

- *Should the local vault have an inactivity-based auto-lock (re-prompt for passphrase after N minutes idle)?* — Post-MVP. The cloud-shaped session-cache code (`internal/auth/kek_session.go`) supports this pattern; wiring it into the local CLI mode is straightforward when real usage justifies it.
- *Should Aileron offer recovery codes for passphrase loss?* — Post-MVP. v1 ships without; the documentation makes the no-recovery property explicit.
- *Should the local vault optionally back to the OS keychain for passphrase storage (zero-friction startup)?* — Post-MVP enhancement; the trade-off is that the OS keychain becomes the new weakest link.
- *When does Stage 2 (TEE-backed) ship, and what triggers the deployment?* — Paired with the hosted backend introduced in [ADR-0009](/adr/0009-user-channel); ratified separately when Phase 2 begins.
- *When does Stage 3 (browser enclave) ship?* — Even later; depends on a hosted Aileron Cloud deployment.

## Examples

### First run: vault setup

```
$ aileron launch
No vault detected. Create one now? [Y/n] Y
Choose a passphrase to protect your credentials:
> ********
Confirm passphrase:
> ********

Deriving encryption key (Argon2id, 64 MiB memory, ~500ms)...  ✓
✓ Vault created at ~/.aileron/secrets.json
✓ Listening on http://localhost:8721/v1
```

### Subsequent run: vault unlock

```
$ aileron launch
Vault is encrypted. Enter passphrase to unlock:
> ********

Verifying passphrase...  ✓
✓ Vault unlocked
✓ Listening on http://localhost:8721/v1
```

### Wrong passphrase

```
$ aileron launch
Vault is encrypted. Enter passphrase to unlock:
> ********

Verifying passphrase...  ✗

ERROR: incorrect passphrase
The vault could not be unlocked. Try again:

> ********

Verifying passphrase...  ✓
✓ Vault unlocked
✓ Listening on http://localhost:8721/v1
```

### Tampered vault

```
$ aileron launch
Vault is encrypted. Enter passphrase to unlock:
> ********

Verifying passphrase...  ✗

ERROR: vault verification failed
The vault file at ~/.aileron/secrets.json appears to have been
modified or corrupted (authentication tag mismatch). This may
indicate disk corruption or tampering.

Aileron will not start with a tampered vault. Please restore
~/.aileron/secrets.json from a known-good backup or rotate
your credentials at upstream services and re-create the vault.
```
