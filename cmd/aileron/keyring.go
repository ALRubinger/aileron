package main

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
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
// The same trust/list/revoke surface also covers Flight-Plan publishers
// (#1900): a plan frozen with `freeze --publisher <authority>` is gated at
// `aileron skill launch` against the same owner/per-repo grants this map
// holds, so trusting a publisher here trusts both their connectors and their
// Flight Plans with no functional change to the shared owners/publishers map.
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

// runKeyringTrust grants owner-level trust for a connector publisher
// (ADR-0013 per-publisher trust). Two input forms are accepted:
//
//   - A full per-repo FQN (`github://owner/repo`) fetches that repo's
//     own `keys/publisher.pub` on its default branch (per ADR-0002) and
//     writes an owner-level grant for `github://owner`, so the single
//     grant covers every connector that publisher ships.
//   - A bare owner (`github://owner`) resolves the publisher's key from
//     the Hub catalog entry's `key_url` for that owner. When no Hub
//     entry carries a resolvable `key_url`, the command errors with
//     guidance to trust via a specific connector instead of guessing a
//     profile-repo path.
//
// Writing an owner-level key the keyring already trusts is a no-op, so
// re-running is safe.
func runKeyringTrust(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "usage: aileron keyring trust <authority>")
		fmt.Fprintln(stderr, "  authority: github://owner            (trust the whole publisher, key resolved via the Hub)")
		fmt.Fprintln(stderr, "             github://owner/connector  (trust the whole publisher, key from the connector repo)")
		fmt.Fprintln(stderr, "  Trusting an owner covers every connector that publisher ships.")
		return 1
	}
	authority := args[0]
	if authority == "" {
		fmt.Fprintln(stderr, "error: authority cannot be empty")
		return 1
	}

	ownerAuthority, pub, err := resolveTrustKey(authority, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
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

	if keyring.HasOwnerKey(ownerAuthority, pub) {
		fmt.Fprintf(stdout, "Publisher already trusted: %s\n", ownerAuthority)
		fmt.Fprintf(stdout, "  Fingerprint: %s\n", fingerprint(pub))
		fmt.Fprintf(stdout, "  Keyring: %s\n", path)
		return 0
	}

	keyring.AddOwner(ownerAuthority, pub)
	if err := keyring.SaveKeyring(path); err != nil {
		fmt.Fprintf(stderr, "error: save keyring: %v\n", err)
		return 1
	}

	fmt.Fprintf(stdout, "✓ Trusted publisher %s\n", ownerAuthority)
	fmt.Fprintln(stdout, "  This covers every connector this publisher ships.")
	fmt.Fprintf(stdout, "  Fingerprint: %s\n", fingerprint(pub))
	fmt.Fprintf(stdout, "  Keyring: %s\n", path)
	return 0
}

// resolveTrustKey maps a `keyring trust` argument to the owner-level
// authority to grant and the publisher key to grant it. A full per-repo
// FQN resolves the key from that connector's own `keys/publisher.pub`;
// a bare owner resolves it from the Hub entry's `key_url`. Either way
// the returned authority is owner-level (`<scheme>://<owner>`) so the
// grant lands in the owners map per ADR-0013.
func resolveTrustKey(authority string, stderr io.Writer) (string, ed25519.PublicKey, error) {
	if isBareOwnerAuthority(authority) {
		// Normalize a trailing slash so the grant key is canonical
		// (`github://acme`, not `github://acme/`).
		ownerAuthority := strings.TrimRight(authority, "/")
		pub, err := resolveOwnerKeyFromHub(ownerAuthority, stderr)
		if err != nil {
			return "", nil, err
		}
		return ownerAuthority, pub, nil
	}
	fqn, err := cstore.ParseFQN(authority)
	if err != nil {
		return "", nil, fmt.Errorf("parse authority %s: %w", authority, err)
	}
	pub, err := fetchPublisherKey(fqn.Authority())
	if err != nil {
		return "", nil, fmt.Errorf("fetch publisher key for %s: %w", fqn.Authority(), err)
	}
	return fqn.OwnerAuthority(), pub, nil
}

