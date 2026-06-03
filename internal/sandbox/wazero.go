package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
	"github.com/tetratelabs/wazero/sys"

	"github.com/ALRubinger/aileron/internal/cstore"
)

// WazeroRuntime is the default [Runtime] implementation, backed by
// Wazero (the only viable pure-Go WASM engine in v1; see the plan file
// at /Users/alr/.claude/plans/are-there-alternatives-to-stateless-starfish.md).
//
// One WazeroRuntime supports many [Connector]s, instantiated lazily as
// connectors are compiled. Each Connector compiles its WASM bytes once
// and caches the result; per-call invocation creates a fresh module
// instance from the cached compilation, runs `_start`, and tears the
// instance down — the per-invocation isolation required by ADR-0005's
// acceptance criterion #8.
type WazeroRuntime struct {
	rt            wazero.Runtime
	doer          HTTPDoer
	log           *slog.Logger
	spawnExecutor SpawnExecutor

	// hostMu guards host-module installation so concurrent Compile
	// calls do not race the WASI registration.
	hostMu sync.Mutex
}

// RuntimeOption configures a WazeroRuntime at construction.
type RuntimeOption func(*WazeroRuntime)

// WithHTTPDoer overrides the HTTP client the sandbox uses for
// `aileron_host.http_request`. Defaults to [http.DefaultClient]; tests
// substitute fake doers to exercise the host policy without network.
func WithHTTPDoer(d HTTPDoer) RuntimeOption {
	return func(r *WazeroRuntime) { r.doer = d }
}

// WithLogger sets the slog.Logger used to emit connector log lines and
// runtime diagnostics.
func WithLogger(log *slog.Logger) RuntimeOption {
	return func(r *WazeroRuntime) { r.log = log }
}

// NewWazeroRuntime builds a Wazero-backed runtime with the supplied
// options. Callers must Close() the returned runtime when finished.
func NewWazeroRuntime(ctx context.Context, opts ...RuntimeOption) (*WazeroRuntime, error) {
	rtCfg := wazero.NewRuntimeConfig().
		WithCloseOnContextDone(true). // wall-time enforcement: cancel ctx → instance terminates
		WithMemoryLimitPages(uint32(MaxMemoryBytes / wasmPageBytes))
	rt := wazero.NewRuntimeWithConfig(ctx, rtCfg)

	if _, err := wasi_snapshot_preview1.Instantiate(ctx, rt); err != nil {
		_ = rt.Close(ctx)
		return nil, fmt.Errorf("instantiate wasi_snapshot_preview1: %w", err)
	}

	w := &WazeroRuntime{
		rt:   rt,
		doer: http.DefaultClient,
		log:  slog.Default(),
	}
	for _, opt := range opts {
		opt(w)
	}

	if err := w.installHostModule(ctx); err != nil {
		_ = rt.Close(ctx)
		return nil, fmt.Errorf("install aileron_host module: %w", err)
	}

	return w, nil
}

// installHostModule registers the Aileron host functions under the
// `aileron_host` module name. Per-invocation state is threaded through
// context.Context so the module can be installed once and reused.
//
// HTTP-side imports: `log`, `http_request`, `http_response_size`,
// `http_response_status`, `http_response_read`.
//
// Spawn-side imports (per ADR-0002 spawn primitive): `spawn` (low-level
// envelope shape used by connectors that drive the runtime directly),
// `spawn_op` (high-level op-name + args shape), and the output
// retrieval trio `spawn_status`, `spawn_output_size`, `spawn_output_read`.
func (w *WazeroRuntime) installHostModule(ctx context.Context) error {
	w.hostMu.Lock()
	defer w.hostMu.Unlock()

	_, err := w.rt.NewHostModuleBuilder(hostModuleName).
		NewFunctionBuilder().
		WithFunc(hostLog).
		Export("log").
		NewFunctionBuilder().
		WithFunc(hostHTTPRequest).
		Export("http_request").
		NewFunctionBuilder().
		WithFunc(hostHTTPResponseSize).
		Export("http_response_size").
		NewFunctionBuilder().
		WithFunc(hostHTTPResponseStatus).
		Export("http_response_status").
		NewFunctionBuilder().
		WithFunc(hostHTTPResponseRead).
		Export("http_response_read").
		NewFunctionBuilder().
		WithFunc(hostSpawn).
		Export("spawn").
		NewFunctionBuilder().
		WithFunc(hostSpawnOp).
		Export("spawn_op").
		NewFunctionBuilder().
		WithFunc(hostSpawnStatus).
		Export("spawn_status").
		NewFunctionBuilder().
		WithFunc(hostSpawnOutputSize).
		Export("spawn_output_size").
		NewFunctionBuilder().
		WithFunc(hostSpawnOutputRead).
		Export("spawn_output_read").
		Instantiate(ctx)
	return err
}

