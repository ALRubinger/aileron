---
title: "ADR-0005: Sandbox Choice"
description: "WASM Component Model is the connector sandbox; runtime mediates credential use so connectors never hold raw credentials"
order: 5
---


<div class="meta">
<table>
  <tr><th>Status</th><td>Accepted</td></tr>
  <tr><th>Date</th><td>2026-04-29</td></tr>
  <tr><th>Tracking</th><td><a href="https://github.com/ALRubinger/aileron/issues/343">#343</a></td></tr>
</table>
</div>

## Context

[ADR-0002](/adr/0002-connector-model) commits to the *property* that every connector runs under an isolation boundary the runtime enforces, but explicitly defers the choice of sandbox technology. This ADR makes that choice.

The sandbox is the foundation of Aileron's security claim. Every promise about sealed credentials, capability-bounded execution, and audit-precise behavior reduces, eventually, to "the sandbox enforces what the manifest declares." If the sandbox is weak — if a connector can read another connector's memory, dial network hosts not in its grant, or extract a credential it was never given — the whole architecture collapses to convention.

The concrete properties the sandbox must enforce:

1. **Memory isolation.** Connector A cannot read or write Connector B's memory.
2. **Network isolation.** A connector can only dial network destinations its manifest declared.
3. **Filesystem isolation.** A connector has no ambient filesystem access; any file operations are explicit host imports the manifest declared.
4. **Credential isolation.** A connector never holds a raw credential. The runtime mediates credential use.
5. **Resource bounds.** A connector cannot exhaust the host's memory, CPU, or file descriptors.
6. **Deterministic startup.** Sandbox creation is fast enough that per-call isolation is feasible (no expensive VM warmup).
7. **Cross-platform.** The sandbox works the same on Linux, macOS, and Windows. Aileron developers run on all three.
8. **Language-agnostic.** Connector authors can write in Rust, Go, JavaScript, Python, or anything else that targets the sandbox.

Several technologies meet some of these, but not all. The tradeoffs are about *which* compromises are acceptable for the chosen sandbox.

## Decision

### WASM Component Model is the connector sandbox

