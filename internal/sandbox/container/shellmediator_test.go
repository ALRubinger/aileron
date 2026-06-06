package container

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
)

// The mediator is a POSIX sh helper that POSTs an about-to-run command to the
// daemon decision endpoint and exits 0 only on a clean allow. These tests exec
// the real script against an httptest stub so the JSON-escaping, wget call, and
// grep parsing are exercised the same way the image runs them. Pure BusyBox
// fidelity is covered separately by the sandbox-base smoke test (U3).

type interceptResult struct {
	exit   int
	stdout string
	stderr string
}

func mediatorScriptPath(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot resolve test file path")
	}
	p := filepath.Join(filepath.Dir(file), "..", "..", "..", "images", "sandbox-base", "bin", "aileron-shell-mediator")
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("mediator script not found at %s: %v", p, err)
	}
	return p
}

func requireShellTooling(t *testing.T) {
	t.Helper()
	for _, bin := range []string{"sh", "wget", "awk", "grep", "sed"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("required tool %q not available on host: %v", bin, err)
		}
	}
}

// runIntercept execs `sh <script> intercept <command>` with a clean env so no
// ambient AILERON_* values leak in. PATH is forwarded so sh can find wget/awk.
func runIntercept(t *testing.T, script, command string, env map[string]string) interceptResult {
	t.Helper()
	cmd := exec.Command("sh", script, "intercept", command)
	baseEnv := []string{"PATH=" + os.Getenv("PATH")}
	for k, v := range env {
		baseEnv = append(baseEnv, k+"="+v)
	}
	cmd.Env = baseEnv
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err := cmd.Run()
	exit := 0
	if err != nil {
		var ee *exec.ExitError
		if !asExitError(err, &ee) {
			t.Fatalf("running mediator: %v\nstderr: %s", err, errBuf.String())
		}
		exit = ee.ExitCode()
	}
	return interceptResult{exit: exit, stdout: outBuf.String(), stderr: errBuf.String()}
}

func asExitError(err error, target **exec.ExitError) bool {
	if ee, ok := err.(*exec.ExitError); ok {
		*target = ee
		return true
	}
	return false
}

const allowBody = `{"status":"decided","decision":"allow","audit_id":"aud-123","reason":"allow-only"}`

type recordedRequest struct {
	method      string
	path        string
	authz       string
	sessionID   string
	contentType string
	body        []byte
}

// stubDaemon returns a server, a counter of received requests, and a pointer to
// the last recorded request. The handler responds with respStatus/respBody.
func stubDaemon(t *testing.T, respStatus int, respBody string) (*httptest.Server, *int32, *recordedRequest) {
	t.Helper()
	var count int32
	last := &recordedRequest{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&count, 1)
		body, _ := io.ReadAll(r.Body)
		*last = recordedRequest{
			method:      r.Method,
			path:        r.URL.Path,
			authz:       r.Header.Get("Authorization"),
			sessionID:   r.Header.Get("X-Aileron-Session-Id"),
			contentType: r.Header.Get("Content-Type"),
			body:        body,
		}
		w.WriteHeader(respStatus)
		_, _ = io.WriteString(w, respBody)
	}))
	t.Cleanup(srv.Close)
	return srv, &count, last
}

type sentBody struct {
	Command string `json:"command"`
	Cwd     string `json:"cwd"`
	Shell   string `json:"shell"`
	Pid     int    `json:"pid"`
	Ppid    int    `json:"ppid"`
}

