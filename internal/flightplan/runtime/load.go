package runtime

import (
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
	// lock (rung-1 or rung-2). When non-empty, Run boots the pinned image and
	// runs the plan inside it; when empty, Run stays on the in-process path.
	// The pins come from the verified manifest lock block, so the digest booted
	// is exactly the one the author signature attested.
	ResolvedImages []freeze.ImagePin
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
	// Thread the verified image pins into decode so a rung-3 plan attaches each
	// step's pinned tool dispatch (a rung-3 step whose pin is absent is refused).
	// Non-rung-3 plans ignore the pins.
	plan, err := DecodeWithImages(m, verified.ResolvedImages)
	if err != nil {
		return LoadedPlan{}, err
	}
	return LoadedPlan{
		Plan:           plan,
		ContentHash:    verified.ContentHash,
		ResolvedImages: verified.ResolvedImages,
	}, nil
}
