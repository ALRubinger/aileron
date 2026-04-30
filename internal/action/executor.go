package action

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/ALRubinger/aileron/internal/credential"
	"github.com/ALRubinger/aileron/internal/cstore"
	"github.com/ALRubinger/aileron/internal/failure"
	"github.com/ALRubinger/aileron/internal/sandbox"
	"github.com/ALRubinger/aileron/internal/vault"
)

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

	// Vault resolves the credentials referenced by the action's
	// `[[bindings]]` block at call time. Per ADR-0005, the vault read
	// happens host-side and the connector never sees the bytes; the
	// executor builds a per-step credential.Resolver from the vault
	// and the action's binding metadata. Nil disables the credential
	// path — connectors that emit a `credential` field on their
	// http_request envelope receive `binding_required`.
	Vault vault.Vault

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
// owned by the executor and must be closed separately. Vault may be
// nil — the credential-mediation path is then disabled and any
// connector that emits a `credential` field will receive
// `binding_required` from the sandbox boundary.
func NewSandboxExecutor(actions *Store, store *cstore.Store, runtime sandbox.Runtime, v vault.Vault) *SandboxExecutor {
	return &SandboxExecutor{
		Actions: actions,
		Store:   store,
		Runtime: runtime,
		Vault:   v,
		cache:   map[string]cachedConnector{},
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
func (e *SandboxExecutor) Execute(ctx context.Context, name string, args map[string]any) (Result, error) {
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

	// Build per-step output map keyed by step ID; later steps may
	// reference prior outputs via interpolation. Interpolation itself
	// is post-MVP; v1 just records outputs so consumers can see them.
	stepOutputs := map[string]map[string]any{}
	for i, step := range manifest.Execute {
		// Defense-in-depth: action-boundary check. Per ADR-0003 the
		// op name is treated as the capability string for v1 — the
		// action's `capabilities = [...]` list must include the op
		// the step invokes.
		if capErr := EnforceCapability(manifest, step.Connector, step.Op); capErr != nil {
			return errorResult(capErr), nil
		}

		conn, connManifest, compileErr := e.connectorFor(ctx, manifest, step.Connector)
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
		// manifest declares `[capabilities.credential]` and the action
		// supplies a matching `[[bindings]]` entry, the host-side
		// http_request handler resolves the bound vault entry on the
		// connector's behalf (per ADR-0005). When either is missing
		// the resolver is nil and the sandbox returns
		// `binding_required` the moment the connector emits a
		// credential reference.
		resolver := e.resolverFor(connManifest, step.Connector, manifest)

		allowed := allowedAuthorityFor(manifest, step.Connector)
		res, invErr := conn.Invoke(ctx, sandbox.Call{
			Op:                 step.Op,
			Args:               opArgs,
			AllowedAuthority:   allowed,
			CredentialResolver: resolver,
		})
		if invErr != nil {
			return errorResult(invErr), nil
		}
		stepOutputs[step.ID] = res.Output

		// Last step's output is the action's output by convention.
		if i == len(manifest.Execute)-1 {
			body, mErr := json.Marshal(map[string]any{
				"action": name,
				"output": res.Output,
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

// connectorFor returns a compiled sandbox.Connector and the parsed
// connector manifest for the connector FQN referenced by
// `connectorFQN`. The connector is looked up in the action's
// [[requires.connectors]] block (for hash + version), the matching
// entry is fetched from the content-addressed store, and the binary
// is compiled — cached by hash for subsequent calls. The connector
// manifest is returned so the caller can read the connector's
// declared `[capabilities.credential].kind` without re-parsing.
func (e *SandboxExecutor) connectorFor(ctx context.Context, m *Manifest, connectorFQN string) (sandbox.Connector, *cstore.Manifest, error) {
	// Find the [[requires.connectors]] entry that matches this FQN.
	var dep *RequiresConnector
	for i := range m.Requires.Connectors {
		if m.Requires.Connectors[i].Name == connectorFQN {
			dep = &m.Requires.Connectors[i]
			break
		}
	}
	if dep == nil {
		return nil, nil, fmt.Errorf("action does not declare connector %q in [[requires.connectors]]", connectorFQN)
	}

	e.mu.Lock()
	if cached, ok := e.cache[dep.Hash]; ok {
		e.mu.Unlock()
		return cached.conn, cached.manifest, nil
	}
	e.mu.Unlock()

	entryDir, err := e.Store.EntryDir(dep.Hash)
	if err != nil {
		return nil, nil, fmt.Errorf("connector %s: %w", connectorFQN, err)
	}
	manifestBytes, err := os.ReadFile(filepath.Join(entryDir, "manifest.toml"))
	if err != nil {
		return nil, nil, fmt.Errorf("read connector manifest: %w", err)
	}
	cmf, err := cstore.ParseManifest(filepath.Join(entryDir, "manifest.toml"), manifestBytes)
	if err != nil {
		return nil, nil, err
	}
	binPath := filepath.Join(entryDir, "connector.wasm")
	if _, statErr := os.Stat(binPath); errors.Is(statErr, os.ErrNotExist) {
		binPath = filepath.Join(entryDir, "connector.wat")
	}
	binBytes, err := os.ReadFile(binPath)
	if err != nil {
		return nil, nil, fmt.Errorf("read connector binary: %w", err)
	}
	conn, err := e.Runtime.Compile(ctx, cmf, binBytes)
	if err != nil {
		return nil, nil, err
	}
	e.mu.Lock()
	e.cache[dep.Hash] = cachedConnector{conn: conn, manifest: cmf}
	e.mu.Unlock()
	return conn, cmf, nil
}

// resolverFor returns the credential.Resolver for the named connector,
// or nil when no credential mediation is needed for this step.
//
// Returns nil when:
//   - the connector manifest declares no [capabilities.credential]
//     (the connector won't request a credential at runtime); or
//   - the action manifest has no [[bindings]] entry for the connector
//     (the connector will request one but the host short-circuits to
//     `binding_required`); or
//   - the executor has no Vault wired (dev-mode without credentials).
//
// When non-nil, the returned resolver is scoped to a single Invoke —
// the host drops the reference on completion, satisfying the
// per-invocation lifetime requirement of ADR-0005.
func (e *SandboxExecutor) resolverFor(connManifest *cstore.Manifest, connectorFQN string, action *Manifest) credential.Resolver {
	if connManifest == nil || connManifest.Capabilities.Credential == nil {
		return nil
	}
	if e.Vault == nil {
		return nil
	}
	binding := bindingFor(action, connectorFQN)
	if binding == nil {
		return nil
	}
	return &credential.VaultResolver{
		Vault:        e.Vault,
		VaultPath:    binding.VaultPath,
		ExpectedKind: connManifest.Capabilities.Credential.Kind,
	}
}

// bindingFor returns the action's [[bindings]] entry for the named
// connector, or nil when none exists. Validate() guarantees at most
// one binding per connector.
func bindingFor(action *Manifest, connectorFQN string) *Binding {
	if action == nil {
		return nil
	}
	for i := range action.Bindings {
		if action.Bindings[i].Connector == connectorFQN {
			return &action.Bindings[i]
		}
	}
	return nil
}

// mergeArgs combines the call-time args with the action's declared
// step inputs. Caller args win on key conflict (the LLM's
// interpretation of the user message takes precedence over the
// author's static defaults).
func mergeArgs(args, stepInputs map[string]any) map[string]any {
	out := make(map[string]any, len(args)+len(stepInputs))
	for k, v := range stepInputs {
		out[k] = v
	}
	for k, v := range args {
		out[k] = v
	}
	return out
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
