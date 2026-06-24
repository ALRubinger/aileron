package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	systestlib "github.com/ALRubinger/aileron/test/system/lib"
)

// sbxPrefix is the stable sandbox container name prefix (launcher.go
// aileron-sbx-<id>); the session id is derived at runtime, so the harness
// discovers the container by this prefix rather than predicting the full name.
const sbxPrefix = "aileron-sbx-"

// env holds the validated environment the scenario reads, mirroring the bash
// `: "${VAR:?...}"` preconditions. AILERON_SYSTEST_LIB is intentionally absent:
// the lib is now an imported Go package, so that var is vestigial for this path.
type env struct {
	aileronBin    string
	workspace     string
	stateDir      string // optional; empty means the R10 audit assertion is skipped
	expectedImage string // optional EXPECTED_IMAGE override; empty means the agent default
}

// loadEnv reads + validates the required environment, returning a faithful
// remediation error for a missing required var (matching the bash `:?` message).
func loadEnv() (env, error) {
	e := env{
		aileronBin:    os.Getenv("AILERON_BIN"),
		workspace:     os.Getenv("WORKSPACE"),
		stateDir:      os.Getenv("AILERON_STATE_DIR"),
		expectedImage: os.Getenv("EXPECTED_IMAGE"),
	}
	if e.aileronBin == "" {
		return env{}, fmt.Errorf("AILERON_BIN must point at the built aileron binary")
	}
	if e.workspace == "" {
		return env{}, fmt.Errorf("WORKSPACE must be a writable temp dir")
	}
	return e, nil
}

// logf writes a systest-prefixed informational line to stderr, mirroring the
// shell `log` helper so the Go path's output reads the same as the bash path's.
func logf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "systest: "+format+"\n", a...)
}

// runCodex is the faithful Go port of codex.sh's scenario body. It launches
// `aileron launch codex -- exec --skip-git-repo-check "<prompt>"` in the
// background, runs the live-container R8 probes while codex is resident, reaps
// the launch and asserts its exit code, verifies clean teardown, then performs
// the host-side R9 sentinel and conditional R10 audit assertions. It is the
// codex binding of the shared runScenario body; its R8.2 MCP probe is the
// config-file wrapper probeMCPConfigFile.
func runCodex(e env) error {
	return runScenario(e, systestlib.CodexConfig(e.expectedImage), probeMCPConfigFile)
}

// runClaude is the faithful Go port of claude.sh's scenario body. It is the
// claude binding of the shared runScenario body, differing from codex only in the
// claude AgentConfig (cmdline-mode MCP, `-p` batch flag, the claude credential
// path, the local sandbox-agent-claude image) and its R8.2 MCP probe, which is
// the flag-mode wrapper probeMCPCmdline (asserting `--mcp-config` + the
// `"aileron"` server marker on the container command line).
func runClaude(e env) error {
	return runScenario(e, systestlib.ClaudeConfig(e.expectedImage), probeMCPCmdline)
}

