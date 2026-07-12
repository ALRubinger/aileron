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
			writeConflictDiag(v.diag, publisher, signingKey, res)
		} else {
			fmt.Fprintf(v.diag, "publisher %s trusted (key %s)\n", publisher, fingerprint(signingKey))
		}
	}
	return nil
}

// writeConflictDiag emits the owner-vs-per-repo publisher-trust divergence
// note. The launch is already permitted (the plan's signing key is a member of
// the union and res.Conflict was set only after Trusted passed), so the message
// leads with that decision and states plainly that it is informational, never
// alarming. It then lists both diverging scopes' fingerprints, marks which
// scope carries this plan's signing key, and offers a concrete reconcile step:
// `aileron keyring revoke --key <fp>` to drop whichever key material the
// operator decides is stale. signingKey is the plan's verified key so the "(this
// plan)" marker can be placed on the exact fingerprint the launch resolved to.
func writeConflictDiag(w io.Writer, publisher string, signingKey ed25519.PublicKey, res cstore.PublisherTrustResult) {
	planFP := fingerprint(signingKey)
	fmt.Fprintf(w, "publisher %s trusted (key %s); launch proceeding.\n", publisher, planFP)
	fmt.Fprintln(w, "note (informational, no action required): this publisher's owner-level and per-repo trust grants list different keys.")
	fmt.Fprintln(w, "  owner-level grant:")
	writeScopeKeys(w, res.OwnerKeys, planFP)
	fmt.Fprintln(w, "  per-repo grant:")
	writeScopeKeys(w, res.PerRepoKeys, planFP)
	fmt.Fprintf(w, "  to reconcile, revoke the key you no longer trust: aileron keyring revoke --key <fp>\n")
}

// writeScopeKeys prints one indented line per trusted key in a scope, marking
// the fingerprint that matches the plan's signing key with "(this plan)" so the
// operator can see at a glance which scope authorized the running launch. An
// empty scope prints "(none)".
func writeScopeKeys(w io.Writer, keys []ed25519.PublicKey, planFP string) {
	if len(keys) == 0 {
		fmt.Fprintln(w, "    (none)")
		return
	}
	for _, k := range keys {
		fp := fingerprint(k)
		if fp == planFP {
			fmt.Fprintf(w, "    %s (this plan)\n", fp)
		} else {
			fmt.Fprintf(w, "    %s\n", fp)
		}
	}
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
