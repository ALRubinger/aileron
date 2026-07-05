package runtime

import (
	"context"

	"github.com/ALRubinger/aileron/internal/flightplan/freeze"
)

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

	// The following carry the non-secret actor provenance the daemon
	// surfaced on the action-run response (issue #1753). They are references
	// (a version, a content hash, a label, a binding name, a consent
	// posture), never credentials: the host injects credentials at the
	// boundary and this SPI never sees them. Each is "" when the daemon did
	// not populate it (a credential-less action, an unresolved binding).
	// The runtime records them per dispatch so a materialized output's audit
	// record walks back to the connector build and identity that produced it.

	// ConnectorVersion is the pinned semantic version of the connector that
	// produced the result.
	ConnectorVersion string
	// ConnectorHash is the connector's content hash (`sha256:<hex>`).
	ConnectorHash string
	// IdentityLabel is the non-secret identity label of the credential
	// binding the action used, or "" when none resolved.
	IdentityLabel string
	// CredentialBinding is the name of the credential binding the action
	// used (a reference, never the secret), or "" when none resolved.
	CredentialBinding string
	// ConsentDecision is the consent posture the action ran under
	// (`unattended` on the synchronous read path), or "" when unset.
	ConsentDecision string
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
	// RecordKindReach is one per tool-step record declaring the step's
	// per-step network reach (#1784, enforced by #1829): the declared Effect
	// plus the hosts the step ran under. `enforced:true` means the hosts are
	// the verified lock's sealed reach and the step ran under a step-scoped
	// proxy credential restricted to exactly them; `enforced:false` means the
	// step declared a contract but no sealed reach existed (a
	// directly-constructed plan outside the verified load path).
	RecordKindReach
)

