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
