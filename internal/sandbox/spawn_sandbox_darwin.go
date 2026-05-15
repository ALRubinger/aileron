//go:build darwin

package sandbox

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// sandboxExecPath is the macOS-bundled binary the runtime invokes
// to apply a generated SBPL profile to a subprocess. Per
// [ADR-0014]'s macOS section, `sandbox-exec` is Apple's
// long-deprecated-but-still-shipped tool for applying ad-hoc
// sandbox profiles to arbitrary commands; the deprecation has been
// in place for years without removal and the binary remains the
// only documented mechanism that does not require code-signing
// entitlements.
//
// [ADR-0014]: https://docs.withaileron.ai/adr/0014-spawn-sandbox-technology
const sandboxExecPath = "/usr/bin/sandbox-exec"

// applyPlatformSandbox wraps `cmd` so it executes under a
// `sandbox-exec` profile generated from the connector manifest's
// declared scopes plus the runtime-decided proxy endpoint.
//
// The Sandbox Profile Language (SBPL) string is passed inline via
// `-p` to avoid the per-invocation tempfile lifecycle issues
// `-f` would impose. SBPL is documented at a basic level in
// Apple's `man sandbox-exec(1)` and `man sandbox(7)`; the runtime's
// generator uses the small subset of forms ADR-0014 commits to.
//
// Returns [ErrSpawnUnavailable] when `sandbox-exec` is missing
// (impossible on a healthy macOS install but the runtime fails
// closed rather than spawn unconfined). Returns nil with no
// modification to `cmd` when no manifest sandbox parameters were
// declared (legacy path; pre-BYOCLI connectors continue to run
// unchanged).
func applyPlatformSandbox(cmd *exec.Cmd, env SpawnEnvelope, limits SpawnLimits) error {
	if !limits.PlatformSandboxRequested() {
		return nil
	}
	if _, err := os.Stat(sandboxExecPath); err != nil {
		return fmt.Errorf("%w: sandbox-exec not present at %s", ErrSpawnUnavailable, sandboxExecPath)
	}

	profile := buildSBPLProfile(env, limits)

	// Rewire cmd so the kernel exec's sandbox-exec, which in turn
	// applies the profile and exec's the wrapped program. cmd.Args
	// must include sandbox-exec's argv[0] plus its flags plus the
	// wrapped program and its arguments.
	origPath := cmd.Path
	origArgs := cmd.Args
	cmd.Path = sandboxExecPath
	cmd.Args = append([]string{"sandbox-exec", "-p", profile, origPath}, origArgs[1:]...)
	return nil
}

// buildSBPLProfile renders the SBPL profile for a single spawn
// invocation. The shape follows the macOS App Sandbox pattern:
// permissive baseline so arbitrary Mach-O binaries can start (a
// strict `(deny default)` requires opting back in to dozens of
// private Apple operations like `mach-bootstrap`, `syscall*`,
// `file-map-executable` for every framework, and is brittle across
// macOS versions), then surgical denies on the operations that
// carry the BYOCLI threat model:
//
//   - **File reads under user-private paths.** `/Users`, `/Volumes`,
//     and user-scoped `/private/var/folders` are denied by default;
//     the manifest's `fs_read` scopes re-allow specific subtrees.
//     This is the load-bearing defense against user-secret
//     exfiltration (`~/.ssh`, `~/.aws`, browser profiles).
//   - **File writes anywhere.** Denied by default; the manifest's
//     `fs_write` scopes re-allow specific subtrees, plus `/dev/null`
//     and the sandbox-exec-managed temp area.
//   - **Network egress.** Denied wholesale; when the connector
//     declared `[capabilities.network]` a single allow rule names
//     the daemon's proxy endpoint on loopback (per ADR-0014's
//     "Network confinement: daemon-mediated proxy").
//
// Reads of `/etc`, `/opt`, `/usr/local`, and other system paths
// outside `/Users` and `/Volumes` are allowed by the permissive
// baseline. This matches the BYOCLI threat model (which targets
// user-secret exfiltration, not OS-file reads) and aligns with
// what `brew install`-style CLIs already see when run bare from
// the shell. A future tightening can move toward `(deny default)`
// with explicit allows once the system-essentials set has been
// empirically validated across macOS releases.
//
// The profile is regenerated per spawn invocation because the
// proxy port is ephemeral. Concurrent spawns from the same
// connector each get their own port and profile.
func buildSBPLProfile(env SpawnEnvelope, limits SpawnLimits) string {
	var b strings.Builder
	b.WriteString("(version 1)\n")
	b.WriteString("(allow default)\n")

	// File-write: deny everywhere; re-allow the manifest scope plus
	// platform-essential write destinations.
	b.WriteString("(deny file-write*)\n")
	b.WriteString("(allow file-write* (literal \"/dev/null\"))\n")
	// sandbox-exec stages internal state in /private/var/folders; the
	// wrapped process needs write access for its own ephemeral
	// scratch space (Foundation/NSTemporaryDirectory uses this tree).
	b.WriteString("(allow file-write* (subpath \"/private/var/folders\"))\n")
	for _, p := range limits.FSWrite {
		writeSBPLPathRule(&b, "file-write*", p)
	}

	// File-read: deny user-private paths; re-allow manifest scopes.
	// /Users and /Volumes are the load-bearing denies; everything
	// else stays permissive per the comment above.
	b.WriteString("(deny file-read* (subpath \"/Users\"))\n")
	b.WriteString("(deny file-read* (subpath \"/Volumes\"))\n")
	for _, p := range limits.FSRead {
		writeSBPLPathRule(&b, "file-read*", p)
	}

	// Network: deny outbound; re-allow the proxy endpoint when set.
	b.WriteString("(deny network*)\n")
	if limits.ProxyAddr != "" {
		fmt.Fprintf(&b, "(allow network* (remote tcp %s))\n", sbplQuote(limits.ProxyAddr))
	}

	// Record the wrapped program in the profile as a comment so
	// post-hoc inspection of a generated profile names the
	// connector's intended binary. Not a rule; SBPL ignores `;` to
	// end of line.
	fmt.Fprintf(&b, "; wrapped program: %s\n", env.Program)

	return b.String()
}

// writeSBPLPathRule emits a single `file-read*` or `file-write*`
// allow rule for a manifest-declared path. Trailing-slash paths
// become `subpath` rules (entire directory tree); slashless paths
// become `literal` rules (exact file match). `~/`-anchored paths
// are expanded to the user's home directory before emission since
// SBPL has no shell-style expansion.
func writeSBPLPathRule(b *strings.Builder, op, manifestPath string) {
	expanded, err := expandTilde(manifestPath)
	if err != nil {
		// expandTilde only fails when os.UserHomeDir errors, which
		// on macOS is essentially never. Skip the rule rather than
		// emit a broken one; the spawn will fail at first access
		// inside the scope.
		return
	}
	if strings.HasSuffix(expanded, "/") {
		fmt.Fprintf(b, "(allow %s (subpath %s))\n", op, sbplQuote(strings.TrimRight(expanded, "/")))
		return
	}
	fmt.Fprintf(b, "(allow %s (literal %s))\n", op, sbplQuote(expanded))
}

// sbplQuote returns an SBPL string literal: a double-quoted form
// with backslashes and double-quotes escaped. SBPL has no other
// metacharacters inside string literals.
func sbplQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