// Close releases the underlying Wazero runtime. Outstanding Compile/Invoke
// calls return errors after Close.
func (w *WazeroRuntime) Close(ctx context.Context) error {
	if w == nil || w.rt == nil {
		return nil
	}
	return w.rt.Close(ctx)
}

// Compile implements [Runtime].
func (w *WazeroRuntime) Compile(ctx context.Context, manifest *cstore.Manifest, binary []byte) (Connector, error) {
	if manifest == nil {
		return nil, fmt.Errorf("manifest is required")
	}
	compiled, err := w.rt.CompileModule(ctx, binary)
	if err != nil {
		return nil, newConnectorLoadFailed(fmt.Sprintf("compile WASM: %s", err.Error()))
	}
	return &wazeroConnector{
		runtime:     w,
		manifest:    manifest,
		compiled:    compiled,
		policy:      NewHostPolicy(manifest),
		spawnPolicy: NewSpawnPolicy(manifest),
	}, nil
}

// wazeroConnector is the per-binary handle: caches the compiled module
// and the parsed policies so repeated invocations don't redo either.
type wazeroConnector struct {
	runtime     *WazeroRuntime
	manifest    *cstore.Manifest
	compiled    wazero.CompiledModule
	policy      *HostPolicy
	spawnPolicy *SpawnPolicy
}

// Close implements [Connector]. Releases the cached compiled module.
func (c *wazeroConnector) Close(ctx context.Context) error {
	if c == nil || c.compiled == nil {
		return nil
	}
	return c.compiled.Close(ctx)
}

// Invoke implements [Connector]. Spins a fresh sandbox instance, runs
// the call, tears down. Concurrent invocations are safe — Wazero
// produces independent linear memory per instance.
func (c *wazeroConnector) Invoke(ctx context.Context, call Call) (Result, error) {
	limits := call.Limits.Resolve()

	// Wall-time enforcement: cancel the context after the wall-time
	// limit. WithCloseOnContextDone (set on the runtime) propagates the
	// cancellation into the WASM execution and produces a sys.ExitError
	// with sys.ExitCodeContextCanceled or
	// sys.ExitCodeDeadlineExceeded.
	callCtx, cancel := context.WithTimeout(ctx, limits.WallTime)
	defer cancel()

	state := &hostState{
		policy:                 c.policy,
		doer:                   c.runtime.doer,
		actionGrant:            toSet(call.AllowedAuthority),
		logger:                 c.runtime.log,
		connectorFQN:           manifestFQN(c.manifest),
		expectedCredentialKind: manifestCredentialKind(c.manifest),
		credentialHeader:       manifestCredentialHeader(c.manifest),
		credentialFormat:       manifestCredentialFormat(c.manifest),
		credentialResolver:     call.CredentialResolver,
		spawnPolicy:            c.spawnPolicy,
		spawnExecutor:          c.runtime.spawnExecutor,
		actionSpawnGrant:       toSet(call.AllowedSpawnPrograms),
	}
	callCtx = ctxWithState(callCtx, state)

	stdin, err := encodeInvocationInput(call)
	if err != nil {
		return Result{}, newConnectorRuntimeError(fmt.Sprintf("encode input: %s", err.Error()))
	}
	stdoutBuf := &bytes.Buffer{}
	stderrBuf := &bytes.Buffer{}

	modCfg := wazero.NewModuleConfig().
		WithName(""). // anonymous instance — distinct from any other live instance
		WithStdin(bytes.NewReader(stdin)).
		WithStdout(stdoutBuf).
		WithStderr(stderrBuf).
		WithStartFunctions("_start")

	mod, instErr := c.runtime.rt.InstantiateModule(callCtx, c.compiled, modCfg)
	if instErr != nil {
		// If the module imports a function the runtime did not expose,
		// Wazero raises this at instantiation; treat as
		// connector_load_failed per AC #2 ("out-of-grant imports
		// refused at instantiation").
		var exitErr *sys.ExitError
		if errors.As(instErr, &exitErr) {
			return c.handleExit(state, exitErr, stdoutBuf, stderrBuf, limits)
		}
		// A capability_denied raised inside a host function during _start
		// would surface as a wrapped error; check the state's sticky error.
		if state.respErr != nil {
			return Result{Logs: state.logs}, state.respErr
		}
		return Result{}, newConnectorLoadFailed(instErr.Error())
	}
	defer mod.Close(callCtx)

	// _start returned normally. Decode stdout as the result envelope.
	return c.decodeResult(state, stdoutBuf.Bytes())
}