// runScenario is the shared, agent-agnostic live-scenario body both runCodex and
// runClaude delegate to. cfg carries the per-agent bindings (image, batch flags,
// credential path, MCP mode); probeMCP is the agent's R8.2 MCP probe wrapper
// (config-file for codex, cmdline for claude). The launch/reap/discovery-poll and
// the R8.1/R8.3-8.6/R9/R7a/R10 assertions are identical across agents, so they
// live here once. Every decision delegates to systestlib; this function holds
// only the impure docker/exec/poll plumbing.
func runScenario(e env, cfg systestlib.AgentConfig, probeMCP func(container string, cfg systestlib.AgentConfig) error) error {
	// Per-run sentinel (R9): a fresh token so a stale workspace file can't pass.
	runID := systestlib.NewRunID()
	sentinel := systestlib.Sentinel(runID)
	prompt := systestlib.BuildPrompt(cfg, sentinel)
	execArgs := systestlib.BuildExecArgs(cfg, prompt)

	logf("%s scenario: runid=%s workspace=%s image=%s", cfg.Name, runID, e.workspace, cfg.ExpectedImage)

	// --- launch in the background (R7a arg forwarding) ---------------------
	// `-- exec --skip-git-repo-check "<prompt>"` forwards verbatim through
	// LaunchConfig.Args into the container command. We run it in the background
	// so the live-container probes can inspect the container while codex is
	// resident, then reap the exit code. stdin is /dev/null so codex exec never
	// blocks waiting for piped input (the prompt is supplied as an arg);
	// stdout+stderr are captured to <workspace>/.launch.log.
	launchLog := filepath.Join(e.workspace, ".launch.log")
	logFile, err := os.Create(launchLog)
	if err != nil {
		return fmt.Errorf("creating launch log %s: %w", launchLog, err)
	}
	defer logFile.Close()

	devNull, err := os.Open(os.DevNull)
	if err != nil {
		return fmt.Errorf("opening %s: %w", os.DevNull, err)
	}
	defer devNull.Close()

	launchArgs := append([]string{"launch", cfg.Name, "--"}, execArgs...)
	cmd := exec.Command(e.aileronBin, launchArgs...)
	cmd.Dir = e.workspace
	cmd.Stdin = devNull
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting aileron launch: %w", err)
	}

	// A single goroutine owns cmd.Wait (calling Wait twice is undefined), so we
	// learn the exit code exactly once and never race the runtime's process
	// reaping. waitDone closes when the launch exits on its own; exitCode then
	// holds its code.
	var exitCode int
	waitDone := make(chan struct{})
	go func() {
		exitCode = waitExitCode(cmd)
		close(waitDone)
	}()

	// reap tears the launch down. Killing the `aileron launch` process stops its
	// `docker run --rm` container too. This is the cross-platform analogue of the
	// bash trap (no `kill -0`, no shell trap): we call it on any early return so a
	// mid-probe failure tears the launch down, and again on the normal path. It is
	// idempotent and blocks until the single Wait goroutine has set exitCode, so
	// launchExit() is valid afterward.
	reaped := false
	reap := func() {
		if reaped {
			return
		}
		reaped = true
		_ = cmd.Process.Kill()
		<-waitDone
	}
	defer reap()
	// launchExit returns the reaped exit code; only valid after reap() (or a
	// natural exit) has closed waitDone.
	launchExit := func() int { return exitCode }

	// --- discovery poll: wait up to 120s for the container ------------------
	container := ""
	for i := 0; i < 120; i++ {
		// If the launch already exited, stop polling: a short-lived run still
		// reaches the post-run assertions below (the bash does the same).
		select {
		case <-waitDone:
			logf("launch exited before container probe window; continuing to post-run checks")
			i = 120
		default:
		}
		if i >= 120 {
			break
		}
		if name, derr := systestlib.DiscoverContainer(sbxPrefix, systestlib.SplitNames(dockerPSNames(sbxPrefix))); derr == nil {
			container = name
			break
		}
		time.Sleep(1 * time.Second)
	}

	// --- live-container R8 probes ------------------------------------------
	if container != "" {
		logf("probing running container: %s", container)
		if err := probeImage(container, cfg.ExpectedImage); err != nil {
			return err
		}
		if err := probeMCP(container, cfg); err != nil {
			return err
		}
		if err := probeCredentials(container, cfg.AuthPath); err != nil {
			return err
		}
		if err := probeDaemonReachable(container); err != nil {
			return err
		}
	} else {
		return systestlib.Fail(fmt.Sprintf("never observed a running %s* container during the launch", sbxPrefix))
	}

	// --- reap and assert the launch exit code (R8.6) -----------------------
	reap()
	if err := systestlib.ProbeExitCode(launchExit(), 0); err != nil {
		logf("launch log follows:")
		dumpFile(launchLog)
		return err
	}

	// --- R8.5 clean teardown: no sandbox container survives ----------------
	if err := systestlib.ProbeTeardown(sbxPrefix, dockerPSAllNames(sbxPrefix)); err != nil {
		return err
	}
	logf("ok: R8.5 no sandbox container survived (docker run --rm teardown)")

	// --- R9 deterministic result: read the sentinel host-side --------------
	sentinelHost := filepath.Join(e.workspace, cfg.SentinelName)
	data, err := os.ReadFile(sentinelHost)
	if err != nil {
		return systestlib.Fail(fmt.Sprintf("R9 sentinel file not found at %s (agent did not write it)", sentinelHost))
	}
	// Trim a single trailing newline the editor/agent may add, matching the
	// bash `$(cat …)` (which strips trailing newlines) so the comparison is
	// robust without accepting arbitrary trailing content.
	actual := trimOneTrailingNewline(string(data))
	if err := systestlib.AssertEq(sentinel, actual, "R9 sentinel content byte-exact"); err != nil {
		return err
	}
	logf("ok: R9 sentinel content byte-exact")

	// --- R7a forwarding: the agent acted on the forwarded exec instruction --
	if err := systestlib.AssertNotEmpty(actual, "R7a post-`--` exec args forwarded to "+cfg.Name); err != nil {
		return err
	}
	logf("ok: R7a post-`--` exec args forwarded to %s", cfg.Name)

	// --- R10 round-trip audit (conditional on AILERON_STATE_DIR) -----------
	if err := assertAuditR10(e.stateDir); err != nil {
		return err
	}

	logf("%s scenario: all assertions passed (R7a, R8.1-8.6, R9, R10)", cfg.Name)
	return nil
}

