---
title: "ADR-0014: Spawn Sandbox Technology"
description: "Cross-platform mechanism for confining subprocesses spawned by the runtime on a connector's behalf"
order: 14
---


<div class="meta">
<table>
  <tr><th>Status</th><td>Accepted</td></tr>
  <tr><th>Date</th><td>2026-05-14</td></tr>
  <tr><th>Tracking</th><td><a href="https://github.com/ALRubinger/aileron/issues/508">#508</a>, <a href="https://github.com/ALRubinger/aileron/issues/619">#619</a></td></tr>
</table>
</div>

## Context

[ADR-0002](/adr/0002-connector-model) introduces the **spawn primitive**: a connector may declare `[capabilities.spawn]` in its manifest and call the runtime's `aileron_host.spawn` host function to invoke a local CLI on its behalf. That ADR commits to the property: the subprocess executes under an enforcement boundary the runtime owns. It does not commit to the mechanism. This ADR makes that choice.

The choice matters. The spawn primitive widens the set of syscalls the runtime makes on a connector's behalf. Where [ADR-0005](/adr/0005-sandbox-choice) handles the WASM connector itself (a single sandbox layer for code running inside the runtime's address space), this ADR handles a different layer: an OS subprocess that the runtime forks. The subprocess gets a real OS identity. It can touch the filesystem, open sockets, and read environment variables. If it does any of those beyond the manifest's declaration, the spawn primitive's value collapses to convention.

Aileron developers run on Linux, macOS, and Windows. A single connector manifest must express bounds the runtime can enforce consistently across all three. The question is what set of platform mechanisms, used how, gives a defensible boundary on each.

The properties the spawn sandbox must enforce, derived from the manifest fields in [ADR-0002](/adr/0002-connector-model)'s spawn-primitive section:

1. **Filesystem scoping.** The subprocess can read files only within `fs_read` and write only within `fs_write`. No ambient access to the user's home directory, the runtime's working dir, or the rest of the filesystem.
2. **Environment scoping.** The subprocess sees only the env keys in `env_passthrough`, plus credential env vars the runtime injects per [ADR-0002](/adr/0002-connector-model)'s credential-injection rule. Nothing from the host's ambient environment.
3. **Network egress mediated by the daemon.** A subprocess reaches the network only through a daemon-run local proxy on loopback. The proxy enforces the connector's `[capabilities.network]` host:port allowlist ([ADR-0002](/adr/0002-connector-model)) and tunnels TLS via HTTP CONNECT. Direct egress to anything other than the daemon's proxy address is denied at the sandbox boundary. A connector that omits `[capabilities.network]` gets no network at all, same as v1.
4. **Process scoping.** The subprocess cannot signal, ptrace, or otherwise interfere with the parent runtime, other subprocesses, or any unrelated host process. It receives its own session and a job-control boundary keyed to the runtime's lifetime.
5. **No privilege escalation.** The subprocess runs at the host user's privilege level or lower. It cannot acquire capabilities the host user did not already have. On all platforms the runtime refuses to spawn setuid binaries.
6. **Graceful unavailability.** If the chosen mechanism is unavailable on the running platform (kernel feature absent, OS version too old, capability missing), the runtime refuses to spawn with a structured error and emits an audit row. A missing sandbox is not silently permitted.

## Decision

### Mechanism by platform

| Platform | Mechanism | Status |
|---|---|---|
| Linux | Pure-Go `unshare(2)` (user, mount, pid, net namespaces) plus Landlock LSM (Linux 5.13+) for FS scoping. Fallback to deny-spawn when unprivileged user namespaces are disabled. | Required |
| macOS | `sandbox-exec` with a runtime-generated Sandbox Profile Language (SBPL) policy. | Required |
| Windows | Job object plus restricted token; AppContainer deferred. | Required |
| Other (BSD, illumos, etc.) | Deny-spawn with `spawn_sandbox_unavailable`. | Refused |

A connector that declares `[capabilities.spawn]` works on the three supported platforms identically from the manifest's point of view. The runtime translates the declaration into the platform-appropriate mechanism. Spawn is unavailable on every other platform; the runtime refuses to install the connector there, surfacing the unavailability at install time rather than at first call.

### Linux: namespaces plus Landlock

The runtime invokes the subprocess via `os/exec` with `SysProcAttr.Cloneflags` set to `CLONE_NEWUSER | CLONE_NEWNS | CLONE_NEWPID | CLONE_NEWNET`. This shape is what `bwrap` does internally; the runtime does it directly to avoid an external binary dependency.

