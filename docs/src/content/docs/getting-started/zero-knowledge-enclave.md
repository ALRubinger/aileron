---
title: "Zero-Knowledge Enclave"
description: "Hardware-isolated credential execution — Aileron provably cannot access your data"
---

The zero-knowledge enclave is a hardware-isolated service that handles all credential operations: OAuth token exchange, API calls to Gmail/Calendar/Slack/GitHub, and connector execution. Your encryption key (KEK) and plaintext tokens only exist inside the enclave's AMD SEV-SNP protected memory — the Aileron server never sees them.

## What it gives you

Without the enclave (Phase 2), the server briefly holds your KEK in process memory during active sessions. A compromised server could theoretically extract it.

With the enclave (Phase 3), the server only passes opaque encrypted blobs. Even with full root access to the server, an attacker cannot read enclave memory — it's encrypted at the hardware level.

| Threat | Without enclave | With enclave |
|--------|----------------|--------------|
| Database breach | Protected (ciphertext) | Protected |
| Network sniffing | Protected (TLS) | Protected |
| Compromised server | Exposed | **Protected** |
| Malicious operator | Exposed | **Protected** |
| Host memory dump | Exposed | **Protected** (AMD SEV-SNP) |

## What changes for you as a user

Nothing visible. The unlock flow is the same — enter your passphrase, and your vault unlocks. Under the hood, your KEK is transmitted via an end-to-end encrypted channel (ECDH) to the enclave instead of being cached in server memory. The enclave proves its identity via hardware attestation before receiving your key.

## How it works

```
Browser
  │
  ├─ 1. Derive KEK from passphrase (Argon2id, client-side)
  ├─ 2. Verify passphrase locally (decrypt verification blob)
  ├─ 3. Request attestation from enclave
  ├─ 4. Verify enclave's identity:
  │     a. Validate JWT claims (issuer, expiry)
  │     b. Fetch JWKS from /v1/tee/jwks (server-proxied)
  │     c. Verify JWT signature via Web Crypto
  │     d. Pin image digest and project ID from /v1/tee/status
  ├─ 5. ECDH key exchange (establish encrypted channel)
  ├─ 6. Encrypt KEK with shared secret, send to enclave
  │
  ▼
Aileron Server (Railway)
  │  Passes opaque blobs — cannot decrypt
  │  Proxies Google JWKS for client verification
  ▼
Enclave (GCP Confidential Space)
  ├─ Receives KEK via encrypted channel
  ├─ Stores KEK in hardware-isolated memory
  ├─ Decrypts OAuth tokens when needed
  ├─ Makes API calls (Gmail, Calendar, Slack)
  └─ Returns only structured results (no raw tokens)
```

## Verifying it's active

Check the TEE status endpoint:

```sh
curl -H "Authorization: Bearer $TOKEN" \
  https://api.yourdomain.com/v1/tee/status
```

Expected response:

```json
{
  "enabled": true,
  "provider": "confidential-space",
  "attested": false,
  "session_active": false,
  "expected_identity": {
    "image_digest": "sha256:...",
    "project_id": "your-gcp-project"
  }
}
```

After unlocking your vault, `attested` and `session_active` become `true`.

## Credential escrow (offline execution)

When you unlock your vault, the server automatically escrows your connected account credentials into the enclave. This allows Aileron to process incoming Slack messages and generate drafts even when you're offline — the enclave has the credentials it needs without requiring an active browser session.

Escrowed credentials are **durable** — they survive enclave and server restarts. The enclave persists them to disk encrypted with AES-256-GCM, so a routine restart or redeployment doesn't require users to re-unlock.

Escrowed credentials expire after a configurable TTL (default 7 days, set via `AILERON_ESCROW_TTL`). After expiry, you must unlock again for Aileron to access your connected accounts.

## Deployment

The enclave runs as a separate service on GCP Confidential Space. See the [TEE Enclave deployment guide](/deployment/tee-enclave) for full provisioning instructions.

For local development, set `AILERON_TEE_PROVIDER=local` on the server. This uses the same ECDH protocol but without hardware isolation — useful for testing the attestation and session flow end-to-end.

## Troubleshooting

| Symptom | Likely cause |
|---------|-------------|
| "TEE is not enabled" | `AILERON_TEE_PROVIDER` not set on the server |
| Attestation fails | Image digest mismatch — enclave was updated but `AILERON_ENCLAVE_IMAGE_DIGEST` wasn't. Update the digest and restart. |
| Session expired | ECDH session has a 30-minute TTL. The client re-attests automatically on next vault operation. |
| "Enclave unreachable" | Network connectivity between Railway and GCP. Check firewall rules and `AILERON_ENCLAVE_URL`. |
| Escrowed credentials expired | User hasn't unlocked vault in >7 days. Unlock to re-escrow. |
| Escrow lost after restart | `AILERON_ENCLAVE_DATA_DIR` points to an ephemeral disk. Attach persistent storage or set the var to a durable mount. See [TEE deployment guide](/deployment/tee-enclave#persistent-escrow-storage). |
| 403 on direct unlock | Expected — when TEE is enabled, direct unlock is blocked. The UI uses the TEE path automatically. |
