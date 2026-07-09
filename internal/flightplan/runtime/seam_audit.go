package runtime

// NonSourceSeamBindings returns the subset of a seam step's resolved bindings
// that is safe to persist in the flightplan.launch.seam audit record (#2119),
// honoring the ADR-0027 audit boundary. The full bindings map is deliberately
// sent whole to the agent on the seam_pending envelope (the agent needs it),
// but a binding of BindInput kind that references a `source`-rule input carries
// the source dataset inline in that map. Such an inline dataset must never land
// in the persisted audit trail, so this helper drops any binding whose
// BindInput name resolves to a source input; every BindStep binding and every
// non-source BindInput binding passes through verbatim.
//
// It single-sources the ADR-0027 boundary rule in the runtime (mirroring
// launchConfigInputs) so the daemon-side emission stays free of binding/input
// knowledge. Deterministic and nil-safe: a nil plan, an unknown step id, or nil
// bindings all yield an empty (non-nil) map.
func NonSourceSeamBindings(plan *Plan, stepID string, bindings map[string]any) map[string]any {
	out := map[string]any{}
	if plan == nil || len(bindings) == 0 {
		return out
	}
	// Build the set of source-rule input names once.
	sourceInputs := map[string]bool{}
	for _, in := range plan.Inputs {
		if in.Resolution.Rule == ResolutionSource {
			sourceInputs[in.Name] = true
		}
	}
	// Locate the seam step so its declared binding kinds can be read.
	var stepBinds map[string]Binding
	for _, st := range plan.Steps {
		if st.ID == stepID {
			stepBinds = st.binds()
			break
		}
	}
	if stepBinds == nil {
		// Unknown step id: no binding knowledge, so nothing is provably
		// non-source. Fail closed to an empty map rather than leaking an
		// unclassified value.
		return out
	}
	for name, v := range bindings {
		b, ok := stepBinds[name]
		if !ok {
			// A resolved binding value with no declared binding entry cannot be
			// classified against the source rule; exclude it to stay on the safe
			// side of the audit boundary.
			continue
		}
		if b.Kind == BindInput && sourceInputs[b.Name] {
			continue
		}
		out[name] = v
	}
	return out
}
