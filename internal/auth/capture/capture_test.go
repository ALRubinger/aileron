package capture

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

// These tests pin the contract of the provider-independent capture
// driver:
//
//   - The shared container is load-bearing: exactly one `run`, and both
//     the login and token execs target it, with teardown on every exit.
//   - The TTY gate puts a standalone `-t` (plus `-i`) on the login exec
//     only when stdin is a terminal; the token read never gets `-t`.
//   - The BROWSER shim and config-dir env appear exactly where the
//     contract says (browser: login only; config-dir: both execs).
//   - Acquire stores the trimmed token via the injected StoreFunc on the
//     happy path, and NEVER calls Store on a capture failure or an empty
//     token. Store's error is surfaced verbatim.

// --- fakes ---------------------------------------------------------------

// recordingRunner records every invocation and models the load-bearing
// shared-container behavior: the token read only yields a token when a
// prior login exec ran against the SAME container name. An
// ephemeral-container implementation cannot satisfy this.
type recordingRunner struct {
	calls         [][]string
	loggedInNames map[string]bool
	runContainer  string
	token         string
}

func (r *recordingRunner) Run(_ context.Context, _ string, args []string, stdout, _ io.Writer) error {
	r.calls = append(r.calls, args)
	if r.loggedInNames == nil {
		r.loggedInNames = map[string]bool{}
	}
	if len(args) == 0 {
		return nil
	}
	switch args[0] {
	case "run":
		r.runContainer = containerNameFromRunArgs(args)
		return nil
	case "exec":
		cname, sub := parseExecArgs(args)
		switch sub {
		case "login":
			if cname != r.runContainer {
				return errors.New("login targeted a container that was never created")
			}
			r.loggedInNames[cname] = true
			return nil
		case "token":
			if !r.loggedInNames[cname] {
				return errors.New("token read: not logged in to this container")
			}
			_, _ = io.WriteString(stdout, r.token+"\n")
			return nil
		}
	case "rm":
		return nil
	}
	return nil
}

// parseExecArgs extracts the container name and the tool subcommand from
// an `exec [flags] <name> <tool> <verb> <sub> ...` arg slice. The login
// vector here is {"<tool>","auth","login",...} and the token vector is
// {"<tool>","auth","token"}, so the meaningful token is the one after
// "auth".
func parseExecArgs(args []string) (container, sub string) {
	i := 1 // skip "exec"
	for i < len(args) && strings.HasPrefix(args[i], "-") {
		i++ // skip exec flags like -i, -t, --env=...
	}
	if i >= len(args) {
		return "", ""
	}
	container = args[i]
	i++
	// Walk to "auth" and read the verb after it.
	for j := i; j+1 < len(args); j++ {
		if args[j] == "auth" {
			return container, args[j+1]
		}
	}
	return container, ""
}

