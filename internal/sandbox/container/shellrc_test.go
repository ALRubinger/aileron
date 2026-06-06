package container

import (
	"bytes"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
)

// aileron-bashrc installs a bash DEBUG trap that routes each about-to-run
// command through aileron-shell-mediator and vetoes it when the mediator exits
// nonzero. These tests run a real `bash --rcfile` against an httptest stub so
// the extdebug veto, recursion guard, and opt-in gating are exercised the way
// the image runs them. They skip on hosts without the required tooling; the
// sandbox-base smoke test (U3) is the BusyBox-fidelity gate.

func bashrcPath(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot resolve test file path")
	}
	p := filepath.Join(filepath.Dir(file), "..", "..", "..", "images", "sandbox-base", "shell", "aileron-bashrc")
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("bashrc not found at %s: %v", p, err)
	}
	return p
}

func requireBashTooling(t *testing.T) {
	t.Helper()
	for _, bin := range []string{"bash", "wget", "awk", "grep", "sed"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("required tool %q not available on host: %v", bin, err)
		}
	}
}

// installMediatorBin copies the real mediator script into a temp dir as an
// executable named aileron-shell-mediator and returns that dir, so the rcfile's
// bare `aileron-shell-mediator` invocation resolves on PATH.
func installMediatorBin(t *testing.T) string {
	t.Helper()
	src := mediatorScriptPath(t)
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read mediator: %v", err)
	}
	dir := t.TempDir()
	dst := filepath.Join(dir, "aileron-shell-mediator")
	if err := os.WriteFile(dst, data, 0o755); err != nil {
		t.Fatalf("install mediator: %v", err)
	}
	return dir
}

func runBashRC(t *testing.T, rcfile, command, binDir string, env map[string]string) interceptResult {
	t.Helper()
	cmd := exec.Command("bash", "--rcfile", rcfile, "-ic", command)
	baseEnv := []string{"PATH=" + binDir + string(os.PathListSeparator) + os.Getenv("PATH")}
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
			t.Fatalf("running bash: %v\nstderr: %s", err, errBuf.String())
		}
		exit = ee.ExitCode()
	}
	return interceptResult{exit: exit, stdout: outBuf.String(), stderr: errBuf.String()}
}

func mediationEnv(srv string) map[string]string {
	return map[string]string{
		"AILERON_SANDBOX_SHELL_MEDIATION": "1",
		"AILERON_API_URL":                 srv,
		"AILERON_TOKEN":                   "tok",
		"AILERON_SESSION_ID":              "sess",
		"AILERON_REAL_SHELL":              "/bin/bash",
	}
}

func TestBashrcAllowRunsCommand(t *testing.T) {
	requireBashTooling(t)
	rc := bashrcPath(t)
	binDir := installMediatorBin(t)
	srv, _, _ := stubDaemon(t, http.StatusOK, allowBody)

	res := runBashRC(t, rc, "echo mediated-ok", binDir, mediationEnv(srv.URL))

	if !strings.Contains(res.stdout, "mediated-ok") {
		t.Errorf("expected allowed command to run; stdout=%q stderr=%q", res.stdout, res.stderr)
	}
}

func TestBashrcDenyVetoesCommandSideEffect(t *testing.T) {
	requireBashTooling(t)
	rc := bashrcPath(t)
	binDir := installMediatorBin(t)
	denyBody := `{"status":"decided","decision":"deny","audit_id":"a","reason":"policy blocked"}`
	srv, _, _ := stubDaemon(t, http.StatusOK, denyBody)

	marker := filepath.Join(t.TempDir(), "created-if-not-vetoed")
	res := runBashRC(t, rc, "touch "+marker, binDir, mediationEnv(srv.URL))

	if _, err := os.Stat(marker); err == nil {
		t.Errorf("deny did not veto: side-effect file was created at %s", marker)
	}
	if !strings.Contains(res.stderr, "[Aileron]") {
		t.Errorf("expected [Aileron] message on stderr, got: %s", res.stderr)
	}
}

func TestBashrcUnreachableDaemonVetoesCommand(t *testing.T) {
	requireBashTooling(t)
	rc := bashrcPath(t)
	binDir := installMediatorBin(t)
	srv, _, _ := stubDaemon(t, http.StatusOK, allowBody)
	refused := srv.URL
	srv.Close()

	marker := filepath.Join(t.TempDir(), "created-if-not-vetoed")
	res := runBashRC(t, rc, "touch "+marker, binDir, mediationEnv(refused))

	if _, err := os.Stat(marker); err == nil {
		t.Errorf("unreachable daemon did not veto: file created at %s", marker)
	}
	if !strings.Contains(res.stderr, "[Aileron]") {
		t.Errorf("expected [Aileron] message on stderr, got: %s", res.stderr)
	}
}

func TestBashrcHitsDaemonOncePerCommand(t *testing.T) {
	requireBashTooling(t)
	rc := bashrcPath(t)
	binDir := installMediatorBin(t)
	srv, count, _ := stubDaemon(t, http.StatusOK, allowBody)

	res := runBashRC(t, rc, "echo single", binDir, mediationEnv(srv.URL))

	if !strings.Contains(res.stdout, "single") {
		t.Fatalf("command did not run; stdout=%q stderr=%q", res.stdout, res.stderr)
	}
	if got := atomic.LoadInt32(count); got != 1 {
		t.Errorf("expected exactly 1 daemon request for one command (recursion guard), got %d", got)
	}
}

func TestBashrcMediationOffIsInert(t *testing.T) {
	requireBashTooling(t)
	rc := bashrcPath(t)
	binDir := installMediatorBin(t)
	srv, count, _ := stubDaemon(t, http.StatusOK, allowBody)

	marker := filepath.Join(t.TempDir(), "should-exist")
	env := map[string]string{
		// AILERON_SANDBOX_SHELL_MEDIATION intentionally unset.
		"AILERON_API_URL":    srv.URL,
		"AILERON_TOKEN":      "tok",
		"AILERON_SESSION_ID": "sess",
	}
	res := runBashRC(t, rc, "touch "+marker+"; echo off-ok", binDir, env)

	if !strings.Contains(res.stdout, "off-ok") {
		t.Errorf("command did not run with mediation off; stdout=%q stderr=%q", res.stdout, res.stderr)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("expected side-effect to occur with mediation off: %v", err)
	}
	if got := atomic.LoadInt32(count); got != 0 {
		t.Errorf("expected no daemon requests with mediation off, got %d", got)
	}
}