func TestInterceptAllowSendsWellFormedRequest(t *testing.T) {
	requireShellTooling(t)
	script := mediatorScriptPath(t)
	srv, count, last := stubDaemon(t, http.StatusOK, allowBody)

	res := runIntercept(t, script, "ls -la", map[string]string{
		"AILERON_SANDBOX_SHELL_MEDIATION": "1",
		"AILERON_API_URL":                 srv.URL,
		"AILERON_TOKEN":                   "secret-token",
		"AILERON_SESSION_ID":              "sess-42",
		"AILERON_REAL_SHELL":              "/bin/bash",
	})

	if res.exit != 0 {
		t.Fatalf("expected exit 0 on allow, got %d (stderr: %s)", res.exit, res.stderr)
	}
	if got := atomic.LoadInt32(count); got != 1 {
		t.Fatalf("expected exactly 1 request, got %d", got)
	}
	if last.method != http.MethodPost {
		t.Errorf("method = %q, want POST", last.method)
	}
	if last.path != "/sandbox-shell/decide" {
		t.Errorf("path = %q, want /sandbox-shell/decide", last.path)
	}
	if last.authz != "Bearer secret-token" {
		t.Errorf("Authorization = %q, want Bearer secret-token", last.authz)
	}
	if last.sessionID != "sess-42" {
		t.Errorf("X-Aileron-Session-Id = %q, want sess-42", last.sessionID)
	}
	if last.contentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", last.contentType)
	}
	var decoded sentBody
	if err := json.Unmarshal(last.body, &decoded); err != nil {
		t.Fatalf("request body is not valid JSON: %v\nbody: %s", err, last.body)
	}
	if decoded.Command != "ls -la" {
		t.Errorf("command = %q, want %q", decoded.Command, "ls -la")
	}
	if decoded.Shell != "/bin/bash" {
		t.Errorf("shell = %q, want /bin/bash", decoded.Shell)
	}
}

func TestInterceptEscapesCommandAndOmitsCredentials(t *testing.T) {
	requireShellTooling(t)
	script := mediatorScriptPath(t)
	srv, _, last := stubDaemon(t, http.StatusOK, allowBody)

	command := `echo "a\"b\\c d"`
	res := runIntercept(t, script, command, map[string]string{
		"AILERON_SANDBOX_SHELL_MEDIATION": "1",
		"AILERON_API_URL":                 srv.URL,
		"AILERON_TOKEN":                   "super-secret-token",
		"AILERON_SESSION_ID":              "sess-99",
		"AILERON_REAL_SHELL":              "/bin/bash",
	})

	if res.exit != 0 {
		t.Fatalf("expected exit 0, got %d (stderr: %s)", res.exit, res.stderr)
	}
	var decoded sentBody
	if err := json.Unmarshal(last.body, &decoded); err != nil {
		t.Fatalf("request body is not valid JSON: %v\nbody: %s", err, last.body)
	}
	if decoded.Command != command {
		t.Errorf("command round-trip mismatch:\n got %q\nwant %q", decoded.Command, command)
	}
	if strings.Contains(string(last.body), "super-secret-token") {
		t.Errorf("token leaked into request body: %s", last.body)
	}
	if strings.Contains(string(last.body), "sess-99") {
		t.Errorf("session id leaked into request body: %s", last.body)
	}
}

func TestInterceptMissingAPIURLFailsClosed(t *testing.T) {
	requireShellTooling(t)
	script := mediatorScriptPath(t)
	srv, count, _ := stubDaemon(t, http.StatusOK, allowBody)
	_ = srv // server exists but URL is intentionally not passed

	res := runIntercept(t, script, "ls", map[string]string{
		"AILERON_SANDBOX_SHELL_MEDIATION": "1",
		"AILERON_TOKEN":                   "secret-token",
		"AILERON_SESSION_ID":              "sess-1",
	})

	if res.exit == 0 {
		t.Fatalf("expected nonzero exit when API URL missing")
	}
	if !strings.Contains(res.stderr, "[Aileron]") {
		t.Errorf("expected [Aileron] message on stderr, got: %s", res.stderr)
	}
	if got := atomic.LoadInt32(count); got != 0 {
		t.Errorf("expected no HTTP call, got %d", got)
	}
}

func TestInterceptMissingTokenFailsClosed(t *testing.T) {
	requireShellTooling(t)
	script := mediatorScriptPath(t)
	srv, count, _ := stubDaemon(t, http.StatusOK, allowBody)

	res := runIntercept(t, script, "ls", map[string]string{
		"AILERON_SANDBOX_SHELL_MEDIATION": "1",
		"AILERON_API_URL":                 srv.URL,
		"AILERON_SESSION_ID":              "sess-1",
	})

	if res.exit == 0 {
		t.Fatalf("expected nonzero exit when token missing")
	}
	if !strings.Contains(res.stderr, "[Aileron]") {
		t.Errorf("expected [Aileron] message on stderr, got: %s", res.stderr)
	}
	if got := atomic.LoadInt32(count); got != 0 {
		t.Errorf("expected no HTTP call, got %d", got)
	}
}

