// Package runtime is the deterministic Launch executor for a frozen Aileron
// Flight Plan (ADR-0027, issue #1511). It loads a sealed, signed plan from
// the store, verifies it, resolves declared inputs once at the launch
// boundary (#1523), and walks the step graph in topological order through the
// sealed action boundary (#1507), materializing declared file artifacts
// (#1519) and emitting a customer-owned audit record.
//
// The no-LLM guarantee is structural, not a runtime check. The executor has
// exactly four step-kind branches (action-call, transform, tool, llm-seam)
// and only the explicitly marked llm-seam branch can reach a language model.
// The action-call, transform, and tool branches hold no reference to any
// seam type, so no deterministic step can reach an LLM by construction. In
// v1 the seam is unwired by default: an llm-seam step with no configured
// provider is a hard error, so a default launch reaches no LLM at all.
//
// The runtime depends on the manifest, freeze, and store packages but takes
// the action boundary, the approval channel, and the audit sink as thin SPI
// interfaces (ActionDispatcher, Approver, AuditSink, Clock, LLMSeam). The CLI
// wires those seams to the real daemon-backed implementations; the runtime
// core stays unit-testable with fakes, mirroring the freeze
// DigestResolver/FeatureComposer seam discipline.
//
// This package never reads or carries a credential value. Step args are
// binding references only (the closed binding grammar enforces this at
// decode), and credentials are injected host-side at the action boundary
// (ADR-0005, ADR-0019). No secret ever reaches plan code through this
// runtime.
package runtime

import "regexp"

// Plan is the typed, validated model of a frozen plan's `aileron` block that
// the executor walks. It is produced by Decode from a manifest.Manifest and
// is the single in-memory shape the runtime reads; the loosely-typed `[]any`
// fields on the manifest never escape the decode boundary.
type Plan struct {
	// Name is the skill name from the manifest frontmatter.
	Name string
	// Actions are the declared action requirements indexed by ref. The
	// trust contract drives approval routing, idempotency, redaction, and
	// audit for each action-call.
	Actions map[string]Action
	// Inputs are the declared inputs in declaration order. Each resolves
	// once at the launch boundary.
	Inputs []Input
	// Outputs are the declared output artifacts indexed by name.
	Outputs map[string]Output
	// Steps are the step-graph steps in declaration order. The executor
	// walks them in a topologically-sorted order (see Order).
	Steps []Step
	// Order is the deterministic topological order of step indices the
	// executor walks, computed from `steps.*` edges only.
	Order []int
}

// Action is one declared action requirement plus its decoded trust contract.
type Action struct {
	Ref           string
	TrustContract TrustContract
}

// Effect is the operation effect that drives approval routing (ADR-0009).
type Effect string

const (
	EffectRead         Effect = "read"
	EffectWrite        Effect = "write"
	EffectDelete       Effect = "delete"
	EffectSpend        Effect = "spend"
	EffectExternalSend Effect = "external-send"
)

// TrustContract mirrors the manifest trust-contract fields the runtime
// enforces per action-call: effect routing, idempotency, redaction, audit.
// Credential/host/path are the declared access scope; the runtime grants
// nothing undeclared.
type TrustContract struct {
	CredentialKind string
	Hosts          []string
	Paths          []string
	Effect         Effect
	Idempotency    Idempotency
	Redaction      []RedactionRule
	IdentityLabel  string
	Audit          AuditStructure
}

// Idempotency records whether an action is safe to retry and whether it
// accepts a client-supplied idempotency key.
type Idempotency struct {
	SafeToRetry    bool
	IdempotencyKey bool
}

// RedactionRule names a result field path and how it is redacted before the
// result surfaces into the graph or any audit summary.
type RedactionRule struct {
	Field string
	Rule  RedactionKind
}

// RedactionKind is the closed set of redaction operations.
type RedactionKind string

const (
	RedactDrop RedactionKind = "drop"
	RedactMask RedactionKind = "mask"
	RedactHash RedactionKind = "hash"
)

// AuditStructure declares the closed set of audit fields an action emits and
// the customer-owned sink reference.
type AuditStructure struct {
	Fields []string
	Sink   string
}

// InputType is the declared value type of an input.
type InputType string

// ResolutionRule discriminates how an input resolves at the launch boundary.
type ResolutionRule string

const (
	ResolutionLiteral ResolutionRule = "literal"
	ResolutionDynamic ResolutionRule = "dynamic"
	ResolutionSource  ResolutionRule = "source"
)

