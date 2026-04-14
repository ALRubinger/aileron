---
title: "ADR-0011: TEE Provider SPI and Google Confidential Space"
---


<div class="meta">
<table>
  <tr><th>Status</th><td>Accepted</td></tr>
  <tr><th>Date</th><td>2026-04-06</td></tr>
  <tr><th>Supplements</th><td><a href="/adr/0010-zero-knowledge-vault-trust-model">ADR-0010</a></td></tr>
</table>
</div>

## Context

ADR-0010 established the zero-knowledge vault trust model with a two-stage approach: encrypted-at-rest (Stage 1, implemented) and confidential computing (Stage 2, planned). Stage 2 assumed AWS Nitro Enclaves as the TEE provider — vsock communication, COSE Sign1 attestation documents, and PCR-based code identity verification.

During implementation of Stage 2 (Phases 4-6, [#52](https://github.com/ALRubinger/aileron/issues/52)), several constraints emerged:

1. **Vendor lock-in.** Nitro Enclaves are AWS-only, requiring EC2 instances with enclave-capable instance types. Aileron currently deploys on Railway (GCP). A Nitro-first approach would force a hosting migration before TEE could be used.

2. **Specialized communication.** Nitro uses vsock (virtual sockets), a non-standard protocol that requires bespoke client code. Other TEE providers (AMD SEV-SNP, Intel TDX, Google Confidential Space) use standard networking.

3. **Multiple viable providers.** The confidential computing landscape has matured. AMD SEV-SNP is available on Azure, GCP, and bare metal. Google Confidential Space wraps SEV-SNP with a container runtime and OIDC-based attestation. Intel TDX is available on Azure and GCP. Each has different attestation models.

4. **SPI consistency.** Aileron uses SPIs (Service Provider Interfaces) for every major subsystem — connectors, policy engine, vault, stores, auth providers. The TEE layer should follow the same pattern.

### Options Considered

1. **Nitro-only (per ADR-0010 Stage 2).** Build vsock client, COSE Sign1 verification, PCR validation. Requires EC2 migration. Cannot test without Nitro hardware.

2. **SPI with Nitro as first provider.** Abstract the TEE behind interfaces, implement Nitro first. Still requires EC2 migration for production use.

3. **SPI with Google Confidential Space as first provider.** Abstract the TEE behind interfaces, implement Confidential Space first. Runs on GCP (where Railway deploys). Uses standard HTTPS and OIDC attestation — simpler integration. Add Nitro or SEV-SNP later as additional providers.

4. **SPI with AMD SEV-SNP as first provider.** Multi-cloud (Azure, GCP, bare metal), but requires more infrastructure setup than Confidential Space. No managed attestation service.

## Decision

Aileron adopts **Option 3: SPI with Google Confidential Space as first provider.** The TEE layer uses a provider-agnostic SPI with two initial implementations: `local` (dev/test, no hardware isolation) and `confidential-space` (Google Confidential Space in production).

### TEE SPI Design

Three interfaces define the TEE abstraction, mirroring the connector SPI pattern:

**`enclave.Client`** — how the host communicates with the TEE:

```go
type Client interface {
    Attest(ctx, AttestationRequest) (AttestationResponse, error)
    EstablishSession(ctx, SessionRequest) (SessionResponse, error)
    Execute(ctx, ExecuteRequest) (ExecuteResponse, error)
    EscrowStore(ctx, EscrowStoreRequest) (EscrowStoreResponse, error)
    EscrowRevoke(ctx, EscrowRevokeRequest) error
    Close() error
}
```

**`enclave.Verifier`** — how the host validates attestation evidence:

```go
type Verifier interface {
    Verify(ctx, token string, nonce []byte) (AttestationClaims, error)
}
```

**Wire protocol** — request/response types for host-enclave communication (`enclave.ExecuteRequest`, `enclave.ExecuteResponse`, etc.) defined as plain Go structs with JSON tags.

### Provider Selection

A single provider is selected per deployment via `AILERON_TEE_PROVIDER` environment variable:

| Value | Provider | Use Case |
|-------|----------|----------|
| *(empty)* | Disabled | TEE not enabled; existing direct-execution path |
| `local` | `enclave/local` | Development and testing; in-process, no hardware isolation |
| `confidential-space` | `enclave/gcs` | Production on GCP; container in AMD SEV-SNP confidential VM |

### Module Architecture

The TEE layer introduces two new Go modules to the workspace:

**`enclave/`** (repo root) — lightweight shared module containing:
- SPI interfaces (`Client`, `Verifier`)
- Wire protocol types
- Error sentinels
- Provider implementations (`enclave/local/`, `enclave/gcs/`)

Both `core/` and `cmd/aileron-enclave/` import this module. It has no dependencies on either, preventing circular imports.

**`cmd/aileron-enclave/`** — the enclave binary, a separate Go module with its own `go.mod`. Runs as an HTTP server inside the TEE (or locally for dev). Imports `enclave/` for protocol types and `core/` for connector implementations and crypto primitives.

### Google Confidential Space Integration

Unlike Nitro (vsock + COSE Sign1 + PCRs), Confidential Space uses:

- **Standard HTTPS** for host-enclave communication (no vsock)
- **OIDC JWT** for attestation (issued by `https://confidentialcomputing.googleapis.com`)
- **Container image digest + GCP project ID** for workload identity (instead of PCR values)
- **GCE metadata service** for attestation token retrieval inside the enclave

The enclave binary is a Docker container built on `gcr.io/confidential-space-images/confidential-space:latest` and deployed to a Confidential Space VM with AMD SEV-SNP memory encryption.

### Attestation Flow

```
Host                                    Enclave (in Confidential Space VM)
  │                                       │
  │  POST /attest {nonce, audience}       │
  ├──────────────────────────────────────►│
  │                                       │── fetch OIDC token from GCE metadata
  │  {token (OIDC JWT), public_key}       │── generate ephemeral ECDH key pair
  │◄──────────────────────────────────────┤
  │                                       │
  │── verify JWT signature (Google JWKS)  │
  │── validate claims (iss, exp, nonce)   │
  │── check image_digest, project_id      │
  │── generate host ECDH key pair         │
  │                                       │
  │  POST /session {host_public_key}      │
  ├──────────────────────────────────────►│
  │                                       │── ECDH shared secret derivation
  │  {session_id, expires_at}             │
  │◄──────────────────────────────────────┤
  │                                       │
  │  POST /execute {encrypted_credential} │
  ├──────────────────────────────────────►│
  │                                       │── decrypt credential with session key
  │                                       │── execute connector
  │  {status, output, receipt_ref}        │── zero credential from memory
  │◄──────────────────────────────────────┤
```

### Key Exchange

Both local and Confidential Space providers use P-256 ECDH for session key derivation (reusing `core/crypto/ecdh.go`). The session key encrypts credentials in transit between host and enclave. For the local provider this is technically unnecessary but maintains protocol parity.

### Credential Escrow (Phase 6)

For async/scheduled actions when the user is offline, credentials are escrowed inside the enclave:

1. While the user's KEK session is active, the host sends `EscrowStore` with the encrypted credential, a grant ID, allowed action types, and an expiry.
2. The enclave decrypts the credential using the session key and stores the plaintext in memory, indexed by escrow ID.
3. At execution time, `Execute` references the escrow ID instead of sending a credential. The enclave retrieves the plaintext from its escrow store.
4. On expiry or revocation, the enclave zeros the credential bytes from memory.

Escrow entries are scoped to a specific grant and action types. They cannot be used beyond their approved scope.

## Consequences

### Changes from ADR-0010 Stage 2

| Aspect | ADR-0010 (original) | ADR-0011 (actual) |
|--------|--------------------|--------------------|
| Provider | AWS Nitro Enclaves (only) | SPI with pluggable providers |
| First provider | Nitro | Google Confidential Space |
| Communication | vsock | Standard HTTPS |
| Attestation format | COSE Sign1 + PCR values | OIDC JWT + image digest |
| Code identity | PCR hash of EIF image | Container image digest (sha256) |
| Module location | `core/enclave/` | `enclave/` (separate repo-root module) |
| Enclave binary module | `cmd/aileron-enclave/go.mod` (same) | Same |

### What stays the same

The trust model from ADR-0010 is unchanged:
- Key hierarchy (passphrase → Argon2id → KEK → AES-256-GCM → ciphertext)
- Stage 1 implementation (Phases 1-3) — all existing code unchanged
- Threat model — identical guarantees at each stage
- `vault.Vault` SPI — untouched
- Zero-knowledge property — Aileron cannot access plaintext credentials

### New files

- `enclave/go.mod`, `protocol.go`, `client.go`, `verifier.go`, `errors.go`
- `enclave/local/` — `client.go`, `escrow.go`, `verifier.go` + tests
- `enclave/gcs/` — `client.go`, `verifier.go` + tests
- `cmd/aileron-enclave/go.mod`, `main.go`, `server.go`, `attestation.go`, `escrow.go`, `Dockerfile` + tests
- `core/config/tee.go`, `core/app/enclave.go`, `core/app/handlers_tee.go` + tests

### Modified files

- `go.work` — added `./enclave`, `./cmd/aileron-enclave`
- `core/go.mod` — added `enclave` dependency
- `core/api/openapi.yaml` — TEE endpoints, schemas, `escrow_policy` on `ExecutionGrant`
- `core/app/app.go` — enclave client wiring
- `core/app/handlers.go` — TEE execution branch in `RunExecution`
- `Taskfile.yml` — `build:enclave`, updated `lint:go` and `test:go`

### New environment variables

| Variable | Default | Description |
|----------|---------|-------------|
| `AILERON_TEE_PROVIDER` | *(empty)* | TEE provider: `local`, `confidential-space`, or empty (disabled) |
| `AILERON_ENCLAVE_URL` | | Base URL of the enclave binary (required for `confidential-space`) |
| `AILERON_ENCLAVE_IMAGE_DIGEST` | | Expected container image digest for attestation verification |
| `AILERON_GCP_PROJECT_ID` | | Expected GCP project ID for attestation verification |
| `AILERON_ENCLAVE_PORT` | `8443` | Port the enclave binary listens on (inside the TEE) |

### New API endpoints

| Method | Path | Purpose |
|--------|------|---------|
| `GET` | `/v1/tee/status` | TEE configuration and session status |
| `POST` | `/v1/tee/attestation` | Initiate remote attestation |
| `POST` | `/v1/tee/session` | Verify attestation and establish ECDH session |

### Adding a new TEE provider

To add support for a new TEE provider (e.g. AWS Nitro, Azure Confidential VMs):

1. Create `enclave/<provider>/client.go` implementing `enclave.Client`
2. Create `enclave/<provider>/verifier.go` implementing `enclave.Verifier`
3. Add a case to the factory function in `core/app/enclave.go`
4. Add the provider value to the `AILERON_TEE_PROVIDER` enum

The enclave binary (`cmd/aileron-enclave/`) is provider-agnostic — it serves the same HTTP API regardless of the TEE environment. The only provider-specific code is the attestation token fetch in `attestation.go`.
