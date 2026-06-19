package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/auth/capture"
)

// These tests pin the CALLER-SIDE contract of the descriptor-driven
// `aileron auth github` verb, after the bespoke device-flow body was
// dissolved into the generic capture.Driver (#1288):
//
//   - The captured bearer token is PUT to /vault/user/github/credentials
//     (the daemon's user namespace) with metadata.type == "user".
//   - A capture failure, an empty token, and a non-204 daemon status each
//     surface a clean message with exit 1 and (for capture/empty) no PUT.
//   - The verb routes through the descriptor registry: a half-wired
//     dispatch (verb resolved but no driver / no store) reddens the route
//     regression test.
//
// The DRIVER-INTERNAL behaviors (one shared container for both gh calls,
// the TTY gate, the BROWSER shim, teardown-on-failure, stderr surfacing,
// the empty-token guard mechanics) are owned and tested by
// internal/auth/capture/capture_test.go and are NOT duplicated here.

// fakeCaptureRunner is a sandboxcontainer.Runner that drives the generic
// capture.Driver to a captured token without touching a container or
// GitHub. It models the load-bearing shared-container behavior: `gh auth
// token` only yields a token when a prior `gh auth login` ran against the
// SAME container name, so a half-wired driver (wrong container, no login
// exec) fails to produce a token.
type fakeCaptureRunner struct {
	token         string
	loggedInNames map[string]bool
	runContainer  string
}

func (r *fakeCaptureRunner) Run(_ context.Context, _ string, args []string, stdout, _ io.Writer) error {
	if r.loggedInNames == nil {
		r.loggedInNames = map[string]bool{}
	}
	if len(args) == 0 {
		return nil
	}
	switch args[0] {
	case "run":
		for i, a := range args {
			if a == "--name" && i+1 < len(args) {
				r.runContainer = args[i+1]
			}
		}
		return nil
	case "exec":
		// Skip exec flags to find the container name and the gh subcommand.
		i := 1
		for i < len(args) && strings.HasPrefix(args[i], "-") {
			i++
		}
		if i >= len(args) {
			return nil
		}
		cname := args[i]
		i++
		if i >= len(args) || args[i] != "gh" {
			return nil
		}
		var sub string
		if i+2 < len(args) && args[i+1] == "auth" {
			sub = args[i+2]
		}
		switch sub {
		case "login":
			r.loggedInNames[cname] = true
		case "token":
			if !r.loggedInNames[cname] {
				return errors.New("gh auth token: not logged in to this container")
			}
			_, _ = io.WriteString(stdout, r.token+"\n")
		}
		return nil
	default:
		return nil
	}
}

// withFakeCaptureDriver swaps newCaptureDriver for one that binds the gh
// descriptor onto a Driver backed by the given runner, with runtime/image
// resolution stubbed out. constructErr, when non-nil, models a
// runtime-resolution / registry failure before any driver runs. It uses
// the real registry + descriptor so the route stays exercised end-to-end.
func withFakeCaptureDriver(t *testing.T, runner *fakeCaptureRunner, constructErr error) {
	t.Helper()
	orig := newCaptureDriver
	newCaptureDriver = func(descriptorName, runtime, image string, store capture.StoreFunc) (*capture.Driver, error) {
		if constructErr != nil {
			return nil, constructErr
		}
		registry, err := capture.DefaultRegistry()
		if err != nil {
			return nil, err
		}
		desc, ok := registry.Resolve(descriptorName)
		if !ok {
			return nil, errors.New("descriptor not registered: " + descriptorName)
		}
		drv := &capture.Driver{Runner: runner, RuntimeExe: "docker"}
		desc.Apply(drv, "img", store)
		return drv, nil
	}
	t.Cleanup(func() { newCaptureDriver = orig })
}

