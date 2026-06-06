package container

import (
	"bytes"
	"fmt"
	"net"
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

// Alpine-pinned shell-mediation tests. These mirror the host-bash scenarios in
// shellrc_test.go but exercise them against the real `aileron-sandbox-base`
// image (Alpine + bash 5.x + BusyBox wget/awk/grep/sed + tini PID 1), which is
// the bash version and tooling the agent actually hits. The host-bash tests
// give fast iteration; the Alpine-pinned variant is the canonical gate.
//
// These tests are the U1 falsification gate against the Alpine target (see
// docs/plans/2026-06-05-001-feat-sandbox-shell-deny-plan.md). They fail
// against the slice-5 rcfile and pass once U3 wires the chosen halt mechanism.
//
// Tests skip when Docker is unavailable or the smoke image is not built. To
// build it locally:
//
//	docker build -f images/sandbox-base/Containerfile -t aileron-sandbox-base:smoke images/sandbox-base
//
// CI builds the image as part of .github/workflows/sandbox-base.yml (U4).

const sandboxSmokeImage = "aileron-sandbox-base:smoke"

func requireDocker(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skipf("docker not available on host: %v", err)
	}
	// Image must already be present locally; building it in-test would multiply
	// test wall time by minutes and obscure the gate's signal.
	cmd := exec.Command("docker", "image", "inspect", sandboxSmokeImage)
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Run(); err != nil {
		t.Skipf("smoke image %q not available locally: build with `docker build -f images/sandbox-base/Containerfile -t %s images/sandbox-base`",
			sandboxSmokeImage, sandboxSmokeImage)
	}
}

// repoBashrcPath resolves the rcfile under images/sandbox-base/shell/. Mounting
// it over the baked-in /etc/aileron/shell/aileron-bashrc lets tests exercise
// the WORKING-COPY rcfile against the IMAGE's bash 5.x, without rebuilding the
// image on every iteration.
func repoBashrcPath(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot resolve test file path")
	}
	p := filepath.Join(filepath.Dir(file), "..", "..", "..", "images", "sandbox-base", "shell", "aileron-bashrc")
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("bashrc not found at %s: %v", p, err)
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		t.Fatalf("absolute bashrc path: %v", err)
	}
	return abs
}

// startAlpineStub starts an HTTP server bound to 0.0.0.0 so the docker
// container can reach it through host-gateway routing. Returns the URL the
// container should call (using host.docker.internal so the same URL works on
// Linux CI runners with `--add-host=host.docker.internal:host-gateway` and on
// macOS Docker Desktop, which resolves the same name natively).
func startAlpineStub(t *testing.T, respStatus int, respBody string) (containerURL string, requestCount *int32) {
	t.Helper()
	listener, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatalf("listen 0.0.0.0:0: %v", err)
	}
	var count int32
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&count, 1)
		w.WriteHeader(respStatus)
		_, _ = w.Write([]byte(respBody))
	})
	srv := &httptest.Server{
		Listener: listener,
		Config:   &http.Server{Handler: mux},
	}
	srv.Start()
	t.Cleanup(srv.Close)
	port := listener.Addr().(*net.TCPAddr).Port
	return fmt.Sprintf("http://host.docker.internal:%d", port), &count
}

// dockerBash runs `bash -c <command>` inside the sandbox-base smoke image with
// the working-copy rcfile mounted over the baked-in one and AILERON_API_URL
// pointed at the host stub. Returns stdout, stderr, exit code.
func dockerBash(t *testing.T, apiURL, command string, extraArgs ...string) interceptResult {
	t.Helper()
	rc := repoBashrcPath(t)
	args := []string{
		"run", "--rm",
		"--add-host=host.docker.internal:host-gateway",
		"-v", rc + ":/etc/aileron/shell/aileron-bashrc:ro",
		"-e", "AILERON_SANDBOX_SHELL_MEDIATION=1",
		"-e", "AILERON_API_URL=" + apiURL,
		"-e", "AILERON_TOKEN=tok",
		"-e", "AILERON_SESSION_ID=sess",
		"-e", "AILERON_REAL_SHELL=/bin/bash",
	}
	args = append(args, extraArgs...)
	args = append(args, sandboxSmokeImage, "bash", "-c", command)
	cmd := exec.Command("docker", args...)
	return runDocker(t, cmd)
}