// Input is one declared input with its resolution rule. Inputs resolve once,
// at the launch boundary, into a concrete resolved-input set (#1523).
type Input struct {
	Name        string
	Type        InputType
	Description string
	Resolution  Resolution
	// Constraint bounds the resolved value. Nil means unconstrained (today's
	// behavior). When non-nil it holds exactly one of an Enum allow-set or a
	// compiled Pattern; the launch/resolution boundary rejects a resolved
	// value that falls outside it.
	Constraint *Constraint
}

// Constraint bounds an input's resolved value. Exactly one field is set: Enum
// is the closed set of allowed string forms, or Pattern is the compiled
// author-anchored RE2 regexp the resolved value's string form must match. The
// pattern is compiled once at decode and stored here so enforcement never
// recompiles.
type Constraint struct {
	Enum    []string
	Pattern *regexp.Regexp
}

// Resolution is a decoded input resolution rule. Exactly one of the three
// rule shapes is populated, discriminated by Rule.
type Resolution struct {
	Rule ResolutionRule
	// Literal fields.
	HasDefault bool
	Default    any
	// Dynamic field: "now" or "today".
	DynamicValue string
	// Source fields.
	SourceActionRef string
	SourceSelect    string
}

// Encoding is the artifact content encoding. v1 implements utf-8 only;
// base64 is reserved and refused at materialization.
type Encoding string

const (
	EncodingUTF8   Encoding = "utf-8"
	EncodingBase64 Encoding = "base64"
)

// PublishTarget is where the runtime materializes an output artifact.
type PublishTarget string

const (
	PublishFile PublishTarget = "file"
	PublishNone PublishTarget = "none"
)

// Output is one declared output artifact: the declared interface the runtime
// materializes through the file-map transport (#1519).
type Output struct {
	Name     string
	MimeType string
	Encoding Encoding
	Target   PublishTarget
	Path     string
}

// StepKind is the closed step-kind enum. The no-LLM guarantee is structural:
// only KindLLMSeam can reach a language model. KindTool runs a declared
// environment tool as a deterministic subprocess inside the plan's single
// composed container (#1829); it never reaches an LLM.
type StepKind string

const (
	KindActionCall StepKind = "action-call"
	KindTransform  StepKind = "transform"
	KindTool       StepKind = "tool"
	KindLLMSeam    StepKind = "llm-seam"
)

// Step is one step in the deterministic step graph. A single struct carries
// every kind's fields; Kind selects which are meaningful. ActionRef is set
// only for action-call; Args is the action-call binding map; Bindings is the
// transform/tool/llm-seam binding map. Keeping one struct keeps the
// executor's kind switch the single dispatch point.
type Step struct {
	ID        string
	Kind      StepKind
	ActionRef string
	// Transform names the deterministic transform to apply, selected from the
	// runtime's closed TransformRegistry. Meaningful only for KindTransform;
	// empty means the identity passthrough (the v1 default). Never carries an
	// LLM-backed transform: that is what KindLLMSeam is for.
	Transform string
	// Args binds action-call argument names to binding references.
	Args map[string]Binding
	// Bindings binds transform / tool / llm-seam input names to binding
	// references.
	Bindings map[string]Binding
	// Outputs are the named results this step produces, referenced by a
	// later step as steps.<id>.<output>.
	Outputs []string
	// MaterializesOutput names a declared output this step's result
	// materializes into. Empty when the step materializes nothing.
	MaterializesOutput string

	// The following fields are meaningful only for KindTool (#1829): a tool
	// step runs its argv as a deterministic subprocess inside the plan's
	// pinned environment with mount → run → collect file I/O.

	// Command is the argv the tool step executes: the first element is the
	// program, the rest its arguments. Always exec'd directly (no shell), so
	// the signed invocation carries no injection surface.
	Command []string
	// MountPath is the in-environment path the resolved step input is
	// written to (as input.json), or empty when the step declares no mount.
	MountPath string
	// CollectPath is the in-environment path whose contents are collected
	// as the step's single declared output, or empty when the step declares
	// no collect.
	CollectPath string
	// TrustContract, when non-nil, is the tool step's declared per-step
	// trust contract: its Hosts declare the step's network reach and Effect
	// the operation effect, validated at decode exactly like an action's
	// contract. The reach the runtime ENFORCES comes from the verified
	// lock's sealed stepTrust section (LoadedPlan.StepTrust), never from
	// this frontmatter copy; a contracted tool step with no sealed entry is
	// a load refusal. Nil when the step declares no reach.
	TrustContract *TrustContract
}

// binds returns the step's binding map regardless of kind (Args for
// action-call, Bindings otherwise), so dependency extraction and execution
// read one source.
func (s Step) binds() map[string]Binding {
	if s.Kind == KindActionCall {
		return s.Args
	}
	return s.Bindings
}