// assertAuditR10 performs the conditional R10 audit assertion: when stateDir is
// set, read today's audit JSONL and assert it has at least one event record;
// when unset, skip with a logged note (matching the bash). The audit filename is
// audit-YYYY-MM-DD.jsonl per internal/audit/local.go.
func assertAuditR10(stateDir string) error {
	if stateDir == "" {
		logf("R10 audit assertion skipped: AILERON_STATE_DIR unset (set it to the daemon state dir to enable)")
		return nil
	}
	auditFile := filepath.Join(stateDir, "audit", "audit-"+time.Now().Format("2006-01-02")+".jsonl")
	data, err := os.ReadFile(auditFile)
	if err != nil {
		return systestlib.Fail(fmt.Sprintf("R10 audit file not found: %s", auditFile))
	}
	if err := systestlib.AssertAuditHasEvents(data, auditFile); err != nil {
		return err
	}
	count, _ := systestlib.CountAuditEventRecords(data)
	logf("ok: R10 today's audit JSONL has %d event record(s) (%s)", count, auditFile)
	return nil
}

// --- live probe wrappers: gather docker facts, delegate the decision -------

func probeImage(container, expectedImage string) error {
	actualImage, err := dockerInspect(container, "{{.Config.Image}}")
	if err != nil {
		return systestlib.Fail(fmt.Sprintf("docker inspect %s failed (probe_image)", container))
	}
	running, _ := dockerInspect(container, "{{.State.Running}}")
	status, _ := dockerInspect(container, "{{.State.Status}}")
	if err := systestlib.ProbeImageRunning(expectedImage, actualImage, running, status); err != nil {
		return err
	}
	logf("ok: R8.1 container image matches %s (actual: %s)", expectedImage, actualImage)
	return nil
}

func probeMCPConfigFile(container string, cfg systestlib.AgentConfig) error {
	// Agent-agnostic R8.2 core: aileron-mcp present + executable, daemon-wiring
	// env vars all set.
	binOK := dockerExecOK(container, "test", "-x", "/usr/local/bin/aileron-mcp")
	envOK := true
	for _, v := range []string{"AILERON_URL", "AILERON_COMMS_URL", "AILERON_SESSION_ID", "AILERON_APPROVAL_URL", "AILERON_TOKEN"} {
		if !dockerExecOK(container, "printenv", v) {
			envOK = false
			break
		}
	}
	if err := systestlib.ProbeMCPRuntime(binOK, envOK); err != nil {
		return err
	}
	logf("ok: R8.2 aileron-mcp present + executable and daemon-wiring env set")

	// Config-file tail: the agent's MCP config contains the aileron block.
	config, err := dockerExecOutput(container, "cat", cfg.ConfigPath)
	if err != nil {
		return systestlib.Fail(fmt.Sprintf("R8.2 could not read %s in %s", cfg.ConfigPath, container))
	}
	if err := systestlib.ProbeMCPConfigContains(config, cfg.ConfigPath, cfg.MCPMarker); err != nil {
		return err
	}
	logf("ok: R8.2 %s contains %s", cfg.ConfigPath, cfg.MCPMarker)
	return nil
}