func TestRunAuthCapture_HappyPathStoresTokenAtUserGitHub(t *testing.T) {
	token := "gho_exampletoken1234567890"
	var gotMethod, gotPath, gotType string
	var gotValue []byte
	fakeVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		var body userCredentialsBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		gotValue = body.Value
		if body.Metadata != nil {
			gotType = body.Metadata.Type
		}
		w.WriteHeader(http.StatusNoContent)
	})
	withFakeCaptureDriver(t, &fakeCaptureRunner{token: token}, nil)

	var stdout, stderr bytes.Buffer
	code := runAuthCapture("gh", nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	if gotMethod != http.MethodPut || gotPath != "/vault/user/github/credentials" {
		t.Errorf("request = %s %s, want PUT /vault/user/github/credentials", gotMethod, gotPath)
	}
	if string(gotValue) != token {
		t.Errorf("stored value = %q, want %q", gotValue, token)
	}
	// The metadata Type must be "user" so the daemon's host-binding
	// resolver (ADR-0019, #1195) validates the kind it resolves for
	// user/github at the TLS boundary. It comes from the descriptor's kind.
	if gotType != "user" {
		t.Errorf("stored metadata.type = %q, want user", gotType)
	}
	if !strings.Contains(stdout.String(), "Stored user/github") {
		t.Errorf("stdout = %q, want success message", stdout.String())
	}
}

// TestRunAuth_GitHubVerbRoutesThroughRegistry is the route regression
// guard the issue requires: `aileron auth github` must reach the
// descriptor-driven path and succeed end-to-end. A half-wired dispatch
// (verb resolved but driver not built / store not wired) cannot produce
// the PUT + success message, so it reddens this test.
func TestRunAuth_GitHubVerbRoutesThroughRegistry(t *testing.T) {
	token := "gho_routedtoken0987654321"
	var gotPath string
	fakeVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	})
	withFakeCaptureDriver(t, &fakeCaptureRunner{token: token}, nil)

	var stdout, stderr bytes.Buffer
	// Drive the full runAuth dispatch with the user-facing `github` verb so
	// the verb→descriptor alias + registry lookup are both exercised.
	code := runAuth([]string{"github"}, authTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	if gotPath != "/vault/user/github/credentials" {
		t.Errorf("PUT path = %q, want /vault/user/github/credentials (route did not reach the driver)", gotPath)
	}
	if !strings.Contains(stdout.String(), "Stored user/github") {
		t.Errorf("stdout = %q, want success message", stdout.String())
	}
}

func TestRunAuthCapture_CaptureFailureNoPUT(t *testing.T) {
	called := false
	fakeVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	})
	// A runner that errors the login exec makes Capture fail before any
	// token read, so Acquire never calls Store.
	withFailingLoginCaptureDriver(t)

	var stdout, stderr bytes.Buffer
	code := runAuthCapture("gh", nil, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if called {
		t.Error("daemon PUT was issued despite a capture failure")
	}
	if !strings.Contains(stderr.String(), "error:") {
		t.Errorf("stderr = %q, want a clean capture-failure message", stderr.String())
	}
}

// withFailingLoginCaptureDriver swaps newCaptureDriver for one whose runner
// errors the login exec, so Capture fails before any token read or store.
func withFailingLoginCaptureDriver(t *testing.T) {
	t.Helper()
	orig := newCaptureDriver
	newCaptureDriver = func(descriptorName, runtime, image string, store capture.StoreFunc) (*capture.Driver, error) {
		registry, err := capture.DefaultRegistry()
		if err != nil {
			return nil, err
		}
		desc, ok := registry.Resolve(descriptorName)
		if !ok {
			return nil, errors.New("descriptor not registered: " + descriptorName)
		}
		drv := &capture.Driver{Runner: runnerFunc(func(_ context.Context, _ string, args []string, _, _ io.Writer) error {
			if len(args) > 0 && args[0] == "exec" {
				// Find the gh subcommand; fail the login exec.
				for i, a := range args {
					if a == "gh" && i+2 < len(args) && args[i+1] == "auth" && args[i+2] == "login" {
						return errors.New("login failed")
					}
				}
			}
			return nil
		}), RuntimeExe: "docker"}
		desc.Apply(drv, "img", store)
		return drv, nil
	}
	t.Cleanup(func() { newCaptureDriver = orig })
}

// runnerFunc adapts a function to the sandboxcontainer.Runner interface.
type runnerFunc func(context.Context, string, []string, io.Writer, io.Writer) error