Connectors are compiled to WebAssembly components targeting the [WASM Component Model](https://component-model.bytecodealliance.org/) with [WASI Preview 2](https://github.com/WebAssembly/WASI/tree/main/preview2) imports. The Aileron runtime hosts a WASM engine (Wazero in the Go runtime; analogous engines if Aileron is later embedded in non-Go hosts) and instantiates connectors as components.

This is *the* sandbox for every connector. Connectors that cannot target WASM are out of scope for v1; an escape hatch for non-WASM-capable code is deliberately deferred until a concrete connector requires it (see "Open implementation questions" below).

Why WASM, evaluated against the eight properties:

| Property | How WASM satisfies it |
|---|---|
| Memory isolation | Linear memory model; one component cannot address another's memory |
| Network isolation | WASI `wasi:sockets` and `wasi:http` imports are gated; runtime only grants the host:port pairs declared in the manifest |
| Filesystem isolation | No ambient access; `wasi:filesystem` imports are not granted by default and require explicit manifest declaration |
| Credential isolation | The runtime mediates credential use through host functions (see below); connectors never see raw credential bytes |
| Resource bounds | WASM has built-in memory limits and "fuel" (instruction-counting) for time bounds; both enforced by the host engine |
| Deterministic startup | Component instantiation is sub-millisecond cold-start in mainstream engines; per-call instances are practical |
| Cross-platform | WASM is platform-neutral by design; Wazero is pure-Go and runs identically on every supported OS |
| Language-agnostic | Rust, C/C++, AssemblyScript, Go, and increasingly Python and TypeScript compile to WASM components |

WASM is not perfect. Real costs include: connector authors must use a WASM-capable build pipeline; some native libraries don't yet have WASM-targetable equivalents; the WASI Preview 2 ecosystem is still maturing across language toolchains. We accept these costs because the alternative is no enforced sandbox at all, and the trajectory of the WASM ecosystem strongly favors waiting it out.

### Resource limits are enforced and bounded

Every connector instance runs under enforced resource limits:

| Limit | Default | Override |
|---|---|---|
| Memory | 64 MiB | Connector manifest may request a higher cap up to a hard ceiling of 1 GiB |
| Wall time per call | 30 s | Per-call override via action manifest, capped at 5 minutes |
| CPU "fuel" (instruction count) | proportional to wall time | Disabled in `--debug` mode for development |

Hitting a limit terminates the connector instance and surfaces a structured error to the calling action (per [ADR-0010](/adr/0010-failure-handling)). Limits are intentionally modest: a connector that needs gigabytes of memory or hours of wall time is almost certainly the wrong tool for the job.

### IPC protocol

The runtime and connector communicate through host-function imports declared in the manifest's `[capabilities.runtime]` block. Calls are direct ABI invocations; no serialization for the call mechanism itself, though arguments and results are serialized using the WASM Component Model's canonical ABI.

The *content* of cross-boundary calls is structured: a verb (e.g., `connector.execute`), a target operation (e.g., `post_message`), and an argument record. The runtime validates the argument record against the connector's declared operation schema before forwarding the call into the sandbox.

### The runtime mediates credential use; connectors never hold raw credentials

This is the load-bearing rule for credential isolation. A connector's manifest declares the *kind* and *scope* of credential it needs (per [ADR-0002](/adr/0002-connector-model)). When the connector executes, the runtime does *not* hand the credential's raw bytes into the sandbox. Instead:

- For **HTTP-style credentials** (OAuth2, API keys, basic auth), the connector emits a `wasi:http` outgoing request with placeholder authorization. The runtime intercepts the outgoing request, attaches the credential, and forwards it. The connector sees the response but never the credential.
- For **request-signing credentials** (AWS SigV4, GCP signed requests), the connector emits an unsigned request; the runtime signs it host-side and forwards it. The connector never holds the signing key.
- For **session-token credentials** (a short-lived bearer the connector must present multiple times in one session), the runtime issues a *capability handle* — an opaque value the connector includes in subsequent requests; the runtime resolves the handle to the actual token at request time. The handle is unforgeable, single-use-per-call, and invalidated when the connector instance terminates.

The connector's only reach into the credential world is through these mediated paths. There is no `vault.read("credential-x")` host function and there will not be.

The mechanics of *how the user binds an abstract capability to a concrete credential* belong to [ADR-0006](/adr/0006-capability-binding-ux); this ADR ratifies only that the runtime mediates the binding's *use*.

### Sandbox-boundary capability denial is the last line of defense

[ADR-0002](/adr/0002-connector-model) establishes that the runtime grants nothing not declared in the connector's manifest. [ADR-0003](/adr/0003-action-model) establishes a second check at the action boundary: the action's declared capability subset is enforced *in addition to* the connector's manifest grant. This ADR adds the third structural check: the sandbox itself enforces capability denials at the kernel/WASM-engine boundary.

If, somehow, both the action-boundary and connector-manifest checks were bypassed (a runtime bug, an unforeseen edge case), the sandbox boundary would still refuse the unauthorized syscall. The defense is in depth not because each layer is unreliable, but because the cost of a single missing check is unacceptable in this trust model.

## Alternatives Considered

### Container-based isolation (Docker, OCI runtimes) (rejected)

Each connector runs as a Docker container. Aileron orchestrates container lifecycle, manages networking, mounts limited filesystems.

Rejected on cold-start cost, dependency footprint, and platform fragility. Container startup is hundreds of milliseconds at best; per-call instantiation is impractical, forcing per-call reuse, which complicates state isolation. Docker (or another OCI runtime) becomes a hard runtime dependency for every Aileron user — a significant additional install. On macOS and Windows, "Linux containers" run inside a Linux VM, which adds another order of magnitude in startup cost and platform-specific complications.

WASM is what containers wish they were for the per-connector use case: lighter, faster, more portable, with stronger memory isolation by construction.

### Lightweight VMs (Firecracker, etc.) (rejected)

Each connector runs in a microVM. The sandbox boundary is the hypervisor.

Rejected primarily because Firecracker (and similar) are Linux-only, kernel-level dependencies that don't run unmodified on macOS or Windows. Aileron must run on every developer laptop. A sandbox technology that excludes 60%+ of the target audience is not the default.

VMs would be a strictly stronger boundary than WASM in some dimensions, but the platform constraint forces them out. WASM gives most of the isolation benefit at a fraction of the platform cost.

### Native code with ptrace/seccomp (no WASM, no separate process) (rejected)

Connectors are native shared objects loaded into the runtime; isolation is enforced via `seccomp-bpf` filtering of syscalls.

Rejected because shared-object loading does not provide memory isolation. A native library loaded into the runtime's address space can read every credential the runtime holds, every audit log, every other connector's state. seccomp filters syscalls but does not partition memory. This is the in-process plugin model dressed up; it has the same fatal flaw.

### Deno or Node permission models (rejected)

Connectors are JavaScript modules. The host enforces permissions via `--allow-net`, `--allow-read`, etc.

Rejected because JavaScript-as-the-only-language is a non-starter (we want Rust, Go, Python connector authors). And because Deno's permission model, while well-designed, is a JavaScript-runtime feature; its security claims rest on V8 and Deno's specific implementation, not on a portable spec. WASM is the language-agnostic version of the same idea: capability-based, declarative, runtime-enforced.

A Deno-shaped connector is achievable today by compiling JavaScript to a WASM component. The choice of WASM doesn't preclude JavaScript authors; it just doesn't single them out.

### Capability-only enforcement at the action boundary (no sandbox at all) (rejected)

Trust the connector code. Enforce capabilities only at the action boundary where the runtime sees the call. Sandbox is conceptual, not technical.

Rejected because "trust the connector code" is exactly what Aileron promises *not* to require. The whole ecosystem premise is that third-party connectors run under genuine isolation, not under "we promise we'll review the code." A purely advisory enforcement model fails the moment a malicious connector goes around the action boundary by, e.g., calling a syscall directly.

## Consequences

### For connector authors

- The sandbox is WASM. Author connectors targeting the WASM Component Model with WASI Preview 2 imports.
- Toolchains: Rust (`cargo component`), Go (compiled via TinyGo or native Go's WASM target), JavaScript/TypeScript (via componentize-js), Python (componentize-py). Each ecosystem provides its own build path; Aileron specifies the input target, not the build pipeline.
- A connector that genuinely cannot target WASM is out of scope for v1. The escape hatch will exist; it is not yet ratified.
- Connector authors do *not* implement credential handling. Declare the credential capability the connector needs; the runtime injects it at the call site. There is no `read_credential()` API to call.

### For the runtime

- The runtime embeds a WASM engine (Wazero in the Go reference implementation). This is a mandatory dependency, not optional.
- The runtime implements the host-function set declared by WASI Preview 2, plus Aileron-specific imports for audit emission, capability handle resolution, and structured error reporting.
- The runtime enforces resource limits before a connector starts and on every cross-boundary call.

### For Aileron's portability story

- Pure-Go WASM hosting (via Wazero) means Aileron has no native dependencies for the sandbox. A single static binary works on Linux, macOS, and Windows.

### For audit and security

- Every cross-boundary call is logged with: connector identity, capability used, credential bound, resource consumed (memory peak, wall time), and audit ID.
- Capability denials at the sandbox boundary produce the same structured error class (`capability_denied`) as denials at the action or connector-manifest boundary; the `boundary` field disambiguates which layer enforced.
- Resource-limit terminations produce a distinct error class (`resource_limit_exceeded`) with the specific limit named.

### Open implementation questions (deferred)

- *How does the user bind an abstract credential capability to a concrete vault entry, and how does the runtime resolve a capability handle to the actual credential at call time?* — [ADR-0006](/adr/0006-capability-binding-ux).
- *What error categories does a sandbox boundary produce, and how do they map to action retry policy?* — [ADR-0010](/adr/0010-failure-handling).
- *What is the escape hatch for connectors that genuinely cannot target WASM (closed-source SDKs, hardware-token integrations, etc.)?* — deliberately deferred. Will be ratified in a future ADR when a concrete connector requires it. Until then, WASM-only.

## Examples

### Connector manifest

```toml
[connector]
name = "github://aileron/slack"
version = "1.2.0"
provenance_hash = "sha256:abc123..."

[capabilities.network]
hosts = ["slack.com:443"]

[capabilities.credential]
kind = "oauth2"
scope = "chat:write"

[capabilities.runtime]
imports = ["wasi:http/outgoing-handler"]
```

### Capability denial at the WASM sandbox boundary

A WASM connector attempts to dial `evil.example.com:443`. The runtime's WASI host implementation refuses the import call (the manifest declared only `slack.com:443`). The error returned to the connector code is the standard WASI `errno`. The runtime simultaneously emits a structured audit event and surfaces a `capability_denied` error to the calling action:

```json
{
  "error": {
    "class": "capability_denied",
    "boundary": "sandbox",
    "connector": "github://aileron/slack@1.2.0",
    "requested": "network:evil.example.com:443",
    "granted": ["network:slack.com:443"],
    "audit_id": "audit-d4f2..."
  }
}
```

The `boundary: "sandbox"` field distinguishes this from action-boundary or connector-manifest-boundary denials, even though the `class` is identical.

### Resource-limit termination

A WASM connector enters an infinite loop. The runtime's fuel meter exhausts at the wall-time limit; the instance is terminated. The action receives:

```json
{
  "error": {
    "class": "resource_limit_exceeded",
    "connector": "github://acme/some-connector@1.0.0",
    "limit": "wall_time",
    "configured": "30s",
    "audit_id": "audit-e8a1..."
  }
}
```

No partial work is retained; the sandbox is destroyed cleanly.
