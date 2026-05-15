// Package spawnhelper defines the post-namespace setup step that
// runs inside the spawn-sandbox namespace before the wrapped CLI
// exec's. The runtime cannot install Landlock rules or do a
// pivot_root from outside the namespace, and Go's `syscall.SysProcAttr`
// has no hook between clone() and exec() for in-namespace code,
// so the runtime re-execs the daemon binary with a hidden flag
// (`__spawn-helper`) and the daemon's main() dispatches to
// [Run] when it sees that argv.
//
// The re-exec runs as PID 1 inside the namespace ADR-0014's Linux
// section sets up; it applies Landlock filesystem restrictions
// (read scopes from `fs_read`, write scopes from `fs_write`) and
// then `syscall.Exec`s into the wrapped CLI, replacing itself.
//
// This package is split from `internal/sandbox` so `cmd/aileron`
// can import only the dispatch entry point at process start
// without dragging the full sandbox subtree in.
package spawnhelper

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
)

// Request is the directive the runtime serializes onto the
// re-exec'd helper's argv. The helper applies the declared
// Landlock rules to its own process and then exec's into the
// wrapped program.
//
// Fields are intentionally pure data (no handles, no pointers,
// no platform types) so the marshalled form is portable and
// debuggable. The transport is base64-encoded JSON via a single
// argv element after the `__spawn-helper` marker; argv was
// chosen over env or FD-passing because it survives /proc/self
// inspection (useful for post-hoc debugging) and the contents
// carry no secrets (credentials are injected into Env by the
// runtime as plain env keys the wrapped CLI reads).
type Request struct {
	// Program is the absolute path of the binary the helper
	// should exec into. Required.
	Program string `json:"program"`

	// Argv is the full argument vector (argv[0] = program name
	// the CLI sees, etc.). Required, non-empty.
	Argv []string `json:"argv"`

	// Env is the environment the wrapped CLI inherits. Each entry
	// is `KEY=value`. The runtime injects credentials and proxy
	// addresses here; the helper passes the slice through unchanged.
	Env []string `json:"env,omitempty"`

	// Cwd, when non-empty, is the working directory the helper
	// chdir's into before exec'ing. The runtime resolves any
	// `~/`-anchored manifest cwd before assembling the request,
	// so this is always an absolute host path.
	Cwd string `json:"cwd,omitempty"`

	// FSRead and FSWrite are the manifest's filesystem scopes
	// the helper translates into Landlock allow rules. Paths are
	// absolute host paths (the runtime expands `~/` before
	// assembling the request). A trailing slash conventionally
	// indicates a directory subtree; Landlock's `path-beneath`
	// rules apply to subtrees regardless.
	FSRead  []string `json:"fs_read,omitempty"`
	FSWrite []string `json:"fs_write,omitempty"`

	// ProxyUDSPath, when non-empty, is the host-filesystem path
	// of the daemon's per-spawn CONNECT proxy. The helper runs
	// [internal/sandbox.RunSpawnShim] on the namespace-local TCP
	// loopback and bridges accepted connections to this socket;
	// it injects `HTTPS_PROXY` / `HTTP_PROXY` on the wrapped
	// CLI's env to point at the in-namespace TCP listener. Empty
	// when the connector did not declare `[capabilities.network]`
	// or when the platform's network confinement does not need
	// the helper bridge (macOS / Windows).
	ProxyUDSPath string `json:"proxy_uds_path,omitempty"`
}

// HelperArgvMarker is the first argv element the runtime passes
// to the re-exec'd daemon binary to signal "you are running as
// the spawn helper; parse the request from argv[2]." Hidden from
// the user-facing CLI's help text and command dispatch.
const HelperArgvMarker = "__spawn-helper"

// EncodeRequest serializes the request into the single argv
// element the runtime passes after [HelperArgvMarker]. Base64
// shields against shells, logging systems, and `ps` truncating
// or interpreting embedded JSON metacharacters.
func EncodeRequest(req Request) (string, error) {
	if req.Program == "" {
		return "", fmt.Errorf("spawnhelper: Program is required")
	}
	if len(req.Argv) == 0 {
		return "", fmt.Errorf("spawnhelper: Argv is required")
	}
	raw, err := json.Marshal(req)
	if err != nil {
		return "", fmt.Errorf("spawnhelper: marshal request: %w", err)
	}
	return base64.StdEncoding.EncodeToString(raw), nil
}

// DecodeRequest parses the base64-encoded JSON the runtime
// embedded in argv. Returns an error when the encoding is
// malformed or required fields are absent so the helper fails
// loudly rather than silently passing a malformed request to
// `syscall.Exec`.
func DecodeRequest(encoded string) (Request, error) {
	var req Request
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return req, fmt.Errorf("spawnhelper: base64 decode: %w", err)
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return req, fmt.Errorf("spawnhelper: unmarshal request: %w", err)
	}
	if req.Program == "" {
		return req, fmt.Errorf("spawnhelper: decoded request missing Program")
	}
	if len(req.Argv) == 0 {
		return req, fmt.Errorf("spawnhelper: decoded request missing Argv")
	}
	return req, nil
}