func (f runnerFunc) Run(ctx context.Context, name string, args []string, stdout, stderr io.Writer) error {
	return f(ctx, name, args, stdout, stderr)
}

func TestRunAuthCapture_EmptyTokenGuardNoPUT(t *testing.T) {
	called := false
	fakeVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	})
	// A whitespace-only token is empty after trimming: the driver's
	// ErrEmptyToken guard fires and Store is never called.
	withFakeCaptureDriver(t, &fakeCaptureRunner{token: "  "}, nil)

	var stdout, stderr bytes.Buffer
	code := runAuthCapture("gh", nil, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit = %d, want 1; stderr=%s", code, stderr.String())
	}
	if called {
		t.Error("daemon PUT was issued despite an empty token")
	}
	if !strings.Contains(stderr.String(), "empty token") {
		t.Errorf("stderr = %q, want empty-token message", stderr.String())
	}
}

func TestRunAuthCapture_StoreFailureSurfacesCleanly(t *testing.T) {
	cases := []struct {
		name   string
		status int
		want   string
	}{
		{"locked", http.StatusLocked, "vault is locked"},
		{"noVault", http.StatusServiceUnavailable, "not configured with a vault"},
		{"unexpected", http.StatusInternalServerError, "server returned 500"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fakeVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
			})
			withFakeCaptureDriver(t, &fakeCaptureRunner{token: "gho_tok"}, nil)

			var stdout, stderr bytes.Buffer
			code := runAuthCapture("gh", nil, &stdout, &stderr)
			if code != 1 {
				t.Fatalf("exit = %d, want 1; stderr=%s", code, stderr.String())
			}
			if !strings.Contains(stderr.String(), tc.want) {
				t.Errorf("stderr = %q, want substring %q", stderr.String(), tc.want)
			}
		})
	}
}

func TestRunAuthCapture_DriverConstructionErrorSurfaces(t *testing.T) {
	withFakeCaptureDriver(t, nil, errors.New("no container runtime found on PATH"))

	var stdout, stderr bytes.Buffer
	code := runAuthCapture("gh", nil, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "no container runtime") {
		t.Errorf("stderr = %q, want construction error", stderr.String())
	}
}

