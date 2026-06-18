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
)

// These tests pin the contract of `aileron auth github`:
//
//   - The captured bearer token is PUT base64-encoded to
//     /v1/vault/user/github/credentials (the #1147 user namespace).
//   - A capture failure, an empty token, and a non-204 daemon status
//     each surface a clean message with exit 1 and (for capture/empty)
//     no PUT.
//   - The production containerDeviceFlow runs ONE shared container for
//     both gh calls (req 1): the token read sees the hosts.yml the login
//     wrote because it is the same container, not a second docker run.

// stubDeviceFlow is an injectable deviceFlowRunner for the store-path
// tests. It never touches a container or GitHub.
type stubDeviceFlow struct {
	token []byte
	err   error
}

func (s stubDeviceFlow) Capture(context.Context) ([]byte, error) {
	return s.token, s.err
}

// withStubDeviceFlow swaps newDeviceFlowRunner for one that returns the
// given stub, restoring the original on cleanup.
func withStubDeviceFlow(t *testing.T, stub deviceFlowRunner, capErr error) {
	t.Helper()
	orig := newDeviceFlowRunner
	newDeviceFlowRunner = func(runtime, image string) (deviceFlowRunner, error) {
		return stub, capErr
	}
	t.Cleanup(func() { newDeviceFlowRunner = orig })
}

func TestRunAuthGitHub_HappyPathStoresTokenAtUserGitHub(t *testing.T) {
	token := []byte("gho_exampletoken1234567890")
	var gotMethod, gotPath string
	var gotValue []byte
	fakeVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		var body agentCredentialsBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		gotValue = body.Value
		w.WriteHeader(http.StatusNoContent)
	})
	withStubDeviceFlow(t, stubDeviceFlow{token: token}, nil)

	var stdout, stderr bytes.Buffer
	code := runAuthGitHub(nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	if gotMethod != http.MethodPut || gotPath != "/vault/user/github/credentials" {
		t.Errorf("request = %s %s, want PUT /vault/user/github/credentials", gotMethod, gotPath)
	}
	if !bytes.Equal(gotValue, token) {
		t.Errorf("stored value = %q, want %q", gotValue, token)
	}
	if !strings.Contains(stdout.String(), "Stored user/github") {
		t.Errorf("stdout = %q, want success message", stdout.String())
	}
}

func TestRunAuthGitHub_CaptureFailureNoPUT(t *testing.T) {
	called := false
	fakeVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	})
	withStubDeviceFlow(t, stubDeviceFlow{err: errors.New("device flow timed out")}, nil)

	var stdout, stderr bytes.Buffer
	code := runAuthGitHub(nil, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if called {
		t.Error("daemon PUT was issued despite a capture failure")
	}
	if !strings.Contains(stderr.String(), "device-flow login failed") {
		t.Errorf("stderr = %q, want capture failure message", stderr.String())
	}
}

func TestRunAuthGitHub_EmptyTokenGuardNoPUT(t *testing.T) {
	called := false
	fakeVaultServer(t, func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	})
	// Whitespace-only is treated as empty after trimming.
	withStubDeviceFlow(t, stubDeviceFlow{token: []byte("  \n")}, nil)

	var stdout, stderr bytes.Buffer
	code := runAuthGitHub(nil, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if called {
		t.Error("daemon PUT was issued despite an empty token")
	}
	if !strings.Contains(stderr.String(), "empty token") {
		t.Errorf("stderr = %q, want empty-token message", stderr.String())
	}
}

func TestRunAuthGitHub_StoreFailureSurfacesCleanly(t *testing.T) {
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
			withStubDeviceFlow(t, stubDeviceFlow{token: []byte("gho_tok")}, nil)

			var stdout, stderr bytes.Buffer
			code := runAuthGitHub(nil, &stdout, &stderr)
			if code != 1 {
				t.Fatalf("exit = %d, want 1; stderr=%s", code, stderr.String())
			}
			if !strings.Contains(stderr.String(), tc.want) {
				t.Errorf("stderr = %q, want substring %q", stderr.String(), tc.want)
			}
		})
	}
}

func TestRunAuthGitHub_RunnerConstructionErrorSurfaces(t *testing.T) {
	orig := newDeviceFlowRunner
	newDeviceFlowRunner = func(runtime, image string) (deviceFlowRunner, error) {
		return nil, errors.New("no container runtime found on PATH")
	}
	t.Cleanup(func() { newDeviceFlowRunner = orig })

	var stdout, stderr bytes.Buffer
	code := runAuthGitHub(nil, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "no container runtime") {
		t.Errorf("stderr = %q, want runtime-resolution error", stderr.String())
	}
}

