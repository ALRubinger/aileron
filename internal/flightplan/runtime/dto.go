package runtime

import (
	"regexp"
	"strings"
)

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
	// OAuth and Verification are modeled (but not consumed by the runtime) so
	// strict decode does not reject a manifest that legitimately declares
	// them. The schema's full trustContract shape is represented here; the
	// runtime enforces effect/idempotency/redaction/audit and the access
	// scope, and the host injects credentials at the boundary.
	OAuth        map[string]any `yaml:"oauth"`
	Verification map[string]any `yaml:"verification"`
	Hosts        []string       `yaml:"hosts"`
	Paths        []string       `yaml:"paths"`
	Effect       string         `yaml:"effect"`
	Idempotency  struct {
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

// toContract validates the trust-contract fields and builds the typed
// TrustContract. It is the single validation body shared by the action path
// (toAction) and the per-step tool path (populateToolStep): the effect
// must be a known enum, the hosts must be non-empty (the access scope is the
// boundary), and each redaction rule must be a known kind. ref names the
// offending element in any error so a tool step and an action report the
// same failures identically.
func (d trustContractDTO) toContract(ref string) (TrustContract, error) {
	eff, ok := validEffects[d.Effect]
	if !ok {
		return TrustContract{}, decodeErrf("%s: unknown effect %q", ref, d.Effect)
	}
	if len(d.Hosts) == 0 {
		return TrustContract{}, decodeErrf("%s: trust contract declares no hosts (access scope is the boundary)", ref)
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
			return TrustContract{}, decodeErrf("%s: unknown redaction rule %q on field %q", ref, r.Rule, r.Field)
		}
		tc.Redaction = append(tc.Redaction, RedactionRule{Field: r.Field, Rule: rk})
	}
	return tc, nil
}

