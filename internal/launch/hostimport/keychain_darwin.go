//go:build darwin

package hostimport

import (
	"errors"
)

// extract resolves the host credential bytes on macOS.
//
// Claude is keychain-only: Claude Code stores .credentials.json content
// in the macOS Keychain under the "Claude Code-credentials" service, so
// hostimport reads it with `security find-generic-password -w`.
//
// Codex supports both modes. The Codex CLI writes ~/.codex/auth.json in
// the common case, and only falls back to the Keychain ("Codex Auth"
// service) when configured for keyring storage. hostimport prefers the
// file when it exists (it is the cheaper, non-interactive read) and
// falls back to the Keychain otherwise. A FilePathOverride forces the
// file path regardless of agent.
//
// The first real Keychain read prompts the user with a macOS access
// dialog; the operator approves it once. Tests never hit that dialog:
// they point KeychainPath at a throwaway keychain pre-populated with
// set-key-partition-list so the read is non-interactive.
func extract(agent string, opts Options) ([]byte, error) {
	if opts.FilePathOverride != "" {
		return readCredentialFile(opts.FilePathOverride)
	}

	switch agent {
	case AgentClaude:
		service := opts.KeychainService
		if service == "" {
			service = DefaultClaudeKeychainService
		}
		return readKeychain(service, opts.KeychainPath)
	case AgentCodex:
		// Prefer the file when the Codex CLI wrote one; fall back to the
		// keyring service only when the file is absent. Read directly
		// rather than stat-then-read so a file that exists but is
		// unreadable surfaces its real error instead of silently falling
		// back to the keychain (and so there is no TOCTOU window).
		//
		// Accepted gap (ADR-0025): a stale auth.json masks a fresher
		// Keychain entry, because we never read the Keychain when the file
		// exists. Comparing the two on every import would force the macOS
		// access dialog each time and defeat the non-interactive file read.
		// The gap fails loud, not silent: a stale file with an expired,
		// unrefreshable token surfaces a clear re-login error rather than a
		// wrong credential. Recovery is to remove the stale file.
		path, err := defaultCredentialPath(agent)
		if err != nil {
			return nil, err
		}
		if path != "" {
			data, readErr := readCredentialFile(path)
			if readErr == nil {
				return data, nil
			}
			// Only the absent/empty case (ErrNotAuthenticated) falls
			// through to the keychain; any other read error propagates.
			if !errors.Is(readErr, ErrNotAuthenticated) {
				return nil, readErr
			}
		}
		service := opts.KeychainService
		if service == "" {
			service = DefaultCodexKeychainService
		}
		return readKeychain(service, opts.KeychainPath)
	default:
		return nil, ErrNotAuthenticated
	}
}