// isBareOwnerAuthority reports whether s is a `<scheme>://<owner>` with
// no repo segment after the host. ParseFQN rejects this shape ("missing
// repo segment"), so the bare-owner branch is detected here before
// parsing. A trailing slash (`github://owner/`) is treated as bare so
// it routes to Hub resolution rather than a per-repo fetch with an
// empty repo.
func isBareOwnerAuthority(s string) bool {
	const sep = "://"
	idx := strings.Index(s, sep)
	if idx < 0 {
		return false
	}
	rest := s[idx+len(sep):]
	slash := strings.Index(rest, "/")
	if slash < 0 {
		return rest != ""
	}
	// `github://owner/` (trailing slash, nothing after) is still bare.
	return strings.TrimRight(rest[slash+1:], "/") == ""
}

// resolveOwnerKeyFromHub resolves a publisher's signing key from the Hub
// catalog for a bare owner authority (`github://owner`). It queries the
// connectors catalog and uses the first entry whose publisher matches
// the owner and whose `key_url` is non-empty, fetching and decoding that
// key. The key always comes from a connector's own per-repo
// `keys/publisher.pub` (the `key_url` the Hub records); this never
// guesses a `<owner>/<owner>` profile-repo path. When no entry yields a
// usable `key_url`, it returns an error pointing the user at the
// specific-connector form.
func resolveOwnerKeyFromHub(ownerAuthority string, stderr io.Writer) (ed25519.PublicKey, error) {
	// Append a placeholder repo so ParseFQN (which requires a repo
	// segment) yields the owner; trim any trailing slash on the input
	// first so `github://owner/` does not produce a double slash.
	fqn, err := cstore.ParseFQN(strings.TrimRight(ownerAuthority, "/") + "/_")
	if err != nil {
		return nil, fmt.Errorf("parse owner authority %s: %w", ownerAuthority, err)
	}
	owner := fqn.Owner
	ownerAuthority = fqn.OwnerAuthority()

	entries, code := fetchHubConnectors("", stderr)
	if code != 0 {
		return nil, fmt.Errorf("query Hub for publisher %s; trust via a specific connector, e.g. keyring trust %s/<repo>", ownerAuthority, ownerAuthority)
	}
	for _, e := range entries {
		if e.KeyURL == "" {
			continue
		}
		if !ownerMatchesEntry(owner, e) {
			continue
		}
		pub, err := fetchKeyFromURL(e.KeyURL)
		if err != nil {
			return nil, fmt.Errorf("fetch publisher key from %s (Hub key_url for %s): %w", e.KeyURL, ownerAuthority, err)
		}
		return pub, nil
	}
	return nil, fmt.Errorf("no Hub entry with a publisher key for %s; trust via a specific connector, e.g. keyring trust %s/<repo>", ownerAuthority, ownerAuthority)
}

// ownerMatchesEntry reports whether a Hub connector entry belongs to the
// named owner. The owner is matched against the entry's FQN owner
// segment (authoritative) and, as a fallback, the publisher_github
// field, so the resolution does not depend on a single field's
// population.
func ownerMatchesEntry(owner string, e hubConnectorEntry) bool {
	if e.FQN != "" {
		if fqn, err := cstore.ParseFQN(e.FQN); err == nil && fqn.Owner == owner {
			return true
		}
	}
	return e.PublisherGithub == owner
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
	return fetchKeyFromURL(keyURL)
}