1. **User namespace.** Maps the runtime's UID to a fresh nobody-equivalent inside the namespace. The subprocess cannot escalate to root within the namespace and cannot affect any host UID's resources.
2. **Mount namespace.** The runtime constructs a private mount tree before exec'ing the subprocess: bind-mounts of each `fs_read` path read-only, bind-mounts of each `fs_write` path read-write, a tmpfs for `/tmp`, and a private `/proc`. Nothing outside `fs_read ∪ fs_write` is visible.
3. **PID namespace.** The subprocess is PID 1 inside its namespace; it cannot see or signal host processes. Exit propagates cleanly back to the runtime.
4. **Network namespace.** The new namespace has only its own loopback interface and no routes to external networks. Outbound network calls fail at the kernel level. When the connector declares `[capabilities.network]`, the daemon's per-spawn CONNECT proxy listens on a Unix-domain socket that the runtime bind-mounts into the child's mount namespace; a small TCP-to-UDS shim runs as a sibling of the wrapped CLI inside the namespace and bridges `127.0.0.1:<port>` to that socket. The CLI honors the standard `HTTPS_PROXY` env var and reaches the daemon transparently. See "Network confinement" below for the full mechanism. When the connector omits `[capabilities.network]`, the namespace is fully isolated and the shim is not started.
5. **Landlock (when available).** On Linux 5.13+, the runtime adds Landlock rules as a second filesystem layer, restricting reads and writes to the declared scopes even within the mount namespace. This is defense in depth. When Landlock is unavailable, the mount namespace alone is the enforcement.

**Unprivileged user namespaces required.** On distributions or sysctl configurations that disable `kernel.unprivileged_userns_clone`, the runtime cannot create the user namespace and falls into the deny-spawn path with a structured error naming the missing capability. Users on such systems may enable the sysctl, switch distributions, or accept that spawn-using connectors will not run.

**Why not `bwrap` or `firejail` directly.** Both are wrappers around the same kernel primitives. Pulling in either as a runtime dependency commits Aileron to a non-Go install footprint on Linux. The kernel primitives are the same; the runtime calls them directly through `syscall` and `golang.org/x/sys/unix`.

### macOS: sandbox-exec with generated SBPL profile

The runtime invokes `/usr/bin/sandbox-exec -p <profile> <program> <argv...>`, where `<profile>` is an SBPL string generated from the manifest's `[capabilities.spawn]` declaration. SBPL is Apple's Sandbox Profile Language, the same engine the OS uses internally for app sandboxing.

A typical generated profile:

```
(version 1)
(deny default)
(allow process-fork)
(allow process-exec (literal "/usr/bin/git"))
(allow file-read* (subpath "/Users/alr/code"))
(allow file-read* (literal "/Users/alr/.gitconfig"))
(allow file-write* (subpath "/Users/alr/.cache/aileron/gitcrawl"))
(deny network*)
(allow network* (remote tcp "localhost:54321"))
```

Defaults deny everything. Each `fs_read` entry becomes one or more `file-read*` allow rules (literal for files, subpath for directories). Each `fs_write` entry becomes a `file-write*` allow rule. The declared `programs` list becomes `process-exec` literals. Network is denied wholesale except for a single loopback exception to the daemon's per-spawn proxy port (the literal port number is bound by the daemon at spawn time and inlined into the generated profile). See "Network confinement" below for the proxy model.

**`sandbox-exec` is deprecated.** Apple has carried the deprecation warning for years without removing the binary; it remains the only tool available to confine arbitrary subprocesses on macOS without code-signing entitlements. The underlying `sandbox_init` API is private and unstable. The runtime accepts the deprecation risk for v1 and tracks Apple's posture. When `sandbox-exec` is removed in a future macOS release, the spawn primitive on macOS will require either Apple's evolving alternative (currently `containerd`-style XPC-only sandboxing for App Store apps) or explicit deny-spawn until a replacement is ratified.

**Why not a hand-rolled Mach-port-based isolation.** Mach-level isolation requires entitlements and code-signing infrastructure Aileron does not yet have. SBPL via `sandbox-exec` is what every major macOS sandboxing tool (Chrome, Firefox, Docker Desktop) reaches for; the runtime uses it under the same constraints.

### Windows: job object plus restricted token

The runtime calls `CreateProcessAsUserW` with a token derived via `CreateRestrictedTokenW`, dropping the subprocess's integrity level to Low and stripping all groups except `Everyone`. The runtime assigns the subprocess to a job object via `AssignProcessToJobObject` with these limits:

