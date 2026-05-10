---
title: "ADR-0014: Spawn Sandbox Technology"
description: "Cross-platform mechanism for confining subprocesses spawned by the runtime on a connector's behalf"
order: 14
---


<div class="meta">
<table>
  <tr><th>Status</th><td>Accepted</td></tr>
  <tr><th>Date</th><td>2026-05-10</td></tr>
  <tr><th>Tracking</th><td><a href="https://github.com/ALRubinger/aileron/issues/508">#508</a></td></tr>
</table>
</div>

## Context

[ADR-0002](/adr/0002-connector-model) introduces the **spawn primitive**: a connector may declare `[capabilities.spawn]` in its manifest and call the runtime's `aileron_host.spawn` host function to invoke a local CLI on its behalf. That ADR commits to the property: the subprocess executes under an enforcement boundary the runtime owns. It does not commit to the mechanism. This ADR makes that choice.

The choice matters. The spawn primitive widens the set of syscalls the runtime makes on a connector's behalf. Where [ADR-0005](/adr/0005-sandbox-choice) handles the WASM connector itself (a single sandbox layer for code running inside the runtime's address space), this ADR handles a different layer: an OS subprocess that the runtime forks. The subprocess gets a real OS identity. It can touch the filesystem, open sockets, and read environment variables. If it does any of those beyond the manifest's declaration, the spawn primitive's value collapses to convention.

Aileron developers run on Linux, macOS, and Windows. A single connector manifest must express bounds the runtime can enforce consistently across all three. The question is what set of platform mechanisms, used how, gives a defensible boundary on each.

The properties the spawn sandbox must enforce, derived from the manifest fields in [ADR-0002](/adr/0002-connector-model)'s spawn-primitive section:

