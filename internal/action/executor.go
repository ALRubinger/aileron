package action

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/ALRubinger/aileron/internal/binding"
	"github.com/ALRubinger/aileron/internal/credential"
	"github.com/ALRubinger/aileron/internal/cstore"
	"github.com/ALRubinger/aileron/internal/failure"
	"github.com/ALRubinger/aileron/internal/sandbox"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// tracerName names the OTel instrumentation library reported on
// spans this package emits. Distinct from the resource service.name
// (which identifies Aileron itself); per OTel conventions this names
// the *instrumentation*, so traces carry a stable provenance for
// the action-execution layer.
const tracerName = "github.com/ALRubinger/aileron/internal/action"

// Executor runs an installed action with the given call-time arguments
// and returns a Result whose Content becomes the tool-result message
// the LLM observes.
//
// Per [ADR-0010], action-side failures are returned as Results carrying
// a non-nil [failure.Failure] (mapped to ADR-0010's structured
// envelope) rather than as Go errors — the LLM sees the failure as a
// tool result and can decide how to proceed (retry, fall back, ask the
// user). A returned `error` is reserved for gateway-fatal conditions
// that should terminate the conversation turn (e.g. action not found,
// executor misconfigured).
//
// [ADR-0010]: https://docs.withaileron.ai/adr/0010-failure-handling
type Executor interface {
	Execute(ctx context.Context, name string, args map[string]any) (Result, error)
}

// Result is the synthesized tool-result content surfaced to the LLM.
// Success and failure are mutually exclusive: when Failure is non-nil
// the result is an error and Content is unused; when Failure is nil
// the action succeeded and Content carries the payload (a JSON
// document or plain prose).
//
// On the wire, Failure is rendered into the agent-visible tool-result
// shape via [failure.ToOpenAIToolMessage] / [failure.ToAnthropicToolResult],
// so the LLM sees a stable structured envelope regardless of provider.
type Result struct {
	// Content is the success payload. JSON the LLM can parse is the
	// preferred shape, but plain prose is acceptable for actions
	// whose output is naturally a sentence.
	Content string

	// Failure marks an action-side error per ADR-0010. Mutually
	// exclusive with a populated Content; the gateway's intercept
	// layer renders the failure into the provider-shaped tool-result
	// the LLM sees.
	Failure *failure.Failure
}

// StubExecutor returns a placeholder JSON result describing what
// would have been executed. Retained for tests that don't need real
// connector execution; production wiring uses [SandboxExecutor].
type StubExecutor struct{}

// Execute returns a Result whose Content is a small JSON object
// summarising the call. Always succeeds; never returns a Go error.
func (StubExecutor) Execute(_ context.Context, name string, args map[string]any) (Result, error) {
	if args == nil {
		args = map[string]any{}
	}
	body, err := json.Marshal(map[string]any{
		"executed": false,
		"stub":     true,
		"action":   name,
		"args":     args,
		"note":     "Action execution is not yet implemented; this is a placeholder result.",
	})
	if err != nil {
		return Result{}, err
	}
	return Result{Content: string(body)}, nil
}

