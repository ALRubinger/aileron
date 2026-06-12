//go:build !darwin

package hostimport

// extract resolves the host credential bytes on non-macOS platforms
// (Linux and Windows) by reading the agent's credential file. Both
// supported agents store their credential in a file under the user's
// home directory:
//
//	Claude:  ~/.claude/.credentials.json  (%USERPROFILE%\.claude\... on Windows)
//	Codex:   ~/.codex/auth.json           (%USERPROFILE%\.codex\... on Windows)
//
// A FilePathOverride wins over the default so tests read a fixture.
//
// On Linux the agent CLI may instead store the credential in a keyring
// (libsecret/secret-tool). hostimport does not implement keyring
// extraction in v1; when the file is absent the caller sees
// ErrNotAuthenticated, whose recovery text covers both re-running login
// so the file is written and the interactive in-container path. The
// dedicated ErrLinuxKeyringUnsupported sentinel exists for callers that
// want to detect a keyring-only install explicitly once that detection
// lands; v1 maps file-absence to ErrNotAuthenticated.
func extract(agent string, opts Options) ([]byte, error) {
	path := opts.FilePathOverride
	if path == "" {
		p, err := defaultCredentialPath(agent)
		if err != nil {
			return nil, err
		}
		path = p
	}
	return readCredentialFile(path)
}
