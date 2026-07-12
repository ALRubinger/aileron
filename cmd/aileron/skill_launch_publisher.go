package main

import (
	"crypto/ed25519"
	"fmt"
	"io"
	"strings"

	"github.com/ALRubinger/aileron/internal/cstore"
	"github.com/ALRubinger/aileron/internal/flightplan/runtime"
)

// keyringPublisherVerifier is the CLI-side runtime.PublisherVerifier impl
// (#1900). It re-reads the operator's keyring at path on every launch (like
// cstore.ReloadingKeyring) so an out-of-band `aileron keyring trust ...` write
// is observed without a restart, resolves the plan's verified signing key
// against the owner∪per-repo union for the declared publisher, and refuses to
// run when the publisher is not trusted (fail-closed).
//
// diag is where the trust/conflict diagnostic line is written; it mirrors
// containerImageRunner.diag. Production wires it to os.Stderr so the operator
// sees the decision on stderr, never stdout (which carries the launch result).
type keyringPublisherVerifier struct {
	// path is the keyring file re-read on every VerifyPublisher call. Empty
	// means the home directory could not be resolved; LoadKeyring treats that
	// as a missing file (empty keyring), so an unresolvable path fails closed
	// for a publisher-declaring plan rather than skipping the gate.
	path string
	// op labels the operation this verifier gates ("launch" or "install") so
	// the fail-closed refusals read for the command the operator actually ran.
	// The same keyringPublisherVerifier is wired on both the launch and install
	// paths (#1900); only the message prefix and the trust-remediation tail
	// differ by op. Empty defaults to "launch" so a zero-value struct (as some
	// tests construct) keeps the historical launch wording.
	op string
	// planName is the frozen plan being gated. When set, the fail-closed refusal
	// suggests the WORKING self-trust command `aileron keyring trust --plan
	// <planName>` (#2136), which grants the plan's OWN signing key at its
	// per-repo publisher authority. The bare `keyring trust <publisher>` a
	// refusal used to print fetches the repo's committed publisher.pub, a
	// different key that no-ops when the org already has an owner grant, so it
	// never unblocks self-launch. Empty falls back to the bare-publisher hint.
	planName string
	// diag receives the one-line trust/conflict diagnostic. Nil suppresses it.
	diag io.Writer
}

// opLabel returns the operation label for the refusal prefix, defaulting an
// empty op to "launch" so a zero-value verifier keeps the historical wording.
func (v keyringPublisherVerifier) opLabel() string {
	if v.op == "" {
		return "launch"
	}
	return v.op
}

// trustRemediationCommand returns the exact `aileron keyring trust ...` command
// that unblocks this operation. With a known plan name it points at the author
// self-trust path (`--plan <name>`, #2136), which grants the plan's own signing
// key at its per-repo publisher authority and actually converges with the key
// launch gates on. Without a plan name (e.g. the install path, or a zero-value
// verifier) it falls back to the bare-publisher form, which still names the
// authority to trust.
func (v keyringPublisherVerifier) trustRemediationCommand(publisher string) string {
	if v.planName != "" {
		return fmt.Sprintf("aileron keyring trust --plan %s", v.planName)
	}
	return fmt.Sprintf("aileron keyring trust %s", publisher)
}

// VerifyPublisher implements runtime.PublisherVerifier. It loads the keyring,
// resolves the publisher-trust union, and returns a fail-closed error when the
// signing key is not trusted for the declared publisher. On success it emits a
// trust line (and a conflict note when the owner and per-repo scopes diverge)
// to diag and returns nil.
func (v keyringPublisherVerifier) VerifyPublisher(publisher string, signingKey ed25519.PublicKey) error {
	keyring, err := cstore.LoadKeyring(v.path)
	if err != nil {
		// A malformed keyring is surfaced rather than silently failing closed,
		// so the operator sees the parse error instead of a misleading
		// "publisher not trusted" for an authority they believe they trusted.
		return fmt.Errorf("skill %s: load keyring %q: %w", v.opLabel(), v.path, err)
	}
	res, err := keyring.PublisherTrust(publisher, signingKey)
	if err != nil {
		return fmt.Errorf("skill %s: resolve publisher trust for %s: %w", v.opLabel(), publisher, err)
	}
	if !res.Trusted {
		return fmt.Errorf(
			"skill %s: publisher %s is not trusted for this plan's signing key (%s); trust it with `%s` or %s a plan you trust",
			v.opLabel(), publisher, fingerprint(signingKey), v.trustRemediationCommand(publisher), v.opLabel())
	}
	if v.diag != nil {
		if res.Conflict {
			v.writeConflictDiag(publisher, signingKey, res)
		} else {
			fmt.Fprintf(v.diag, "publisher %s trusted (key %s)\n", publisher, fingerprint(signingKey))
		}
	}
	return nil
}