// AuditRecord is one customer-owned audit entry. Fields holds exactly the
// declared audit.fields for the action (data reads referenced by resolved
// binding, never the dataset inline; ADR-0027 audit boundary).
type AuditRecord struct {
	// Kind is the record's kind, the explicit discriminator the CLI sink maps
	// to a model.EventType (RecordKindAction → flightplan.launch.action,
	// RecordKindLaunch → flightplan.launch, RecordKindOutput →
	// output.materialized, RecordKindReach → flightplan.launch.reach).
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

// ImageRunner boots the verified pinned environment image and runs the frozen
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

// ToolStepSpec is the input to the ToolStepRunner seam (#1829): one
// `kind: tool` step executed as a deterministic subprocess INSIDE the plan's
// single pinned environment (the container the whole-plan boot entered).
// There is no per-step image: the environment identity is the plan's one
// composed pin, already asserted by the signed lock at boot. The runner
// writes the resolved Input to MountPath (as input.json), execs the argv
// directly (no shell), and reads CollectPath back as the step's output.
type ToolStepSpec struct {
	// StepID is the executing step's id, for scope addressing and error
	// context.
	StepID string
	// Command is the argv to exec: Command[0] is the program, the rest its
	// arguments. It comes from the verified manifest, is exec'd with no
	// shell interpretation, and is the executed-command identity the audit
	// records.
	Command []string
	// MountPath is the in-environment directory the resolved Input is
	// written to before the exec (the read side of the mount boundary).
	// Empty means the step declared no mount and no input file is written.
	MountPath string
	// Input is the step's resolved binding input, written under MountPath.
	// It is a binding-resolved value only, never a credential (credentials
	// are injected host-side at the network boundary, not here).
	Input any
	// CollectPath is the in-environment path whose contents are read back as
	// the step's output (the run-and-collect boundary). Empty means the step
	// declared no collect and the step produces no collected output.
	CollectPath string
	// Hosts is the step's SEALED network reach from the verified lock's
	// stepTrust section, never the re-read frontmatter. Non-empty means the
	// runner MUST run the subprocess under a step-scoped proxy credential
	// registered for exactly these hosts, and MUST fail closed (never run
	// unscoped) when it cannot obtain one. Empty means the step declared no
	// reach and runs under the plan-boot proxy environment unchanged.
	Hosts []string
}

// ToolStepResult is the outcome of one tool-step execution: the value
// collected from CollectPath, which becomes the step's declared output and
// flows into downstream steps' dataflow unchanged.
type ToolStepResult struct {
	// Output is the value read back from CollectPath. It becomes the step's
	// named output; a step that declared no collect returns a nil Output.
	//
	// The production runner returns the collect file's RAW BYTES as a
	// string, never a decoded structure. That is the same contract the
	// materialize path already speaks: a string carrier is parsed as the
	// JSON it emitted (decodeCarrier), so a tool that wants its collected
	// output to materialize as a file artifact writes a file-map JSON
	// document ({path, mimeType, encoding, content}) or a JSON data
	// object/array to its collect path. A non-JSON collect file still flows
	// to downstream bindings as a plain string; it only refuses at the
	// materialize boundary if a step tries to materialize it directly.
	Output any
}

// LocalImageDigestResolver resolves a locally-resolvable image reference (a
// tag the local daemon carries) to its content digest, so the runtime can
// re-check that the composed local tag it is about to boot still resolves to
// the digest the signed lock attested (#1863). The runtime core depends only
// on this seam; the CLI (cmd/aileron) wires the production implementation over
// the same `image inspect` (RepoDigests-then-.Id) logic that PRODUCED the
// pin's Digest at freeze time, so the runtime never imports the container
// package (mirroring the ImageRunner discipline).
//
// Its contract, consumed ONLY on the composed-tools boot path (pin.LocalTag !=
// ""): return the local `sha256:` digest the daemon resolves image to. The
// boot compares that digest against the pin's attested Digest and fails closed
// on any mismatch. A resolve ERROR (the image is gone from the daemon, the
// inspect fails) is likewise fail-closed at the call site: the attested image
// is not present, so the boot must refuse rather than boot an unverified tag.
type LocalImageDigestResolver interface {
	Resolve(ctx context.Context, image string) (string, error)
}

// RegistryImageOrigin is the recorded install source of a plan installed by OCI
// reference (#1902/#1903): the registry+repository the signed artifact was
// pulled from and the version tag it was published under. Present is false for a
// locally-frozen plan, which has no origin sidecar and boots by its local tag.
// It answers only WHERE to pull the published image; the signed lock pin answers
// WHAT to verify, so this carries no trust anchor.
type RegistryImageOrigin struct {
	// Registry is the registry+repository the published image lives in (e.g.
	// "ghcr.io/acme/plan"), without tag or digest.
	Registry string
	// VersionTag is the store version id (freeze slug) the artifact was published
	// under; the composed image was published under a tag derived from it.
	VersionTag string
	// Present reports whether the loaded plan carries a registry origin. When
	// false, the runtime stays on the local-tag boot path; when true, the runtime
	// pulls and verifies the published image via RegistryImageResolver.
	Present bool
}

// RegistryImageResolver pulls a published composed (or foreign-base) image from
// the recorded registry origin and verifies it against the signed lock pin under
// the pin's binding kind (freeze.BindingKind), returning a bootable reference
// (#1903). The runtime core depends only on this seam; the CLI (cmd/aileron)
// wires the production implementation over pull.PullImage, so the runtime never
// imports oras/registry code (mirroring the ImageRunner discipline).
//
// Its contract, consumed ONLY when a loaded plan carries a registry origin
// (RegistryImageOrigin.Present): pull the published image the origin points at,
// verify it equals the signature-covered pin.Digest per the pin's binding, and
// return a content-addressed bootable "ref@manifest-digest" (both binding kinds
// anchor the boot to a manifest digest so a mutable tag is never booted after
// verification). Any mismatch, missing image, or pull failure is a fail-closed
// error and no reference: a registry-origin plan must never boot an unverified
// image.
type RegistryImageResolver interface {
	Resolve(ctx context.Context, origin RegistryImageOrigin, pin freeze.ImagePin) (bootRef string, err error)
}

// ToolStepRunner executes a single `kind: tool` step as a subprocess in the
// current (pinned) environment (#1829). Unlike ImageRunner (which boots one
// image and runs the WHOLE plan inside it), ToolStepRunner is a per-step
// seam consumed by the executor already running inside that boot: no
// sibling container is ever dispatched. The contract: write the resolved
// input at MountPath, exec the argv with the step-scoped proxy env when
// Hosts is non-empty (failing closed when the scope cannot be obtained),
// and read back CollectPath as the step's output. The runtime core depends
// only on this seam; the CLI wires the production subprocess implementation.
type ToolStepRunner interface {
	Run(ctx context.Context, spec ToolStepSpec) (ToolStepResult, error)
}