// fetchKeyFromURL GETs an ed25519 public key from an absolute URL and
// decodes it (PEM or base64). Shared by fetchPublisherKey (the
// convention-path fetch) and the Hub `key_url` resolution
// (resolveOwnerKeyFromHub), so both honor the same timeout, size cap,
// and decode rules. The 404 message names the convention path because
// that is the most common cause for the per-repo fetch; the Hub path
// fetches a fully-qualified `key_url` and surfaces the same status.
func fetchKeyFromURL(keyURL string) (ed25519.PublicKey, error) {
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
	// Key the in-run memo on the owner authority so the first connector
	// of a publisher resolves trust for every sibling connector in the
	// same run: a second `<owner>/<other-repo>` maps to the same owner
	// key and short-circuits without re-prompting (suite single-prompt-
	// per-owner). A malformed authority degrades to keying on the raw
	// string, which is still safe (it just loses cross-repo sharing).
	owner := ownerAuthorityOf(authority)
	// A previously-resolved owner suppresses this run's re-prompt for any
	// sibling connector; a previously-resolved exact per-repo authority
	// suppresses only itself. Check both scopes so an owner grant earned
	// for one connector covers the next under the same publisher, while a
	// per-repo-only resolution does not.
	if (owner != "" && (s.declined[owner] || s.trusted[owner])) || s.declined[authority] || s.trusted[authority] {
		if s.declined[owner] || s.declined[authority] {
			return fmt.Errorf("publisher %s trust previously declined this run; skipping", authority)
		}
		return nil
	}
	ownerCovered, err := ensureAuthorityTrusted(authority, autoYes, stdin, stdout, stderr)
	if err != nil {
		// Decline at the granularity that was attempted: the owner when an
		// owner grant was the path, else the exact authority. Declining at
		// owner granularity matches the suite single-prompt-per-owner rule.
		if owner != "" {
			s.declined[owner] = true
		} else {
			s.declined[authority] = true
		}
		return err
	}
	// Record trust at owner granularity only when the resolution actually
	// covered the owner (owner grant present or written). A per-repo-pin
	// no-op records only the exact authority, so a sibling still prompts.
	if ownerCovered && owner != "" {
		s.trusted[owner] = true
	} else {
		s.trusted[authority] = true
	}
	return nil
}