// SandboxExecutor is the production [Executor] for issue #359. It
// composes the action [Store] (action lookup), the cstore [cstore.Store]
// (connector binary lookup by content hash), and the sandbox
// [sandbox.Runtime] (per-call WASM instantiation) into the full
// execution chain ratified by ADR-0003 and ADR-0005:
//
//  1. Resolve the named action.
//  2. For each `[[execute]]` step, locate the connector entry the
//     step references in the action's `[[requires.connectors]]`
//     block.
//  3. Apply the action-boundary capability check (defense-in-depth
//     per ADR-0003 / issue #359 acceptance #7).
//  4. Read the connector's manifest.toml + binary from the
//     content-addressed store at the entry's hash.
//  5. Compile (cached by hash) and invoke a fresh sandbox.
//  6. Aggregate per-step outputs into the Result.
//
// First-failure-terminates per ADR-0010: the first step error returns
// a Result whose Failure is populated; later steps are not invoked.
// Successful prior steps are *not* rolled back (ADR-0010's "no
// auto-compensation" rule).
type SandboxExecutor struct {
	Actions *Store
	Store   *cstore.Store
	Runtime sandbox.Runtime

	// Bindings resolves the credentials connectors need at call time
	// per ADR-0005 + ADR-0006. The store is keyed by (connector FQN,
	// capability kind); the executor consults it on each step that
	// references a connector with a declared `[capabilities.credential]`
	// and threads the matching `credential.Resolver` into the sandbox
	// Call. Nil disables the credential path — connectors that emit a
	// `credential` field on their http_request envelope receive
	// `binding_required` from the sandbox boundary.
	Bindings binding.Store

	mu    sync.Mutex
	cache map[string]cachedConnector // keyed by canonical hash "sha256:<hex>"
}

// cachedConnector pairs a compiled connector with the parsed
// connector manifest so subsequent calls can read the connector's
// declared `[capabilities.credential].kind` without re-parsing the
// manifest from disk.
type cachedConnector struct {
	conn     sandbox.Connector
	manifest *cstore.Manifest
}

// NewSandboxExecutor builds a SandboxExecutor with the supplied
// dependencies. Callers must Close it (which closes cached
// sandbox.Connectors) when finished; the runtime itself is not
// owned by the executor and must be closed separately. Bindings may
// be nil — the credential-mediation path is then disabled and any
// connector that emits a `credential` field will receive
// `binding_required` from the sandbox boundary.
func NewSandboxExecutor(actions *Store, store *cstore.Store, runtime sandbox.Runtime, bindings binding.Store) *SandboxExecutor {
	return &SandboxExecutor{
		Actions:  actions,
		Store:    store,
		Runtime:  runtime,
		Bindings: bindings,
		cache:    map[string]cachedConnector{},
	}
}

