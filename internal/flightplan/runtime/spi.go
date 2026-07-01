package runtime

import "context"

// The SPIs in this file are the thin seams the runtime core depends on so it
// stays unit-testable with fakes. The CLI (cmd/aileron) wires each one to the
// real daemon-backed implementation: ActionDispatcher → the action boundary
// (internal/action), Approver → the approval queue (internal/approval),
// AuditSink → the audit recorder (internal/audit), LLMSeam → an explicit LLM
// provider (unset by default in v1). The runtime never imports those packages
// concretely, mirroring the freeze DigestResolver/FeatureComposer discipline.

// DispatchResult is the outcome of one action dispatch through the sealed
// boundary. Output is the parsed action result the runtime reads downstream;
// it carries no credential (credentials are injected host-side).
type DispatchResult struct {
	// Output is the action's result payload, a JSON-shaped map the runtime
	// binds downstream steps against and redacts before surfacing.
	Output map[string]any
}

// ActionDispatcher dispatches a declared action through the sealed action
// boundary (ADR-0003/0005/0008). ref is the manifest action ref
// (aileron:<connector>.<action>); args are resolved binding values, never
// secrets. The host injects credentials at the boundary; dispatcher code
// never sees them.
type ActionDispatcher interface {
	Dispatch(ctx context.Context, ref string, args map[string]any) (DispatchResult, error)
}

// ApprovalRequest is the effect-driven gate the runtime raises before a
// non-read action-call. It mirrors the ActionApproval shape the daemon
// approval lifecycle consumes (ADR-0009) without taking the dependency.
type ApprovalRequest struct {
	// ActionRef is the action awaiting a decision.
	ActionRef string
	// Effect is the operation effect that routed this action to approval.
	Effect Effect
	// Args is a redacted summary of the resolved args presented to the
	// approver. Never secrets (bindings are references).
	Args map[string]any
}

// Decision is an approval outcome.
type Decision struct {
	Approved bool
	// Reason carries an optional human reason recorded in the audit on deny.
	Reason string
}

// Approver routes an effect-gated action through the out-of-band approval
// channel and blocks until a decision lands (ADR-0009). A read action never
// reaches the Approver; the runtime calls it only for write/delete/spend/
// external-send.
type Approver interface {
	Approve(ctx context.Context, req ApprovalRequest) (Decision, error)
}

// AuditRecordKind is the closed set of runtime audit record kinds. It is the
// explicit discriminator the CLI sink switches on to map each record to an
// internal/model.EventType, so the runtime stays free of an internal/model
// import while the third (output) kind is explicit rather than overloaded onto
// ActionRef.
type AuditRecordKind int

const (
	// RecordKindAction is one per-action-call dispatch record.
	RecordKindAction AuditRecordKind = iota
	// RecordKindLaunch is the per-launch summary record.
	RecordKindLaunch
	// RecordKindOutput is one per-materialized-output provenance record (#1752),
	// emitted for both action-call and transform materializing steps.
	RecordKindOutput
)

// AuditRecord is one customer-owned audit entry. Fields holds exactly the
// declared audit.fields for the action (data reads referenced by resolved
// binding, never the dataset inline; ADR-0027 audit boundary).
type AuditRecord struct {
	// Kind is the record's kind, the explicit discriminator the CLI sink maps
	// to a model.EventType (RecordKindAction → flightplan.launch.action,
	// RecordKindLaunch → flightplan.launch, RecordKindOutput →
	// output.materialized).
	Kind AuditRecordKind
	// ActionRef is the action the record describes, or "" for a per-launch
	// summary or per-output record.
	ActionRef string
	// Fields holds the declared audit field values. For a RecordKindOutput
	// record it holds the flat `aileron.*` attribute map the sink surfaces as
	// the top-level event payload.
	Fields map[string]any
	// Sink is the customer-owned sink reference from the trust contract.
	Sink string
}

// AuditSink receives the per-action and per-launch audit records. The CLI
// wires it to internal/audit.Recorder over the configured store; tests use a
// recording fake. Record returns a record id the RunResult surfaces.
type AuditSink interface {
	Record(ctx context.Context, rec AuditRecord) string
}

// SeamRequest is the input to the single marked LLM seam (the llm-seam step).
type SeamRequest struct {
	StepID   string
	Bindings map[string]any
	// Outputs are the named results the seam must produce.
	Outputs []string
}

// LLMSeam is the single marked non-deterministic seam (ADR-0027). It is the
// ONLY interface in the runtime that may reach a language model. In v1 the
// seam is unset by default, so an llm-seam step errors with "no seam provider
// configured" and a default launch reaches no LLM at all. The action-call and
// transform branches hold no reference to this type, so no deterministic step
// can reach an LLM by construction.
type LLMSeam interface {
	Run(ctx context.Context, req SeamRequest) (map[string]any, error)
}

