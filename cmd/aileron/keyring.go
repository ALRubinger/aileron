package main

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ALRubinger/aileron/internal/cstore"
)

// publisherKeyPath is the conventional location of a connector
// publisher's ed25519 public key inside the source repo (per
// ADR-0002). One arg `aileron keyring trust <authority>` fetches
// `<raw-host>/<owner>/<repo>/HEAD/keys/publisher.pub`.
const publisherKeyPath = "keys/publisher.pub"

// rawGitHubBase is the base URL for fetching files at a ref from
// public GitHub repositories. Overridable in tests.
var rawGitHubBase = "https://raw.githubusercontent.com"

// publisherKeyFetchTimeout caps the auto-fetch HTTP call so a stalled
// network does not leave the user staring at a quiet terminal.
const publisherKeyFetchTimeout = 15 * time.Second

// runKeyring dispatches `aileron keyring <subcommand>` to one of the
// keyring management subcommands. The keyring is the v1 source of
// trust for connector signature verification (per ADR-0002): every
// `aileron connector install` checks the binary's signature against
// keys registered for the FQN's authority. Without an entry, install
// fails closed with class signature_failure.
//
// The keyring file lives at `~/.aileron/keyring.json`; users edit it
// to authorize a publisher (or use these subcommands).
func runKeyring(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: aileron keyring <trust|list|revoke> [args...]")
		return 1
	}
	switch args[0] {
	case "trust":
		return runKeyringTrust(args[1:], stdout, stderr)
	case "list":
		return runKeyringList(args[1:], stdout, stderr)
	case "revoke":
		return runKeyringRevoke(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown keyring subcommand: %q\n", args[0])
		fmt.Fprintln(stderr, "usage: aileron keyring <trust|list|revoke> [args...]")
		return 1
	}
}

// runKeyringTrust fetches a publisher's ed25519 public key from the
// conventional `keys/publisher.pub` path on the source repo's default
// branch (per ADR-0002) and adds it to the local keyring. Adding a
// key the keyring already trusts for the same authority is a no-op,
// so re-running is safe.
func runKeyringTrust(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "usage: aileron keyring trust <authority>")
		fmt.Fprintln(stderr, "  authority: e.g. github://ALRubinger/aileron-connector-google")
		return 1
	}
	authority := args[0]
	if authority == "" {
		fmt.Fprintln(stderr, "error: authority cannot be empty")
		return 1
	}

	pub, err := fetchPublisherKey(authority)
	if err != nil {
		fmt.Fprintf(stderr, "error: fetch publisher key for %s: %v\n", authority, err)
		return 1
	}

	path := cstore.DefaultKeyringPath()
	if path == "" {
		fmt.Fprintln(stderr, "error: cannot determine home directory; set $HOME or write ~/.aileron/keyring.json by hand")
		return 1
	}
	keyring, err := cstore.LoadKeyring(path)
	if err != nil {
		fmt.Fprintf(stderr, "error: load keyring %q: %v\n", path, err)
		return 1
	}

	if keyring.HasKey(authority, pub) {
		fmt.Fprintf(stdout, "Known publisher already trusted: %s\n", authority)
		fmt.Fprintf(stdout, "  Fingerprint: %s\n", fingerprint(pub))
		fmt.Fprintf(stdout, "  Keyring: %s\n", path)
		return 0
	}

	keyring.Add(authority, pub)
	if err := keyring.SaveKeyring(path); err != nil {
		fmt.Fprintf(stderr, "error: save keyring: %v\n", err)
		return 1
	}

	fmt.Fprintf(stdout, "✓ Trusted publisher %s\n", authority)
	fmt.Fprintf(stdout, "  Fingerprint: %s\n", fingerprint(pub))
	fmt.Fprintf(stdout, "  Keyring: %s\n", path)
	return 0
}

// fetchPublisherKey downloads the ed25519 public key a connector
// publisher commits at `keys/publisher.pub` on the default branch
// (per ADR-0002). v1 supports `github://` authorities only.
//
// The HTTP call is anonymous — the convention path is on the public
// internet by definition, so no token plumbing is needed here.
func fetchPublisherKey(authority string) (ed25519.PublicKey, error) {
	fqn, err := cstore.ParseFQN(authority)
	if err != nil {
		return nil, fmt.Errorf("parse authority: %w", err)
	}
	if fqn.Scheme != "github" {
		return nil, fmt.Errorf("auto-fetch supports github:// authorities only (got %s://)", fqn.Scheme)
	}

	// `HEAD` resolves to the repo's default branch on raw.githubusercontent.com,
	// so we never have to ask the API "what's the default branch?" first.
	keyURL := fmt.Sprintf("%s/%s/%s/HEAD/%s",
		rawGitHubBase,
		url.PathEscape(fqn.Owner),
		url.PathEscape(fqn.Repo),
		publisherKeyPath,
	)

	client := &http.Client{Timeout: publisherKeyFetchTimeout}
	resp, err := client.Get(keyURL)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", keyURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: HTTP %d (publisher must commit %s on the default branch)",
			keyURL, resp.StatusCode, publisherKeyPath)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	pub, err := decodePublicKey(body)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", keyURL, err)
	}
	return pub, nil
}