// Close releases any cached compiled connectors.
func (e *SandboxExecutor) Close(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	var firstErr error
	for _, c := range e.cache {
		if err := c.conn.Close(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	e.cache = map[string]cachedConnector{}
	return firstErr
}

// Execute implements [Executor].
//
// Emits an `aileron.action.execute` span around the full call and a
// nested `aileron.connector.call` span around each step's
// connector invocation. Span attributes use the OTel-namespaced shape
// shared with the audit log (PR #452) so spans and audit events carry
// identical keys — single source of truth.
func (e *SandboxExecutor) Execute(ctx context.Context, name string, args map[string]any) (res Result, err error) {
	tracer := otel.GetTracerProvider().Tracer(tracerName)
	ctx, span := tracer.Start(ctx, "aileron.action.execute",
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(attribute.String("aileron.action.name", name)),
	)
	defer func() { annotateActionSpan(span, res, err); span.End() }()

	if e.Actions == nil || e.Store == nil || e.Runtime == nil {
		return Result{}, fmt.Errorf("SandboxExecutor: Actions, Store, and Runtime are all required")
	}
	loaded, err := e.Actions.Get(name)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return Result{}, fmt.Errorf("action %q not found", name)
		}
		return Result{}, err
	}
	manifest := loaded.Manifest
	if len(manifest.Execute) == 0 {
		return Result{}, fmt.Errorf("action %q has no execute steps", name)
	}
	span.SetAttributes(attribute.Int("aileron.action.steps_count", len(manifest.Execute)))

	// Build per-step output map keyed by step ID; later steps may
	// reference prior outputs via interpolation. Interpolation itself
	// is post-MVP; v1 just records outputs so consumers can see them.
	stepOutputs := map[string]map[string]any{}
	for i, step := range manifest.Execute {
		// Defense-in-depth: action-boundary check. Per ADR-0003 the
		// op name is treated as the capability string for v1 — the
		// action's `capabilities = [...]` list must include the op
		// the step invokes.
		if capErr := enforceCapabilityWithSpan(ctx, manifest, step.Connector, step.Op); capErr != nil {
			return errorResult(capErr), nil
		}

		conn, connManifest, connHash, compileErr := e.connectorFor(ctx, manifest, step.Connector)
		if compileErr != nil {
			return errorResult(compileErr), nil
		}

		// Build the op's args. v1 uses the call-time args directly for
		// the first step and merges the action's declared inputs. A
		// richer interpolator (`${args.X}`, `${prior_step.field}`) is
		// post-MVP; for now, untemplated `Inputs` are passed through
		// alongside `args`.
		opArgs := mergeArgs(args, step.Inputs)

		// Build a per-step credential resolver. When the connector
		// manifest declares `[capabilities.credential]` and the user
		// has created a matching binding via `aileron binding setup`,
		// the host-side http_request handler resolves the bound vault
		// entry on the connector's behalf (per ADR-0005 + ADR-0006).
		// When no binding exists, the resolver is nil and the sandbox
		// returns `binding_required` the moment the connector emits a
		// credential reference. Ambiguous resolution (multiple bindings
		// for the same connector+kind) also yields nil — the failure
		// envelope's details carry the candidates so the agent can
		// guide the user.
		resolver := e.resolverFor(ctx, connManifest, step.Connector)

		allowed := allowedAuthorityFor(manifest, step.Connector)
		stepRes, invErr := e.invokeWithSpan(ctx, conn, step.Connector, step.Op, connHash, sandbox.Call{
			Op:                 step.Op,
			Args:               opArgs,
			AllowedAuthority:   allowed,
			CredentialResolver: resolver,
		})
		if invErr != nil {
			return errorResult(invErr), nil
		}
		stepOutputs[step.ID] = stepRes.Output

		// Last step's output is the action's output by convention.
		if i == len(manifest.Execute)-1 {
			body, mErr := json.Marshal(map[string]any{
				"action": name,
				"output": stepRes.Output,
				"steps":  stepOutputs,
			})
			if mErr != nil {
				return Result{}, mErr
			}
			return Result{Content: string(body)}, nil
		}
	}
	// Unreachable: every loop iteration either errors or, on the last
	// iteration, returns the success result.
	return Result{}, nil
}

// enforceCapabilityWithSpan wraps the action-boundary capability
// check (ADR-0003 §"defense in depth") in an
// `aileron.capability.check` span. Net-new emission point — until
// this lands, "how often does the sandbox say no?" had no
// observability surface. Attributes use the OTel-namespaced shape
// shared with the audit log: aileron.connector.fqn,
// aileron.capability.kind, aileron.action.name. Denial sets span
// status Error with the *Error's message; success leaves status
// Unset.
func enforceCapabilityWithSpan(ctx context.Context, m *Manifest, connectorFQN, capability string) error {
	tracer := otel.GetTracerProvider().Tracer(tracerName)
	_, span := tracer.Start(ctx, "aileron.capability.check",
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(
			attribute.String("aileron.connector.fqn", connectorFQN),
			attribute.String("aileron.capability.kind", capability),
		),
	)
	defer span.End()
	if m != nil && m.Name != "" {
		span.SetAttributes(attribute.String("aileron.action.name", m.Name))
	}
	if err := EnforceCapability(m, connectorFQN, capability); err != nil {
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	return nil
}

// invokeWithSpan wraps a single connector.Invoke call in an
// `aileron.connector.call` span. Attributes follow the OTel-shaped
// audit schema: aileron.connector.{fqn,op,hash}. Invoke errors mark
// the span as Error; success leaves status Unset.
func (e *SandboxExecutor) invokeWithSpan(ctx context.Context, conn sandbox.Connector, fqn, op, hash string, call sandbox.Call) (sandbox.Result, error) {
	tracer := otel.GetTracerProvider().Tracer(tracerName)
	ctx, span := tracer.Start(ctx, "aileron.connector.call",
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			attribute.String("aileron.connector.fqn", fqn),
			attribute.String("aileron.connector.op", op),
			attribute.String("aileron.connector.hash", hash),
		),
	)
	defer span.End()
	res, err := conn.Invoke(ctx, call)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
	}
	return res, err
}

