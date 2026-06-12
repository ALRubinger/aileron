package hostimport

import (
	"os"
	"path/filepath"
)

// defaultCredentialPath returns the host file path the agent's CLI
// writes its credential to, resolved from the user's home directory.
// os.UserHomeDir resolves $HOME on Linux/macOS and %USERPROFILE% on
// Windows, so the same logic yields the right path on every OS.
//
// Claude:  ~/.claude/.credentials.json
// Codex:   ~/.codex/auth.json
//
// The agent has already been validated as claude or codex by Extract,
// so the default branch is unreachable in practice; it returns an empty
// path which the caller maps to ErrNotAuthenticated.
func defaultCredentialPath(agent string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	switch agent {
	case AgentClaude:
		return filepath.Join(home, ".claude", ".credentials.json"), nil
	case AgentCodex:
		return filepath.Join(home, ".codex", "auth.json"), nil
	default:
		return "", nil
	}
}

// readCredentialFile reads the credential file at path verbatim. An
// absent file or an empty file both map to ErrNotAuthenticated: a
// zero-byte credential is as useless as a missing one and the recovery
// path is identical. Any other read error (permission denied, IO
// failure) is returned wrapped so the operator sees the underlying
// cause.
//
// The bytes are returned whole-file with no trailing-newline munging,
// mirroring runVaultPut's --from-file semantics: agent credential files
// are exact-byte artifacts.
func readCredentialFile(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotAuthenticated
		}
		return nil, err
	}
	if len(data) == 0 {
		return nil, ErrNotAuthenticated
	}
	return data, nil
}
