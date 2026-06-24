package runtime

// The DTOs in this file mirror the manifest schema wire shapes. They are the
// strict-decode boundary: the loosely-typed manifest `[]any`/`map[string]any`
// elements round-trip through these structs (via remarshal) and the toX
// converters validate closed enums and required fields exactly as the schema
// declares them, then build the clean typed model the executor reads.

// ---- trust contract ----

type trustContractDTO struct {
	Credential struct {
		Kind          string `yaml:"kind"`
		Placement     string `yaml:"placement"`
		IdentityLabel string `yaml:"identityLabel"`
	} `yaml:"credential"`
	Hosts       []string `yaml:"hosts"`
	Paths       []string `yaml:"paths"`
	Effect      string   `yaml:"effect"`
	Idempotency struct {
		SafeToRetry    bool `yaml:"safeToRetry"`
		IdempotencyKey bool `yaml:"idempotencyKey"`
	} `yaml:"idempotency"`
	Redaction []struct {
		Field string `yaml:"field"`
		Rule  string `yaml:"rule"`
	} `yaml:"redaction"`
	Audit struct {
		Fields []string `yaml:"fields"`
		Sink   string   `yaml:"sink"`
	} `yaml:"audit"`
}

var validEffects = map[string]Effect{
	"read":          EffectRead,
	"write":         EffectWrite,
	"delete":        EffectDelete,
	"spend":         EffectSpend,
	"external-send": EffectExternalSend,
}

var validRedactKinds = map[string]RedactionKind{
	"drop": RedactDrop,
	"mask": RedactMask,
	"hash": RedactHash,
}

func (d trustContractDTO) toAction(ref string) (Action, error) {
	eff, ok := validEffects[d.Effect]
	if !ok {
		return Action{}, decodeErrf("action %q: unknown effect %q", ref, d.Effect)
	}
	if len(d.Hosts) == 0 {
		return Action{}, decodeErrf("action %q: trust contract declares no hosts (access scope is the boundary)", ref)
	}
	tc := TrustContract{
		CredentialKind: d.Credential.Kind,
		Hosts:          d.Hosts,
		Paths:          d.Paths,
		Effect:         eff,
		Idempotency:    Idempotency{SafeToRetry: d.Idempotency.SafeToRetry, IdempotencyKey: d.Idempotency.IdempotencyKey},
		IdentityLabel:  d.Credential.IdentityLabel,
		Audit:          AuditStructure{Fields: d.Audit.Fields, Sink: d.Audit.Sink},
	}
	for _, r := range d.Redaction {
		rk, ok := validRedactKinds[r.Rule]
		if !ok {
			return Action{}, decodeErrf("action %q: unknown redaction rule %q on field %q", ref, r.Rule, r.Field)
		}
		tc.Redaction = append(tc.Redaction, RedactionRule{Field: r.Field, Rule: rk})
	}
	return Action{Ref: ref, TrustContract: tc}, nil
}

// ---- input ----

type inputDTO struct {
	Name        string `yaml:"name"`
	Type        string `yaml:"type"`
	Description string `yaml:"description"`
	Resolution  struct {
		Rule    string `yaml:"rule"`
		Default any    `yaml:"default"`
		Value   string `yaml:"value"`
		Source  struct {
			ActionRef string `yaml:"actionRef"`
			Select    string `yaml:"select"`
		} `yaml:"source"`
	} `yaml:"resolution"`
}

var validInputTypes = map[string]InputType{
	"string": "string", "number": "number", "boolean": "boolean",
	"timestamp": "timestamp", "object": "object", "array": "array",
}

func (d inputDTO) toInput() (Input, error) {
	if d.Name == "" {
		return Input{}, decodeErrf("input has no name")
	}
	it, ok := validInputTypes[d.Type]
	if !ok {
		return Input{}, decodeErrf("input %q: unknown type %q", d.Name, d.Type)
	}
	res := Resolution{}
	switch d.Resolution.Rule {
	case "literal":
		res.Rule = ResolutionLiteral
		if d.Resolution.Default != nil {
			res.HasDefault = true
			res.Default = d.Resolution.Default
		}
	case "dynamic":
		res.Rule = ResolutionDynamic
		if d.Resolution.Value != "now" && d.Resolution.Value != "today" {
			return Input{}, decodeErrf("input %q: dynamic value must be now or today, got %q", d.Name, d.Resolution.Value)
		}
		res.DynamicValue = d.Resolution.Value
	case "source":
		res.Rule = ResolutionSource
		if d.Resolution.Source.ActionRef == "" {
			return Input{}, decodeErrf("input %q: source resolution needs an actionRef", d.Name)
		}
		res.SourceActionRef = d.Resolution.Source.ActionRef
		res.SourceSelect = d.Resolution.Source.Select
	default:
		return Input{}, decodeErrf("input %q: unknown resolution rule %q", d.Name, d.Resolution.Rule)
	}
	return Input{Name: d.Name, Type: it, Description: d.Description, Resolution: res}, nil
}