func TestInterceptHTTP500FailsClosed(t *testing.T) {
	requireShellTooling(t)
	script := mediatorScriptPath(t)
	srv, _, _ := stubDaemon(t, http.StatusInternalServerError, "boom")

	res := runIntercept(t, script, "ls", map[string]string{
		"AILERON_SANDBOX_SHELL_MEDIATION": "1",
		"AILERON_API_URL":                 srv.URL,
		"AILERON_TOKEN":                   "secret-token",
		"AILERON_SESSION_ID":              "sess-1",
	})

	if res.exit == 0 {
		t.Fatalf("expected nonzero exit on HTTP 500")
	}
	if !strings.Contains(res.stderr, "[Aileron] mediation unavailable") {
		t.Errorf("expected mediation-unavailable message, got: %s", res.stderr)
	}
}

func TestInterceptDenyDecisionFailsClosed(t *testing.T) {
	requireShellTooling(t)
	script := mediatorScriptPath(t)
	denyBody := `{"status":"decided","decision":"deny","audit_id":"aud-7","reason":"policy blocked"}`
	srv, _, _ := stubDaemon(t, http.StatusOK, denyBody)

	res := runIntercept(t, script, "rm -rf /", map[string]string{
		"AILERON_SANDBOX_SHELL_MEDIATION": "1",
		"AILERON_API_URL":                 srv.URL,
		"AILERON_TOKEN":                   "secret-token",
		"AILERON_SESSION_ID":              "sess-1",
	})

	if res.exit == 0 {
		t.Fatalf("expected nonzero exit on deny decision")
	}
	if !strings.Contains(res.stderr, "[Aileron] denied") {
		t.Errorf("expected denied message, got: %s", res.stderr)
	}
}

func TestInterceptMalformedBodyFailsClosed(t *testing.T) {
	requireShellTooling(t)
	script := mediatorScriptPath(t)
	srv, _, _ := stubDaemon(t, http.StatusOK, "not json at all")

	res := runIntercept(t, script, "ls", map[string]string{
		"AILERON_SANDBOX_SHELL_MEDIATION": "1",
		"AILERON_API_URL":                 srv.URL,
		"AILERON_TOKEN":                   "secret-token",
		"AILERON_SESSION_ID":              "sess-1",
	})

	if res.exit == 0 {
		t.Fatalf("expected nonzero exit on malformed body")
	}
	if !strings.Contains(res.stderr, "[Aileron]") {
		t.Errorf("expected [Aileron] message on stderr, got: %s", res.stderr)
	}
}

func TestInterceptConnectionRefusedFailsClosed(t *testing.T) {
	requireShellTooling(t)
	script := mediatorScriptPath(t)
	// Start then immediately close a server to obtain a refused URL.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	refusedURL := srv.URL
	srv.Close()

	res := runIntercept(t, script, "ls", map[string]string{
		"AILERON_SANDBOX_SHELL_MEDIATION": "1",
		"AILERON_API_URL":                 refusedURL,
		"AILERON_TOKEN":                   "secret-token",
		"AILERON_SESSION_ID":              "sess-1",
	})

	if res.exit == 0 {
		t.Fatalf("expected nonzero exit on connection refused")
	}
	if !strings.Contains(res.stderr, "[Aileron] mediation unavailable") {
		t.Errorf("expected mediation-unavailable message, got: %s", res.stderr)
	}
}

func requireSh(t *testing.T) {
	t.Helper()
	// The --check probe is a POSIX shell contract that resolves /bin/bash and
	// compares PATH-resolved paths. Under Windows the test runs in Git bash,
	// where command -v returns MSYS-translated paths (/tmp/...) that never
	// string-match the native wrapper path (C:\...), so the comparison is not
	// meaningful. Linux CI and the sandbox-base smoke test are the real gates.
	if runtime.GOOS == "windows" {
		t.Skip("POSIX --check probe is not meaningful under Windows path translation")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skipf("sh not available on host: %v", err)
	}
}

