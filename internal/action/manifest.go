// Package action implements parsing, validation, storage, and capability
// enforcement for Aileron action files.
//
// Per [ADR-0001] and [ADR-0003], an action is a single file in
// ~/.aileron/actions/ with TOML frontmatter delimited by `+++` and a
// Markdown body. The frontmatter is the contract Aileron executes; the body
// doubles as the LLM-facing function description when the action is surfaced
// to the agent's tool catalog.
//
// [ADR-0001]: https://docs.withaileron.ai/adr/0001-manifest-format
// [ADR-0003]: https://docs.withaileron.ai/adr/0003-action-model
package action

// Manifest is the parsed contents of a single action file: TOML frontmatter
// fields plus the Markdown body.
type Manifest struct {
	// Name is the bare local handle for the action (e.g. "ship-update").
	// Per ADR-0003, the user owns the file post-install and chooses the name;
	// FQN does not apply to the action's own identity.
	Name string `toml:"name"`

	// Version is a strict semantic version (e.g. "1.0.0").
	Version string `toml:"version"`

	// Source is a fully-qualified URI recording where the action template was
	// copied from (e.g. "hub://aileron/ship-update@1.0.0"). Provenance only —
	// not consulted at runtime.
	Source string `toml:"source"`

	// Requires holds dependency declarations.
	Requires Requires `toml:"requires"`

	// Match describes how the runtime matches agent intent to this action
	// ([ADR-0008]). The shape evolves as intent matching matures; v1 only
	// requires the `intent` phrase to be present.
	//
	// [ADR-0008]: https://docs.withaileron.ai/adr/0008-intent-matching
	Match Match `toml:"match"`

	// Inputs declares the call-time arguments the action accepts. Per
	// [ADR-0003], inputs become the JSON Schema `parameters` object the
	// LLM sees when Aileron exposes the action as a tool. Empty when the
	// action takes no arguments.
	//
	// [ADR-0003]: https://docs.withaileron.ai/adr/0003-action-model
	Inputs []Input `toml:"inputs"`

	// Execute is the ordered list of connector operations the action runs.
	// v1 actions are linear chains with first-failure-terminates semantics
	// (per ADR-0010).
	Execute []ExecuteStep `toml:"execute"`

	// Approval declares whether invocations of this action require explicit
	// user approval before the runtime executes the underlying connector
	// ops. When `Approval.Required` is true, `POST /v1/actions/{name}/run`
	// holds its HTTP response open while it queues an approval request to
	// the orchestrator; the response unblocks only when the user approves
	// (success path: action runs, normal result returned) or denies (the
	// runtime returns a `binding_required`-style failure with class
	// `approval_denied`). Absent or `Required: false` means the action
	// runs immediately as before. Per the action-approval design (#418),
	// the user surface is Aileron's webapp; the agent learns to surface
	// the approval URL to the user via templated tool descriptions in
	// `aileron-mcp`.
	Approval *ApprovalPolicy `toml:"approval"`

	// Body is the Markdown content following the closing `+++` delimiter.
	// The first paragraph (or a designated section) is surfaced to the LLM
	// as the function `description` when the action is exposed as a tool.
	Body string `toml:"-"`
}

// ApprovalPolicy describes the approval gating for an action. v1 carries
// `Required` plus an optional `Preview` ([ADR-0016]) directive naming a
// read-only op the runtime calls before showing the approval prompt so
// the user sees an authoritative summary of what they are approving
// rather than agent-supplied (and forgeable) hints.
//
// [ADR-0016]: https://docs.withaileron.ai/adr/0016-approval-preview
type ApprovalPolicy struct {
	// Required gates execution on a user decision when true. Default
	// false (i.e. absent block ⇒ no approval needed).
	Required bool `toml:"required"`

	// Preview is the optional [approval.preview] sub-block. When non-nil,
	// the runtime invokes the named connector op before showing the
	// approval prompt and renders the result alongside the gated call's
	// inputs. See [ADR-0016].
	//
	// [ADR-0016]: https://docs.withaileron.ai/adr/0016-approval-preview
	Preview *PreviewPolicy `toml:"preview"`
}

// PreviewPolicy is the [approval.preview] sub-block. Names the read-only
// connector op the runtime calls before showing the approval prompt,
// the argument template (with `${args.X}` interpolation against the
// gated action's call-time inputs), and the field-paths to render from
// the response. See [ADR-0016].
//
// [ADR-0016]: https://docs.withaileron.ai/adr/0016-approval-preview
type PreviewPolicy struct {
	// Op is the connector op name. Must live on the same connector as
	// the gated action's first execute step, must be declared
	// idempotent = true in at least one bundled action manifest, and
	// must not itself carry [approval] required = true (no preview-of-
	// preview recursion). Enforced at manifest validation time.
	Op string `toml:"op"`

	// Args is the inline argument table passed to the preview op.
	// Values may interpolate `${args.<name>}` against the gated
	// action's call-time inputs, matching the [[execute]] step
	// convention. The TOML decoder yields any of string, []any, or
	// map[string]any leaves; the runtime resolves interpolations
	// before invoking the op.
	Args map[string]any `toml:"args"`

	// Render maps user-facing labels to dotted JSON paths into the
	// preview op's response. The runtime extracts each path and emits
	// a {label: value} entry into the approval prompt. Paths that
	// fail to resolve are omitted; the UI shows "n/a" for those labels.
	// Empty render is rejected at validation time.
	//
	// Header-style arrays (Gmail's `message.payload.headers: [{name,
	// value}, ...]`) use a shorthand: a path segment beyond an array
	// of {name, value} entries is interpreted as "find the entry whose
	// `name` equals the segment, return its `value`". For other shapes
	// the path is a plain object/array walk.
	Render map[string]string `toml:"render"`

	// RenderOrder preserves the declaration order of Render keys from
	// the source manifest so the approval prompt renders labels in the
	// author's intended order rather than Go's randomized map iteration
	// order. Populated by the parser from the TOML decoder's key list;
	// not part of the TOML shape itself.
	RenderOrder []string `toml:"-"`
}