func TestRunAuthCapture_RejectsUnknownFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runAuthCapture("gh", []string{"--nope"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
}

func TestRunAuthCapture_RejectsExtraPositionalArgs(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runAuthCapture("gh", []string{"unexpected"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "usage:") {
		t.Errorf("stderr = %q, want usage", stderr.String())
	}
}

func TestRunAuthCapture_TransportErrorSurfaces(t *testing.T) {
	// Capture succeeds, but the daemon is unreachable: the store closure's
	// transport error must surface cleanly with exit 1 and no panic.
	withFakeCaptureDriver(t, &fakeCaptureRunner{token: "gho_tok"}, nil)
	// Point at a closed port so the HTTP client errors at the transport
	// layer rather than returning a status.
	t.Setenv("AILERON_API_URL", "http://127.0.0.1:0/v1")
	t.Setenv("AILERON_TOKEN", "test-token")

	var stdout, stderr bytes.Buffer
	code := runAuthCapture("gh", nil, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit = %d, want 1; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "error:") {
		t.Errorf("stderr = %q, want a transport error", stderr.String())
	}
}

func TestResolveDeviceFlowImage(t *testing.T) {
	const override = "ghcr.io/example/custom:tag"
	if got := resolveDeviceFlowImage(override); got != override {
		t.Errorf("override image = %q, want %q", got, override)
	}
	// With no override it falls back to the gh-bearing sandbox base image.
	def := resolveDeviceFlowImage("")
	if !strings.HasPrefix(def, "ghcr.io/alrubinger/aileron-sandbox-base:") {
		t.Errorf("default image = %q, want sandbox-base reference", def)
	}
}

// TestUserCredentialStore_ProductionRealRuntime exercises the production
// newCaptureDriver path far enough to assert the runtime-resolution error
// surfaces: an unsupported runtime fails fast in capture.New before any
// container work, exercising the real (non-stubbed) constructor.
func TestNewCaptureDriver_RejectsUnsupportedRuntime(t *testing.T) {
	if _, err := newCaptureDriver("gh", "podman", "img", userCredentialStore); err == nil {
		t.Fatal("expected an error for an unsupported runtime")
	}
}

// TestNewCaptureDriver_UnknownDescriptor exercises the registry-miss
// branch of the production binder: an unregistered name is a clear error
// naming the registered tools, not a nil driver.
func TestNewCaptureDriver_UnknownDescriptor(t *testing.T) {
	_, err := newCaptureDriver("nope", "docker", "img", userCredentialStore)
	if err == nil {
		t.Fatal("expected an error for an unregistered descriptor")
	}
}

// TestDescriptorForVerb pins the verb→descriptor spelling alias: the
// user-facing `github` verb maps to the shipped `gh` descriptor, while a
// verb with no alias entry is used as the descriptor name directly.
func TestDescriptorForVerb(t *testing.T) {
	if got := descriptorForVerb("github"); got != "gh" {
		t.Errorf("descriptorForVerb(github) = %q, want gh", got)
	}
	if got := descriptorForVerb("gh"); got != "gh" {
		t.Errorf("descriptorForVerb(gh) = %q, want gh (no alias, used directly)", got)
	}
}

// TestIsCaptureDescriptor confirms the dispatch predicate resolves the
// shipped descriptor and rejects an unknown name, so runAuth dispatches
// `github` (via its `gh` alias) but falls through for an agent name.
func TestIsCaptureDescriptor(t *testing.T) {
	if !isCaptureDescriptor("gh") {
		t.Error("isCaptureDescriptor(gh) = false, want true (shipped descriptor)")
	}
	if isCaptureDescriptor("claude") {
		t.Error("isCaptureDescriptor(claude) = true, want false (an agent, not a capture verb)")
	}
}

// withFailingCaptureRegistry forces the shared registry constructor to
// fail, exercising the fail-closed branch in both isCaptureDescriptor and
// newCaptureDriver: a registry that cannot load must not crash or
// mis-dispatch.
func withFailingCaptureRegistry(t *testing.T) {
	t.Helper()
	orig := captureRegistry
	captureRegistry = func() (*capture.Registry, error) {
		return nil, errors.New("registry load failed")
	}
	t.Cleanup(func() { captureRegistry = orig })
}

// TestIsCaptureDescriptor_RegistryLoadErrorFailsClosed pins the fail-closed
// dispatch: when the registry cannot load, isCaptureDescriptor returns
// false so runAuth falls through to the agent path rather than treating the
// verb as a (now-unresolvable) capture descriptor.
func TestIsCaptureDescriptor_RegistryLoadErrorFailsClosed(t *testing.T) {
	withFailingCaptureRegistry(t)
	if isCaptureDescriptor("gh") {
		t.Error("isCaptureDescriptor(gh) = true on a registry load error, want false (fail closed)")
	}
}

// TestNewCaptureDriver_RegistryLoadErrorSurfaces pins that a registry load
// error surfaces from the driver builder rather than panicking on a nil
// registry.
func TestNewCaptureDriver_RegistryLoadErrorSurfaces(t *testing.T) {
	withFailingCaptureRegistry(t)
	if _, err := newCaptureDriver("gh", "docker", "img", userCredentialStore); err == nil {
		t.Fatal("expected the registry load error to surface")
	}
}

// TestRunAuth_GitHubVerbFallsThroughOnRegistryError confirms the
// integration of the fail-closed dispatch: with the registry unavailable,
// `aileron auth github` is not treated as a capture verb and instead hits
// the agent path, which rejects "github" as unsupported (no PUT, exit 1).
func TestRunAuth_GitHubVerbFallsThroughOnRegistryError(t *testing.T) {
	withFailingCaptureRegistry(t)
	var stdout, stderr bytes.Buffer
	code := runAuth([]string{"github"}, authTestRegistry(), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	// It fell through to the agent path: --import-from-host is required.
	if !strings.Contains(stderr.String(), "--import-from-host is required") {
		t.Errorf("stderr = %q, want the agent-path fall-through error", stderr.String())
	}
}
