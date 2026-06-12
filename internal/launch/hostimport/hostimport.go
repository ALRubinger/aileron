// Package hostimport reads an already-authenticated host install's
// credential state for a supported coding agent so the bytes can be
// seeded into the Aileron vault.
//
// The package is the extraction half of `aileron auth <agent>
// --import-from-host` (issue #986). It resolves the raw credential
// bytes for an agent from whichever host location that agent's CLI
// writes — a file under the user's home directory on Linux/Windows, or
// the macOS Keychain via `security find-generic-password`. It performs
// no validation of the envelope: the caller validates by running the
// bytes through the agent's AuthSpec FileBinding Capture, which is the
// same validation the launcher performs on a clean-exit capture. This
// keeps the validation logic in one place (the agent definitions) and
// makes hostimport a pure byte-extraction surface.
//
// The package is split across build-tagged files so the macOS Keychain
// shell-out (keychain_darwin.go) and the Linux/Windows file read
// (file_other.go) each live where they compile and run. Platform-gated
// CI jobs run the matching tests on the runner where the platform
// exists; see .github/workflows/ci.yml.
//
// Extraction matrix (authoritative; ADR-0025):
//
//	Claude — Linux:    ~/.claude/.credentials.json
//	         macOS:    security -s "Claude Code-credentials" -w
//	         Windows:  %USERPROFILE%\.claude\.credentials.json
//	Codex  — Linux:    ~/.codex/auth.json
//	         macOS:    ~/.codex/auth.json (preferred), else "Codex Auth" keyring
//	         Windows:  %USERPROFILE%\.codex\auth.json
//
// The Linux keyring (libsecret/secret-tool) path is deliberately
// unsupported in v1: it returns ErrLinuxKeyringUnsupported naming the
// file-mode and interactive-login recovery paths.
package hostimport

import (
	"errors"
	"fmt"
)

// Agent identifiers hostimport recognizes. Only these two have a host
// install whose credential state hostimport knows how to read; every
// other registry agent is rejected with ErrUnsupportedAgent.
const (
	AgentClaude = "claude"
	AgentCodex  = "codex"
)

// Default keychain/service names the macOS `security` shell-out queries.
// They mirror the service names Claude Code and Codex CLI register their
// generic-password items under. Tests override them via Options so reads
// never touch the operator's real Keychain items.
const (
	// DefaultClaudeKeychainService is the generic-password service name
	// Claude Code stores its credentials under on macOS.
	DefaultClaudeKeychainService = "Claude Code-credentials"
	// DefaultCodexKeychainService is the generic-password service name
	// Codex CLI stores its credentials under on macOS keyring mode.
	DefaultCodexKeychainService = "Codex Auth"
)

// ErrUnsupportedAgent is returned by Extract when the agent name is not
// claude or codex. Other registry agents (goose, opencode, pi) have no
// host-import support in v1.
var ErrUnsupportedAgent = errors.New("hostimport: agent does not support host import")

// ErrNotAuthenticated is returned when the host install has no usable
// credential state: the credential file is absent or empty, or the
// macOS Keychain item does not exist. The CLI surfaces this with the
// in-container interactive-login recovery path.
var ErrNotAuthenticated = errors.New("hostimport: host install is not authenticated")

// ErrLinuxKeyringUnsupported is returned when a Linux host stores the
// agent credential in a keyring (libsecret/secret-tool) rather than the
// file location hostimport reads. v1 does not implement keyring
// extraction; the error names the file-mode and interactive-login
// recovery paths.
var ErrLinuxKeyringUnsupported = errors.New("hostimport: Linux keyring extraction is not supported in v1; use file mode (re-run the agent's login so it writes the credential file) or interactive in-container login (aileron launch <agent> --sandbox=docker)")

// Options carries the test seams that let extraction target fixtures
// instead of the operator's real host install. Production callers pass
// the zero value, which resolves the real default file paths and
// keychain service names.
type Options struct {
	// FilePathOverride, when non-empty, is the exact path the file-mode
	// extractor reads instead of the agent's default
	// (~/.claude/.credentials.json or ~/.codex/auth.json). Tests point
	// this at a fixture under t.TempDir().
	FilePathOverride string

	// KeychainService, when non-empty, overrides the macOS Keychain
	// service name the `security` shell-out queries. Tests set this to a
	// throwaway service so reads never touch the real Claude/Codex items.
	KeychainService string

	// KeychainPath, when non-empty, appends a keychain argument to the
	// `security` invocation so reads target a throwaway test keychain
	// rather than the user's login keychain. Production leaves this
	// empty (security searches the default search list).
	KeychainPath string
}

// Extract reads the raw host credential bytes for the given agent. It
// returns the bytes verbatim with no validation or munging; the caller
// validates by running them through the agent's Capture. The agent must
// be AgentClaude or AgentCodex; any other name returns ErrUnsupportedAgent.
//
// The concrete extraction is platform-specific: on macOS it shells out
// to `security` (with a file-mode fallback for Codex); on Linux/Windows
// it reads the credential file. A missing/empty source maps to
// ErrNotAuthenticated.
func Extract(agent string, opts Options) ([]byte, error) {
	switch agent {
	case AgentClaude, AgentCodex:
		return extract(agent, opts)
	default:
		return nil, fmt.Errorf("%w: %q (supported: %s, %s)",
			ErrUnsupportedAgent, agent, AgentClaude, AgentCodex)
	}
}