// ownerAuthorityOf derives the owner-level authority
// (`<scheme>://<owner>`) from a per-repo authority by reusing fqn.go
// parsing. Returns "" when the authority does not parse, so callers can
// fall back to the raw string rather than widening trust on a malformed
// input (matching verify.go's degrade-to-per-repo behavior).
func ownerAuthorityOf(authority string) string {
	fqn, err := cstore.ParseFQN(authority)
	if err != nil {
		return ""
	}
	return fqn.OwnerAuthority()
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
// The boolean return reports whether trust now resolves at owner
// granularity (an owner-level grant is present or was just written), as
// opposed to a per-repo-only pin. trustState.ensure uses it to decide
// whether to suppress re-prompts for sibling connectors under the same
// owner this run.
func ensureAuthorityTrusted(authority string, autoYes bool, stdin io.Reader, stdout, stderr io.Writer) (bool, error) {
	path := cstore.DefaultKeyringPath()
	if path == "" {
		return false, fmt.Errorf("cannot determine home directory; set $HOME or run `aileron keyring trust %s` manually", authority)
	}
	keyring, err := cstore.LoadKeyring(path)
	if err != nil {
		return false, fmt.Errorf("load keyring %q: %w", path, err)
	}

	// Trusted predicate == verify.go's union resolution: an install
	// verifies when the owner-level grant OR the per-repo grant has a
	// key. Short-circuit on either so we never re-prompt for an
	// authority that already verifies. The owner-level scope is what an
	// accepted prompt writes; the per-repo scope honors a standalone pin
	// a user may have placed by hand.
	ownerAuthority := ownerAuthorityOf(authority)
	ownerHasKey := ownerAuthority != "" && len(keyring.OwnerKeys(ownerAuthority)) > 0
	if ownerHasKey {
		return true, nil
	}
	if len(keyring.Keys(authority)) > 0 {
		// Satisfied by a per-repo pin only: not owner-covered.
		return false, nil
	}

	fmt.Fprintln(stdout)
	fmt.Fprintf(stdout, "Publisher %s is not yet trusted.\n", authority)
	fmt.Fprintf(stdout, "  Aileron will fetch %s on the publisher's default branch\n", publisherKeyPath)
	if ownerAuthority != "" {
		fmt.Fprintf(stdout, "  and trust %s to verify signed installs from every connector\n", ownerAuthority)
		fmt.Fprintln(stdout, "  this publisher ships.")
	} else {
		fmt.Fprintln(stdout, "  and use that key to verify signed installs from this publisher.")
	}

	if !autoYes {
		ans := strings.ToLower(strings.TrimSpace(promptLine(stdin, stdout,
			fmt.Sprintf("Trust publisher %s? [y/N]: ", authority))))
		if ans != "y" && ans != "yes" {
			return false, fmt.Errorf("publisher %s not trusted; aborting", authority)
		}
	}

	// Exactly one fetch: the per-repo connector key already on the
	// convention path. The grant is written owner-level so it covers
	// every connector the publisher ships (ADR-0013) — no second fetch,
	// avoiding the #563 single-install regression.
	pub, err := fetchPublisherKey(authority)
	if err != nil {
		return false, fmt.Errorf("fetch publisher key for %s: %w", authority, err)
	}
	ownerCovered := ownerAuthority != ""
	grantAuthority := ownerAuthority
	if grantAuthority == "" {
		// Malformed authority: fall back to a per-repo grant so the
		// install can still proceed, mirroring verify.go's degrade path.
		grantAuthority = authority
		if !keyring.HasKey(grantAuthority, pub) {
			keyring.Add(grantAuthority, pub)
		}
	} else if !keyring.HasOwnerKey(grantAuthority, pub) {
		keyring.AddOwner(grantAuthority, pub)
	}
	if err := keyring.SaveKeyring(path); err != nil {
		return false, fmt.Errorf("save keyring: %w", err)
	}
	fmt.Fprintf(stdout, "✓ Trusted publisher %s\n", grantAuthority)
	fmt.Fprintf(stdout, "  Fingerprint: %s\n", fingerprint(pub))
	fmt.Fprintf(stdout, "  Keyring: %s\n", path)
	return ownerCovered, nil
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

	// Group every authority under its owner. Owner-level grants surface
	// as the owner's own fingerprints; per-repo grants nest under the
	// same owner. An owner with only per-repo grants still gets a header
	// (no owner-level line). Owners are printed sorted; per-repo
	// authorities under each owner are printed sorted.
	owners := map[string]bool{}
	perRepoByOwner := map[string][]string{}
	for _, authority := range authorities {
		owner := ownerAuthorityOf(authority)
		if owner == "" {
			// Authority that does not parse: treat it as its own owner
			// bucket so it is never silently dropped from the listing.
			owner = authority
		}
		owners[owner] = true
		if authority != owner {
			perRepoByOwner[owner] = append(perRepoByOwner[owner], authority)
		}
	}
	ownerList := make([]string, 0, len(owners))
	for owner := range owners {
		ownerList = append(ownerList, owner)
	}
	sort.Strings(ownerList)

	fmt.Fprintf(stdout, "Trusted publishers (%d):\n", len(ownerList))
	for _, owner := range ownerList {
		// OwnerKeys is the documented owner-read accessor; owner-level and
		// per-repo grants share one flat key map, so this returns exactly
		// the owner-level fingerprints for the header.
		ownerKeys := keyring.OwnerKeys(owner)
		fmt.Fprintf(stdout, "  %s  (%d owner %s)\n", owner, len(ownerKeys), pluralKeys(len(ownerKeys)))
		for _, key := range ownerKeys {
			fmt.Fprintf(stdout, "    %s\n", fingerprint(key))
		}
		repos := perRepoByOwner[owner]
		sort.Strings(repos)
		for _, repo := range repos {
			keys := keyring.Keys(repo)
			fmt.Fprintf(stdout, "    %s  (%d %s)\n", repo, len(keys), pluralKeys(len(keys)))
			for _, key := range keys {
				fmt.Fprintf(stdout, "      %s\n", fingerprint(key))
			}
		}
	}
	fmt.Fprintf(stdout, "\nKeyring: %s\n", path)
	return 0
}

// runKeyringRevoke retracts trust. Two mutually-exclusive forms:
//
//   - `revoke <authority>` removes a whole grant. An owner authority
//     (`github://owner`) drops the owner-level grant via RemoveOwner; an
//     exact per-repo authority (`github://owner/repo`) drops that
//     per-repo grant via Remove. Either way, installs that depended on
//     the removed grant fail closed until re-trusted.
//   - `revoke --key <fingerprint>` removes one key wherever it appears —
//     across every owner and per-repo authority that registered it,
//     leaving each authority's other keys intact. This is the path for
//     retiring a single rotated or compromised key. `--key=<fp>` and
//     `--key <fp>` are both accepted.
//
// Supplying both forms, or neither, is a usage error.
func runKeyringRevoke(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("keyring revoke", flag.ContinueOnError)
	flags.SetOutput(stderr)
	keyFP := flags.String("key", "", "Revoke a single key everywhere by its fingerprint (sha256:...)")
	if err := flags.Parse(args); err != nil {
		return 1
	}
	rest := flags.Args()

	switch {
	case *keyFP != "" && len(rest) > 0:
		fmt.Fprintln(stderr, "usage: aileron keyring revoke <authority>  OR  aileron keyring revoke --key <fingerprint>")
		fmt.Fprintln(stderr, "  (supply exactly one of an authority or --key, not both)")
		return 1
	case *keyFP == "" && len(rest) != 1:
		fmt.Fprintln(stderr, "usage: aileron keyring revoke <authority>  OR  aileron keyring revoke --key <fingerprint>")
		return 1
	}

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

	if *keyFP != "" {
		return revokeByFingerprint(keyring, path, *keyFP, stdout, stderr)
	}
	return revokeAuthority(keyring, path, rest[0], stdout, stderr)
}

// revokeAuthority removes a whole grant: the owner-level grant when the
// argument is a bare owner, else the exact per-repo grant.
func revokeAuthority(keyring *cstore.Ed25519Keyring, path, authority string, stdout, stderr io.Writer) int {
	removed := false
	if owner := ownerAuthorityOf(authority); owner == authority {
		removed = keyring.RemoveOwner(authority)
	} else {
		removed = keyring.Remove(authority)
	}
	if !removed {
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

// revokeByFingerprint removes the key whose fingerprint matches fp from
// every authority (owner-level and per-repo) that registered it. An
// authority emptied by the removal is dropped entirely (RemoveKey).
func revokeByFingerprint(keyring *cstore.Ed25519Keyring, path, fp string, stdout, stderr io.Writer) int {
	removedFrom := 0
	// Snapshot authorities first: RemoveKey can delete an authority,
	// which would otherwise mutate the set mid-iteration.
	for _, authority := range keyring.Authorities() {
		for _, key := range keyring.Keys(authority) {
			if fingerprint(key) == fp {
				if keyring.RemoveKey(authority, key) {
					removedFrom++
				}
			}
		}
	}
	if removedFrom == 0 {
		fmt.Fprintf(stdout, "No key with fingerprint %s (no change)\n", fp)
		return 0
	}
	if err := keyring.SaveKeyring(path); err != nil {
		fmt.Fprintf(stderr, "error: save keyring: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "✓ Revoked key %s from %d %s\n", fp, removedFrom, pluralAuthorities(removedFrom))
	fmt.Fprintf(stdout, "  Keyring: %s\n", path)
	return 0
}

func pluralAuthorities(n int) string {
	if n == 1 {
		return "authority"
	}
	return "authorities"
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