// handleExit translates a sys.ExitError into either a successful
// Result (exit 0) or a structured *Error (deadline / canceled / non-zero
// exit).
func (c *wazeroConnector) handleExit(state *hostState, exitErr *sys.ExitError, stdoutBuf, stderrBuf *bytes.Buffer, limits Limits) (Result, error) {
	switch exitErr.ExitCode() {
	case 0:
		return c.decodeResult(state, stdoutBuf.Bytes())
	case sys.ExitCodeDeadlineExceeded, sys.ExitCodeContextCanceled:
		return Result{Logs: state.logs}, newResourceLimit(LimitWallTime, limits.WallTime.String())
	default:
		// Connector exited non-zero. If a host function had stuck a
		// structured error on the state, prefer that — it carries the
		// boundary info; otherwise surface a generic
		// connector_runtime_error with the stderr tail.
		if state.respErr != nil {
			return Result{Logs: state.logs}, state.respErr
		}
		return Result{Logs: state.logs}, newConnectorRuntimeError(fmt.Sprintf(
			"connector exited %d; stderr: %s", exitErr.ExitCode(), tail(stderrBuf.String(), 256)))
	}
}

// decodeResult parses the stdout JSON envelope written by the
// connector. The envelope is a single JSON object — implementations
// may include `output` (the operation's result) and may indicate
// failure by including a top-level `error` key (per ADR-0010).
func (c *wazeroConnector) decodeResult(state *hostState, stdout []byte) (Result, error) {
	if state.respErr != nil {
		// A host function flagged an error before _start returned.
		return Result{Logs: state.logs}, state.respErr
	}
	if len(bytes.TrimSpace(stdout)) == 0 {
		// Empty stdout is allowed — connectors with no result return
		// an empty document. The Result.Output stays nil.
		return Result{Logs: state.logs}, nil
	}
	var env struct {
		Output map[string]any `json:"output"`
		Error  *struct {
			Class   string `json:"class"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(stdout, &env); err != nil {
		return Result{Logs: state.logs}, newConnectorRuntimeError(fmt.Sprintf(
			"connector stdout is not a valid JSON envelope: %s", err.Error()))
	}
	if env.Error != nil {
		class := env.Error.Class
		if class == "" {
			class = string(ClassConnectorRuntimeError)
		}
		return Result{Logs: state.logs}, &Error{
			Class:    FailureClass(class),
			Boundary: BoundarySandbox,
			Message:  env.Error.Message,
		}
	}
	return Result{Output: env.Output, Logs: state.logs}, nil
}

// encodeInvocationInput marshals the Call into the JSON shape the
// connector reads from stdin.
func encodeInvocationInput(call Call) ([]byte, error) {
	args := call.Args
	if args == nil {
		args = map[string]any{}
	}
	return json.Marshal(map[string]any{
		"op":   call.Op,
		"args": args,
	})
}

// manifestFQN safely returns the connector's fully-qualified name from
// its parsed manifest. Empty when the manifest is nil.
func manifestFQN(m *cstore.Manifest) string {
	if m == nil {
		return ""
	}
	return m.Connector.Name
}

// manifestCredentialKind safely returns the connector manifest's
// declared `[capabilities.credential].kind`. Empty string means the
// connector did not declare a credential capability — any credential
// reference on an http_request envelope is then refused at the sandbox
// boundary (capability_denied).
func manifestCredentialKind(m *cstore.Manifest) string {
	if m == nil || m.Capabilities.Credential == nil {
		return ""
	}
	return m.Capabilities.Credential.Kind
}

// manifestCredentialHeader safely returns the connector manifest's
// optional `[capabilities.credential].header`. Empty string means the
// injection path uses its default (Authorization).
func manifestCredentialHeader(m *cstore.Manifest) string {
	if m == nil || m.Capabilities.Credential == nil {
		return ""
	}
	return m.Capabilities.Credential.Header
}

// manifestCredentialFormat safely returns the connector manifest's
// optional `[capabilities.credential].format`. Empty string means the
// injection path uses its default (`Bearer {key}`).
func manifestCredentialFormat(m *cstore.Manifest) string {
	if m == nil || m.Capabilities.Credential == nil {
		return ""
	}
	return m.Capabilities.Credential.Format
}

// toSet converts a slice of capability strings to a lookup set used by
// the host functions for action-boundary checks.
func toSet(in []string) map[string]struct{} {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]struct{}, len(in))
	for _, s := range in {
		out[s] = struct{}{}
	}
	return out
}

// tail returns the last n bytes of s as a string (truncating from the
// front when s is longer than n). Used to keep error messages bounded.
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}

// Compile-time assertion: WazeroRuntime implements Runtime.
var _ Runtime = (*WazeroRuntime)(nil)

// Compile-time assertion: wazeroConnector implements Connector.
var _ Connector = (*wazeroConnector)(nil)