// runCheck execs `sh <script> --check` with binDir prepended to PATH so
// `command -v bash` resolves the way the validation probe resolves it in the
// image.
func runCheck(t *testing.T, script, binDir string, env map[string]string) interceptResult {
	t.Helper()
	cmd := exec.Command("sh", script, "--check")
	pathVal := os.Getenv("PATH")
	if binDir != "" {
		pathVal = binDir + string(os.PathListSeparator) + pathVal
	}
	baseEnv := []string{"PATH=" + pathVal}
	for k, v := range env {
		baseEnv = append(baseEnv, k+"="+v)
	}
	cmd.Env = baseEnv
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err := cmd.Run()
	exit := 0
	if err != nil {
		var ee *exec.ExitError
		if !asExitError(err, &ee) {
			t.Fatalf("running --check: %v\nstderr: %s", err, errBuf.String())
		}
		exit = ee.ExitCode()
	}
	return interceptResult{exit: exit, stdout: outBuf.String(), stderr: errBuf.String()}
}

// installWrapperOnPath writes an executable named `bash` into a fresh dir and
// returns the dir and the full wrapper path. The check only verifies the
// resolved path, so the file's contents do not matter here.
func installWrapperOnPath(t *testing.T) (binDir, wrapperPath string) {
	t.Helper()
	binDir = t.TempDir()
	wrapperPath = filepath.Join(binDir, "bash")
	if err := os.WriteFile(wrapperPath, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("install wrapper: %v", err)
	}
	return binDir, wrapperPath
}

// The --check mode is the image-validation probe. Beyond the real shell and
// rcfile, it must confirm the bash wrapper sits ahead of the real shell on
// PATH so routing actually works, and fail with an actionable error otherwise
// (R6). The expected wrapper path is AILERON_SHELL_WRAPPER (default
// /usr/local/bin/bash); the override exists so the contract is testable
// without writing under /usr/local/bin.

func TestCheckPassesWhenWrapperResolvesOnPath(t *testing.T) {
	requireSh(t)
	script := mediatorScriptPath(t)
	binDir, wrapper := installWrapperOnPath(t)

	res := runCheck(t, script, binDir, map[string]string{
		"AILERON_SHELL_WRAPPER": wrapper,
		"AILERON_REAL_SHELL":    "/bin/bash",
		"AILERON_SHELL_RCFILE":  bashrcPath(t),
	})

	if res.exit != 0 {
		t.Fatalf("expected --check to pass with wrapper on PATH, got %d (stderr: %s)", res.exit, res.stderr)
	}
}

func TestCheckFailsWhenWrapperMissingFromPath(t *testing.T) {
	requireSh(t)
	script := mediatorScriptPath(t)

	// No wrapper dir on PATH and the default wrapper path is expected, so
	// `command -v bash` resolves to the bare real shell (or nothing). The probe
	// must reject this before the agent starts.
	res := runCheck(t, script, "", map[string]string{
		"AILERON_SHELL_WRAPPER": "/usr/local/bin/bash",
		"AILERON_REAL_SHELL":    "/bin/bash",
		"AILERON_SHELL_RCFILE":  bashrcPath(t),
	})

	if res.exit == 0 {
		t.Fatalf("expected --check to fail when wrapper missing from PATH; stderr: %s", res.stderr)
	}
	if !strings.Contains(res.stderr, "bash wrapper") {
		t.Errorf("expected error naming the bash wrapper, got: %s", res.stderr)
	}
	if !strings.Contains(res.stderr, "disable sandbox shell mediation") {
		t.Errorf("expected actionable remediation in error, got: %s", res.stderr)
	}
}

func wrapperScriptPath(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot resolve test file path")
	}
	p := filepath.Join(filepath.Dir(file), "..", "..", "..", "images", "sandbox-base", "bin", "aileron-shell-wrapper")
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("wrapper script not found at %s: %v", p, err)
	}
	return p
}

func copyExecutable(t *testing.T, src, dst string) {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read %s: %v", src, err)
	}
	if err := os.WriteFile(dst, data, 0o755); err != nil {
		t.Fatalf("install %s: %v", dst, err)
	}
}