// annotateActionSpan reflects the execute outcome onto the
// aileron.action.execute span. Two failure modes per ADR-0010:
//
//   - Result.Failure non-nil: structured action-side failure
//     (capability_denied, binding_required, etc.). The LLM sees
//     these as tool results and can decide how to proceed; the span
//     is still marked Error because the action's operation did fail
//     in the OTel sense, with the failure attributes attached for
//     filtering.
//   - err non-nil: gateway-fatal Go error (executor misconfigured,
//     action not found). Span Error with the err's message.
func annotateActionSpan(span trace.Span, res Result, err error) {
	if !span.IsRecording() {
		return
	}
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return
	}
	if res.Failure != nil {
		span.SetStatus(codes.Error, res.Failure.Message())
		span.SetAttributes(
			attribute.String("aileron.failure.class", string(res.Failure.Class())),
			attribute.String("aileron.failure.boundary", string(res.Failure.Boundary())),
			attribute.Bool("aileron.failure.retriable", res.Failure.Retriable()),
		)
	}
}

// connectorFor returns a compiled sandbox.Connector, the parsed
// connector manifest, and the resolved content hash for the
// connector FQN referenced by `connectorFQN`. The connector is
// looked up in the action's [[requires.connectors]] block (for hash
// + version), the matching entry is fetched from the content-
// addressed store, and the binary is compiled — cached by hash for
// subsequent calls. The connector manifest is returned so the
// caller can read the connector's declared
// `[capabilities.credential].kind` without re-parsing; the hash is
// returned so observability spans can attach `aileron.connector.hash`
// without re-walking the manifest.
func (e *SandboxExecutor) connectorFor(ctx context.Context, m *Manifest, connectorFQN string) (sandbox.Connector, *cstore.Manifest, string, error) {
	// Find the [[requires.connectors]] entry that matches this FQN.
	var dep *RequiresConnector
	for i := range m.Requires.Connectors {
		if m.Requires.Connectors[i].Name == connectorFQN {
			dep = &m.Requires.Connectors[i]
			break
		}
	}
	if dep == nil {
		return nil, nil, "", fmt.Errorf("action does not declare connector %q in [[requires.connectors]]", connectorFQN)
	}

	e.mu.Lock()
	if cached, ok := e.cache[dep.Hash]; ok {
		e.mu.Unlock()
		return cached.conn, cached.manifest, dep.Hash, nil
	}
	e.mu.Unlock()

	entryDir, err := e.Store.EntryDir(dep.Hash)
	if err != nil {
		return nil, nil, "", fmt.Errorf("connector %s: %w", connectorFQN, err)
	}
	manifestBytes, err := os.ReadFile(filepath.Join(entryDir, "manifest.toml"))
	if err != nil {
		return nil, nil, "", fmt.Errorf("read connector manifest: %w", err)
	}
	cmf, err := cstore.ParseManifest(filepath.Join(entryDir, "manifest.toml"), manifestBytes)
	if err != nil {
		return nil, nil, "", err
	}
	binBytes, err := loadConnectorBytes(entryDir)
	if err != nil {
		return nil, nil, "", err
	}
	conn, err := e.Runtime.Compile(ctx, cmf, binBytes)
	if err != nil {
		return nil, nil, "", err
	}
	e.mu.Lock()
	e.cache[dep.Hash] = cachedConnector{conn: conn, manifest: cmf}
	e.mu.Unlock()
	return conn, cmf, dep.Hash, nil
}

