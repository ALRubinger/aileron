package runtime

import (
	"crypto/ed25519"
	"fmt"

	"github.com/ALRubinger/aileron/internal/flightplan/freeze"
	"github.com/ALRubinger/aileron/internal/flightplan/manifest"
	"github.com/ALRubinger/aileron/internal/flightplan/store"
)

// LoadError reports a load/verify refusal. A frozen unit that fails
// verification returns a *LoadError and runs zero steps.
type LoadError struct {
	Reason string
}

func (e *LoadError) Error() string { return "flightplan load: " + e.Reason }

// LoadedPlan is a verified, decoded plan ready to run, plus the verified
// content hash for the audit trail.
type LoadedPlan struct {
	Plan        *Plan
	ContentHash string
	// ResolvedImages carries the verified image digest pins from the frozen
	// lock. When non-empty, Run boots the pinned image and
	// runs the plan inside it; when empty, Run stays on the in-process path.
	// The pins come from the verified manifest lock block, so the digest booted
	// is exactly the one the author signature attested.
	ResolvedImages []freeze.ImagePin
	// StepTrust is the verified lock's sealed per-step reach
	// (lock.stepTrust), keyed by tool step id (#1829). It is the ONLY source
	// of the network reach the runtime enforces for a tool step: the
	// frontmatter trust-contract copy is audit context, never the enforcement
	// input, so a reach can never be re-supplied at launch. Populated only on
	// the verified path. Nil when the frozen unit seals no tool-step reach.
	StepTrust map[string]freeze.StepReach
	// SignerFingerprint is the `sha256:<hex>` fingerprint of the verified
	// author public key (from freeze.VerifyFrozen). It is threaded into the
	// launch audit as the plan's signer identity (#1752). Populated only on the
	// verified path, so it names the key that actually attested this unit.
	SignerFingerprint string
	// Publisher is the connector-style publisher authority the frozen plan
	// declares in its verified lock (`github://owner/repo` or bare
	// `github://owner`), or "" when the plan declares no publisher. The
	// host-side publisher-trust gate (#1900) enforces trust only when this is
	// non-empty. Populated only on the verified path from freeze.VerifyFrozen,
	// so a tampered publisher refuses at verification before it is read here.
	Publisher string
	// SignerKey is the raw ed25519 public key the plan's signature verified
	// against (from freeze.VerifyFrozen). The publisher-trust gate checks its
	// membership in the keyring for the declared Publisher. Populated only on
	// the verified path.
	SignerKey ed25519.PublicKey
}

// LoadVerified loads a frozen skill version from the store, verifies it
// (signature + content hash), parses the verified manifest, and decodes it
// into a typed Plan. Any verification failure or decode refusal returns an
// error and the runtime runs no step.
//
// The verification gate (#1509/#1511) is the security boundary: a tampered
// manifest, a flipped signature, or a content-hash mismatch all refuse before
// execution. Verification reuses freeze.VerifyFrozen so the canonical-bytes
// reconstruction lives in exactly one place.
func LoadVerified(s *store.Store, name, id string) (LoadedPlan, error) {
	if s == nil {
		return LoadedPlan{}, &LoadError{Reason: "nil store"}
	}
	fv, err := s.ReadFrozen(name, id)
	if err != nil {
		return LoadedPlan{}, &LoadError{Reason: fmt.Sprintf("read frozen version %s/%s: %v", name, id, err)}
	}
	return verifyAndDecode(fv)
}

// verifyAndDecode is the verify→parse→decode core, factored out so tests can
// drive it from an in-memory FrozenVersion without a store on disk.
func verifyAndDecode(fv store.FrozenVersion) (LoadedPlan, error) {
	verified, err := freeze.VerifyFrozen(fv.SkillMD, fv.Lockfile, fv.Signature, fv.PublicKey)
	if err != nil {
		return LoadedPlan{}, &LoadError{Reason: err.Error()}
	}
	m, err := manifest.Parse(verified.SkillMD)
	if err != nil {
		return LoadedPlan{}, &LoadError{Reason: fmt.Sprintf("parse verified manifest: %v", err)}
	}
	plan, err := Decode(m)
	if err != nil {
		return LoadedPlan{}, err
	}
	if err := crossCheckStepTrust(plan, verified.StepTrust); err != nil {
		return LoadedPlan{}, err
	}
	return LoadedPlan{
		Plan:              plan,
		ContentHash:       verified.ContentHash,
		ResolvedImages:    verified.ResolvedImages,
		StepTrust:         verified.StepTrust,
		SignerFingerprint: verified.SignerFingerprint,
		Publisher:         verified.Publisher,
		SignerKey:         verified.SignerKey,
	}, nil
}

// crossCheckStepTrust cross-checks the decoded tool steps against the sealed
// per-step reach (#1829, fail-closed): a tool step that declares a
// trustContract but has no sealed lock.stepTrust entry is an inconsistent
// frozen unit — freeze always seals a contracted tool step's reach, so a
// missing entry means the artifacts disagree, and running would leave the
// step's declared reach unenforced. A sealed entry with no matching
// contracted step is tolerated (it grants nothing: the executor keys
// enforcement off the step's own graph entry).
func crossCheckStepTrust(plan *Plan, sealed map[string]freeze.StepReach) error {
	for _, s := range plan.Steps {
		if s.Kind != KindTool || s.TrustContract == nil {
			continue
		}
		if _, ok := sealed[s.ID]; !ok {
			return &LoadError{Reason: fmt.Sprintf(
				"tool step %q declares a trust contract but the verified lock seals no stepTrust reach for it; the frozen unit is inconsistent", s.ID)}
		}
	}
	return nil
}