- `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE`: subprocess dies if the runtime exits, no zombies.
- `JOB_OBJECT_LIMIT_DIE_ON_UNHANDLED_EXCEPTION`: clean shutdown semantics.
- `JOB_OBJECT_LIMIT_BREAKAWAY_OK` set to false; subprocesses cannot escape the job.
- `JOB_OBJECT_UILIMIT_HANDLES`: prevents access to UI objects outside the job.

Filesystem scoping comes from the restricted token plus ACLs. The runtime adjusts ACLs on `fs_write` paths to grant write to the low-integrity token; everything else is denied by default to a Low-integrity caller. `fs_read` paths whose ACLs already grant read to `Everyone` need no adjustment. Paths with stricter ACLs that the connector needs to read are denied at the kernel level by the restricted token, which is the desired behavior.

Network confinement on Windows uses Windows Filtering Platform (WFP) rules applied to the subprocess's PID. The default rule denies all outbound; when the connector declares `[capabilities.network]`, the runtime adds a second rule permitting outbound TCP to `127.0.0.1:<proxy-port>` for the spawn invocation's lifetime. See "Network confinement" below for the proxy model the rule connects to.

**AppContainer deferred.** AppContainer would provide stronger guarantees (capability-token-based access to network, filesystem, and devices), but requires an Application Identity SID, an AppContainer profile registered with the OS, and significant manifest-side changes. The job-object plus restricted-token combination is the v1 mechanism. AppContainer is a future ADR.

### Network confinement: daemon-mediated proxy

Spawn connectors that need outbound network declare it via the connector's existing `[capabilities.network]` block ([ADR-0002](/adr/0002-connector-model)). The block's shape is unchanged from WASM connectors: a closed list of `host:port` pairs, no wildcards. What changes is the enforcement seam. WASM connectors call the runtime's HTTP host function and the runtime checks each URL before dialing. Native subprocesses cannot be intercepted at the function-call layer; the runtime instead constrains the network paths the subprocess can reach so that every outbound flow is forced through a daemon-controlled proxy where the same allowlist is checked.

The flow is the same conceptually on all three platforms; only the substrate between the CLI and the proxy differs.

1. At spawn time, the daemon binds an HTTP CONNECT proxy on a per-invocation local endpoint. On macOS and Windows the endpoint is an ephemeral TCP port on `127.0.0.1`; on Linux it is a Unix-domain socket file under `$XDG_RUNTIME_DIR` (the namespace barrier prevents a host-loopback TCP listener from being reachable). Neither endpoint is exposed to any non-local interface.
2. The runtime sets `HTTPS_PROXY` and `HTTP_PROXY` on the subprocess's environment to point at `http://127.0.0.1:<port>`. On Linux that port lives on the namespace's own loopback and is served by an in-namespace shim that bridges to the UDS; on macOS and Windows it is the daemon's proxy port directly. Both env keys are added to the closed `env_passthrough` set automatically when the connector declares `[capabilities.network]`; the connector author does not declare them.
3. The OS sandbox permits exactly one outbound network operation: TCP to the configured loopback port. On macOS and Windows that's the daemon's host-loopback port; on Linux it's the in-namespace loopback the shim listens on. Everything else is denied at the platform mechanism (the per-platform rules above).
4. For each CONNECT request the proxy receives, it parses the target host:port, checks it against the connector's `[capabilities.network].hosts` allowlist (the same `internal/sandbox.HostPolicy` used by the WASM HTTP gate), tunnels the bytes through to the destination on a match, or returns `403 Forbidden` and emits a `capability_denied` audit row on a miss.

The proxy is the single enforcement seam for the host:port allowlist across all platforms. The match logic lives in Go in the daemon. Per-platform glue is uniform from the CLI's point of view: `HTTPS_PROXY` points at a loopback TCP address, and only that address is reachable.

**CONNECT, not MITM.** The proxy operates at the CONNECT method only; it does not terminate TLS. The proxy sees host:port and the encrypted bytes; it does not see request URLs, headers, or bodies. The trade-off is intentional. No CA certificate gets installed in the subprocess's trust store, no TLS interception complications, no per-CLI quirks around certificate handling. The cost is audit granularity: spawn audit rows record `host:port` per CONNECT, not full request URLs. For deeper observability the connector author can use a WASM connector instead, where the HTTP gate sees the full request shape.