// writeConflictDiag renders the owner-vs-per-repo trust-divergence note for a
// launch that was PERMITTED. The prior one-liner ("... grants disagree ...")
// read like a warning and gave the operator nothing to act on: it never said
// the launch proceeded, never showed which fingerprints diverged, and offered
// no reconcile path (#2139). This message leads with the permit decision, names
// the diverging owner-level vs per-repo fingerprints (marking which set the
// plan's signing key came from), and offers a concrete `keyring revoke` next
// step for an operator who wants the two scopes to agree. It is purely
// informational — nothing is broken and there is nothing to fix for
// correctness (ADR-0013, ADR-0027).
func (v keyringPublisherVerifier) writeConflictDiag(publisher string, signingKey ed25519.PublicKey, res cstore.PublisherTrustResult) {
	signingFP := fingerprint(signingKey)
	inPerRepo := containsKey(res.PerRepoKeys, signingKey)
	scope := "owner-level"
	if inPerRepo {
		scope = "per-repo"
	}

	fmt.Fprintf(v.diag, "publisher %s trusted (key %s); %s proceeding\n",
		publisher, signingFP, v.opLabel())
	fmt.Fprintf(v.diag, "  note: the owner-level and per-repo trust scopes for this publisher list different keys, but the plan's signing key is trusted in the %s scope, so the %s was permitted. This is informational; nothing is wrong.\n",
		scope, v.opLabel())
	fmt.Fprintf(v.diag, "  owner-level (%s://%s): %s\n",
		schemeOf(publisher), ownerOf(publisher), fingerprintList(res.OwnerKeys, signingKey))
	fmt.Fprintf(v.diag, "  per-repo   (%s): %s\n",
		publisher, fingerprintList(res.PerRepoKeys, signingKey))
	fmt.Fprintf(v.diag, "  to reconcile so the scopes agree, revoke a stale key by fingerprint, e.g. `aileron keyring revoke --key <sha256:...>`\n")
}

// fingerprintList renders a keyset as a comma-separated list of fingerprints,
// marking the plan's signing key with a trailing " (this plan)" so the operator
// can see, at a glance, which side of the divergence authorized the launch.
// Returns "(none)" for an empty scope.
func fingerprintList(keys []ed25519.PublicKey, signingKey ed25519.PublicKey) string {
	if len(keys) == 0 {
		return "(none)"
	}
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		fp := fingerprint(k)
		if k.Equal(signingKey) {
			fp += " (this plan)"
		}
		parts = append(parts, fp)
	}
	return strings.Join(parts, ", ")
}

// containsKey reports whether target is present in keys.
func containsKey(keys []ed25519.PublicKey, target ed25519.PublicKey) bool {
	for _, k := range keys {
		if k.Equal(target) {
			return true
		}
	}
	return false
}

// schemeOf and ownerOf split a `<scheme>://<owner>/<repo>` publisher authority
// into the parts of its owner-level authority (`<scheme>://<owner>`) for
// display. They are best-effort: a string that doesn't match the expected shape
// falls back to the whole input so the diagnostic never panics or drops
// context on a malformed authority.
func schemeOf(publisher string) string {
	if i := strings.Index(publisher, "://"); i >= 0 {
		return publisher[:i]
	}
	return publisher
}

func ownerOf(publisher string) string {
	rest := publisher
	if i := strings.Index(rest, "://"); i >= 0 {
		rest = rest[i+3:]
	}
	if i := strings.Index(rest, "/"); i >= 0 {
		return rest[:i]
	}
	return rest
}

// newLaunchPublisherVerifier builds the keyring-backed publisher-trust gate
// for a host launch. planName names the plan being launched so a fail-closed
// refusal can suggest the WORKING self-trust command `aileron keyring trust
// --plan <planName>` (#2136). It is a package-level seam so CLI tests point
// path at a temp keyring and capture diag. The runtime skips the gate entirely
// for a plan that declares no publisher, so a publisher-less plan launches
// whether or not this verifier is wired.
var newLaunchPublisherVerifier = func(planName string, diag io.Writer) runtime.PublisherVerifier {
	return keyringPublisherVerifier{path: cstore.DefaultKeyringPath(), op: "launch", planName: planName, diag: diag}
}