// installShellRouting lays out the image's shell-routing contract in a temp
// dir: the real mediator plus the wrapper copied to both `bash` and `sh`,
// mirroring /usr/local/bin. It returns that dir (to prepend to PATH) and the
// rcfile path. With binDir first on PATH, the wrapper's bare
// `aileron-shell-mediator` call and any nested `bash`/`sh` resolve here the
// way they resolve to /usr/local/bin in the image.
func installShellRouting(t *testing.T) (binDir, rcPath string) {
	t.Helper()
	binDir = t.TempDir()
	copyExecutable(t, mediatorScriptPath(t), filepath.Join(binDir, "aileron-shell-mediator"))
	wp := wrapperScriptPath(t)
	copyExecutable(t, wp, filepath.Join(binDir, "bash"))
	copyExecutable(t, wp, filepath.Join(binDir, "sh"))
	return binDir, bashrcPath(t)
}

// runWrapper invokes binDir/<name> (the wrapper standing in for bash or sh)
// with args, env, and optional stdin, capturing the result. PATH is binDir
// first so the wrapper and trap resolve the mediator and nested shells here.
func runWrapper(t *testing.T, binDir, name string, args []string, env map[string]string, stdin string) interceptResult {
	t.Helper()
	cmd := exec.Command(filepath.Join(binDir, name), args...)
	baseEnv := []string{"PATH=" + binDir + string(os.PathListSeparator) + os.Getenv("PATH")}
	for k, v := range env {
		baseEnv = append(baseEnv, k+"="+v)
	}
	cmd.Env = baseEnv
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err := cmd.Run()
	exit := 0
	if err != nil {
		var ee *exec.ExitError
		if !asExitError(err, &ee) {
			t.Fatalf("running wrapper: %v\nstderr: %s", err, errBuf.String())
		}
		exit = ee.ExitCode()
	}
	return interceptResult{exit: exit, stdout: outBuf.String(), stderr: errBuf.String()}
}

func wrapperMediationEnv(srv, rcPath string) map[string]string {
	return map[string]string{
		"AILERON_SANDBOX_SHELL_MEDIATION": "1",
		"AILERON_API_URL":                 srv,
		"AILERON_TOKEN":                   "tok",
		"AILERON_SESSION_ID":              "sess",
		"AILERON_REAL_SHELL":              "/bin/bash",
		"AILERON_SHELL_RCFILE":            rcPath,
	}
}

// The wrapper stands in for /usr/local/bin/bash and /usr/local/bin/sh. Under
// mediation it routes the caller into the trap-bearing real bash; with
// mediation off it is transparent. These tests run the real wrapper + mediator
// + rcfile against the httptest stub; BusyBox fidelity on the real image is
// the sandbox-base smoke test (U3).

func TestWrapBashRoutesAndMediates(t *testing.T) {
	requireBashTooling(t)
	binDir, rc := installShellRouting(t)
	srv, count, _ := stubDaemon(t, http.StatusOK, allowBody)

	res := runWrapper(t, binDir, "bash", []string{"-c", "echo wrapped-ok"}, wrapperMediationEnv(srv.URL, rc), "")

	if !strings.Contains(res.stdout, "wrapped-ok") {
		t.Errorf("expected routed command to run; stdout=%q stderr=%q", res.stdout, res.stderr)
	}
	if got := atomic.LoadInt32(count); got != 1 {
		t.Errorf("expected exactly 1 decision call for one command, got %d", got)
	}
}

func TestWrapBashMediationOffIsTransparent(t *testing.T) {
	requireBashTooling(t)
	binDir, _ := installShellRouting(t)
	srv, count, _ := stubDaemon(t, http.StatusOK, allowBody)

	env := map[string]string{
		// AILERON_SANDBOX_SHELL_MEDIATION intentionally unset.
		"AILERON_API_URL":    srv.URL,
		"AILERON_TOKEN":      "tok",
		"AILERON_SESSION_ID": "sess",
		"AILERON_REAL_SHELL": "/bin/bash",
	}
	res := runWrapper(t, binDir, "bash", []string{"-c", "echo off-ok"}, env, "")

	if !strings.Contains(res.stdout, "off-ok") {
		t.Errorf("expected transparent passthrough to run command; stdout=%q stderr=%q", res.stdout, res.stderr)
	}
	if got := atomic.LoadInt32(count); got != 0 {
		t.Errorf("expected no decision call with mediation off, got %d", got)
	}
}