// dockerBashInteractive runs `bash -ic <command>` against the image. The image
// has no real pty; the interactive-flag (`-i`) is the load-bearing signal the
// rcfile's `[[ $- == *i* ]]` check observes, so the soft-veto branch fires.
// The plain `docker run -i` path here does NOT allocate a terminal; that
// stricter assertion lives in U4's `script -qc` workflow step.
func dockerBashInteractive(t *testing.T, apiURL, command string) interceptResult {
	t.Helper()
	rc := repoBashrcPath(t)
	args := []string{
		"run", "--rm",
		"--add-host=host.docker.internal:host-gateway",
		"-v", rc + ":/etc/aileron/shell/aileron-bashrc:ro",
		"-e", "AILERON_SANDBOX_SHELL_MEDIATION=1",
		"-e", "AILERON_API_URL=" + apiURL,
		"-e", "AILERON_TOKEN=tok",
		"-e", "AILERON_SESSION_ID=sess",
		"-e", "AILERON_REAL_SHELL=/bin/bash",
		sandboxSmokeImage,
		"bash", "--rcfile", "/etc/aileron/shell/aileron-bashrc", "-ic", command,
	}
	cmd := exec.Command("docker", args...)
	return runDocker(t, cmd)
}

func runDocker(t *testing.T, cmd *exec.Cmd) interceptResult {
	t.Helper()
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err := cmd.Run()
	exit := 0
	if err != nil {
		var ee *exec.ExitError
		if !asExitError(err, &ee) {
			t.Fatalf("running docker: %v\nstderr: %s", err, errBuf.String())
		}
		exit = ee.ExitCode()
	}
	return interceptResult{exit: exit, stdout: outBuf.String(), stderr: errBuf.String()}
}

func TestBashrcAlpine_NonInteractiveAllowRunsCommand(t *testing.T) {
	requireDocker(t)
	url, count := startAlpineStub(t, http.StatusOK, allowBody)

	res := dockerBash(t, url, "echo allowed-ok")

	if !strings.Contains(res.stdout, "allowed-ok") {
		t.Errorf("allow path did not run command; stdout=%q stderr=%q", res.stdout, res.stderr)
	}
	if res.exit != 0 {
		t.Errorf("allow path exited nonzero: %d; stderr=%q", res.exit, res.stderr)
	}
	if atomic.LoadInt32(count) < 1 {
		t.Errorf("expected at least one daemon call on allow, got %d", atomic.LoadInt32(count))
	}
}

func TestBashrcAlpine_NonInteractiveDenyHaltsChain(t *testing.T) {
	requireDocker(t)
	url, _ := startAlpineStub(t, http.StatusOK, denyBody)

	res := dockerBash(t, url, "echo FIRST && echo SECOND")

	if strings.Contains(res.stdout, "FIRST") {
		t.Errorf("deny did not veto: FIRST printed; stdout=%q", res.stdout)
	}
	if strings.Contains(res.stdout, "SECOND") {
		t.Errorf("deny did not halt && chain: SECOND printed; stdout=%q", res.stdout)
	}
	if res.exit == 0 {
		t.Errorf("deny did not halt with nonzero exit; stderr=%q", res.stderr)
	}
	if !strings.Contains(res.stderr, "[Aileron] denied:") {
		t.Errorf("expected [Aileron] denied: on stderr, got: %s", res.stderr)
	}
}

func TestBashrcAlpine_NonInteractiveDenyExitsNonzero(t *testing.T) {
	requireDocker(t)
	url, _ := startAlpineStub(t, http.StatusOK, denyBody)

	res := dockerBash(t, url, "echo ONLY")

	if strings.Contains(res.stdout, "ONLY") {
		t.Errorf("deny did not veto: ONLY printed; stdout=%q", res.stdout)
	}
	if res.exit == 0 {
		t.Errorf("deny did not exit nonzero; stderr=%q", res.stderr)
	}
}