func (d trustContractDTO) toAction(ref string) (Action, error) {
	tc, err := d.toContract("action " + ref)
	if err != nil {
		return Action{}, err
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
	// Constraint is the optional resolution constraint. A pointer distinguishes
	// an absent constraint from an empty one; the strict KnownFields decoder
	// requires the field to exist on the DTO or a manifest carrying it is
	// refused. Exactly one of Enum/Pattern is populated; toInput enforces that
	// as defense-in-depth over the schema oneOf.
	Constraint *struct {
		Enum    []string `yaml:"enum"`
		Pattern string   `yaml:"pattern"`
	} `yaml:"constraint"`
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
	con, err := d.toConstraint()
	if err != nil {
		return Input{}, err
	}
	return Input{Name: d.Name, Type: it, Description: d.Description, Resolution: res, Constraint: con}, nil
}

// toConstraint validates the optional input constraint and compiles a pattern
// once. It is the fail-closed gate for the one thing the schema cannot check
// (RE2 compilability) and re-checks the exactly-one-of-enum-or-pattern shape so
// the typed path refuses a bad constraint even when a caller bypasses schema
// validation. An absent constraint returns (nil, nil): unconstrained is the
// default and today's behavior.
func (d inputDTO) toConstraint() (*Constraint, error) {
	if d.Constraint == nil {
		return nil, nil
	}
	hasEnum := len(d.Constraint.Enum) > 0
	// Surrounding whitespace on a pattern is not significant: a YAML block
	// scalar can append a trailing newline that would otherwise silently change
	// the enforced regexp (an anchored "^x$" would become "^x$\n"). Trim once
	// and use the trimmed text for both the presence check and the compile, so
	// detection and enforcement never disagree and a whitespace-only pattern is
	// refused as the plan requires.
	pattern := strings.TrimSpace(d.Constraint.Pattern)
	hasPattern := pattern != ""
	switch {
	case hasEnum && hasPattern:
		return nil, decodeErrf("input %q: constraint declares both enum and pattern (exactly one is allowed)", d.Name)
	case hasEnum:
		for _, e := range d.Constraint.Enum {
			if e == "" {
				return nil, decodeErrf("input %q: constraint enum has an empty value", d.Name)
			}
		}
		return &Constraint{Enum: d.Constraint.Enum}, nil
	case hasPattern:
		re, err := regexp.Compile(pattern)
		if err != nil {
			return nil, decodeErrf("input %q: constraint pattern %q does not compile as a Go RE2 regexp: %v", d.Name, pattern, err)
		}
		return &Constraint{Pattern: re}, nil
	default:
		// The constraint block is present but names neither enum nor pattern
		// (or an empty enum with an absent pattern): a bad shape, refused.
		return nil, decodeErrf("input %q: constraint declares neither a non-empty enum nor a pattern (exactly one is required)", d.Name)
	}
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
	Transform          string            `yaml:"transform"`
	Args               map[string]string `yaml:"args"`
	Bindings           map[string]string `yaml:"bindings"`
	Outputs            []string          `yaml:"outputs"`
	MaterializesOutput string            `yaml:"materializesOutput"`

	// The following mirror the schema's toolStep shape (#1829) and are
	// meaningful only for kind: tool; toStep refuses them on any other kind.
	// Mount and Collect are pointers so their absence is distinguishable from
	// an empty mapping (an empty path inside a present block is a refusal).

	// Command is the tool step's argv array (schema-required, minItems 1).
	Command []string `yaml:"command"`
	// Mount is the optional mount declaration (input file I/O boundary).
	Mount *struct {
		Path string `yaml:"path"`
	} `yaml:"mount"`
	// Collect is the optional run-and-collect declaration (output file I/O
	// boundary).
	Collect *struct {
		Path string `yaml:"path"`
	} `yaml:"collect"`
	// TrustContract is the tool step's optional per-step trust contract,
	// declaring its network reach (hosts) and effect. Validated at decode via
	// the same shared toContract body an action's contract uses.
	TrustContract *trustContractDTO `yaml:"trustContract"`
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
	case "tool":
		kind = KindTool
	case "llm-seam":
		kind = KindLLMSeam
	default:
		return Step{}, decodeErrf("step %q: unknown kind %q (only action-call, transform, tool, and the marked llm-seam may appear)", d.ID, d.Kind)
	}

	step := Step{ID: d.ID, Kind: kind, Outputs: d.Outputs, MaterializesOutput: d.MaterializesOutput}

	// The tool-step surface (command/mount/collect/trustContract, #1829) is
	// closed to kind: tool. Any other kind carrying one of these fields is a
	// malformed step (the closed schema forbids it), refused rather than
	// silently ignored.
	if kind != KindTool {
		if len(d.Command) != 0 {
			return Step{}, decodeErrf("step %q: kind %q must not declare a command (only a tool step executes one)", d.ID, d.Kind)
		}
		if d.Mount != nil || d.Collect != nil {
			return Step{}, decodeErrf("step %q: kind %q must not declare mount/collect (the tool-step file I/O boundary)", d.ID, d.Kind)
		}
		if d.TrustContract != nil {
			return Step{}, decodeErrf("step %q: kind %q must not declare a trustContract (only a tool step declares per-step reach)", d.ID, d.Kind)
		}
	}

	if kind == KindActionCall {
		if d.ActionRef == "" {
			return Step{}, decodeErrf("step %q: action-call has no actionRef", d.ID)
		}
		// An action-call carries args, not bindings: a stray bindings block is
		// a malformed step (the closed schema forbids it), refused rather than
		// silently ignored.
		if len(d.Bindings) != 0 {
			return Step{}, decodeErrf("step %q: action-call must not declare bindings (use args)", d.ID)
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
		// A transform / tool / llm-seam carries bindings, not args: a stray
		// args block is a malformed step, refused rather than silently ignored.
		if len(d.Args) != 0 {
			return Step{}, decodeErrf("step %q: kind %q must not declare args (use bindings)", d.ID, d.Kind)
		}
		binds, err := parseBindings(d.ID, d.Bindings)
		if err != nil {
			return Step{}, err
		}
		step.Bindings = binds
	}

	// A transform name selects which deterministic transform the registry
	// applies; it is meaningful only on a transform step. Any other kind naming
	// a transform is a malformed step, refused rather than silently ignored.
	if kind == KindTransform {
		step.Transform = d.Transform
	} else if d.Transform != "" {
		return Step{}, decodeErrf("step %q: kind %q must not declare a transform (only a transform step names one)", d.ID, d.Kind)
	}

	if kind == KindTool {
		if err := d.populateToolStep(&step); err != nil {
			return Step{}, err
		}
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

// populateToolStep validates and copies the tool-step surface (#1829) onto a
// KindTool step: the required non-empty argv (each element non-empty — the
// schema's minLength:1, re-checked here so a manifest tampered past the
// schema gate never execs an empty program), the optional mount/collect
// paths (a present block with an empty path is a refusal, mirroring the
// schema's minLength gate), and the optional per-step trust contract,
// validated via the same shared toContract body an action's contract uses.
func (d stepDTO) populateToolStep(step *Step) error {
	if len(d.Command) == 0 {
		return decodeErrf("step %q: tool step declares no command (the argv is the executed identity)", d.ID)
	}
	// A tool step collects exactly one blob, so it must declare exactly one
	// output to carry it. Refusing zero/multiple outputs at decode (not only
	// in the executor, whose arity check is the direct-construct backstop)
	// means a misdeclared step never reaches execution — its subprocess, and
	// any side effects, never run for a result that could only be discarded.
	if len(d.Outputs) != 1 {
		return decodeErrf("step %q: a tool step must declare exactly one output (the collected value's landing), got %d", d.ID, len(d.Outputs))
	}
	for i, arg := range d.Command {
		if arg == "" {
			return decodeErrf("step %q: tool command element %d is empty", d.ID, i)
		}
		// Defense-in-depth over the launch load path (#1958): reject a malformed
		// `{{ inputs.<name> }}` token here so a tampered manifest never reaches
		// instantiation with an unbalanced or non-inputs token. The
		// constraint-presence guard stays freeze-only; decode checks only the
		// grammar (here) and the declared-input reference (validateReferences).
		if _, err := commandInputRefs(arg); err != nil {
			return decodeErrf("step %q: tool command element %d: %v", d.ID, i, err)
		}
	}
	step.Command = d.Command
	if d.Mount != nil {
		if d.Mount.Path == "" {
			return decodeErrf("step %q: tool step declares a mount with an empty path", d.ID)
		}
		step.MountPath = d.Mount.Path
	}
	if d.Collect != nil {
		if d.Collect.Path == "" {
			return decodeErrf("step %q: tool step declares a collect with an empty path", d.ID)
		}
		step.CollectPath = d.Collect.Path
	}
	if d.TrustContract != nil {
		tc, err := d.TrustContract.toContract("tool step " + d.ID)
		if err != nil {
			return err
		}
		step.TrustContract = &tc
	}
	return nil
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
