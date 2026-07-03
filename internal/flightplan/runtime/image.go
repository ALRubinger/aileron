package runtime

import (
	"context"
	"fmt"

	"github.com/ALRubinger/aileron/internal/flightplan/freeze"
)

// hasWholePlanImage reports whether the verified pins carry a whole-plan-boot
// image. Every pin is a whole-plan pin: the plan runs in ONE composed
// container (#1839), and tool steps execute as subprocesses inside it
// (#1829), so there is no per-step pin shape anymore.
func hasWholePlanImage(pins []freeze.ImagePin) bool {
	return len(pins) > 0
}

// runInImage boots the verified pinned environment image and runs the frozen
// plan inside it (#1731). It is reached only when the loaded plan carries a
// non-empty ResolvedImages set, so the pin is always present here.
//
// The image booted is taken straight from the verified lock (pins[0]), never a
// re-parse of untrusted bytes: that is the load-bearing security property. A
// plan that pins an image but supplies no ImageRunner is an explicit error, not
// a silent in-process fallback. An environment was declared and pinned; running the
// plan in-process would enter an environment the attestation never certified.
func runInImage(ctx context.Context, lp LoadedPlan, opts Options) (RunResult, error) {
	// The environment resolves exactly one image pin, and the attestation certifies
	// that single environment. Guard the invariant so a future multi-pin environment
	// can never silently boot only pins[0] while ignoring the rest.
	if len(lp.ResolvedImages) != 1 {
		return RunResult{}, fmt.Errorf(
			"flightplan: expected exactly one resolved image pin, got %d", len(lp.ResolvedImages))
	}
	pin := lp.ResolvedImages[0]
	if opts.ImageRunner == nil {
		return RunResult{}, fmt.Errorf(
			"flightplan: frozen unit pins image %q but no image runner is configured", pin.Ref)
	}
	spec := ImageRunSpec{
		Image:   imageRef(pin.Ref, pin.Digest),
		Name:    opts.Name,
		Version: opts.Version,
		Inputs:  opts.Inputs,
		OutDir:  opts.OutDir,
	}
	res, err := opts.ImageRunner.Run(ctx, spec)
	if err != nil {
		return RunResult{}, err
	}
	return RunResult{
		ContentHash:    res.ContentHash,
		ResolvedInputs: res.ResolvedInputs,
		StepOutputs:    res.StepOutputs,
		Artifacts:      res.Artifacts,
		AuditIDs:       res.AuditIDs,
	}, nil
}

// imageRef composes the `ref@digest` string the runner boots. Freeze records
// the digest as a bare `sha256:<hex>`, so the ref and digest are joined with
// `@`. An empty digest degrades to the bare ref rather than a dangling `@`. The
// digest is the content-addressed identity, so a runner handed this string
// boots the exact image the signature attested rather than resolving the
// mutable tag.
func imageRef(ref, digest string) string {
	if digest == "" {
		return ref
	}
	return ref + "@" + digest
}
