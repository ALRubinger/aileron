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

// runBashEnv runs `bash -c <command>` with no --rcfile and no -i, sourcing the
// rcfile via BASH_ENV instead. This is the agent's real invocation model: a
// non-interactive child shell. BASH_ENV is set to rcfile so non-interactive
// bash sources it at startup and installs the DEBUG trap before the command
// runs. The mediator bin dir is prepended to PATH so the trap's bare
// `aileron-shell-mediator intercept` call resolves.
func runBashEnv(t *testing.T, rcfile, command, binDir string, env map[string]string) interceptResult {
	t.Helper()
	cmd := exec.Command("bash", "-c", command)
	baseEnv := []string{
		"PATH=" + binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"BASH_ENV=" + rcfile,
	}
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

// The cases below exercise the agent's real invocation model: a non-interactive
// `bash -c` child that sources the rcfile via BASH_ENV (no --rcfile, no -i).
// This is the path #801's fifth slice routes (R3). The interactive cases above
// proved the trap under `bash --rcfile -ic`; these prove it survives the
// non-interactive startup the agent actually uses.
//
// Veto contract: a denied command does not run (its side effect is suppressed).
// The bash exit status stays 0 because extdebug skips the command rather than
// failing it, so these assert on the side effect and the [Aileron] veto
// message, not on the exit code. Later #801 slices that add deny semantics own
// any exit-status or chain-halting behavior.

func TestBashrcNonInteractiveAllowRunsCommand(t *testing.T) {
	requireBashTooling(t)
	rc := bashrcPath(t)
	binDir := installMediatorBin(t)
	srv, _, _ := stubDaemon(t, http.StatusOK, allowBody)

	res := runBashEnv(t, rc, "echo mediated-ok", binDir, mediationEnv(srv.URL))

	if !strings.Contains(res.stdout, "mediated-ok") {
		t.Errorf("expected allowed command to run; stdout=%q stderr=%q", res.stdout, res.stderr)
	}
}

func TestBashrcNonInteractiveDenyVetoesCommandSideEffect(t *testing.T) {
	requireBashTooling(t)
	rc := bashrcPath(t)
	binDir := installMediatorBin(t)
	denyBody := `{"status":"decided","decision":"deny","audit_id":"a","reason":"policy blocked"}`
	srv, _, _ := stubDaemon(t, http.StatusOK, denyBody)

	marker := filepath.Join(t.TempDir(), "created-if-not-vetoed")
	res := runBashEnv(t, rc, "touch "+marker, binDir, mediationEnv(srv.URL))

	if _, err := os.Stat(marker); err == nil {
		t.Errorf("deny did not veto under non-interactive bash: side-effect file created at %s", marker)
	}
	if !strings.Contains(res.stderr, "[Aileron]") {
		t.Errorf("expected [Aileron] message on stderr, got: %s", res.stderr)
	}
}

func TestBashrcNonInteractiveUnreachableDaemonVetoesCommand(t *testing.T) {
	requireBashTooling(t)
	rc := bashrcPath(t)
	binDir := installMediatorBin(t)
	srv, _, _ := stubDaemon(t, http.StatusOK, allowBody)
	refused := srv.URL
	srv.Close()

	marker := filepath.Join(t.TempDir(), "created-if-not-vetoed")
	res := runBashEnv(t, rc, "touch "+marker, binDir, mediationEnv(refused))

	if _, err := os.Stat(marker); err == nil {
		t.Errorf("unreachable daemon did not veto under non-interactive bash: file created at %s", marker)
	}
	if !strings.Contains(res.stderr, "[Aileron]") {
		t.Errorf("expected [Aileron] message on stderr, got: %s", res.stderr)
	}
}

func TestBashrcNonInteractiveHitsDaemonOncePerCommand(t *testing.T) {
	requireBashTooling(t)
	rc := bashrcPath(t)
	binDir := installMediatorBin(t)
	srv, count, _ := stubDaemon(t, http.StatusOK, allowBody)

	res := runBashEnv(t, rc, "echo single", binDir, mediationEnv(srv.URL))

	if !strings.Contains(res.stdout, "single") {
		t.Fatalf("command did not run; stdout=%q stderr=%q", res.stdout, res.stderr)
	}
	if got := atomic.LoadInt32(count); got != 1 {
		t.Errorf("expected exactly 1 daemon request for one non-interactive command (recursion guard), got %d", got)
	}
}

func TestBashrcNonInteractiveMediationOffIsInert(t *testing.T) {
	requireBashTooling(t)
	rc := bashrcPath(t)
	binDir := installMediatorBin(t)
	srv, count, _ := stubDaemon(t, http.StatusOK, allowBody)

	marker := filepath.Join(t.TempDir(), "should-exist")
	env := map[string]string{
		// AILERON_SANDBOX_SHELL_MEDIATION intentionally unset: BASH_ENV still
		// sources the rcfile, but the rcfile returns before installing the trap.
		"AILERON_API_URL":    srv.URL,
		"AILERON_TOKEN":      "tok",
		"AILERON_SESSION_ID": "sess",
	}
	res := runBashEnv(t, rc, "touch "+marker+"; echo off-ok", binDir, env)

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

// The cases below are #801 slice 6's deny-contract additions on top of the
// slice-5 veto behavior above. Slice 5 suppressed the about-to-run command's
// side effect but left the shell exit code at 0 and the rest of an `&&` chain
// running. Slice 6's contract on a denied command under the agent's real
// `bash -c` model is:
//
//   - the denied command does NOT run (existing slice-5 assertion above),
//   - the rest of `&& cmd` / `; cmd` chains does NOT run,
//   - the bash process exits NONZERO,
//   - stderr carries `[Aileron] denied:` but does NOT leak the denied
//     command's text or arguments.
//
// Interactive shells preserve the slice-5 soft-veto contract: a human at a
// REPL is not killed by a single denied command.
//
// These tests are the U1 falsification gate (see docs/plans/2026-06-05-001-
// feat-sandbox-shell-deny-plan.md). They fail against the slice-5 rcfile and
// pass once U3 wires the chosen halt mechanism.

const denyBody = `{"status":"decided","decision":"deny","audit_id":"a","reason":"policy:rule-42:blocked"}`

func TestBashrcNonInteractiveDenyHaltsChain(t *testing.T) {
	requireBashTooling(t)
	rc := bashrcPath(t)
	binDir := installMediatorBin(t)
	srv, _, _ := stubDaemon(t, http.StatusOK, denyBody)

	dir := t.TempDir()
	first := filepath.Join(dir, "first")
	second := filepath.Join(dir, "second")
	res := runBashEnv(t, rc, "touch "+first+" && touch "+second, binDir, mediationEnv(srv.URL))

	if _, err := os.Stat(first); err == nil {
		t.Errorf("deny did not veto: first command's side effect at %s was created", first)
	}
	if _, err := os.Stat(second); err == nil {
		t.Errorf("deny did not halt the && chain: second command's side effect at %s was created", second)
	}
	if res.exit == 0 {
		t.Errorf("deny did not halt with nonzero exit: bash exited 0; stderr=%q", res.stderr)
	}
	if !strings.Contains(res.stderr, "[Aileron] denied:") {
		t.Errorf("expected [Aileron] denied: message on stderr, got: %s", res.stderr)
	}
}

func TestBashrcNonInteractiveDenyExitsNonzero(t *testing.T) {
	requireBashTooling(t)
	rc := bashrcPath(t)
	binDir := installMediatorBin(t)
	srv, _, _ := stubDaemon(t, http.StatusOK, denyBody)

	marker := filepath.Join(t.TempDir(), "only")
	res := runBashEnv(t, rc, "touch "+marker, binDir, mediationEnv(srv.URL))

	if _, err := os.Stat(marker); err == nil {
		t.Errorf("deny did not veto: side-effect file created at %s", marker)
	}
	if res.exit == 0 {
		t.Errorf("deny did not exit nonzero: bash exited 0; stderr=%q", res.stderr)
	}
	if !strings.Contains(res.stderr, "[Aileron] denied:") {
		t.Errorf("expected [Aileron] denied: message on stderr, got: %s", res.stderr)
	}
}

func TestBashrcNonInteractiveDenySemicolonChainHalts(t *testing.T) {
	requireBashTooling(t)
	rc := bashrcPath(t)
	binDir := installMediatorBin(t)
	srv, _, _ := stubDaemon(t, http.StatusOK, denyBody)

	dir := t.TempDir()
	first := filepath.Join(dir, "first")
	second := filepath.Join(dir, "second")
	res := runBashEnv(t, rc, "touch "+first+"; touch "+second, binDir, mediationEnv(srv.URL))

	if _, err := os.Stat(first); err == nil {
		t.Errorf("deny did not veto: first command's side effect at %s was created", first)
	}
	if _, err := os.Stat(second); err == nil {
		t.Errorf("deny did not halt the `;` chain: second command's side effect at %s was created", second)
	}
	if res.exit == 0 {
		t.Errorf("deny did not exit nonzero: bash exited 0; stderr=%q", res.stderr)
	}
}

// TestBashrcNonInteractiveDenyOrRecoveryHalts records the observed behavior of
// `denied || recover` under the chosen halt mechanism. With the C1 mechanism
// (DEBUG trap calls `exit` under non-interactive), the whole shell is gone
// before bash sees the `||`, so the recovery branch does NOT run. If a future
// slice swaps to a different mechanism that propagates `||`, update this test
// to match the new contract.
func TestBashrcNonInteractiveDenyOrRecoveryHalts(t *testing.T) {
	requireBashTooling(t)
	rc := bashrcPath(t)
	binDir := installMediatorBin(t)
	srv, _, _ := stubDaemon(t, http.StatusOK, denyBody)

	res := runBashEnv(t, rc, "false-cmd || echo RECOVERED", binDir, mediationEnv(srv.URL))

	if strings.Contains(res.stdout, "RECOVERED") {
		t.Errorf("`||` recovery branch ran after deny: stdout=%q stderr=%q", res.stdout, res.stderr)
	}
	if res.exit == 0 {
		t.Errorf("deny did not exit nonzero: bash exited 0; stderr=%q", res.stderr)
	}
}

func TestBashrcNonInteractiveDenyDoesNotLeakCommandToStderr(t *testing.T) {
	requireBashTooling(t)
	rc := bashrcPath(t)
	binDir := installMediatorBin(t)
	srv, _, _ := stubDaemon(t, http.StatusOK, denyBody)

	// A command name and an arg-shaped value that must NOT appear on stderr.
	// Either of these on stderr means the chosen halt mechanism is leaking the
	// $BASH_COMMAND text via bash's own diagnostics (e.g. exit-from-trap under
	// `set -eu` or xtrace) and the candidate has to be revised.
	res := runBashEnv(t, rc, "denied-cmd --arg=SECRET-12345", binDir, mediationEnv(srv.URL))

	if !strings.Contains(res.stderr, "[Aileron] denied:") {
		t.Errorf("expected [Aileron] denied: on stderr, got: %s", res.stderr)
	}
	for _, leaked := range []string{"denied-cmd", "--arg", "SECRET-12345"} {
		if strings.Contains(res.stderr, leaked) {
			t.Errorf("stderr leaked $BASH_COMMAND substring %q: %s", leaked, res.stderr)
		}
	}
}

func TestBashrcNonInteractiveUnreachableDaemonHaltsChain(t *testing.T) {
	requireBashTooling(t)
	rc := bashrcPath(t)
	binDir := installMediatorBin(t)
	srv, _, _ := stubDaemon(t, http.StatusOK, allowBody)
	refused := srv.URL
	srv.Close()

	dir := t.TempDir()
	first := filepath.Join(dir, "first")
	second := filepath.Join(dir, "second")
	res := runBashEnv(t, rc, "touch "+first+" && touch "+second, binDir, mediationEnv(refused))

	if _, err := os.Stat(first); err == nil {
		t.Errorf("unreachable daemon did not veto: first command's side effect at %s was created", first)
	}
	if _, err := os.Stat(second); err == nil {
		t.Errorf("unreachable daemon did not halt chain: second command's side effect at %s was created", second)
	}
	if res.exit == 0 {
		t.Errorf("unreachable daemon did not exit nonzero: bash exited 0; stderr=%q", res.stderr)
	}
	if !strings.Contains(res.stderr, "[Aileron] mediation unavailable:") {
		t.Errorf("expected [Aileron] mediation unavailable: on stderr, got: %s", res.stderr)
	}
}

// TestBashrcInteractiveDenyKeepsReplAlive documents the KTD3 interactive
// contract: a denied command in an interactive REPL suppresses the side
// effect but the shell itself stays alive (exit 0). This passes against the
// slice-5 rcfile and continues to pass after U3.
func TestBashrcInteractiveDenyKeepsReplAlive(t *testing.T) {
	requireBashTooling(t)
	rc := bashrcPath(t)
	binDir := installMediatorBin(t)
	srv, _, _ := stubDaemon(t, http.StatusOK, denyBody)

	marker := filepath.Join(t.TempDir(), "created-if-not-vetoed")
	res := runBashRC(t, rc, "touch "+marker, binDir, mediationEnv(srv.URL))

	if _, err := os.Stat(marker); err == nil {
		t.Errorf("interactive deny did not suppress side effect: %s was created", marker)
	}
	if res.exit != 0 {
		t.Errorf("interactive deny killed the REPL: exit=%d, want 0; stderr=%q", res.exit, res.stderr)
	}
	if !strings.Contains(res.stderr, "[Aileron] denied:") {
		t.Errorf("expected [Aileron] denied: on stderr, got: %s", res.stderr)
	}
}
