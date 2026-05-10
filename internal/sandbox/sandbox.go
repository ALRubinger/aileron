// Package sandbox implements the WASM connector runtime.
//
// Per [ADR-0005], every connector runs under an isolation boundary the
// runtime enforces. v1 targets WASI Preview 1 plus Aileron-specific host
// imports (`aileron_http_request`, `aileron_log`); the WASI Preview 2 /
// Component Model surface ratified by ADR-0005 lights up when Wazero
// supports it. The capability enforcement, resource limits, structured
// errors, and per-call lifecycle in this package satisfy issue #359's
// acceptance criteria without depending on P2.
//
// The package exposes a small interface — [Runtime] and [Connector] — so
// the engine is swappable. The default implementation is Wazero; tests
// substitute fakes where they want to exercise the host-side policy
// independently.
//
// [ADR-0005]: https://docs.withaileron.ai/adr/0005-sandbox-choice
package sandbox

import (
	"context"
	"time"

	"github.com/ALRubinger/aileron/internal/credential"
	"github.com/ALRubinger/aileron/internal/cstore"
)

// Runtime is the engine that compiles connector binaries and produces
// invokable [Connector] handles. A Runtime is goroutine-safe; multiple
// Connectors compiled from a single Runtime may be invoked in parallel.
type Runtime interface {
	// Compile prepares a connector for invocation. Compilation is
	// typically expensive (Wazero parses, validates, and ahead-of-time
	// translates the WASM module); instances produced from the compiled
	// form are cheap.
	//
	// `manifest` is the connector's parsed `manifest.toml` — the runtime
	// uses its `[capabilities.network]` and resource declarations to
	// gate calls and configure limits per ADR-0002 / ADR-0005.
	// `binary` is the raw `.wasm` bytes from the content-addressed
	// store.
	Compile(ctx context.Context, manifest *cstore.Manifest, binary []byte) (Connector, error)

	// Close releases the runtime and any compiled connectors. Calling
	// Compile after Close is a programming error.
	Close(ctx context.Context) error
}

// Connector is a compiled, ready-to-instantiate connector binary. Each
// [Connector.Invoke] call creates a fresh sandbox instance, runs the
// call, and tears it down — per-invocation isolation per ADR-0005's
// "Connector instances are scoped to a single invocation" requirement.
type Connector interface {
	// Invoke runs `call` in a freshly-instantiated sandbox. Returns a
	// structured *Error on capability denial or resource-limit
	// termination per ADR-0010.
	Invoke(ctx context.Context, call Call) (Result, error)

	// Close releases the compiled-module cache. Outstanding Invoke
	// calls may complete; calling Invoke after Close is a programming
	// error.
	Close(ctx context.Context) error
}

// Call describes one connector operation invocation.
//
// `Op` and `Args` correspond to the action manifest's `[[execute]]` step
// fields (per ADR-0003) — the executor builds a Call from each step and
// hands it to the corresponding connector. `Limits` overrides the
// per-runtime defaults; the zero value uses ADR-0005 defaults
// (64 MiB / 30 s).
//
// `AllowedAuthority` is the invoking action's declared subset of the
// connector's capabilities; the network host import checks this in
// addition to the connector manifest's grant (ADR-0003 §"defense in
// depth"). When empty, only the connector manifest gates network
// access.
//
// `CredentialResolver` is the per-invocation handle the host uses to
// resolve a bound credential when the connector emits an outbound
// request that references its declared `[capabilities.credential]`
// (per ADR-0005 credential mediation). Nil means no binding is wired
// for this call; the host returns `binding_required` if the connector
// tries to use the credential path. Resolver lifetime is the single
// Invoke; per-call hostState drops the reference on completion so the
// connector cannot stash it across calls.
type Call struct {
	Op                 string
	Args               map[string]any
	Limits             Limits
	AllowedAuthority   []string
	CredentialResolver credential.Resolver

	// AllowedSpawnPrograms is the action manifest's declared subset of
	// programs the connector may invoke via aileron_host.spawn. Each
	// entry is an absolute (or ~/-anchored) program path matching the
	// connector manifest's [capabilities.spawn].programs declaration.
	// Empty means the action did not narrow the connector's grant; the
	// connector manifest's policy is the only spawn gate.
	AllowedSpawnPrograms []string
}

// Result is what the sandbox produced. Successful runs surface their
// `Output` map; the executor encodes it into the tool-result content the
// LLM observes. `Logs` is the connector's structured log emissions
// during the call (via `aileron_log`).
type Result struct {
	Output map[string]any
	Logs   []LogLine
}

// LogLine is a single emission from the connector's `aileron_log` host
// import.
type LogLine struct {
	Level   string
	Message string
}

// Limits configure resource ceilings per ADR-0005's "Resource limits are
// enforced and bounded" table. The zero value means "use ADR defaults".
//
// MemoryBytes default is 64 MiB; the connector manifest may request a
// higher cap up to a hard ceiling of 1 GiB. WallTime default is 30 s,
// per-call override capped at 5 minutes.
type Limits struct {
	MemoryBytes uint64
	WallTime    time.Duration
}