func TestRunAuthGitHub_RejectsUnknownFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runAuthGitHub([]string{"--nope"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
}

func TestRunAuthGitHub_TransportErrorSurfaces(t *testing.T) {
	// Capture succeeds, but the daemon is unreachable: vaultDoRequest's
	// transport error must surface cleanly with exit 1 and no panic.
	withStubDeviceFlow(t, stubDeviceFlow{token: []byte("gho_tok")}, nil)
	// Point at a closed port so the HTTP client errors at the transport
	// layer rather than returning a status.
	t.Setenv("AILERON_API_URL", "http://127.0.0.1:0/v1")
	t.Setenv("AILERON_TOKEN", "test-token")

	var stdout, stderr bytes.Buffer
	code := runAuthGitHub(nil, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit = %d, want 1; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "error:") {
		t.Errorf("stderr = %q, want a transport error", stderr.String())
	}
}

func TestRunAuthGitHub_RejectsExtraPositionalArgs(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runAuthGitHub([]string{"unexpected"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "usage:") {
		t.Errorf("stderr = %q, want usage", stderr.String())
	}
}

func TestNewDeviceFlowRunner_RejectsUnsupportedRuntime(t *testing.T) {
	// An unsupported runtime fails fast in ResolveRuntime before any
	// container work, exercising the production constructor's error path.
	if _, err := newDeviceFlowRunner("podman", ""); err == nil {
		t.Fatal("expected an error for an unsupported runtime")
	}
}

func TestStderrSuffix(t *testing.T) {
	if got := stderrSuffix(bytes.NewBufferString("")); got != "" {
		t.Errorf("empty stderr suffix = %q, want empty", got)
	}
	if got := stderrSuffix(bytes.NewBufferString("   \n")); got != "" {
		t.Errorf("whitespace stderr suffix = %q, want empty", got)
	}
	if got := stderrSuffix(bytes.NewBufferString("boom\n")); got != ": boom" {
		t.Errorf("stderr suffix = %q, want %q", got, ": boom")
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

// --- req 1: shared-container regression guard ---

// recordingRunner is a fake sandboxcontainer.Runner that records every
// invocation and models the load-bearing shared-container behavior:
// `gh auth token` only yields a token when a prior `gh auth login` exec
// ran against the SAME container name. An ephemeral-container
// implementation (a second `docker run` before the token read) cannot
// satisfy this — there would be no logged-in container to read from.
type recordingRunner struct {
	calls         [][]string
	loggedInNames map[string]bool
	runContainer  string // the container name created by `run -d`
	token         string
}

func (r *recordingRunner) Run(ctx context.Context, name string, args []string, stdout, stderr io.Writer) error {
	r.calls = append(r.calls, args)
	if r.loggedInNames == nil {
		r.loggedInNames = map[string]bool{}
	}
	if len(args) == 0 {
		return nil
	}
	switch args[0] {
	case "run":
		// docker run -d --name <name> <image> sleep <n>
		r.runContainer = containerNameFromRunArgs(args)
		return nil
	case "exec":
		cname, gh := parseExecArgs(args)
		if !gh.isGH {
			return nil
		}
		switch {
		case gh.sub == "login":
			if cname != r.runContainer {
				return errors.New("login targeted a container that was never created")
			}
			r.loggedInNames[cname] = true
			return nil
		case gh.sub == "token":
			// The token read must hit the SAME container the login ran in.
			if !r.loggedInNames[cname] {
				return errors.New("gh auth token: not logged in to this container")
			}
			_, _ = io.WriteString(stdout, r.token+"\n")
			return nil
		}
	case "rm":
		return nil
	}
	return nil
}

type ghCall struct {
	isGH bool
	sub  string
}

// parseExecArgs extracts the container name and the gh subcommand from a
// `docker exec [-i] <name> gh <sub> ...` arg slice.
func parseExecArgs(args []string) (container string, gh ghCall) {
	i := 1 // skip "exec"
	for i < len(args) && strings.HasPrefix(args[i], "-") {
		i++ // skip exec flags like -i
	}
	if i >= len(args) {
		return "", ghCall{}
	}
	container = args[i]
	i++
	// gh invocations are `gh auth <sub> ...`; the meaningful subcommand
	// is the token after "auth".
	if i < len(args) && args[i] == "gh" {
		gh.isGH = true
		if i+1 < len(args) && args[i+1] == "auth" && i+2 < len(args) {
			gh.sub = args[i+2]
		}
	}
	return container, gh
}

// containerNameFromRunArgs returns the value following --name in a run
// arg slice.
func containerNameFromRunArgs(args []string) string {
	for i, a := range args {
		if a == "--name" && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func TestContainerDeviceFlow_UsesOneSharedContainerForBothGHCalls(t *testing.T) {
	rr := &recordingRunner{token: "gho_sharedcontainertoken"}
	flow := &containerDeviceFlow{
		runner:        rr,
		runtimeExe:    "docker",
		image:         "ghcr.io/alrubinger/aileron-sandbox-base:edge",
		containerName: "aileron-auth-github",
	}

	token, err := flow.Capture(context.Background())
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if string(token) != "gho_sharedcontainertoken" {
		t.Errorf("token = %q, want the captured value (trimmed)", token)
	}

	// Exactly one `docker run` — no ephemeral second run for the token read.
	runs := 0
	for _, c := range rr.calls {
		if len(c) > 0 && c[0] == "run" {
			runs++
		}
	}
	if runs != 1 {
		t.Fatalf("docker run count = %d, want exactly 1 (shared container)", runs)
	}

	// The login and token execs both target the one created container.
	if rr.runContainer != "aileron-auth-github" {
		t.Fatalf("created container = %q, want aileron-auth-github", rr.runContainer)
	}
	var loginContainer, tokenContainer string
	for _, c := range rr.calls {
		if len(c) == 0 || c[0] != "exec" {
			continue
		}
		cname, gh := parseExecArgs(c)
		switch gh.sub {
		case "login":
			loginContainer = cname
		case "token":
			tokenContainer = cname
		}
	}
	if loginContainer != rr.runContainer {
		t.Errorf("login container = %q, want %q", loginContainer, rr.runContainer)
	}
	if tokenContainer != rr.runContainer {
		t.Errorf("token container = %q, want %q (same as login)", tokenContainer, rr.runContainer)
	}

	// The container is torn down.
	teardown := false
	for _, c := range rr.calls {
		if len(c) >= 3 && c[0] == "rm" && c[1] == "-f" && c[2] == "aileron-auth-github" {
			teardown = true
		}
	}
	if !teardown {
		t.Error("container was not torn down with rm -f")
	}
}

// TestContainerDeviceFlow_LoginExecAllocatesTTYWithStdin is the
// regression guard for #1165: `gh auth login --web` renders an
// interactive prompt that needs a pseudo-TTY, so the login exec must
// carry `-t` when the operator's stdin is a real terminal (without it
// the command hangs with no visible prompt). A non-terminal / CI stdin
// must NOT get `-t` (docker rejects a PTY on non-tty input), and the
// non-interactive token read must never allocate a PTY. `-i` and `-t`
// must be separate args so the Runner's interactiveTTYRun gate matches.
func TestContainerDeviceFlow_LoginExecAllocatesTTYWithStdin(t *testing.T) {
	loginAndTokenArgs := func(t *testing.T, isTTY bool) (login, token []string) {
		t.Helper()
		orig := stdinIsTerminal
		stdinIsTerminal = func() bool { return isTTY }
		t.Cleanup(func() { stdinIsTerminal = orig })

		rr := &recordingRunner{token: "gho_tok"}
		flow := &containerDeviceFlow{
			runner:        rr,
			runtimeExe:    "docker",
			image:         "img",
			containerName: "aileron-auth-github",
		}
		if _, err := flow.Capture(context.Background()); err != nil {
			t.Fatalf("Capture: %v", err)
		}
		for _, c := range rr.calls {
			if len(c) == 0 || c[0] != "exec" {
				continue
			}
			_, gh := parseExecArgs(c)
			switch gh.sub {
			case "login":
				login = c
			case "token":
				token = c
			}
		}
		return login, token
	}

	hasFlag := func(args []string, f string) bool {
		for _, a := range args {
			if a == f {
				return true
			}
		}
		return false
	}

	t.Run("terminal stdin allocates a PTY for the login exec", func(t *testing.T) {
		login, token := loginAndTokenArgs(t, true)
		if !hasFlag(login, "-t") {
			t.Errorf("login exec = %v; want a standalone -t (PTY) flag when stdin is a terminal", login)
		}
		if !hasFlag(login, "-i") {
			t.Errorf("login exec = %v; want -i", login)
		}
		if hasFlag(login, "-it") {
			t.Errorf("login exec = %v; -i and -t must be separate args (not -it) so interactiveTTYRun matches -t", login)
		}
		if hasFlag(token, "-t") {
			t.Errorf("token exec = %v; the non-interactive token read must not allocate a PTY", token)
		}
	})

	t.Run("non-terminal stdin omits the PTY flag", func(t *testing.T) {
		login, _ := loginAndTokenArgs(t, false)
		if hasFlag(login, "-t") {
			t.Errorf("login exec = %v; want NO -t when stdin is not a terminal (docker rejects a PTY on non-tty input)", login)
		}
		if !hasFlag(login, "-i") {
			t.Errorf("login exec = %v; want -i even without a terminal", login)
		}
	})
}

func TestContainerDeviceFlow_TearsDownOnLoginFailure(t *testing.T) {
	// A runner that fails the login exec but records all calls, so we can
	// assert the deferred teardown still ran.
	rr := &failingLoginRunner{}
	flow := &containerDeviceFlow{
		runner:        rr,
		runtimeExe:    "docker",
		image:         "img",
		containerName: "aileron-auth-github",
	}
	_, err := flow.Capture(context.Background())
	if err == nil {
		t.Fatal("expected an error from the failing login")
	}
	if !rr.removed {
		t.Error("container was not removed after a mid-flow failure")
	}
}

type failingLoginRunner struct {
	removed bool
}

func (r *failingLoginRunner) Run(ctx context.Context, name string, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return nil
	}
	switch args[0] {
	case "run":
		return nil
	case "exec":
		_, gh := parseExecArgs(args)
		if gh.sub == "login" {
			return errors.New("login failed")
		}
		return nil
	case "rm":
		r.removed = true
		return nil
	}
	return nil
}

// scriptedRunner fails a chosen step and writes a stderr fragment, so we
// can assert Capture surfaces the runtime's stderr (via stderrSuffix).
type scriptedRunner struct {
	failStep  string // "run" or "token"
	stderrMsg string
	removed   bool
}

func (r *scriptedRunner) Run(ctx context.Context, name string, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return nil
	}
	switch args[0] {
	case "run":
		if r.failStep == "run" {
			_, _ = io.WriteString(stderr, r.stderrMsg)
			return errors.New("exit status 125")
		}
		return nil
	case "exec":
		_, gh := parseExecArgs(args)
		if gh.sub == "token" && r.failStep == "token" {
			_, _ = io.WriteString(stderr, r.stderrMsg)
			return errors.New("exit status 1")
		}
		return nil
	case "rm":
		r.removed = true
		return nil
	}
	return nil
}

func TestContainerDeviceFlow_StartFailureSurfacesStderr(t *testing.T) {
	rr := &scriptedRunner{failStep: "run", stderrMsg: "image not found"}
	flow := &containerDeviceFlow{runner: rr, runtimeExe: "docker", image: "img", containerName: "aileron-auth-github"}
	_, err := flow.Capture(context.Background())
	if err == nil {
		t.Fatal("expected an error when the container fails to start")
	}
	if !strings.Contains(err.Error(), "starting container") || !strings.Contains(err.Error(), "image not found") {
		t.Errorf("err = %v, want start-failure context with runtime stderr", err)
	}
	// No container was created, so no teardown is expected; the defer
	// still runs but the assertion here is only that we surfaced cleanly.
}

func TestContainerDeviceFlow_TokenReadFailureSurfacesStderrAndTearsDown(t *testing.T) {
	rr := &scriptedRunner{failStep: "token", stderrMsg: "no oauth token"}
	flow := &containerDeviceFlow{runner: rr, runtimeExe: "docker", image: "img", containerName: "aileron-auth-github"}
	_, err := flow.Capture(context.Background())
	if err == nil {
		t.Fatal("expected an error when gh auth token fails")
	}
	if !strings.Contains(err.Error(), "gh auth token") || !strings.Contains(err.Error(), "no oauth token") {
		t.Errorf("err = %v, want token-failure context with runtime stderr", err)
	}
	if !rr.removed {
		t.Error("container was not torn down after a token-read failure")
	}
}
