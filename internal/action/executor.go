package action

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/ALRubinger/aileron/internal/cstore"
	"github.com/ALRubinger/aileron/internal/sandbox"
)

// Executor runs an installed action with the given call-time arguments
// and returns a Result whose Content becomes the tool-result message
// the LLM observes.
//
// Per [ADR-0010], action-side failures are returned as Results with
// IsError=true rather than as Go errors — the LLM sees the failure as
// a tool result and can decide how to proceed (retry, fall back to a
// different action, ask the user). A returned `error` is reserved for
// gateway-fatal conditions that should terminate the conversation
// turn (e.g. action not found, executor misconfigured).
//
// [ADR-0010]: https://docs.withaileron.ai/adr/0010-failure-handling
type Executor interface {
	Execute(ctx context.Context, name string, args map[string]any) (Result, error)
}

// Result is the synthesized tool-result content surfaced to the LLM.
// Both OpenAI's `tool` message and Anthropic's `tool_result` content
// block use a string body; the IsError flag is mapped to Anthropic's
// `is_error` field and prepended to OpenAI's content as a marker.
type Result struct {
	// Content is the JSON-encoded tool result body. Implementations
	// SHOULD return JSON the LLM can parse, but plain prose is
	// acceptable when the action's output is naturally a sentence.
	Content string

	// IsError reports whether the action failed in a way the LLM
	// should be informed of. Per ADR-0010, this surfaces as
	// `is_error: true` on Anthropic tool_result blocks and is encoded
	// into the OpenAI tool-message body as a sentinel JSON shape.
	IsError bool
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
// an IsError Result; later steps are not invoked. Successful prior
// steps are *not* rolled back (ADR-0010's "no auto-compensation"
// rule).
type SandboxExecutor struct {
	Actions    *Store
	Store      *cstore.Store
	Runtime    sandbox.Runtime

	mu       sync.Mutex
	cache    map[string]sandbox.Connector // keyed by canonical hash "sha256:<hex>"
}

// NewSandboxExecutor builds a SandboxExecutor with the supplied
// dependencies. Callers must Close it (which closes cached
// sandbox.Connectors) when finished; the runtime itself is not
// owned by the executor and must be closed separately.
func NewSandboxExecutor(actions *Store, store *cstore.Store, runtime sandbox.Runtime) *SandboxExecutor {
	return &SandboxExecutor{
		Actions: actions,
		Store:   store,
		Runtime: runtime,
		cache:   map[string]sandbox.Connector{},
	}
}

// Close releases any cached compiled connectors.
func (e *SandboxExecutor) Close(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	var firstErr error
	for _, c := range e.cache {
		if err := c.Close(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	e.cache = map[string]sandbox.Connector{}
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

		conn, compileErr := e.connectorFor(ctx, manifest, step.Connector)
		if compileErr != nil {
			return errorResult(compileErr), nil
		}

		// Build the op's args. v1 uses the call-time args directly for
		// the first step and merges the action's declared inputs. A
		// richer interpolator (`${args.X}`, `${prior_step.field}`) is
		// post-MVP; for now, untemplated `Inputs` are passed through
		// alongside `args`.
		opArgs := mergeArgs(args, step.Inputs)

		allowed := allowedAuthorityFor(manifest, step.Connector)
		res, invErr := conn.Invoke(ctx, sandbox.Call{
			Op:               step.Op,
			Args:             opArgs,
			AllowedAuthority: allowed,
		})
		if invErr != nil {
			return errorResult(invErr), nil
		}
		stepOutputs[step.ID] = res.Output

		// Last step's output is the action's output by convention.
		if i == len(manifest.Execute)-1 {
			body, mErr := json.Marshal(map[string]any{
				"action":  name,
				"output":  res.Output,
				"steps":   stepOutputs,
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

// connectorFor returns a compiled sandbox.Connector for the connector
// FQN referenced by `connectorFQN`. The connector is looked up in the
// action's [[requires.connectors]] block (for hash + version), the
// matching entry is fetched from the content-addressed store, and the
// binary is compiled — cached by hash for subsequent calls.
func (e *SandboxExecutor) connectorFor(ctx context.Context, m *Manifest, connectorFQN string) (sandbox.Connector, error) {
	// Find the [[requires.connectors]] entry that matches this FQN.
	var dep *RequiresConnector
	for i := range m.Requires.Connectors {
		if m.Requires.Connectors[i].Name == connectorFQN {
			dep = &m.Requires.Connectors[i]
			break
		}
	}
	if dep == nil {
		return nil, fmt.Errorf("action does not declare connector %q in [[requires.connectors]]", connectorFQN)
	}

	e.mu.Lock()
	if cached, ok := e.cache[dep.Hash]; ok {
		e.mu.Unlock()
		return cached, nil
	}
	e.mu.Unlock()

	entryDir, err := e.Store.EntryDir(dep.Hash)
	if err != nil {
		return nil, fmt.Errorf("connector %s: %w", connectorFQN, err)
	}
	manifestBytes, err := os.ReadFile(filepath.Join(entryDir, "manifest.toml"))
	if err != nil {
		return nil, fmt.Errorf("read connector manifest: %w", err)
	}
	manifest, err := cstore.ParseManifest(filepath.Join(entryDir, "manifest.toml"), manifestBytes)
	if err != nil {
		return nil, err
	}
	binary, err := loadConnectorBinary(entryDir)
	if err != nil {
		return nil, err
	}

	conn, err := e.Runtime.Compile(ctx, manifest, binary)
	if err != nil {
		return nil, err
	}

	e.mu.Lock()
	if existing, ok := e.cache[dep.Hash]; ok {
		// A concurrent caller won the race; drop ours and use theirs.
		e.mu.Unlock()
		_ = conn.Close(ctx)
		return existing, nil
	}
	e.cache[dep.Hash] = conn
	e.mu.Unlock()
	return conn, nil
}

// loadConnectorBinary reads the connector binary from the entry
// directory. The file is named connector.wasm or connector.bin per
// ADR-0004's tarball layout.
func loadConnectorBinary(entryDir string) ([]byte, error) {
	for _, name := range []string{"connector.wasm", "connector.bin"} {
		data, err := os.ReadFile(filepath.Join(entryDir, name))
		if err == nil {
			return data, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	return nil, fmt.Errorf("entry %s contains no connector binary (connector.wasm or connector.bin)", entryDir)
}

// mergeArgs combines the call-time args with the action's declared
// step Inputs. Call-time args take precedence on key collisions —
// untemplated step Inputs serve as defaults.
func mergeArgs(callArgs map[string]any, stepInputs map[string]any) map[string]any {
	out := make(map[string]any, len(callArgs)+len(stepInputs))
	for k, v := range stepInputs {
		out[k] = v
	}
	for k, v := range callArgs {
		out[k] = v
	}
	return out
}

// allowedAuthorityFor returns the action's declared subset of
// network host:port pairs for the given connector, used by the
// sandbox's host-policy as the action-boundary capability subset.
//
// v1's action manifest does not separately list network hosts (the
// action declares operation capabilities via `capabilities`). Until a
// dedicated network-subset field lands, the empty slice signals "no
// extra restriction" — the connector manifest's grant is the only
// gate. The seam is here so the action-boundary network gate lights
// up automatically when the schema gains the field.
func allowedAuthorityFor(_ *Manifest, _ string) []string {
	return nil
}

// errorResult wraps a sandbox or capability error into a tool-result
// that the LLM sees as IsError per ADR-0010. The structured error's
// fields are preserved in the body so the LLM (and audit log) can
// reason about class/boundary/details.
func errorResult(err error) Result {
	body, mErr := json.Marshal(map[string]any{
		"error": errorEnvelope(err),
	})
	if mErr != nil {
		return Result{Content: fmt.Sprintf(`{"error":{"class":"internal_error","message":%q}}`, err.Error()), IsError: true}
	}
	return Result{Content: string(body), IsError: true}
}

// errorEnvelope produces an ADR-0010-shaped JSON map from any error
// the executor encountered. Recognized types (`*action.Error`,
// `*sandbox.Error`, `*cstore.Error`) preserve their fields; other
// errors are rendered as a generic envelope.
func errorEnvelope(err error) map[string]any {
	out := map[string]any{
		"class":   "internal_error",
		"message": err.Error(),
	}
	var aerr *Error
	if errors.As(err, &aerr) {
		out["class"] = string(aerr.Class)
		out["message"] = aerr.Message
		out["boundary"] = string(aerr.Boundary)
		if aerr.Details != nil {
			out["details"] = aerr.Details
		}
		return out
	}
	var serr *sandbox.Error
	if errors.As(err, &serr) {
		out["class"] = string(serr.Class)
		out["message"] = serr.Message
		out["boundary"] = string(serr.Boundary)
		out["retriable"] = serr.Retriable
		if serr.Details != nil {
			out["details"] = serr.Details
		}
		return out
	}
	var cerr *cstore.Error
	if errors.As(err, &cerr) {
		out["class"] = string(cerr.Class)
		out["message"] = cerr.Message
		out["boundary"] = string(cerr.Boundary)
		out["retriable"] = cerr.Retriable
		if cerr.Details != nil {
			out["details"] = cerr.Details
		}
		return out
	}
	return out
}
