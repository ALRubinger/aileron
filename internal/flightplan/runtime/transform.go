package runtime

import "fmt"

// Transform is one deterministic, no-LLM transform. It reshapes data already
// in the graph and has NO host, network, credential, or LLM surface. A
// Transform receives the step's resolved bindings and the names it must
// produce, and returns one value per declared output.
//
// The signature deliberately holds no reference to any seam or dispatcher
// type: a transform cannot reach an LLM or the action boundary by
// construction. This is half of the structural no-LLM guarantee (the other
// half is that the executor's transform branch only calls into here).
type Transform func(bindings map[string]any, outputs []string) (map[string]any, error)

// TransformRegistry is the closed set of named deterministic transforms a plan
// may use. v1 ships a default registry; the seam discipline keeps it a pure,
// LLM-free unit. A transform step names its transform; an unnamed transform
// step uses the identity passthrough so the worked example (whose transform
// reshapes a series into a CSV) runs without a bespoke registry entry in v1.
type TransformRegistry struct {
	byName map[string]Transform
}

// NewTransformRegistry returns the default registry. v1 registers the
// deterministic transforms the runtime needs; callers may extend it before a
// run. The registry never contains an LLM-backed transform: that is what the
// llm-seam kind is for.
func NewTransformRegistry() *TransformRegistry {
	r := &TransformRegistry{byName: map[string]Transform{}}
	r.byName["identity"] = identityTransform
	r.byName["passthrough"] = identityTransform
	return r
}

// Register adds a named transform. It overwrites an existing name so a host
// can supply its own deterministic transform for a plan.
func (r *TransformRegistry) Register(name string, t Transform) {
	r.byName[name] = t
}

// run executes the transform for a step. v1 transform steps do not name a
// transform in the manifest schema (the transformStep shape carries no
// transform name), so the runtime applies the registry's `identity` transform
// by default: it surfaces each binding under the matching declared output
// name, and for a single-output step with a single binding it maps that
// binding to the output. A host may override `identity` (or register named
// transforms) before a run to supply deterministic reshaping; the registry is
// the single, LLM-free transform vocabulary. This keeps the worked example
// deterministic and LLM-free while a richer named-transform vocabulary lands
// later.
func (r *TransformRegistry) run(step Step, bindings map[string]any) (map[string]any, error) {
	t, ok := r.byName["identity"]
	if !ok {
		t = identityTransform
	}
	return t(bindings, step.Outputs)
}

// identityTransform is the deterministic default: it produces one value per
// declared output. With one output and one binding it forwards the binding
// value; otherwise it groups all bindings under each output name. It is pure
// and total, so the same bindings always yield the same output.
func identityTransform(bindings map[string]any, outputs []string) (map[string]any, error) {
	if len(outputs) == 0 {
		return nil, fmt.Errorf("transform declares no outputs")
	}
	out := make(map[string]any, len(outputs))
	if len(outputs) == 1 && len(bindings) == 1 {
		for _, v := range bindings {
			out[outputs[0]] = v
		}
		return out, nil
	}
	for _, name := range outputs {
		// Each output receives a snapshot of the bindings so the transform is
		// deterministic and the outputs are independent values.
		out[name] = deepCopyValue(any(bindings))
	}
	return out, nil
}