func TestWrapShRoutesIntoBashAndMediates(t *testing.T) {
	requireShellTooling(t)
	requireBashTooling(t)
	binDir, rc := installShellRouting(t)
	srv, count, _ := stubDaemon(t, http.StatusOK, allowBody)

	res := runWrapper(t, binDir, "sh", []string{"-c", "echo via-sh"}, wrapperMediationEnv(srv.URL, rc), "")

	if !strings.Contains(res.stdout, "via-sh") {
		t.Errorf("expected sh -c to route into bash and run; stdout=%q stderr=%q", res.stdout, res.stderr)
	}
	if got := atomic.LoadInt32(count); got != 1 {
		t.Errorf("expected sh -c to be mediated with exactly 1 decision call, got %d", got)
	}
}

func TestWrapShMediationOffIsTransparent(t *testing.T) {
	requireShellTooling(t)
	binDir, _ := installShellRouting(t)
	srv, count, _ := stubDaemon(t, http.StatusOK, allowBody)

	env := map[string]string{
		"AILERON_API_URL":    srv.URL,
		"AILERON_TOKEN":      "tok",
		"AILERON_SESSION_ID": "sess",
		"AILERON_REAL_SHELL": "/bin/bash",
	}
	res := runWrapper(t, binDir, "sh", []string{"-c", "echo sh-off"}, env, "")

	if !strings.Contains(res.stdout, "sh-off") {
		t.Errorf("expected transparent sh passthrough; stdout=%q stderr=%q", res.stdout, res.stderr)
	}
	if got := atomic.LoadInt32(count); got != 0 {
		t.Errorf("expected no decision call with mediation off, got %d", got)
	}
}

func TestWrapNestedBashTerminatesAndMediatesEach(t *testing.T) {
	requireBashTooling(t)
	binDir, rc := installShellRouting(t)
	srv, count, _ := stubDaemon(t, http.StatusOK, allowBody)

	// The inner `bash` resolves the wrapper again on PATH. The wrapper execs
	// the real shell by absolute path, so this is bounded, not an infinite
	// loop (R4): outer command, inner bash invocation, and inner command each
	// produce one decision call.
	res := runWrapper(t, binDir, "bash", []string{"-c", "echo outer; bash -c 'echo inner'"}, wrapperMediationEnv(srv.URL, rc), "")

	if !strings.Contains(res.stdout, "outer") || !strings.Contains(res.stdout, "inner") {
		t.Fatalf("expected both outer and inner to run; stdout=%q stderr=%q", res.stdout, res.stderr)
	}
	if got := atomic.LoadInt32(count); got != 3 {
		t.Errorf("expected 3 decision calls (outer cmd, inner bash, inner cmd), got %d", got)
	}
}

func TestWrapInteractiveSelectsRcfile(t *testing.T) {
	requireBashTooling(t)
	binDir, rc := installShellRouting(t)
	srv, count, _ := stubDaemon(t, http.StatusOK, allowBody)

	// No -c: an interactive invocation. BASH_ENV is ignored for interactive
	// shells, so the mediator must select --rcfile to install the trap. Feed
	// the command on stdin and assert it ran and was mediated.
	res := runWrapper(t, binDir, "bash", []string{"-i"}, wrapperMediationEnv(srv.URL, rc), "echo from-interactive\n")

	if !strings.Contains(res.stdout, "from-interactive") {
		t.Errorf("expected interactive command to run via --rcfile; stdout=%q stderr=%q", res.stdout, res.stderr)
	}
	if got := atomic.LoadInt32(count); got < 1 {
		t.Errorf("expected interactive shell to install the trap and mediate, got %d calls", got)
	}
}

func TestInterceptMediationOffDoesNotCallDaemon(t *testing.T) {
	requireShellTooling(t)
	script := mediatorScriptPath(t)
	srv, count, _ := stubDaemon(t, http.StatusOK, allowBody)

	res := runIntercept(t, script, "ls", map[string]string{
		// AILERON_SANDBOX_SHELL_MEDIATION intentionally unset.
		"AILERON_API_URL":    srv.URL,
		"AILERON_TOKEN":      "secret-token",
		"AILERON_SESSION_ID": "sess-1",
	})

	if res.exit != 0 {
		t.Fatalf("expected exit 0 when mediation off, got %d (stderr: %s)", res.exit, res.stderr)
	}
	if got := atomic.LoadInt32(count); got != 0 {
		t.Errorf("expected no HTTP call when mediation off, got %d", got)
	}
}