func TestBashrcAlpine_NonInteractiveDenySemicolonChainHalts(t *testing.T) {
	requireDocker(t)
	url, _ := startAlpineStub(t, http.StatusOK, denyBody)

	res := dockerBash(t, url, "echo FIRST; echo SECOND")

	if strings.Contains(res.stdout, "FIRST") {
		t.Errorf("deny did not veto: FIRST printed; stdout=%q", res.stdout)
	}
	if strings.Contains(res.stdout, "SECOND") {
		t.Errorf("deny did not halt `;` chain: SECOND printed; stdout=%q", res.stdout)
	}
	if res.exit == 0 {
		t.Errorf("deny did not exit nonzero; stderr=%q", res.stderr)
	}
}

func TestBashrcAlpine_NonInteractiveDenyOrRecoveryHalts(t *testing.T) {
	requireDocker(t)
	url, _ := startAlpineStub(t, http.StatusOK, denyBody)

	res := dockerBash(t, url, "false-cmd || echo RECOVERED")

	if strings.Contains(res.stdout, "RECOVERED") {
		t.Errorf("`||` recovery branch ran after deny: stdout=%q stderr=%q", res.stdout, res.stderr)
	}
	if res.exit == 0 {
		t.Errorf("deny did not exit nonzero; stderr=%q", res.stderr)
	}
}

func TestBashrcAlpine_NonInteractiveDenyDoesNotLeakCommandToStderr(t *testing.T) {
	requireDocker(t)
	url, _ := startAlpineStub(t, http.StatusOK, denyBody)

	res := dockerBash(t, url, "denied-cmd --arg=SECRET-12345")

	if !strings.Contains(res.stderr, "[Aileron] denied:") {
		t.Errorf("expected [Aileron] denied: on stderr, got: %s", res.stderr)
	}
	for _, leaked := range []string{"denied-cmd", "--arg", "SECRET-12345"} {
		if strings.Contains(res.stderr, leaked) {
			t.Errorf("stderr leaked $BASH_COMMAND substring %q: %s", leaked, res.stderr)
		}
	}
}

func TestBashrcAlpine_NonInteractiveUnreachableDaemonHaltsChain(t *testing.T) {
	requireDocker(t)
	// Point at a closed port on host-gateway. The mediator must fail closed.
	closedURL := "http://host.docker.internal:1"

	res := dockerBash(t, closedURL, "echo FIRST && echo SECOND")

	if strings.Contains(res.stdout, "FIRST") {
		t.Errorf("unreachable daemon did not veto: FIRST printed; stdout=%q", res.stdout)
	}
	if strings.Contains(res.stdout, "SECOND") {
		t.Errorf("unreachable daemon did not halt chain: SECOND printed; stdout=%q", res.stdout)
	}
	if res.exit == 0 {
		t.Errorf("unreachable daemon did not exit nonzero; stderr=%q", res.stderr)
	}
	if !strings.Contains(res.stderr, "[Aileron] mediation unavailable:") {
		t.Errorf("expected [Aileron] mediation unavailable: on stderr, got: %s", res.stderr)
	}
}

func TestBashrcAlpine_NonInteractiveHitsDaemonOncePerCommand(t *testing.T) {
	requireDocker(t)
	url, count := startAlpineStub(t, http.StatusOK, allowBody)

	res := dockerBash(t, url, "echo single")

	if !strings.Contains(res.stdout, "single") {
		t.Fatalf("command did not run; stdout=%q stderr=%q", res.stdout, res.stderr)
	}
	if got := atomic.LoadInt32(count); got != 1 {
		t.Errorf("expected exactly 1 daemon request (recursion guard), got %d", got)
	}
}

func TestBashrcAlpine_InteractiveDenyKeepsReplAlive(t *testing.T) {
	requireDocker(t)
	url, _ := startAlpineStub(t, http.StatusOK, denyBody)

	res := dockerBashInteractive(t, url, "echo SHOULD-NOT-PRINT")

	if strings.Contains(res.stdout, "SHOULD-NOT-PRINT") {
		t.Errorf("interactive deny did not suppress side effect; stdout=%q", res.stdout)
	}
	if res.exit != 0 {
		t.Errorf("interactive deny killed the REPL: exit=%d, want 0; stderr=%q", res.exit, res.stderr)
	}
	if !strings.Contains(res.stderr, "[Aileron] denied:") {
		t.Errorf("expected [Aileron] denied: on stderr, got: %s", res.stderr)
	}
}