**Linux: in-namespace TCP-to-UDS shim.** On macOS and Windows, the platform sandbox can selectively permit loopback to one TCP port without granting any other network access to the subprocess. Linux's mechanism in this ADR (a private network namespace) gives the subprocess its own loopback, which is not connected to the host's loopback where a TCP listener would naturally bind. Bridging the two without `CAP_NET_ADMIN` on the host (which the unprivileged user-namespace path does not have) requires moving the path between subprocess and proxy off TCP and onto a substrate that crosses the namespace boundary cleanly. That substrate is a Unix-domain socket file plus a tiny in-namespace shim that translates between the CLI's expected `127.0.0.1:<port>` and the socket.

The mechanism, end to end:

1. Before forking, the daemon binds its per-spawn CONNECT proxy on a Unix-domain socket at `$XDG_RUNTIME_DIR/aileron-proxy-<spawn-id>.sock`. The socket lives on the host filesystem under the daemon's user.
2. The runtime constructs the child's mount namespace (already required for `fs_read` / `fs_write`) and **bind-mounts the socket file** into the child's view at a fixed path (`/run/aileron-proxy.sock`). Bind-mounting inside the runtime's own user+mount namespace is permitted to unprivileged user-namespace processes; no `CAP_NET_ADMIN` is involved.
3. The runtime forks once more inside the new namespace. The forked child immediately exec's the wrapped CLI with `HTTPS_PROXY=http://127.0.0.1:<port>` set. The forked parent (also inside the namespace) runs a tiny **TCP-to-UDS shim**: it listens on `127.0.0.1:<port>` on the namespace's own loopback, and for each accepted connection it dials `/run/aileron-proxy.sock` and `io.Copy`s bytes in both directions.
4. When the CLI exits, the shim observes the SIGCHLD and exits too. The namespace tears down. The socket file is cleaned up by the daemon.

