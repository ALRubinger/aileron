package main

import (
	"crypto/ed25519"
	"fmt"
	"io"

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
	// diag receives the one-line trust/conflict diagnostic. Nil suppresses it.
	diag io.Writer
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
		return fmt.Errorf("skill launch: load keyring %q: %w", v.path, err)
	}
	res, err := keyring.PublisherTrust(publisher, signingKey)
	if err != nil {
		return fmt.Errorf("skill launch: resolve publisher trust for %s: %w", publisher, err)
	}
	if !res.Trusted {
		return fmt.Errorf(
			"skill launch: publisher %s is not trusted for this plan's signing key (%s); trust it with `aileron keyring trust %s` or launch a plan you trust",
			publisher, fingerprint(signingKey), publisher)
	}
	if v.diag != nil {
		if res.Conflict {
			fmt.Fprintf(v.diag, "publisher %s trusted (key %s); note: owner-level and per-repo grants disagree on trusted keys for this publisher\n",
				publisher, fingerprint(signingKey))
		} else {
			fmt.Fprintf(v.diag, "publisher %s trusted (key %s)\n", publisher, fingerprint(signingKey))
		}
	}
	return nil
}

// newLaunchPublisherVerifier builds the keyring-backed publisher-trust gate
// for a host launch. It is a package-level seam so CLI tests point path at a
// temp keyring and capture diag. The runtime skips the gate entirely for a
// plan that declares no publisher, so a publisher-less plan launches whether or
// not this verifier is wired.
var newLaunchPublisherVerifier = func(diag io.Writer) runtime.PublisherVerifier {
	return keyringPublisherVerifier{path: cstore.DefaultKeyringPath(), diag: diag}
}
