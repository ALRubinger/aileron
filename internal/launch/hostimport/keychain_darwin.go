//go:build darwin

package hostimport

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
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
		// keyring service only when the file is absent.
		path, err := defaultCredentialPath(agent)
		if err != nil {
			return nil, err
		}
		if path != "" {
			if _, statErr := os.Stat(path); statErr == nil {
				return readCredentialFile(path)
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

// readKeychain shells out to `security find-generic-password -s
// <service> -w` to read a generic-password item's password bytes. When
// keychainPath is non-empty it is appended so the read targets a
// throwaway test keychain rather than the login keychain.
//
// The -w flag prints the password followed by a trailing newline; the
// stored credential bytes must not carry that newline, so it is
// trimmed. A missing item exits non-zero (the password "could not be
// found"); that maps to ErrNotAuthenticated. An empty password maps to
// ErrNotAuthenticated for the same reason a zero-byte file does.
func readKeychain(service, keychainPath string) ([]byte, error) {
	args := []string{"find-generic-password", "-s", service, "-w"}
	if keychainPath != "" {
		args = append(args, keychainPath)
	}
	cmd := exec.Command("/usr/bin/security", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// security exits non-zero when the item is absent (exit 44,
		// "could not be found"). Treat any non-zero exit as "no usable
		// credential" so the caller surfaces the recovery path rather
		// than a raw exec error.
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return nil, ErrNotAuthenticated
		}
		return nil, err
	}
	// -w appends a single trailing newline after the password. Trim only
	// that trailing newline; the credential JSON itself never ends in a
	// bare \n the keychain added, and inner newlines are preserved.
	out := bytes.TrimSuffix(stdout.Bytes(), []byte("\n"))
	if len(out) == 0 {
		return nil, ErrNotAuthenticated
	}
	return out, nil
}