// loadConnectorBytes returns the WASM bytes the runtime should compile
// for the connector under entryDir. Reads `connector.wasm` and falls
// back to `connector.wat` so test fixtures can ship a text-format
// connector without needing a `wat2wasm` toolchain.
func loadConnectorBytes(entryDir string) ([]byte, error) {
	binPath := filepath.Join(entryDir, "connector.wasm")
	if _, statErr := os.Stat(binPath); errors.Is(statErr, os.ErrNotExist) {
		binPath = filepath.Join(entryDir, "connector.wat")
	}
	binBytes, err := os.ReadFile(binPath)
	if err != nil {
		return nil, fmt.Errorf("read connector binary: %w", err)
	}
	return binBytes, nil
}

// resolverFor returns the credential.Resolver for the named connector,
// or nil when no credential mediation is needed (or possible) for this
// step.
//
// Returns nil when any of the following holds:
//   - the connector manifest declares no [capabilities.credential]
//     (the connector won't request a credential at runtime); or
//   - the executor has no binding store wired (dev-mode without a
//     vault); or
//   - the binding store has no entry for the (connector FQN, kind)
//     tuple — the host then surfaces `binding_required` on the first
//     credential reference; or
//   - the binding store has more than one matching entry — the
//     ambiguity also surfaces as `binding_required`, and the failure
//     envelope's details (populated by the host) name the candidates.
//
// When non-nil, the returned resolver is scoped to a single Invoke —
// the host drops the reference on completion, satisfying the
// per-invocation lifetime requirement of ADR-0005.
func (e *SandboxExecutor) resolverFor(ctx context.Context, connManifest *cstore.Manifest, connectorFQN string) credential.Resolver {
	if connManifest == nil || connManifest.Capabilities.Credential == nil {
		return nil
	}
	if e.Bindings == nil {
		return nil
	}
	return e.Bindings.ResolverFor(ctx, connectorFQN, connManifest.Capabilities.Credential.Kind)
}

// mergeArgs combines the call-time args with the action's declared
// step inputs. Step inputs are run through `${args.<name>}`
// interpolation against the call-time args first, then call-time
// args overwrite any same-key entries (the LLM's interpretation of
// the user message takes precedence over the author's static
// defaults).
//
// Per ADR-0003, validation already guarantees every `${args.X}`
// reference resolves to a declared input — by the time we get here,
// any unresolved reference is a runtime args mismatch (the agent
// failed to supply a declared input). Unmatched references are left
// as the literal `${args.X}` so the connector can either accept the
// raw token or surface its own error.
func mergeArgs(args, stepInputs map[string]any) map[string]any {
	out := make(map[string]any, len(args)+len(stepInputs))
	for k, v := range stepInputs {
		out[k] = interpolateArgs(v, args)
	}
	for k, v := range args {
		out[k] = v
	}
	return out
}

// interpolateArgs walks v (which TOML decodes to a string, []any, or
// map[string]any) and substitutes `${args.NAME}` tokens with the
// corresponding entry from args. Non-string leaves are returned
// unchanged.
func interpolateArgs(v any, args map[string]any) any {
	switch val := v.(type) {
	case string:
		return argsRe.ReplaceAllStringFunc(val, func(match string) string {
			m := argsRe.FindStringSubmatch(match)
			if len(m) != 2 {
				return match
			}
			rep, ok := args[m[1]]
			if !ok {
				return match
			}
			return fmt.Sprintf("%v", rep)
		})
	case []any:
		out := make([]any, len(val))
		for i, item := range val {
			out[i] = interpolateArgs(item, args)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(val))
		for k, item := range val {
			out[k] = interpolateArgs(item, args)
		}
		return out
	default:
		return v
	}
}

