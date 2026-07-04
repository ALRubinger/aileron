package runtime

import "crypto/ed25519"

// PublisherVerifier is the host-side publisher-trust seam (ADR-0013, #1900).
// When a frozen plan declares a publisher in its signed lock, Run resolves the
// plan's verified signing key against the operator's keyring for that publisher
// and refuses to run when the publisher is not trusted.
//
// The interface takes only stdlib ed25519 so the runtime stays free of any
// internal/cstore dependency: the CLI wires the concrete keyring-backed
// implementation (cmd/aileron/skill_launch_publisher.go). VerifyPublisher
// returns a non-nil error when the declared publisher does not trust the
// signing key (fail-closed); it returns nil when the publisher trusts the key.
//
// The gate is host-side only. On the image-boot re-entry (#1731) the CLI wires
// a nil verifier: the host already ran the gate before booting the pin, the
// keyring is not mounted into the sealed container, and re-checking inside the
// container would resolve an empty keyring and fail closed for every
// image-pinned plan. A nil verifier (or a plan that declares no publisher)
// skips the gate.
type PublisherVerifier interface {
	VerifyPublisher(publisher string, signingKey ed25519.PublicKey) error
}

// gatePublisher runs the publisher-trust gate for a loaded plan. It is a no-op
// (returns nil) when no verifier is wired or the plan declares no publisher, so
// a publisher-less plan and the image-boot re-entry both run unchanged. When a
// verifier is wired and the plan declares a publisher, the verifier's refusal
// is returned verbatim so the launch aborts before any step runs.
func gatePublisher(v PublisherVerifier, lp LoadedPlan) error {
	if v == nil || lp.Publisher == "" {
		return nil
	}
	return v.VerifyPublisher(lp.Publisher, lp.SignerKey)
}