The shim is part of the daemon binary (re-exec'd with a `--spawn-shim` flag, or a goroutine in the post-fork parent before its own exit). No separate binary, no external dependency. Implementation lives in `internal/sandbox/spawn_shim_linux.go`.

Why this is privilege-free:

- Every step happens inside the unprivileged user+mount+net namespace the runtime already creates per ADR-0014's existing Linux mechanism.
- Bind-mounting a host file (`...aileron-proxy-<id>.sock`) into the child's mount namespace is permitted to unprivileged user-namespace processes for files the runtime already has read+write access to.
- The shim listens on the namespace's own loopback, which is brought up by default for new network namespaces. No interface configuration, no routing rules, no eBPF.
- The CLI's only network path is the in-namespace loopback (the shim) because the namespace has no external interface. Direct egress to anywhere else fails at the kernel level.

The cost is one extra goroutine and two-way `io.Copy` per CONNECT request. Negligible at the byte volumes spawn connectors realistically push. For CLIs that ignore `HTTPS_PROXY`, the failure mode is `network_unreachable` (no other network path exists), which is the same failure-closed posture as the other platforms.

A future optimization: when the daemon runs with `CAP_NET_ADMIN` (operator-deployed configurations, not the user-laptop default), the runtime can skip the shim and instead share the host's network namespace with the subprocess plus an nftables cgroup rule that drops all egress except to the proxy port on host loopback. The ADR commits to the shim as the baseline; the nftables path is a perf optimization that can ship later without affecting the manifest contract.

**CLIs that ignore `HTTPS_PROXY`.** Even with kernel enforcement, the proxy model only delivers usable network to a CLI when the CLI actually routes through the proxy. CLIs that build their own HTTP clients and ignore the standard proxy env vars cannot reach the network because direct egress is denied. The failure is structured (`network_unreachable`, distinct from `capability_denied`) and the audit row notes the proxy was untouched. Documenting the requirement at install time is the introspector's job (see the BYOCLI sandbox tightening tracked in [#619](https://github.com/ALRubinger/aileron/issues/619)). The trade-off is acceptable: the alternative is permitting unmediated egress, which collapses the spawn primitive's network confinement property.

**Per-invocation lifetime.** The proxy listener and any associated platform rules (SBPL `(allow network*)` line, WFP allow rule, nftables/cgroup binding) live for the duration of a single spawn invocation. The runtime tears them down after `Wait()` returns. Audit rows record proxy decisions correlated with the spawn invocation's audit id. Across invocations of the same connector, each spawn gets a fresh ephemeral port and a fresh proxy goroutine.

### Local-origin (BYOCLI) wrap default

The "omitted `[capabilities.network]` means deny" rule above holds for hub-published, author-declared connectors. It does not hold for connectors with `connector.origin = "local"` — the BYOCLI wraps produced by `aileron cli add` and `aileron pp add` ([#749](https://github.com/ALRubinger/aileron/issues/749)). For those, an omitted `[capabilities.network]` block defaults to **permissive**: the spawn sandbox lets the wrapped CLI reach any host, and the per-spawn CONNECT proxy runs purely for audit (every CONNECT lands in the audit log with `host:port`, no allowlist gate).

The carve-out exists because the wrap path has no source of truth for what hosts the wrapped CLI needs. The wrap heuristic introspects the CLI's `--help` output, which never enumerates outbound endpoints. PrintingPress's catalog ([mvanhorn/printing-press-library](https://github.com/mvanhorn/printing-press-library)) — the primary source of metadata for `aileron pp add` — declares no network or filesystem fields on any of its 87 entries (verified empirically against `schema_version: 2` as of 2026-05). For arbitrary CLIs installed via `aileron cli add`, there is no catalog at all. The strict "omitted = deny" semantics inherited from hub publishers produced a 100% failure rate at first call for every wrapped service-bound CLI: `linear-pp-cli doctor` dies at `dial tcp: lookup api.linear.app: no such host`, every similar wrap fails the same way.

The alternative — requiring the wrap to declare host:port allowlists at install time — was rejected for three reasons. **Theater for service-bound CLIs.** A CLI named `linear-pp-cli` exists to talk to `api.linear.app`. Restricting it to exactly that host adds no defense; a malicious version of the CLI exfiltrates *through* the legitimate API channel (creates a comment whose body is the dumped vault, files an issue with credentials in the title) and the allowlist notices nothing. **Maintenance burden far outweighs the value.** Declaring hosts requires coordinated PRs across every entry in the catalog (87 today, growing) plus every arbitrary CLI a user wraps via `cli add`. The allowlist drifts every time a service adds a CDN host or rotates an endpoint. The wrap-emitter can't probe (—help doesn't tell), so the work falls on humans who don't have the data either. **The install act is the trust grant.** A user running `aileron pp add linear` is expressing trust in the CLI to perform its purpose, which includes talking to its service. The capability model's "no implicit grant" rule (per [ADR-0002](/adr/0002-connector-model)) is preserved for hub publishers — where the publisher *can* declare what their connector does — and relaxed only for the wrap path where the catalog can't carry the metadata.

What stays load-bearing for local-origin wraps, independent of network scope:

- **Credential sealing.** The vault encrypts the credential at rest and injects it only at the spawn boundary via `[capabilities.spawn].credential_env_keys` ([ADR-0011](/adr/0011-local-credential-vault) + [ADR-0006](/adr/0006-capability-binding-ux)). The agent never sees the raw token, and the credential never persists in the CLI's env or config after the spawn completes. This is the protection that actually matters; the network allowlist would not strengthen it.
- **Filesystem scope.** `fs_read` / `fs_write` stay enforced. The wrap emitter's default scope is broadened from `~/.config/<name>` to the XDG triad (`~/.config/<name>`, `~/.local/share/<name>`, `~/.cache/<name>`) so modern CLIs that store sync DBs or caches outside `~/.config` install working out of the box, but the boundary remains tight: the CLI cannot read `~/.ssh`, `~/.aws`, `~/.anthropic`, or arbitrary user files outside its own XDG dirs. The threat this closes (a wrapped CLI silently scraping credentials from adjacent tools' config) is the realistic one, and it survives the network relaxation.
- **Approval gating** ([ADR-0009](/adr/0009-user-channel)) on write and destructive operations. The user sees the operation before it lands.

Hub publishers can still use the wrap path indirectly (a `aileron action wrap --install` output published to the hub), in which case the resulting manifest is no longer `origin = "local"` and the strict "omitted = deny" rule applies — the publisher is expected to declare host:port allowlists explicitly per the original ADR-0002 contract. The carve-out is bounded to the local-install ergonomic path.

### Cross-platform manifest implications

The manifest stays platform-neutral. A `[capabilities.spawn]` declaration is identical whether the connector is consumed on Linux, macOS, or Windows. The runtime is responsible for translating declared scopes into per-platform mechanisms. The following implications follow from that commitment:

- **Path semantics.** `fs_read` and `fs_write` entries are absolute Unix-style or `~/`-anchored. On Windows the runtime maps `~/` to `%USERPROFILE%` and Unix slashes to backslashes. A path like `~/code/aileron` is portable; a path like `/var/spool/...` is not, and is rejected at install if it cannot be sanely mapped.
- **No platform-conditional fields in v1.** A future version may add `[capabilities.spawn.darwin]` or `[capabilities.spawn.linux]` overrides if a real connector needs them. v1 disallows that to keep the manifest schema small and consumer-portable.
- **Environment keys are case-sensitive on POSIX and case-insensitive on Windows.** The manifest writer is responsible for declaring the canonical casing the subprocess expects. The runtime does no normalization.
- **`programs` paths must resolve on the target platform.** A manifest that declares `/usr/bin/git` will fail at install on Windows. The connector author either pins per-platform paths through naming convention (a future schema extension) or ships a manifest that excludes platforms where its programs are absent.

### Graceful unavailability

When the runtime cannot construct the sandbox for the running platform, it refuses the spawn call with a structured error:

```json
{
  "error": {
    "class": "spawn_sandbox_unavailable",
    "boundary": "sandbox",
    "connector": "github://aileron/gitcrawl@1.0.0",
    "platform": "linux",
    "reason": "unprivileged user namespaces disabled (kernel.unprivileged_userns_clone=0)",
    "audit_id": "audit-7c2b..."
  }
}
```

The error class is distinct from `capability_denied`. Capability denial means the connector asked for something the manifest did not grant. Sandbox unavailability means the platform cannot enforce what was granted; the runtime refuses to spawn rather than spawn unconfined. The action receiving this error fails fast and the audit row is preserved.

Connector install checks the sandbox is available on the running platform when `[capabilities.spawn]` is declared. An install where the sandbox is unavailable surfaces the same error class at install time, so the user discovers the constraint before the first call.

### Sandbox-boundary checks are last line of defense

[ADR-0002](/adr/0002-connector-model)'s `[capabilities.spawn]` declaration is the first gate. The action's declared spawn-capability subset is the second gate ([ADR-0003](/adr/0003-action-model)). This ADR adds the third: the OS-level enforcement mechanism itself refuses syscalls outside the granted scope.

If, somehow, both upstream checks were bypassed by a runtime bug, the kernel boundary would still refuse the unauthorized read, write, or network call. Defense in depth is structural, not optional.

## Alternatives Considered

### Pure-Go process isolation without OS primitives (rejected)

Implement the sandbox entirely in user space, by intercepting filesystem and network calls through Go-level interposition before invoking `exec`. The subprocess runs without any kernel-enforced confinement.

Rejected because user-space interposition is not a security boundary. Once the subprocess is exec'd, its syscalls go directly to the kernel. The runtime has no observation of what the subprocess actually does. A subprocess that ignores the runtime's expectations cannot be stopped by anything short of `SIGKILL`. The whole point of the spawn primitive's trust model is that the boundary is enforced by an authority below the connector and the subprocess, which is the kernel.

### One mechanism for all platforms (rejected)

Pick a single sandboxing technology and accept that it runs on only one OS. Either Linux-only namespaces, or macOS-only sandbox-exec, or Windows-only AppContainer.

Rejected because Aileron's portability story (single static Go binary on every developer laptop) is non-negotiable. Spawn-using connectors are part of the connector ecosystem, not a Linux-only side feature. A platform-specific mechanism would force every spawn-using connector to declare which platforms it supports, and the demo audience (developers on Mac and Linux laptops) would split.

### Container-based subprocess isolation (rejected)

Use Docker or Podman to confine the subprocess. The runtime spawns a container per invocation.

Rejected on the same grounds as [ADR-0005](/adr/0005-sandbox-choice) rejects containers for the connector sandbox: cold-start cost, hard dependency on a container runtime the user must install separately, and platform fragility (containers on macOS and Windows run inside a Linux VM, multiplying the install footprint). The spawn primitive is intended to be lightweight enough that per-call invocation is comfortable; containers are an order of magnitude too heavy.

### Defer the choice (rejected)

Ship the spawn primitive's manifest schema and host function but leave the enforcement mechanism abstract until a real connector demands a decision.

Rejected because the spawn primitive's security property is its enforcement. Shipping the schema without the mechanism would be a promise the runtime cannot keep. Connector authors writing against `[capabilities.spawn]` need to know that the declared scope is actually the scope the subprocess runs in. A deferred decision here is a silent unsafe default.

### Per-host network filtering at the OS layer (rejected)

Enforce the connector's `[capabilities.network]` host:port allowlist directly in the per-platform sandbox mechanism: macOS SBPL `(allow network* (remote tcp "api.linear.app:443"))` rules per allowed host, Windows WFP rules per host, Linux nftables rules per host. No daemon-side proxy involved.

Rejected for three reasons. First, the allowlist matching logic would be duplicated across three platform implementations and three rule languages; a bug in one would not be a bug in the others, and per-platform debugging multiplies. Second, hostname-to-IP resolution in the OS rules is brittle: SBPL allowlists on macOS evaluate against the resolved IP at rule-application time, not at connect time, so DNS rotation breaks the rule; WFP and nftables have similar limitations. Third, audit emission would split across platform mechanisms, leaving the daemon without a single source of truth for "what network calls did this spawn make."

The daemon-mediated proxy collapses these problems into one Go implementation of the allowlist check, sees every CONNECT request with its current hostname, and emits a single audit stream. The OS sandbox's only network task is "permit loopback to one port" which is uniform across platforms.

## Consequences

### For connector authors

- Spawn-using connectors work identically on Linux, macOS, and Windows. One manifest, three platforms.
- Path declarations must be Unix-style or `~/`-anchored. Windows paths are computed by the runtime at install/run time.
- `programs` entries that do not resolve on a target platform cause install to fail on that platform. The connector author either accepts the platform constraint or extends the schema in a future ADR.
- A connector that declares `[capabilities.spawn]` is unavailable on BSDs, illumos, and any other non-supported OS. The runtime refuses install with a structured error naming the missing platform.
- A spawn connector that needs outbound network declares `[capabilities.network]` with the same `host:port` allowlist shape WASM connectors use. The runtime injects `HTTPS_PROXY` and `HTTP_PROXY` automatically; the connector author does not declare those env keys. CLIs that ignore standard proxy env vars cannot make outbound calls.

### For the runtime

- The runtime ships per-platform spawn implementations under `internal/sandbox/spawn_linux.go`, `internal/sandbox/spawn_darwin.go`, `internal/sandbox/spawn_windows.go`. Build tags select the right file at compile time.
- The Linux implementation uses `golang.org/x/sys/unix` and direct `syscall` calls; no external binary dependencies.
- The macOS implementation invokes `/usr/bin/sandbox-exec`; an installed macOS always has it. Absence is a structured error.
- The Windows implementation uses `golang.org/x/sys/windows` for token manipulation, job objects, and WFP rules.
- The runtime maintains a per-platform "sandbox available" probe, run at startup and at install of spawn-using connectors. The result feeds the structured `spawn_sandbox_unavailable` error. The probe runs at `aileron action install` time and at `aileron cli add` time (for BYOCLI per [#580](https://github.com/ALRubinger/aileron/issues/580)) so the user sees unavailability before any binary is fetched or stored.
- The proxy implementation lives in `internal/sandbox/proxy.go` and is shared across platforms. It reuses `internal/sandbox.HostPolicy` for the host:port allowlist check. On Linux, the per-spawn proxy binds on a Unix-domain socket under `$XDG_RUNTIME_DIR` rather than on TCP; the runtime bind-mounts the socket into the child's view and starts a TCP-to-UDS shim (`internal/sandbox/spawn_shim_linux.go`) inside the namespace.

### For testing

- Each platform implementation needs its own test plane. Cross-platform CI runs the suite on Linux, macOS, and Windows runners. Unit tests for the gate ([ADR-0002](/adr/0002-connector-model)'s `HostPolicy.CheckSpawn`) are platform-neutral and run on every runner.
- The fake-binary harness (deferred to issue #512) runs on every supported platform and gives connector repo tests a portable substrate for spawn assertions.

### For audit and security

- Every spawn call emits an audit event identifying the connector, the program, the argv pattern, the exit code, and the content hashes of stdout and stderr. The audit row is identical across platforms; the underlying mechanism is not exposed in the audit.
- Sandbox-unavailability denials emit a distinct audit class (`spawn_sandbox_unavailable`) so an operator can distinguish "the subprocess broke the rules" from "the platform cannot enforce the rules."
- The audit record persists the platform identifier so post-hoc analysis of a multi-platform deployment can distinguish per-OS behavior.
- The daemon's spawn proxy emits a `spawn_network` audit event per CONNECT request: connector identity, requested host:port, decision (allow / capability_denied), and the spawn invocation's audit id for correlation. CONNECT does not terminate TLS, so the audit captures host:port granularity only, not full request URLs.
- The runtime caps stdout and stderr at the manifest-declared `[capabilities.spawn.limits]` byte counts (defaults: 1 MiB stdout, 256 KiB stderr) and inserts a structured truncation marker when a stream exceeds its cap. The marker is part of the captured bytes the connector reads back, ensuring downstream consumers see truncation rather than assuming completeness.

### Open implementation questions (deferred)

- *What is the v2 manifest extension for platform-conditional spawn fields (e.g., `programs.linux`, `programs.darwin`)?* — deferred until a connector requires it.
- *Does macOS get a non-`sandbox-exec` alternative when Apple removes the binary?* — deferred until Apple announces deprecation removal, tracked in [ADR-0002](/adr/0002-connector-model)'s spawn-primitive section.
- *Does Windows graduate to AppContainer?* — deferred. The job-object plus restricted-token mechanism is v1.
- *Resource limits on the subprocess (CPU, memory, wall time)?* — out of scope for this ADR. The runtime will apply a wall-time bound at the host-function layer ([ADR-0010](/adr/0010-failure-handling) error classes apply on timeout). Per-subprocess `rlimit` and Windows-job CPU caps are a future refinement.

## Examples

### Linux: subprocess invocation under namespaces and Landlock

```
runtime calls clone(CLONE_NEWUSER|CLONE_NEWNS|CLONE_NEWPID|CLONE_NEWNET, ...)
  in child:
    set up UID map: 0 1000 1
    pivot_root onto runtime-constructed mount tree:
      /tmp                       (tmpfs)
      /proc                      (private procfs)
      /Users/alr/code            (bind, read-only)        # fs_read
      /Users/alr/.cache/aileron  (bind, read-write)       # fs_write
    apply Landlock ruleset: read /Users/alr/code, write /Users/alr/.cache/aileron, deny everything else
    bind-mount /run/user/1000/aileron-proxy-<spawn-id>.sock -> /run/aileron-proxy.sock
    setenv only declared keys plus injected credentials
    setenv HTTPS_PROXY=http://127.0.0.1:54321 HTTP_PROXY=http://127.0.0.1:54321
    fork:
      parent:  spawn-shim listens on 127.0.0.1:54321, bridges to /run/aileron-proxy.sock
      child:   exec("/usr/bin/git", argv...)
```

### macOS: generated SBPL profile

```
(version 1)
(deny default)
(allow process-fork)
(allow process-exec (literal "/usr/bin/git"))
(allow file-read* (subpath "/Users/alr/code"))
(allow file-read* (literal "/Users/alr/.gitconfig"))
(allow file-write* (subpath "/Users/alr/.cache/aileron/gitcrawl"))
(deny network*)
(allow network* (remote tcp "localhost:54321"))
```

The runtime writes the profile to a tempfile and invokes `/usr/bin/sandbox-exec -f <tempfile> /usr/bin/git ...`. The loopback allow rule names the daemon's per-spawn proxy port. When the connector declares `[capabilities.network]`, the runtime also injects `HTTPS_PROXY` and `HTTP_PROXY` into the subprocess's environment pointing at the same address.

### Windows: restricted token plus job object

```
hToken = CreateRestrictedTokenW(currentToken, DISABLE_MAX_PRIVILEGE | LUA_TOKEN, deniedSids, ...)
SetTokenInformation(hToken, TokenIntegrityLevel, LOW_INTEGRITY_SID)
hJob   = CreateJobObjectW(...); set KILL_ON_JOB_CLOSE, UILIMIT_HANDLES, BREAKAWAY_OK=false
CreateProcessAsUserW(hToken, "C:\\Program Files\\Git\\bin\\git.exe", argv, ..., CREATE_SUSPENDED)
AssignProcessToJobObject(hJob, hProcess)
add WFP filter: deny outbound TCP for this PID except 127.0.0.1:54321
setenv HTTPS_PROXY=http://127.0.0.1:54321 HTTP_PROXY=http://127.0.0.1:54321
ResumeThread(hThread)
```

### Sandbox unavailability at install

A user installs a spawn-using connector on a Linux distribution with `kernel.unprivileged_userns_clone=0`. The install command exits non-zero with:

```json
{
  "error": {
    "class": "spawn_sandbox_unavailable",
    "boundary": "sandbox",
    "connector": "github://aileron/gitcrawl@1.0.0",
    "platform": "linux",
    "reason": "unprivileged user namespaces disabled (kernel.unprivileged_userns_clone=0)",
    "remediation": "Enable unprivileged user namespaces or omit spawn-using connectors"
  }
}
```

The user gets the message at install rather than at first connector call. No spawn capability is ever granted on this host.