func containerNameFromRunArgs(args []string) string {
	for i, a := range args {
		if a == "--name" && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// failingLoginRunner fails the login exec but records the teardown.
type failingLoginRunner struct{ removed bool }

func (r *failingLoginRunner) Run(_ context.Context, _ string, args []string, _, _ io.Writer) error {
	if len(args) == 0 {
		return nil
	}
	switch args[0] {
	case "run":
		return nil
	case "exec":
		if _, sub := parseExecArgs(args); sub == "login" {
			return errors.New("login failed")
		}
		return nil
	case "rm":
		r.removed = true
		return nil
	}
	return nil
}

// scriptedRunner fails a chosen step and writes a stderr fragment.
type scriptedRunner struct {
	failStep  string // "run" or "token"
	stderrMsg string
	removed   bool
}

func (r *scriptedRunner) Run(_ context.Context, _ string, args []string, _, stderr io.Writer) error {
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
		if _, sub := parseExecArgs(args); sub == "token" && r.failStep == "token" {
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

// recordingStore records the args of the last Store call and returns a
// configurable error.
type recordingStore struct {
	called  int
	storeAt string
	kind    string
	value   []byte
	retErr  error
}

func (s *recordingStore) fn() StoreFunc {
	return func(_ context.Context, storeAt, kind string, value []byte) error {
		s.called++
		s.storeAt = storeAt
		s.kind = kind
		s.value = append([]byte(nil), value...)
		return s.retErr
	}
}

// newTestDriver builds a Driver wired to the given runner and store with
// a representative tool-specific config (modeled on the gh flow, but no
// gh literals are load-bearing here).
func newTestDriver(runner interface {
	Run(context.Context, string, []string, io.Writer, io.Writer) error
}, store StoreFunc) *Driver {
	return &Driver{
		Runner:        runnerFunc(runner.Run),
		RuntimeExe:    "docker",
		Image:         "img",
		ContainerName: "aileron-auth-cli",
		LoginArgs:     []string{"gh", "auth", "login", "--web"},
		TokenArgs:     []string{"gh", "auth", "token"},
		StoreAt:       "user/github",
		Kind:          "user",
		Store:         store,
	}
}

// runnerFunc adapts a Run method value to the sandboxcontainer.Runner
// interface without importing the package's concrete fakes.
type runnerFunc func(context.Context, string, []string, io.Writer, io.Writer) error

func (f runnerFunc) Run(ctx context.Context, name string, args []string, stdout, stderr io.Writer) error {
	return f(ctx, name, args, stdout, stderr)
}

func execArgsBySub(calls [][]string) (login, token []string) {
	for _, c := range calls {
		if len(c) == 0 || c[0] != "exec" {
			continue
		}
		switch _, sub := parseExecArgs(c); sub {
		case "login":
			login = c
		case "token":
			token = c
		}
	}
	return login, token
}

func hasArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

// --- Capture: shared-container + lifecycle -------------------------------

func TestCapture_UsesOneSharedContainerForBothCalls(t *testing.T) {
	rr := &recordingRunner{token: "tok_shared"}
	d := newTestDriver(rr, (&recordingStore{}).fn())

	token, err := d.Capture(context.Background())
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if string(token) != "tok_shared" {
		t.Errorf("token = %q, want the captured value (trimmed)", token)
	}

	runs := 0
	for _, c := range rr.calls {
		if len(c) > 0 && c[0] == "run" {
			runs++
		}
	}
	if runs != 1 {
		t.Fatalf("run count = %d, want exactly 1 (shared container)", runs)
	}
	if rr.runContainer != "aileron-auth-cli" {
		t.Fatalf("created container = %q, want aileron-auth-cli", rr.runContainer)
	}

	login, token2 := execArgsBySub(rr.calls)
	lc, _ := parseExecArgs(login)
	tc, _ := parseExecArgs(token2)
	if lc != rr.runContainer {
		t.Errorf("login container = %q, want %q", lc, rr.runContainer)
	}
	if tc != rr.runContainer {
		t.Errorf("token container = %q, want %q (same as login)", tc, rr.runContainer)
	}

	teardown := false
	for _, c := range rr.calls {
		if len(c) >= 3 && c[0] == "rm" && c[1] == "-f" && c[2] == "aileron-auth-cli" {
			teardown = true
		}
	}
	if !teardown {
		t.Error("container was not torn down with rm -f")
	}
}

func TestCapture_TTYGate(t *testing.T) {
	driveTTY := func(t *testing.T, isTTY bool) (login, token []string) {
		t.Helper()
		orig := stdinIsTerminal
		stdinIsTerminal = func() bool { return isTTY }
		t.Cleanup(func() { stdinIsTerminal = orig })

		rr := &recordingRunner{token: "tok"}
		d := newTestDriver(rr, (&recordingStore{}).fn())
		if _, err := d.Capture(context.Background()); err != nil {
			t.Fatalf("Capture: %v", err)
		}
		return execArgsBySub(rr.calls)
	}

	t.Run("terminal stdin allocates a PTY for login only", func(t *testing.T) {
		login, token := driveTTY(t, true)
		if !hasArg(login, "-t") {
			t.Errorf("login = %v; want standalone -t when stdin is a terminal", login)
		}
		if !hasArg(login, "-i") {
			t.Errorf("login = %v; want -i", login)
		}
		if hasArg(login, "-it") {
			t.Errorf("login = %v; -i and -t must be separate args", login)
		}
		if hasArg(token, "-t") {
			t.Errorf("token = %v; the non-interactive read must not allocate a PTY", token)
		}
	})

	t.Run("non-terminal stdin omits the PTY flag", func(t *testing.T) {
		login, _ := driveTTY(t, false)
		if hasArg(login, "-t") {
			t.Errorf("login = %v; want NO -t when stdin is not a terminal", login)
		}
		if !hasArg(login, "-i") {
			t.Errorf("login = %v; want -i even without a terminal", login)
		}
	})
}

func TestCapture_BrowserShimOnLoginOnly(t *testing.T) {
	rr := &recordingRunner{token: "tok"}
	d := newTestDriver(rr, (&recordingStore{}).fn())
	d.BrowserShim = "echo"
	if _, err := d.Capture(context.Background()); err != nil {
		t.Fatalf("Capture: %v", err)
	}
	login, token := execArgsBySub(rr.calls)
	if !hasArg(login, "--env=BROWSER=echo") {
		t.Errorf("login = %v; want --env=BROWSER=echo", login)
	}
	if hasArg(token, "--env=BROWSER=echo") {
		t.Errorf("token = %v; the token read must not set BROWSER", token)
	}
}

func TestCapture_BrowserShimAbsentWhenUnset(t *testing.T) {
	rr := &recordingRunner{token: "tok"}
	d := newTestDriver(rr, (&recordingStore{}).fn())
	// BrowserShim left empty.
	if _, err := d.Capture(context.Background()); err != nil {
		t.Fatalf("Capture: %v", err)
	}
	login, _ := execArgsBySub(rr.calls)
	for _, a := range login {
		if strings.HasPrefix(a, "--env=BROWSER=") {
			t.Errorf("login = %v; want no BROWSER env when BrowserShim is unset", login)
		}
	}
}

func TestCapture_ConfigDirEnvOnBothExecs(t *testing.T) {
	rr := &recordingRunner{token: "tok"}
	d := newTestDriver(rr, (&recordingStore{}).fn())
	d.ConfigDirEnv = "GH_CONFIG_DIR=/cfg"
	if _, err := d.Capture(context.Background()); err != nil {
		t.Fatalf("Capture: %v", err)
	}
	login, token := execArgsBySub(rr.calls)
	if !hasArg(login, "--env=GH_CONFIG_DIR=/cfg") {
		t.Errorf("login = %v; want config-dir env on the login exec", login)
	}
	if !hasArg(token, "--env=GH_CONFIG_DIR=/cfg") {
		t.Errorf("token = %v; want config-dir env on the token exec too", token)
	}
}

func TestCapture_ConfigDirEnvAbsentWhenUnset(t *testing.T) {
	rr := &recordingRunner{token: "tok"}
	d := newTestDriver(rr, (&recordingStore{}).fn())
	if _, err := d.Capture(context.Background()); err != nil {
		t.Fatalf("Capture: %v", err)
	}
	login, token := execArgsBySub(rr.calls)
	for _, c := range [][]string{login, token} {
		for _, a := range c {
			if strings.HasPrefix(a, "--env=GH_CONFIG_DIR") {
				t.Errorf("exec = %v; want no config-dir env when ConfigDirEnv is unset", c)
			}
		}
	}
}

func TestCapture_TearsDownOnLoginFailure(t *testing.T) {
	rr := &failingLoginRunner{}
	d := newTestDriver(rr, (&recordingStore{}).fn())
	if _, err := d.Capture(context.Background()); err == nil {
		t.Fatal("expected an error from the failing login")
	}
	if !rr.removed {
		t.Error("container was not removed after a mid-flow failure")
	}
}

func TestCapture_StartFailureSurfacesStderr(t *testing.T) {
	rr := &scriptedRunner{failStep: "run", stderrMsg: "image not found"}
	d := newTestDriver(rr, (&recordingStore{}).fn())
	_, err := d.Capture(context.Background())
	if err == nil {
		t.Fatal("expected an error when the container fails to start")
	}
	if !strings.Contains(err.Error(), "starting container") || !strings.Contains(err.Error(), "image not found") {
		t.Errorf("err = %v, want start-failure context with runtime stderr", err)
	}
}

func TestCapture_TokenReadFailureSurfacesStderrAndTearsDown(t *testing.T) {
	rr := &scriptedRunner{failStep: "token", stderrMsg: "no token"}
	d := newTestDriver(rr, (&recordingStore{}).fn())
	_, err := d.Capture(context.Background())
	if err == nil {
		t.Fatal("expected an error when the token read fails")
	}
	if !strings.Contains(err.Error(), "reading token") || !strings.Contains(err.Error(), "no token") {
		t.Errorf("err = %v, want token-failure context with runtime stderr", err)
	}
	if !rr.removed {
		t.Error("container was not torn down after a token-read failure")
	}
}

func TestCapture_TrimsTrailingCRLF(t *testing.T) {
	// The recordingRunner appends "\n"; assert the trailing newline is gone.
	rr := &recordingRunner{token: "tok_trim"}
	d := newTestDriver(rr, (&recordingStore{}).fn())
	token, err := d.Capture(context.Background())
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if string(token) != "tok_trim" {
		t.Errorf("token = %q, want trailing newline trimmed", token)
	}
}

// --- Acquire: store-and-stamp + the five named scenarios -----------------

func TestAcquire_HappyPathStoresTrimmedToken(t *testing.T) {
	rr := &recordingRunner{token: "tok_happy"}
	store := &recordingStore{}
	d := newTestDriver(rr, store.fn())

	if err := d.Acquire(context.Background()); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if store.called != 1 {
		t.Fatalf("Store called %d times, want exactly 1", store.called)
	}
	if store.storeAt != "user/github" {
		t.Errorf("storeAt = %q, want the configured logical key", store.storeAt)
	}
	if store.kind != "user" {
		t.Errorf("kind = %q, want the configured stamp", store.kind)
	}
	if string(store.value) != "tok_happy" {
		t.Errorf("stored value = %q, want the trimmed token", store.value)
	}
}

func TestAcquire_EmptyTokenDoesNotStore(t *testing.T) {
	rr := &recordingRunner{token: "   "} // whitespace-only → empty after trim
	store := &recordingStore{}
	d := newTestDriver(rr, store.fn())

	err := d.Acquire(context.Background())
	if !errors.Is(err, ErrEmptyToken) {
		t.Fatalf("err = %v, want ErrEmptyToken", err)
	}
	if store.called != 0 {
		t.Error("Store was called despite an empty token")
	}
}

func TestAcquire_VaultLockedErrorPropagates(t *testing.T) {
	rr := &recordingRunner{token: "tok"}
	locked := errors.New("vault is locked")
	store := &recordingStore{retErr: locked}
	d := newTestDriver(rr, store.fn())

	if err := d.Acquire(context.Background()); !errors.Is(err, locked) {
		t.Fatalf("err = %v, want the locked error surfaced verbatim", err)
	}
	if store.called != 1 {
		t.Error("Store should have been attempted before the locked error")
	}
}

func TestAcquire_NoVaultErrorPropagates(t *testing.T) {
	rr := &recordingRunner{token: "tok"}
	noVault := errors.New("daemon is not configured with a vault")
	store := &recordingStore{retErr: noVault}
	d := newTestDriver(rr, store.fn())

	if err := d.Acquire(context.Background()); !errors.Is(err, noVault) {
		t.Fatalf("err = %v, want the no-vault error surfaced verbatim", err)
	}
}

func TestAcquire_NonTTYStillCompletesStore(t *testing.T) {
	orig := stdinIsTerminal
	stdinIsTerminal = func() bool { return false }
	t.Cleanup(func() { stdinIsTerminal = orig })

	rr := &recordingRunner{token: "tok_nontty"}
	store := &recordingStore{}
	d := newTestDriver(rr, store.fn())

	if err := d.Acquire(context.Background()); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	// The gate omits -t but must not break the happy path store.
	login, _ := execArgsBySub(rr.calls)
	if hasArg(login, "-t") {
		t.Errorf("login = %v; want no -t in non-TTY mode", login)
	}
	if store.called != 1 || string(store.value) != "tok_nontty" {
		t.Errorf("store called=%d value=%q, want one call with the token", store.called, store.value)
	}
}

func TestAcquire_CaptureFailureDoesNotStore(t *testing.T) {
	rr := &scriptedRunner{failStep: "run", stderrMsg: "boom"}
	store := &recordingStore{}
	d := newTestDriver(rr, store.fn())

	if err := d.Acquire(context.Background()); err == nil {
		t.Fatal("expected the capture error to propagate")
	}
	if store.called != 0 {
		t.Error("Store was called despite a capture failure")
	}
}

// --- constructor ---------------------------------------------------------

func TestNew_DefaultsRunnerAndResolvesRuntime(t *testing.T) {
	// "auto" resolves to the host's available runtime (docker). The
	// constructor must default the Runner and carry the image through.
	d, err := New("auto", "ghcr.io/example/img:tag")
	if err != nil {
		t.Skipf("no container runtime resolvable in this environment: %v", err)
	}
	if d.Runner == nil {
		t.Error("Runner was not defaulted")
	}
	if d.RuntimeExe == "" {
		t.Error("RuntimeExe was not resolved")
	}
	if d.Image != "ghcr.io/example/img:tag" {
		t.Errorf("Image = %q, want the passed image", d.Image)
	}
}

func TestNew_RejectsUnsupportedRuntime(t *testing.T) {
	// An unsupported runtime fails fast in ResolveRuntime before any
	// container work, exercising the constructor's error path.
	if _, err := New("podman", ""); err == nil {
		t.Fatal("expected an error for an unsupported runtime")
	}
}

// --- helpers -------------------------------------------------------------

func TestStderrSuffix(t *testing.T) {
	if got := stderrSuffix(bytes.NewBufferString("")); got != "" {
		t.Errorf("empty suffix = %q, want empty", got)
	}
	if got := stderrSuffix(bytes.NewBufferString("   \n")); got != "" {
		t.Errorf("whitespace suffix = %q, want empty", got)
	}
	if got := stderrSuffix(bytes.NewBufferString("boom\n")); got != ": boom" {
		t.Errorf("suffix = %q, want %q", got, ": boom")
	}
}