// ApprovalRequired reports whether the action's manifest declares
// `[approval] required = true`. Returns false when `Approval` is nil
// or `Required` is unset, matching the default-no-approval behavior
// of unannotated actions.
func (m Manifest) ApprovalRequired() bool {
	return m.Approval != nil && m.Approval.Required
}

// ApprovalPreview returns the manifest's [approval.preview] block, or
// nil when the action does not declare one. Convenience for callers
// that need to branch on preview-declared vs not without spelling out
// the two-level nil check.
func (m Manifest) ApprovalPreview() *PreviewPolicy {
	if m.Approval == nil {
		return nil
	}
	return m.Approval.Preview
}

// Requires is the `[requires]` table holding connector dependencies.
type Requires struct {
	// Connectors is the list of `[[requires.connectors]]` entries.
	Connectors []RequiresConnector `toml:"connectors"`
}

// RequiresConnector pins a single connector dependency: its fully-qualified
// URI name, exact version, content hash, and the subset of declared
// capabilities the action will exercise. The capability subset is enforced
// at the action boundary at runtime — calls outside the subset are denied
// even when the connector's manifest permits the operation.
type RequiresConnector struct {
	// Name is the connector's fully-qualified URI per ADR-0002
	// (e.g. "github://aileron/slack", "hub://aileron/slack").
	Name string `toml:"name"`

	// Version is the exact pinned semantic version.
	Version string `toml:"version"`

	// Hash is the content hash of the connector binary plus its manifest
	// (e.g. "sha256:abc123..."). Verified at install time.
	Hash string `toml:"hash"`

	// Capabilities is the action's declared subset of operations on the
	// connector (e.g. ["chat:write", "channels:read"]). Empty means the
	// action declares no capabilities and may not invoke the connector.
	Capabilities []string `toml:"capabilities"`
}

// Match holds intent-matching metadata for the action. Shape stays minimal
// in v1; richer fields are added as ADR-0008 matures.
type Match struct {
	// Intent is the canonical natural-language phrase the runtime matches
	// against agent requests when surfacing this action.
	Intent string `toml:"intent"`
}

// Input is one declared call-time argument. The set of inputs maps
// directly to the JSON Schema `parameters` object the LLM sees when
// Aileron augments the agent's tool catalog with this action.
type Input struct {
	// Name is the argument's identifier. Referenced in
	// `[[execute]]` step `inputs` blocks as `${args.<name>}`.
	Name string `toml:"name"`

	// Type is the JSON Schema primitive: "string", "integer",
	// "number", or "boolean". Object/array types are post-MVP.
	Type string `toml:"type"`

	// Required defaults to true when nil. Set to a non-nil pointer
	// (`required = false` in TOML) to mark the argument optional.
	// Pointer rather than bool so absence is distinguishable from
	// an explicit false.
	Required *bool `toml:"required"`

	// Description becomes the field-level prose the LLM sees in the
	// `parameters.properties[name].description` slot. Required.
	Description string `toml:"description"`
}

// IsRequired reports whether the input is required, applying the
// default-true rule when the manifest left `required` unset.
func (i Input) IsRequired() bool {
	return i.Required == nil || *i.Required
}

// ExecuteStep is one step in the action's execution chain. The runtime
// invokes steps in declared order; on the first step failure the action
// terminates and returns the failing step's structured error
// (per ADR-0010). Successful prior steps are not auto-rolled-back.
type ExecuteStep struct {
	// ID is a label for the step. Subsequent steps may reference its outputs
	// via `${id.field}` interpolation in `Inputs`.
	ID string `toml:"id"`

	// Connector is the FQN of the connector whose operation this step calls.
	// Must appear in `Requires.Connectors` and must match one of those FQNs
	// exactly.
	Connector string `toml:"connector"`

	// Op is the connector operation invoked at this step.
	Op string `toml:"op"`

	// Idempotent overrides the connector's declared idempotency for this
	// call (per ADR-0010). v1 recognizes the field but the runtime uses
	// the connector's manifest-level declaration as the authoritative
	// retry signal; per-call overrides are flagged for post-MVP retry tuning.
	Idempotent *bool `toml:"idempotent"`

	// Inputs is the keyed argument map passed to the connector op. Values
	// may be string interpolations referencing call-time arguments
	// (`${args.X}`) or the outputs of prior steps (`${step_id.field}`).
	Inputs map[string]any `toml:"inputs"`
}