// ---- output ----

type outputDTO struct {
	Name     string `yaml:"name"`
	MimeType string `yaml:"mimeType"`
	Encoding string `yaml:"encoding"`
	Publish  struct {
		Target string `yaml:"target"`
		Path   string `yaml:"path"`
	} `yaml:"publish"`
}

var validEncodings = map[string]Encoding{
	"utf-8":  EncodingUTF8,
	"base64": EncodingBase64,
}

func (d outputDTO) toOutput() (Output, error) {
	if d.Name == "" {
		return Output{}, decodeErrf("output has no name")
	}
	enc, ok := validEncodings[d.Encoding]
	if !ok {
		return Output{}, decodeErrf("output %q: unknown encoding %q", d.Name, d.Encoding)
	}
	if d.MimeType == "" {
		return Output{}, decodeErrf("output %q: missing mimeType", d.Name)
	}
	var target PublishTarget
	switch d.Publish.Target {
	case "file":
		target = PublishFile
		if d.Publish.Path == "" {
			return Output{}, decodeErrf("output %q: file publish target needs a path", d.Name)
		}
	case "none":
		target = PublishNone
	default:
		return Output{}, decodeErrf("output %q: unknown publish target %q", d.Name, d.Publish.Target)
	}
	return Output{Name: d.Name, MimeType: d.MimeType, Encoding: enc, Target: target, Path: d.Publish.Path}, nil
}

// ---- step ----

type stepDTO struct {
	ID                 string            `yaml:"id"`
	Kind               string            `yaml:"kind"`
	ActionRef          string            `yaml:"actionRef"`
	Args               map[string]string `yaml:"args"`
	Bindings           map[string]string `yaml:"bindings"`
	Outputs            []string          `yaml:"outputs"`
	MaterializesOutput string            `yaml:"materializesOutput"`
}

func (d stepDTO) toStep() (Step, error) {
	if d.ID == "" {
		return Step{}, decodeErrf("step has no id")
	}
	var kind StepKind
	switch d.Kind {
	case "action-call":
		kind = KindActionCall
	case "transform":
		kind = KindTransform
	case "llm-seam":
		kind = KindLLMSeam
	default:
		return Step{}, decodeErrf("step %q: unknown kind %q (only action-call, transform, and the marked llm-seam may appear)", d.ID, d.Kind)
	}

	step := Step{ID: d.ID, Kind: kind, Outputs: d.Outputs, MaterializesOutput: d.MaterializesOutput}

	if kind == KindActionCall {
		if d.ActionRef == "" {
			return Step{}, decodeErrf("step %q: action-call has no actionRef", d.ID)
		}
		step.ActionRef = d.ActionRef
		args, err := parseBindings(d.ID, d.Args)
		if err != nil {
			return Step{}, err
		}
		step.Args = args
	} else {
		if d.ActionRef != "" {
			return Step{}, decodeErrf("step %q: kind %q must not declare an actionRef", d.ID, d.Kind)
		}
		binds, err := parseBindings(d.ID, d.Bindings)
		if err != nil {
			return Step{}, err
		}
		step.Bindings = binds
	}

	// Every kind requires at least one declared output (schema stepOutputs
	// minItems:1; action-call's outputs is optional in the schema but the
	// worked example always declares them, and a step that produces nothing
	// referenceable is dead). Action-call without outputs is allowed only
	// when it materializes a declared output (terminal write).
	if len(step.Outputs) == 0 && step.MaterializesOutput == "" {
		return Step{}, decodeErrf("step %q: declares no outputs and materializes nothing", d.ID)
	}
	if err := uniqueOutputs(d.ID, step.Outputs); err != nil {
		return Step{}, err
	}
	return step, nil
}

func parseBindings(stepID string, raw map[string]string) (map[string]Binding, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	out := make(map[string]Binding, len(raw))
	for name, ref := range raw {
		b, err := ParseBinding(ref)
		if err != nil {
			return nil, decodeErrf("step %q binding %q: %v", stepID, name, err)
		}
		out[name] = b
	}
	return out, nil
}

func uniqueOutputs(stepID string, outs []string) error {
	seen := map[string]bool{}
	for _, o := range outs {
		if o == "" {
			return decodeErrf("step %q: empty output name", stepID)
		}
		if seen[o] {
			return decodeErrf("step %q: duplicate output name %q", stepID, o)
		}
		seen[o] = true
	}
	return nil
}