// allowedAuthorityFor returns the network authority subset the action
// permits for the given connector — the intersection of the
// connector's manifest network capability and the action's declared
// subset. v1 doesn't yet enforce subset narrowing at the action layer
// for network; the connector's manifest is authoritative until #362
// lands binding-level scoping. This function exists so the call site
// passes through whatever the action allows.
func allowedAuthorityFor(_ *Manifest, _ string) []string {
	// Returning nil tells the sandbox to defer to the connector
	// manifest's `[capabilities.network].hosts`. Action-level
	// subset narrowing for network capabilities lands in a follow-up.
	return nil
}

// errorResult wraps a sandbox or capability error into a Result whose
// Failure is the ADR-0010 envelope the LLM sees as a tool-result.
// Recognised typed errors (`*action.Error`, `*sandbox.Error`,
// `*cstore.Error`) carry Class/Boundary/Retriable/Details and map to
// the corresponding [failure] constructor; other errors fall back to
// `connector_runtime_error` with boundary `runtime`.
func errorResult(err error) Result {
	return Result{Failure: failureFromError(err)}
}

// failureFromError converts an executor-side error into a
// *failure.Failure whose class / boundary / retriable values fit
// ADR-0010's closed taxonomy. The mapping preserves details and
// wraps the original error as the Failure's cause.
func failureFromError(err error) *failure.Failure {
	if err == nil {
		return nil
	}
	var aerr *Error
	if errors.As(err, &aerr) {
		opts := []failure.Option{failure.WithCause(err)}
		if aerr.Details != nil {
			opts = append(opts, failure.WithDetails(aerr.Details))
		}
		switch aerr.Class {
		case ClassCapabilityDenied:
			return failure.CapabilityDeniedAt(failure.Action, aerr.Message, opts...)
		}
		// Other action-package classes (parse_error, validation_error,
		// not_found) are install-time concerns; if they leak into a
		// runtime call, surface as a runtime error so the LLM sees a
		// closed-taxonomy class.
		opts = append(opts, failure.WithBoundary(failure.Runtime))
		return failure.ConnectorRuntime(aerr.Message, false, opts...)
	}
	var serr *sandbox.Error
	if errors.As(err, &serr) {
		opts := []failure.Option{failure.WithCause(err)}
		if serr.Details != nil {
			opts = append(opts, failure.WithDetails(serr.Details))
		}
		switch serr.Class {
		case sandbox.ClassCapabilityDenied:
			return failure.CapabilityDeniedAt(failure.Sandbox, serr.Message, opts...)
		case sandbox.ClassResourceLimitExceeded:
			return failure.ResourceLimitFailure(serr.Message, opts...)
		case sandbox.ClassBindingRequired:
			return failure.BindingRequiredFailure(serr.Message, opts...)
		}
		// connector_runtime_error or connector_load_failed — both map
		// to ConnectorRuntimeError in ADR-0010's closed taxonomy.
		return failure.ConnectorRuntime(serr.Message, serr.Retriable, opts...)
	}
	var cerr *cstore.Error
	if errors.As(err, &cerr) {
		opts := []failure.Option{failure.WithCause(err)}
		if cerr.Details != nil {
			opts = append(opts, failure.WithDetails(cerr.Details))
		}
		switch cerr.Class {
		case cstore.ClassHashMismatch:
			return failure.HashMismatchFailure(cerr.Message, opts...)
		case cstore.ClassSignatureFailure:
			return failure.SignatureFailureFailure(cerr.Message, opts...)
		}
		// Install-time classes (parse, validation, fetch, malformed,
		// fqn_mismatch, unknown_scheme, store_unwritable) leaking into
		// runtime → ConnectorRuntimeError, runtime boundary.
		opts = append(opts, failure.WithBoundary(failure.Runtime))
		return failure.ConnectorRuntime(cerr.Message, cerr.Retriable, opts...)
	}
	return failure.ConnectorRuntime(err.Error(), false,
		failure.WithBoundary(failure.Runtime),
		failure.WithCause(err))
}
