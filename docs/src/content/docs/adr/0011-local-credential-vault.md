---
title: "ADR-0011: Local Credential Vault"
description: "Encrypted-at-rest local vault for user credentials. Argon2id KDF from a passphrase derives a key encryption key (KEK); AES-256-GCM envelope encryption protects bindings on disk. KEK held in memory for the runtime session, never persisted. The same vault format runs unchanged when the customer operates the runtime in their own infrastructure (BYOC)."
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
- **Portable:** the same envelope format works unchanged when the customer operates the runtime in their own infrastructure (BYOC).

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

### Passphrase prompt — CLI-driven four-state machine

Under [ADR-0012](/adr/0012-local-daemon-architecture)'s local daemon model, the daemon is fork-exec'd by the CLI without a controlling TTY (`stdin = nil`, stdout/stderr redirected to a log file). The daemon cannot prompt the user. The CLI does, before issuing any request that would land against a locked vault.

The CLI evaluates a four-state state machine before dispatching any vault-relevant command. The states cover the cross-product of *daemon running?* and *vault file present? unlocked?*:

| Daemon | Vault file | Vault unlocked? | CLI behavior |
| --- | --- | --- | --- |
| running | exists | yes | pass-through, no prompt |
| running | exists | no | prompt for passphrase, POST `/v1/vault/unlock` |
| stopped | exists | n/a | prompt for passphrase, auto-spawn daemon, POST `/v1/vault/unlock` |
| stopped | missing | n/a | prompt for new passphrase + confirmation, write vault file, auto-spawn daemon, POST `/v1/vault/unlock` |

User mental model: "if you have a vault, you'll be asked for the passphrase once per daemon lifetime; if you don't, you'll be asked to create one when you first need it." Matches `gpg-agent`, `ssh-agent`, and password-manager unlock flows.

The first-run case prints a banner before the prompt:

```
$ aileron binding setup github://aileron/slack

  Creating a new Aileron vault.

  The passphrase you choose protects all secrets in this vault.
  It is never stored, transmitted, or recoverable. No one can
  read it, tell you what it is, or help you retrieve it.

  If you lose this passphrase, you must delete the vault file
  (/home/alice/.aileron/secrets.json) and re-add all secrets.

  Store this passphrase securely. Do not share it.

Vault passphrase: ********
Confirm passphrase: ********
```

After the file is written, the CLI fork-execs the daemon (which starts vault-locked under `selectVault`'s no-TTY path), then drives `/v1/vault/unlock` so the daemon's `vault.LockableVault` swaps the unlocked inner vault into place. Subsequent CLI commands hit the running-and-unlocked branch and pass through with no prompt.

After unlock:

- The KEK is held in memory by the daemon process for its lifetime.
- Subsequent action invocations resolve credential bindings against the unlocked vault transparently.
- The KEK is **never written to disk**, never logged, never sent over the network, never visible in audit records (audit records contain binding identities, not credential bytes).
- `aileron daemon stop` exits the daemon process; the next CLI command hits the daemon-stopped branch and re-prompts.

A bypass allowlist short-circuits the state machine for commands that don't talk to the daemon (or don't need an unlocked vault to do their job): `version`, `help`, `init`, `log`, `vault init`, `secret set`, `secret list`, `daemon status/stop/start`, `policy test/save`, and `stop`. Those run without prompting.

