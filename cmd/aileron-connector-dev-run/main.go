// Command aileron-connector-dev-run is a developer test harness for
// running an Aileron WASM connector against real external APIs, without
// going through the full install + binding + launch pipeline.
//
// Usage:
//
//	aileron-connector-dev-run \
//	  --wasm     <path-to-connector.wasm> \
//	  --manifest <path-to-connector/manifest.toml> \
//	  --op       <op-name> \
//	  --args     '<json-args>'
//
// The harness parses the manifest, builds the same Wazero-backed sandbox
// runtime the production gateway uses, and invokes the connector with
// the supplied op + args. The manifest's [capabilities.network] grant
// is enforced exactly as in production. A stub credential resolver
// returns the OAuth bearer token from the AILERON_DEV_TOKEN environment
// variable; calls that mark themselves credential:"oauth2" get the
// token injected at the network boundary just like a vault-bound
// credential would.
//
// This binary is not installed alongside `aileron` for end users — it
// lives in cmd/ so it can import internal/* packages directly. The
// connector author runs it locally during connector development to
// validate behavior against real APIs before tagging a release.
//
// Example (Gmail messages.list against a real account):
//
//	# Get a token from https://developers.google.com/oauthplayground
//	# (gmail.readonly scope, exchange code for access token).
//	export AILERON_DEV_TOKEN=ya29...
//
//	cd ~/git/ALRubinger/aileron-connector-google && task build
//
//	aileron-connector-dev-run \
//	  --wasm     ~/git/ALRubinger/aileron-connector-google/connector.wasm \
//	  --manifest ~/git/ALRubinger/aileron-connector-google/connector/manifest.toml \
//	  --op       list_recent_emails \
//	  --args     '{"query":"is:unread","max_results":3}'
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/ALRubinger/aileron/internal/credential"
	"github.com/ALRubinger/aileron/internal/cstore"
	"github.com/ALRubinger/aileron/internal/sandbox"
)

// tokenEnvVar names the env variable the harness reads to populate the
// stub credential resolver. Any non-empty value is treated as the
// bearer token to inject; the resolver does not interpret the value.
const tokenEnvVar = "AILERON_DEV_TOKEN"

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "aileron-connector-dev-run: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("aileron-connector-dev-run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	wasmPath := fs.String("wasm", "", "path to connector.wasm")
	manifestPath := fs.String("manifest", "", "path to connector/manifest.toml")
	op := fs.String("op", "", "op name (matches the action's [[execute]].op)")
	argsJSON := fs.String("args", "{}", "args as a JSON object")
	verbose := fs.Bool("v", false, "print connector logs and resolver decisions to stderr")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: aileron-connector-dev-run --wasm <path> --manifest <path> --op <name> [--args <json>] [-v]")
		fmt.Fprintln(stderr, "")
		fmt.Fprintf(stderr, "  Env var %s carries the OAuth bearer token; the harness wires a\n", tokenEnvVar)
		fmt.Fprintln(stderr, "  stub credential resolver that returns it as the bound credential.")
		fmt.Fprintln(stderr, "  When unset, calls that mark themselves credential:\"oauth2\" return")
		fmt.Fprintln(stderr, "  binding_required.")
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *wasmPath == "" || *manifestPath == "" || *op == "" {
		fs.Usage()
		return errors.New("--wasm, --manifest, and --op are required")
	}

	wasmBytes, err := os.ReadFile(*wasmPath)
	if err != nil {
		return fmt.Errorf("read wasm: %w", err)
	}
	manifestBytes, err := os.ReadFile(*manifestPath)
	if err != nil {
		return fmt.Errorf("read manifest: %w", err)
	}
	manifest, err := cstore.ParseManifest(*manifestPath, manifestBytes)
	if err != nil {
		return fmt.Errorf("parse manifest: %w", err)
	}
	if err := cstore.ValidateManifest(manifest, *manifestPath); err != nil {
		return fmt.Errorf("validate manifest: %w", err)
	}

	var opArgs map[string]any
	if err := json.Unmarshal([]byte(*argsJSON), &opArgs); err != nil {
		return fmt.Errorf("parse --args JSON: %w", err)
	}

	logLevel := slog.LevelWarn
	if *verbose {
		logLevel = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: logLevel}))

	rt, err := sandbox.NewWazeroRuntime(ctx, sandbox.WithLogger(log))
	if err != nil {
		return fmt.Errorf("init wazero runtime: %w", err)
	}
	defer func() { _ = rt.Close(ctx) }()

	conn, err := rt.Compile(ctx, manifest, wasmBytes)
	if err != nil {
		return fmt.Errorf("compile connector: %w", err)
	}
	defer func() { _ = conn.Close(ctx) }()

	resolver := newDevResolver(manifest, os.Getenv(tokenEnvVar))
	if resolver.kind != "" && resolver.token == "" {
		fmt.Fprintf(stderr, "note: %s not set; credential:%q calls return binding_required.\n",
			tokenEnvVar, resolver.kind)
	}

	result, invokeErr := conn.Invoke(ctx, sandbox.Call{
		Op:                 *op,
		Args:               opArgs,
		CredentialResolver: resolver,
	})

	for _, line := range result.Logs {
		fmt.Fprintf(stderr, "[connector %s] %s\n", line.Level, line.Message)
	}

	if invokeErr != nil {
		// Surface the structured error in the same shape the gateway
		// would (ADR-0010 envelope); for a Go error without that shape,
		// fall back to the prose Error() form.
		_ = json.NewEncoder(stdout).Encode(envelope{Error: invokeErr.Error()})
		return invokeErr
	}

	return json.NewEncoder(stdout).Encode(envelope{Output: result.Output})
}

// envelope is the JSON shape the harness emits on stdout. Mirrors the
// connector ABI's input/output envelope (sandbox.testdata.echo.main.go)
// so the dev-run output looks the same as what the production runtime
// surfaces to the LLM.
type envelope struct {
	Output map[string]any `json:"output,omitempty"`
	Error  string         `json:"error,omitempty"`
}

// devResolver is a stub credential.Resolver that returns a fixed bearer
// token from the environment. Used in place of the production
// vault-backed resolver so a connector author can exercise the
// credential-mediation path against real APIs without binding setup.
type devResolver struct {
	kind  string
	token string
}

func newDevResolver(m *cstore.Manifest, token string) *devResolver {
	r := &devResolver{token: token}
	if m != nil && m.Capabilities.Credential != nil {
		r.kind = m.Capabilities.Credential.Kind
	}
	return r
}

func (r *devResolver) Resolve(_ context.Context) (credential.Credential, error) {
	if r.kind == "" {
		// Connector did not declare a credential capability; resolver
		// shouldn't be called at all in that case. If we are called,
		// surface the same error production would.
		return credential.Credential{}, credential.ErrNoBindingResolver
	}
	if r.token == "" {
		return credential.Credential{}, credential.ErrBindingMissing
	}
	return credential.Credential{Kind: r.kind, Value: []byte(r.token)}, nil
}