func probeMCPCmdline(container string, cfg systestlib.AgentConfig) error {
	// Agent-agnostic R8.2 core (same facts probeMCPConfigFile gathers):
	// aileron-mcp present + executable, daemon-wiring env vars all set.
	binOK := dockerExecOK(container, "test", "-x", "/usr/local/bin/aileron-mcp")
	envOK := true
	for _, v := range []string{"AILERON_URL", "AILERON_COMMS_URL", "AILERON_SESSION_ID", "AILERON_APPROVAL_URL", "AILERON_TOKEN"} {
		if !dockerExecOK(container, "printenv", v) {
			envOK = false
			break
		}
	}

	// Cmdline tail: the container command line carries the MCP flag whose payload
	// references the aileron server marker. Inspect BOTH .Config.Cmd and .Args so
	// the probe is robust to whichever Docker populates, matching probe_mcp_cmdline
	// in lib/probes.sh.
	cmdline, err := dockerInspect(container, "{{range .Config.Cmd}}{{.}} {{end}}{{range .Args}}{{.}} {{end}}")
	if err != nil {
		return systestlib.Fail(fmt.Sprintf("R8.2 docker inspect %s failed (probe_mcp_cmdline)", container))
	}
	if err := systestlib.ProbeMCPCmdline(binOK, envOK, cmdline, cfg.MCPFlag, cfg.MCPMarker); err != nil {
		return err
	}
	logf("ok: R8.2 container command carries %s and references %s", cfg.MCPFlag, cfg.MCPMarker)
	return nil
}

func probeCredentials(container, authPath string) error {
	authFileExists := dockerExecOK(container, "test", "-f", authPath)
	mode := ""
	if authFileExists {
		// `stat -c %a` is GNU coreutils (the Linux sandbox base); octal without a
		// leading zero, e.g. "600".
		mode, _ = dockerExecOutput(container, "stat", "-c", "%a", authPath)
	}
	mounts, _ := dockerInspect(container, `{{range .Mounts}}{{.Type}}:{{.Destination}}{{"\n"}}{{end}}`)
	authDir := filepath.Dir(authPath)
	if err := systestlib.ProbeCredentials(authFileExists, mode, authPath, authDir, mounts); err != nil {
		return err
	}
	logf("ok: R8.3 auth file %s present, mode 0600, %s bind-mounted", authPath, authDir)
	return nil
}

func probeDaemonReachable(container string) error {
	url, err := dockerExecOutput(container, "printenv", "AILERON_URL")
	if err != nil {
		return systestlib.Fail(fmt.Sprintf("R8.4 AILERON_URL not set in %s", container))
	}
	isLinux := runtime.GOOS == "linux"
	extraHosts := ""
	if isLinux {
		extraHosts, _ = dockerInspect(container, `{{range .HostConfig.ExtraHosts}}{{.}}{{"\n"}}{{end}}`)
	}
	if err := systestlib.ProbeDaemonReachable(url, isLinux, extraHosts); err != nil {
		return err
	}
	logf("ok: R8.4 daemon reachable (AILERON_URL host is host.docker.internal)")
	return nil
}

// --- small impure helpers --------------------------------------------------

// waitExitCode waits for cmd and returns its exit code, treating a kill-signal
// termination as the bash `wait` would (the explicit kill in reap is an expected
// teardown, not a launch failure, so a signal exit is reported as the underlying
// process exit). An ExitError carries the code; any other wait error maps to a
// non-zero code so the R8.6 assertion fails loudly rather than silently passing.
func waitExitCode(cmd *exec.Cmd) int {
	err := cmd.Wait()
	if err == nil {
		return 0
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode()
	}
	return -1
}

// trimOneTrailingNewline strips a single trailing "\n" (and a preceding "\r" if
// present), matching the bash `$(cat …)` trailing-newline trim without accepting
// arbitrary trailing content.
func trimOneTrailingNewline(s string) string {
	if len(s) > 0 && s[len(s)-1] == '\n' {
		s = s[:len(s)-1]
	}
	if len(s) > 0 && s[len(s)-1] == '\r' {
		s = s[:len(s)-1]
	}
	return s
}

// dumpFile copies a file's contents to stderr, used to surface the launch log on
// an R8.6 exit-code failure (the bash `cat "$LAUNCH_LOG" >&2`).
func dumpFile(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	_, _ = os.Stderr.Write(data)
}