// trustState tracks per-run trust decisions across one CLI
// invocation. Suite installs (#564) thread the same state through
// every action so a publisher's prompt fires at most once per run.
// Both maps key on FQN authority strings (`<scheme>://<owner>/<repo>`).
//
//   - trusted:  authorities that have been (or already were) trusted
//               this run. A second prompt is suppressed.
//   - declined: authorities the user said "no" to (or fetch failed
//               for) this run. Subsequent actions whose authority is
//               in declined skip silently with a one-line summary
//               line. A re-run of the command clears the state.
type trustState struct {
	trusted  map[string]bool
	declined map[string]bool
}

func newTrustState() *trustState {
	return &trustState{
		trusted:  map[string]bool{},
		declined: map[string]bool{},
	}
}

// ensure is the suite-aware wrapper around ensureAuthorityTrusted.
// It short-circuits when the authority has already been resolved
// (trusted or declined) earlier in the same run, otherwise it falls
// through to the prompt. Any failure marks the authority declined
// so subsequent same-authority actions in the suite skip without
// re-prompting.
func (s *trustState) ensure(authority string, autoYes bool, stdin io.Reader, stdout, stderr io.Writer) error {
	if s.declined[authority] {
		return fmt.Errorf("publisher %s trust previously declined this run; skipping", authority)
	}
	if s.trusted[authority] {
		return nil
	}
	if err := ensureAuthorityTrusted(authority, autoYes, stdin, stdout, stderr); err != nil {
		s.declined[authority] = true
		return err
	}
	s.trusted[authority] = true
	return nil
}

// ensureAuthorityTrusted is the install-time bridge to the keyring:
// it checks whether the supplied authority has any keys registered
// and, if not, prompts the operator (or proceeds when autoYes=true)
// to fetch + add the publisher's `keys/publisher.pub`. Returns nil
// when the authority is now trusted (already was, or just got added),
// non-nil when the user declined or the fetch/persist failed.
//
// This is what collapses the historical "run `aileron keyring trust`
// before `aileron action add`" two-step into the single-command
// install flow per issue #563. Same fetch + verify path as the
// standalone trust subcommand — re-uses fetchPublisherKey and the
// keyring helpers so the policy stays in one place.
//
// Single-action callers can use this directly. Suite callers should
// thread a *trustState through trustState.ensure so the same
// authority isn't prompted twice across the suite.
func ensureAuthorityTrusted(authority string, autoYes bool, stdin io.Reader, stdout, stderr io.Writer) error {
	path := cstore.DefaultKeyringPath()
	if path == "" {
		return fmt.Errorf("cannot determine home directory; set $HOME or run `aileron keyring trust %s` manually", authority)
	}
	keyring, err := cstore.LoadKeyring(path)
	if err != nil {
		return fmt.Errorf("load keyring %q: %w", path, err)
	}
	if len(keyring.Keys(authority)) > 0 {
		return nil
	}

	fmt.Fprintln(stdout)
	fmt.Fprintf(stdout, "Publisher %s is not yet trusted.\n", authority)
	fmt.Fprintf(stdout, "  Aileron will fetch %s on the publisher's default branch\n", publisherKeyPath)
	fmt.Fprintln(stdout, "  and use that key to verify signed installs from this publisher.")

	if !autoYes {
		ans := strings.ToLower(strings.TrimSpace(promptLine(stdin, stdout,
			fmt.Sprintf("Trust publisher %s? [y/N]: ", authority))))
		if ans != "y" && ans != "yes" {
			return fmt.Errorf("publisher %s not trusted; aborting", authority)
		}
	}

	pub, err := fetchPublisherKey(authority)
	if err != nil {
		return fmt.Errorf("fetch publisher key for %s: %w", authority, err)
	}
	keyring.Add(authority, pub)
	if err := keyring.SaveKeyring(path); err != nil {
		return fmt.Errorf("save keyring: %w", err)
	}
	fmt.Fprintf(stdout, "✓ Trusted publisher %s\n", authority)
	fmt.Fprintf(stdout, "  Fingerprint: %s\n", fingerprint(pub))
	fmt.Fprintf(stdout, "  Keyring: %s\n", path)
	return nil
}