// ImageRunSpec is the input to the ImageRunner seam. It carries the verified
// pinned image (the `ref@digest` string the runtime booted from the signed
// lock) plus everything the in-container launch needs to run the plan to
// completion: the frozen-unit selector (Name/Version), the launch input
// overrides, and the out-dir artifacts are written to.
type ImageRunSpec struct {
	// Image is the exact `ref@sha256:<hex>` the verified lock pinned. It is the
	// load-bearing security value: the runner MUST boot this image verbatim so
	// the environment entered corresponds to the lock's signed assertion.
	Image string
	// Name is the frozen skill name (the store selector).
	Name string
	// Version is the frozen version id (the store directory id).
	Version string
	// Inputs are the literal input overrides supplied at launch.
	Inputs LaunchArgs
	// OutDir is the directory file-target artifacts are written to. Empty skips
	// writing (artifacts are still recorded in the result).
	OutDir string
}

// ImageRunResult maps onto RunResult so the image-boot path returns the same
// public shape as the in-process path. A caller cannot tell from the result
// which path produced it, which keeps launch output identical across the
// boot-vs-in-process branch.
type ImageRunResult struct {
	// ContentHash is the verified content hash of the frozen unit that ran.
	ContentHash string
	// ResolvedInputs is the frozen resolved-input set.
	ResolvedInputs map[string]any
	// StepOutputs maps steps.<id> → its named outputs.
	StepOutputs map[string]map[string]any
	// Artifacts are the materialized output artifacts.
	Artifacts []Artifact
	// AuditIDs are the audit record ids emitted to the sink.
	AuditIDs []string
}

// ImageRunner boots the verified pinned rung-1/rung-2 image and runs the frozen
// plan to completion inside it (issue #1731). The runtime core depends only on
// this seam; the CLI (cmd/aileron) wires the production implementation over
// internal/sandbox/container, so the runtime never imports the container
// package. Its contract: boot the exact image named in ImageRunSpec.Image,
// run the selected frozen unit against the given inputs/out-dir, and return the
// RunResult-shaped outcome. The runtime supplies ImageRunSpec.Image straight
// from the verified lock, so the runner is handed a pin it must not re-resolve.
type ImageRunner interface {
	Run(ctx context.Context, spec ImageRunSpec) (ImageRunResult, error)
}

// ToolRunSpec is the input to the ToolImageRunner seam. It is the per-step
// rung-3 sibling-tool dispatch (ADR-0027 rung three): unlike ImageRunSpec, it
// does NOT run the whole plan. The runtime stays on the host and shells a single
// step out to a pinned sibling tool image with mount → run → collect I/O. The
// image is booted, the resolved step Input is mounted read-side at MountPath, the
// tool runs, and the bytes at CollectPath are read back as the step's output.
type ToolRunSpec struct {
	// Image is the exact `ref@sha256:<hex>` the verified lock pinned for this
	// step. It is the load-bearing security value: the runner MUST boot this
	// image verbatim and MUST NOT re-resolve it, so the tool dispatched
	// corresponds to the lock's signed assertion.
	Image string
	// StepID is the dispatching step's id, for addressable naming and error
	// context. It carries no security weight (the pin is Image).
	StepID string
	// MountPath is the path inside the tool image the resolved Input is mounted
	// at (the read side of the mount boundary). Empty means the step declared no
	// mount and the tool receives no mounted input.
	MountPath string
	// Input is the step's resolved binding input, mounted at MountPath. It is a
	// binding-resolved value only, never a credential (credentials are injected
	// host-side at the action boundary, not here).
	Input any
	// CollectPath is the path inside the tool image whose contents are read back
	// as the step's output (the run-and-collect boundary). Empty means the step
	// declared no collect and the tool produces no collected output.
	CollectPath string
}

// ToolRunResult is the outcome of one rung-3 per-step tool dispatch: the value
// collected from CollectPath, which becomes the dispatching step's declared
// output and flows into downstream steps' dataflow unchanged.
type ToolRunResult struct {
	// Output is the value read back from the tool's CollectPath. It becomes the
	// step's named output; a step that declared no collect returns a nil Output.
	Output any
}

// ToolImageRunner dispatches a single rung-3 step to its pinned sibling tool
// image with mount → run → collect I/O (ADR-0027 rung three, issue #1733).
// Unlike ImageRunner (which boots one image and runs the WHOLE plan inside it),
// ToolImageRunner is a per-step seam: the plan orchestration stays in-process on
// the host and only the individual step shells out to the tool image. The
// contract: boot the exact ToolRunSpec.Image, mount the resolved input at
// MountPath (read side), run the tool, and read back CollectPath as the step's
// output. The runtime core depends only on this seam; the CLI wires the
// container-backed implementation, so the runtime never imports the container
// package. The runtime supplies ToolRunSpec.Image straight from the verified
// lock, so the runner is handed a pin it must not re-resolve.
type ToolImageRunner interface {
	Run(ctx context.Context, spec ToolRunSpec) (ToolRunResult, error)
}