1. **Filesystem scoping.** The subprocess can read files only within `fs_read` and write only within `fs_write`. No ambient access to the user's home directory, the runtime's working dir, or the rest of the filesystem.
2. **Environment scoping.** The subprocess sees only the env keys in `env_passthrough`, plus credential env vars the runtime injects per [ADR-0002](/adr/0002-connector-model)'s credential-injection rule. Nothing from the host's ambient environment.
3. **Network denial by default.** A subprocess spawned for a connector does not get network access via the spawn primitive. Network is the connector's own primitive ([ADR-0002](/adr/0002-connector-model) `[capabilities.network]`) and rides through the runtime's HTTP gate, not through the subprocess. Subprocess outbound network is denied at the sandbox boundary.
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
4. **Network namespace.** The new namespace has only a loopback interface and no routes. Outbound network calls fail at the kernel level. Network capability for a connector remains the runtime's own gate, not the subprocess's responsibility.
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
```

Defaults deny everything. Each `fs_read` entry becomes one or more `file-read*` allow rules (literal for files, subpath for directories). Each `fs_write` entry becomes a `file-write*` allow rule. The declared `programs` list becomes `process-exec` literals. Network is denied wholesale.

**`sandbox-exec` is deprecated.** Apple has carried the deprecation warning for years without removing the binary; it remains the only tool available to confine arbitrary subprocesses on macOS without code-signing entitlements. The underlying `sandbox_init` API is private and unstable. The runtime accepts the deprecation risk for v1 and tracks Apple's posture. When `sandbox-exec` is removed in a future macOS release, the spawn primitive on macOS will require either Apple's evolving alternative (currently `containerd`-style XPC-only sandboxing for App Store apps) or explicit deny-spawn until a replacement is ratified.

**Why not a hand-rolled Mach-port-based isolation.** Mach-level isolation requires entitlements and code-signing infrastructure Aileron does not yet have. SBPL via `sandbox-exec` is what every major macOS sandboxing tool (Chrome, Firefox, Docker Desktop) reaches for; the runtime uses it under the same constraints.

### Windows: job object plus restricted token

The runtime calls `CreateProcessAsUserW` with a token derived via `CreateRestrictedTokenW`, dropping the subprocess's integrity level to Low and stripping all groups except `Everyone`. The runtime assigns the subprocess to a job object via `AssignProcessToJobObject` with these limits:

- `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE`: subprocess dies if the runtime exits, no zombies.
- `JOB_OBJECT_LIMIT_DIE_ON_UNHANDLED_EXCEPTION`: clean shutdown semantics.
- `JOB_OBJECT_LIMIT_BREAKAWAY_OK` set to false; subprocesses cannot escape the job.
- `JOB_OBJECT_UILIMIT_HANDLES`: prevents access to UI objects outside the job.

Filesystem scoping comes from the restricted token plus ACLs. The runtime adjusts ACLs on `fs_write` paths to grant write to the low-integrity token; everything else is denied by default to a Low-integrity caller. `fs_read` paths whose ACLs already grant read to `Everyone` need no adjustment. Paths with stricter ACLs that the connector needs to read are denied at the kernel level by the restricted token, which is the desired behavior.

Network denial on Windows uses Windows Filtering Platform (WFP) rules applied to the subprocess's PID, or where unavailable a denylist at the firewall layer. Network capability remains the connector's own gate; the subprocess does not get its own network primitive.

**AppContainer deferred.** AppContainer would provide stronger guarantees (capability-token-based access to network, filesystem, and devices), but requires an Application Identity SID, an AppContainer profile registered with the OS, and significant manifest-side changes. The job-object plus restricted-token combination is the v1 mechanism. AppContainer is a future ADR.

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

## Consequences

### For connector authors

- Spawn-using connectors work identically on Linux, macOS, and Windows. One manifest, three platforms.
- Path declarations must be Unix-style or `~/`-anchored. Windows paths are computed by the runtime at install/run time.
- `programs` entries that do not resolve on a target platform cause install to fail on that platform. The connector author either accepts the platform constraint or extends the schema in a future ADR.
- A connector that declares `[capabilities.spawn]` is unavailable on BSDs, illumos, and any other non-supported OS. The runtime refuses install with a structured error naming the missing platform.

### For the runtime

- The runtime ships per-platform spawn implementations under `internal/sandbox/spawn_linux.go`, `internal/sandbox/spawn_darwin.go`, `internal/sandbox/spawn_windows.go`. Build tags select the right file at compile time.
- The Linux implementation uses `golang.org/x/sys/unix` and direct `syscall` calls; no external binary dependencies.
- The macOS implementation invokes `/usr/bin/sandbox-exec`; an installed macOS always has it. Absence is a structured error.
- The Windows implementation uses `golang.org/x/sys/windows` for token manipulation, job objects, and WFP rules.
- The runtime maintains a per-platform "sandbox available" probe, run at startup and at install of spawn-using connectors. The result feeds the structured `spawn_sandbox_unavailable` error.

### For testing

- Each platform implementation needs its own test plane. Cross-platform CI runs the suite on Linux, macOS, and Windows runners. Unit tests for the gate ([ADR-0002](/adr/0002-connector-model)'s `HostPolicy.CheckSpawn`) are platform-neutral and run on every runner.
- The fake-binary harness (deferred to issue #512) runs on every supported platform and gives connector repo tests a portable substrate for spawn assertions.

### For audit and security

- Every spawn call emits an audit event identifying the connector, the program, the argv pattern, the exit code, and the content hashes of stdout and stderr. The audit row is identical across platforms; the underlying mechanism is not exposed in the audit.
- Sandbox-unavailability denials emit a distinct audit class (`spawn_sandbox_unavailable`) so an operator can distinguish "the subprocess broke the rules" from "the platform cannot enforce the rules."
- The audit record persists the platform identifier so post-hoc analysis of a multi-platform deployment can distinguish per-OS behavior.

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
    setenv only declared keys plus injected credentials
    exec("/usr/bin/git", argv...)
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
```

The runtime writes the profile to a tempfile and invokes `/usr/bin/sandbox-exec -f <tempfile> /usr/bin/git ...`.

### Windows: restricted token plus job object

```
hToken = CreateRestrictedTokenW(currentToken, DISABLE_MAX_PRIVILEGE | LUA_TOKEN, deniedSids, ...)
SetTokenInformation(hToken, TokenIntegrityLevel, LOW_INTEGRITY_SID)
hJob   = CreateJobObjectW(...); set KILL_ON_JOB_CLOSE, UILIMIT_HANDLES, BREAKAWAY_OK=false
CreateProcessAsUserW(hToken, "C:\\Program Files\\Git\\bin\\git.exe", argv, ..., CREATE_SUSPENDED)
AssignProcessToJobObject(hJob, hProcess)
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