// runKeyringList prints the trusted publishers and a fingerprint per
// registered key. Stable output order — authorities sorted, keys
// printed in the order the keyring loaded them.
func runKeyringList(args []string, stdout, stderr io.Writer) int {
	if len(args) != 0 {
		fmt.Fprintln(stderr, "usage: aileron keyring list")
		return 1
	}
	path := cstore.DefaultKeyringPath()
	if path == "" {
		fmt.Fprintln(stderr, "error: cannot determine home directory")
		return 1
	}
	keyring, err := cstore.LoadKeyring(path)
	if err != nil {
		// Missing file → empty keyring; LoadKeyring returns nil error.
		// Anything else here is a real malformation.
		fmt.Fprintf(stderr, "error: load keyring %q: %v\n", path, err)
		return 1
	}

	authorities := keyring.Authorities()
	if len(authorities) == 0 {
		fmt.Fprintln(stdout, "No trusted publishers.")
		fmt.Fprintf(stdout, "  Keyring: %s\n", path)
		fmt.Fprintln(stdout, "  Add one with `aileron keyring trust <authority>`.")
		return 0
	}

	fmt.Fprintf(stdout, "Trusted publishers (%d):\n", len(authorities))
	for _, authority := range authorities {
		keys := keyring.Keys(authority)
		fmt.Fprintf(stdout, "  %s  (%d %s)\n", authority, len(keys), pluralKeys(len(keys)))
		for _, key := range keys {
			fmt.Fprintf(stdout, "    %s\n", fingerprint(key))
		}
	}
	fmt.Fprintf(stdout, "\nKeyring: %s\n", path)
	return 0
}

// runKeyringRevoke removes every key registered for a publisher.
// Subsequent install attempts for that authority fail closed until
// the publisher is re-trusted. There is no per-key revocation in v1 —
// rotation under one authority is rare enough that the additional
// flag space is not worth carrying.
func runKeyringRevoke(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "usage: aileron keyring revoke <authority>")
		return 1
	}
	authority := args[0]
	path := cstore.DefaultKeyringPath()
	if path == "" {
		fmt.Fprintln(stderr, "error: cannot determine home directory")
		return 1
	}
	keyring, err := cstore.LoadKeyring(path)
	if err != nil {
		fmt.Fprintf(stderr, "error: load keyring %q: %v\n", path, err)
		return 1
	}
	if !keyring.Remove(authority) {
		fmt.Fprintf(stdout, "Not trusted: %s (no change)\n", authority)
		return 0
	}
	if err := keyring.SaveKeyring(path); err != nil {
		fmt.Fprintf(stderr, "error: save keyring: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "✓ Revoked publisher %s\n", authority)
	fmt.Fprintf(stdout, "  Keyring: %s\n", path)
	return 0
}

// decodePublicKey decodes an ed25519 public key from raw bytes. Tries
// PEM first (the format `openssl pkey -pubout` produces); falls back
// to trimming whitespace and treating the contents as a base64 raw
// key. Either form is acceptable in v1 — connector authors typically
// commit PEM as their `keys/publisher.pub`, but raw base64 is also a
// reasonable thing to find on the convention path.
func decodePublicKey(data []byte) (ed25519.PublicKey, error) {
	if pub, pemErr := cstore.ParsePEMPublicKey(data); pemErr == nil {
		return pub, nil
	}
	// Fall back to base64 raw form: strip whitespace then decode.
	trimmed := stripWhitespace(string(data))
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding,
		base64.RawStdEncoding,
		base64.URLEncoding,
		base64.RawURLEncoding,
	} {
		if raw, err := enc.DecodeString(trimmed); err == nil {
			if len(raw) == ed25519.PublicKeySize {
				return ed25519.PublicKey(raw), nil
			}
			return nil, fmt.Errorf("decoded %d bytes; ed25519 public key must be %d bytes",
				len(raw), ed25519.PublicKeySize)
		}
	}
	return nil, fmt.Errorf("not PEM and not base64-encoded raw ed25519 public key")
}

// stripWhitespace removes ASCII whitespace from s. Used to tolerate
// pasted keys with stray newlines / spaces.
func stripWhitespace(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case ' ', '\t', '\n', '\r':
			continue
		default:
			out = append(out, s[i])
		}
	}
	return string(out)
}

// fingerprint returns a short, stable identifier for a public key —
// the first 16 hex characters of SHA-256(key). Long enough to
// disambiguate keys at a glance, short enough to fit on a terminal
// line without wrapping. Same shape of fingerprint as the canonical
// ssh-keygen / git short-hash idioms.
func fingerprint(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return "sha256:" + base64.RawStdEncoding.EncodeToString(sum[:])[:22]
}

func pluralKeys(n int) string {
	if n == 1 {
		return "key"
	}
	return "keys"
}
