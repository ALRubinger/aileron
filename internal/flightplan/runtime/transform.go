package runtime

import (
	"bytes"
	"fmt"
	"html/template"
)

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
	r.byName["html-render"] = htmlRenderTransform
	return r
}

// Register adds a named transform. It overwrites an existing name so a host
// can supply its own deterministic transform for a plan.
func (r *TransformRegistry) Register(name string, t Transform) {
	r.byName[name] = t
}

// run executes the transform for a step. A transform step names its transform
// via step.Transform; an unnamed step falls back to the registry's `identity`
// transform (the v1 default), which surfaces each binding under the matching
// declared output name, and for a single-output step with a single binding maps
// that binding to the output. A named transform is looked up in the closed,
// LLM-free registry; an unknown name is a hard error (the seam discipline keeps
// the vocabulary closed rather than silently falling back). A host may override
// `identity` or register additional deterministic transforms before a run.
func (r *TransformRegistry) run(step Step, bindings map[string]any) (map[string]any, error) {
	name := step.Transform
	if name == "" {
		name = "identity"
	}
	t, ok := r.byName[name]
	if !ok {
		if name == "identity" {
			// The identity default is always available even if a host cleared it.
			t = identityTransform
		} else {
			return nil, fmt.Errorf("flightplan: step %q names transform %q, which is not registered", step.ID, name)
		}
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

// htmlRenderTemplateBinding is the binding name that designates the HTML
// template source for the html-render transform. Every other binding is a
// named JSON dataset injected into the template context under its binding name.
const htmlRenderTemplateBinding = "template"

// htmlRenderPathBinding is an OPTIONAL binding naming the advisory file-map
// path. Materialization writes to the declared output's path (the file-map path
// is advisory transport detail, #1519), so this binding only travels through
// to the emitted entry and never decides where the artifact lands.
const htmlRenderPathBinding = "path"

// htmlRenderTransform renders a deterministic, auto-escaping HTML report from a
// designated `template` binding plus named JSON data bindings, emitting a single
// file-map entry {path, mimeType:"text/html", encoding:"utf-8", content} that
// materialize.go consumes unchanged (decodeCarrier keys on `content`) to produce
// a sealed, content-addressed HTML artifact.
//
// It is a pure function of its bindings: it reaches no network and no clock, and
// Go's html/template ranges over map keys in sorted order, so the same bindings
// always render byte-identical output. html/template auto-escapes every
// interpolated value, so injected JSON datasets cannot break out of the HTML
// context. This keeps the transform inside the structural no-LLM guarantee: it
// holds no host, network, credential, or LLM surface.
func htmlRenderTransform(bindings map[string]any, outputs []string) (map[string]any, error) {
	if len(outputs) != 1 {
		return nil, fmt.Errorf("html-render declares %d outputs; it produces exactly one file-map entry", len(outputs))
	}

	tmplRaw, ok := bindings[htmlRenderTemplateBinding]
	if !ok {
		return nil, fmt.Errorf("html-render requires a %q binding naming the HTML template source", htmlRenderTemplateBinding)
	}
	tmplStr, ok := tmplRaw.(string)
	if !ok {
		return nil, fmt.Errorf("html-render %q binding must be a string template, got %T", htmlRenderTemplateBinding, tmplRaw)
	}

	// The advisory transport path, when supplied, is a string. Materialization
	// ignores it in favor of the declared output path.
	var path string
	if raw, present := bindings[htmlRenderPathBinding]; present {
		s, ok := raw.(string)
		if !ok {
			return nil, fmt.Errorf("html-render %q binding must be a string, got %T", htmlRenderPathBinding, raw)
		}
		path = s
	}

	// The template context is every remaining binding, keyed by its binding
	// name, so a template reads a dataset as `.<bindingName>`.
	data := make(map[string]any, len(bindings))
	for name, v := range bindings {
		if name == htmlRenderTemplateBinding || name == htmlRenderPathBinding {
			continue
		}
		data[name] = v
	}

	tmpl, err := template.New("html-render").Parse(tmplStr)
	if err != nil {
		return nil, fmt.Errorf("html-render parse template: %w", err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return nil, fmt.Errorf("html-render execute template: %w", err)
	}

	entry := map[string]any{
		"path":     path,
		"mimeType": "text/html",
		"encoding": string(EncodingUTF8),
		"content":  buf.String(),
	}
	return map[string]any{outputs[0]: entry}, nil
}