The daemon refuses to start if no vault file exists at the canonical path; there is no in-memory dev fallback. The state machine creates the file on first run, so the only way to hit this error is to run `aileron daemon start` directly without first running `aileron vault init`. (For ephemeral CI / smoke-test scenarios that genuinely don't need a vault, a future `--no-vault` flag with explicit "actions cannot bind credentials" semantics may exist; not in MVP.)

#### Non-interactive sources

The state machine reads from `AILERON_VAULT_PASSPHRASE` and `--passphrase-file <path>` before falling back to the interactive `/dev/tty` prompt. CI pipelines and scripts pass the passphrase through one of these so they never block on input. Non-interactive sources also skip the confirmation step on first run — re-reading the same source would just confirm itself.

A wrong passphrase from interactive entry triggers a single retry; non-interactive sources fail immediately on `401`, since the source isn't going to change between attempts.

### KEK session lifetime: while the runtime is running

The KEK lives in memory from the moment the user unlocks the vault until the runtime process exits. There is no separate session timeout in v1.

This is the simpler model: a long-running `aileron launch` keeps the KEK in memory as long as it's running. The user's working session and the KEK's lifetime are the same thing. When the user exits Aileron (Ctrl-C, system shutdown), the KEK is gone and they must re-unlock at next start.

A more elaborate model — a separate session TTL (e.g., 30 minutes of inactivity) — exists in the cloud-shaped vault code (see "Implementation status" below) but is not exercised in v1 user-level mode. Adding TTL'd auto-lock to the local vault is a post-MVP enhancement if real usage shows the runtime stays running for longer than users want their credentials reachable.

### No passphrase recovery in v1

A forgotten passphrase means the vault contents are unrecoverable. The user must rotate every credential at the upstream service (Slack, Linear, Gmail, etc.) and re-bind from scratch.

This is the honest tradeoff for zero-knowledge encryption. Recovery codes, escrow, or any other recovery mechanism would either weaken the trust model (the vault becomes accessible to whoever holds the recovery secret) or duplicate the passphrase problem (the recovery code becomes the new thing to lose).

Recovery codes are a plausible post-MVP feature. v1 ships without them; the documentation will be explicit that passphrase loss = vault loss.

### The same vault format runs under BYOC

The same cryptographic primitives carry forward when the customer operates the runtime in their own environment (BYOC). The local-vault format runs unchanged. Aileron never holds the KEK or the plaintext because the runtime that decrypts is operated by the customer, not by Aileron.

The architectural commitment ratified here is the *direction*: the on-disk file format does not change across deployments. The difference is *who holds the KEK at decryption time* (local process memory in v1; a customer-operated runtime process under BYOC). In both cases the KEK lives only in the memory of a process the user controls.

## Implementation status

The cryptographic primitives and the local vault are implemented in code and wired into the v1 runtime.

**Local vault (v1 active):**

- `internal/crypto/kek.go` — Argon2id KDF (`DeriveKEK`, `GenerateSalt`)
- `internal/crypto/envelope.go` — AES-256-GCM envelope encryption
- `internal/vault/encrypted.go` — `EncryptedVault` decorator that wraps any vault implementation
- `internal/vault/file.go` — JSON file-backed vault at `~/.aileron/secrets.json`
- `internal/vault/lockable.go` — `LockableVault` wrapper. The daemon starts with an empty `LockableVault`; `/v1/vault/unlock` swaps the unlocked inner vault into it once the CLI drives the unlock.
- `internal/server/main.go::selectVault` — chooses between interactive-unlock-at-startup (operator-typed `aileron daemon start`) and start-vault-locked (CLI auto-spawn case). Refuses to start when the vault file is missing.
- `cmd/aileron/vault.go` — `aileron vault init` and the `readVaultPassphrase` helper that resolves passphrases from `AILERON_VAULT_PASSPHRASE` / `--passphrase-file` / interactive in that order.
- `cmd/aileron/vault_state.go` — the four-state CLI flow (`ensureVaultUnlocked`, `bypassesVault`, `postVaultUnlock`).
- `internal/auth/kek_session.go` — KEK session cache (used in cloud mode; v1 uses runtime-lifetime memory holding directly)
- Test coverage: `kek_test.go`, `envelope_test.go`, `encrypted_test.go`, `file_test.go`, `lockable_test.go`, `kek_session_test.go`, `vault_state_test.go`, plus the production-shape integration test in `internal/server/main_test.go::TestRun_NoTTYDaemonHonorsVaultUnlock`.

The v1 vault implementation uses these cryptographic primitives directly. Under BYOC the same primitives run unchanged on customer-operated infrastructure; the storage and prompt layers (file-on-disk, CLI prompt) are the only parts that vary by deployment, not the cryptography.

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
- Portability across deployments (BYOC, where the customer operates the runtime in their own environment) is harder when the canonical credential location is OS-specific.

A future hybrid where the OS keychain stores the passphrase (so the user doesn't re-type it) while Aileron continues to manage the encrypted vault file is plausible post-MVP.

### In-memory only, no encryption at rest (rejected)

The vault is in memory only. Each `aileron launch` requires the user to re-bind every credential.

Rejected because re-binding is high-friction. OAuth flows, API key copy-paste, multiple-account selection — doing all of that at every Aileron startup is unworkable. Encryption at rest is the right tradeoff.

### Symmetric key from a hardware token (rejected for MVP)

A YubiKey or similar hardware token holds the KEK directly; Aileron requests it via PIV / FIDO2 protocol.

Rejected for v1 because hardware tokens are not universally available in the wedge audience (developers using AI coding tools; many do not own a hardware token). It also adds significant design complexity for portability across deployments.

A `--hardware-token` mode is plausible post-MVP for users who want it.

### Different cipher (ChaCha20-Poly1305 instead of AES-256-GCM) (rejected — modest preference for AES)

ChaCha20-Poly1305 is a strong, modern AEAD with no dependence on AES hardware acceleration. On platforms without AES-NI (some embedded environments), ChaCha20 is faster.

AES-256-GCM was chosen because AES-NI is universal on the developer hardware Aileron targets. The performance difference on modern hardware is negligible.

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

### For BYOC (customer-operated runtime)

- The file format and crypto primitives carry forward unchanged. Running under BYOC means the customer operates the runtime that holds the KEK, not Aileron, so the *operator* of the decrypting process changes while the on-disk format does not.
- Aileron never holds the KEK or the plaintext in this model, because the runtime that decrypts is operated by the customer.

### Open implementation questions (deferred)

- *Should the local vault have an inactivity-based auto-lock (re-prompt for passphrase after N minutes idle)?* — Post-MVP. The cloud-shaped session-cache code (`internal/auth/kek_session.go`) supports this pattern; wiring it into the local CLI mode is straightforward when real usage justifies it.
- *Should Aileron offer recovery codes for passphrase loss?* — Post-MVP. v1 ships without; the documentation makes the no-recovery property explicit.
- *Should the local vault optionally back to the OS keychain for passphrase storage (zero-friction startup)?* — Post-MVP enhancement; the trade-off is that the OS keychain becomes the new weakest link.
- *When does the BYOC deployment path (customer-operated runtime) ship, and what triggers it?* — Paired with the hosted backend introduced in [ADR-0009](/adr/0009-user-channel); ratified separately when that work begins.

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
