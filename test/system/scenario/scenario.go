package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	systestlib "github.com/ALRubinger/aileron/test/system/lib"
)

// sbxPrefix is the stable sandbox container name prefix (launcher.go
// aileron-sbx-<id>); the session id is derived at runtime, so the harness
// discovers the container by this prefix rather than predicting the full name.
const sbxPrefix = "aileron-sbx-"

// reapGracePeriod is how long reap waits for the `aileron launch` process to exit
// after a graceful SIGINT (so the launcher's `docker stop` handler can tear the
// sandbox down) before escalating to SIGKILL.
const reapGracePeriod = 10 * time.Second

// r10PollTimeout is how long the R10 assertion polls the audit file for this
// run's http_request_sent record. The daemon writes it from a background
// goroutine (handlers_comms.executeApprovedCommsHTTP), so it can lag the launch
// exit; poll rather than read once.
const r10PollTimeout = 20 * time.Second

// r10Event is the comms audit event the daemon logs for a forwarded http_request
// (internal/app/handlers_comms.go logCommsEvent("http_request_sent", ...)).
const r10Event = "http_request_sent"

// approvalPollTimeout is how long approveSessionHTTPRequest polls `aileron
// approval list` for this session's pending http_request before failing. The
// approval is registered as the agent calls the tool, so it is normally present
// by the time the launch exits; the window only covers a brief lag.
const approvalPollTimeout = 15 * time.Second

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

	// --- daemon lifecycle (R10 prerequisite) -------------------------------
	// Drive the daemon through the CLI exactly as a user would: ensure one is
	// running (`aileron daemon start` is idempotent and prints its host URL),
	// and stop it on exit ONLY if this run started it — never clobber a daemon
	// the operator already had up. `aileron launch` would auto-spawn one too,
	// but starting it here gives us the concrete host URL the agent's R10
	// http_request must target (the daemon issues that fetch from the host, so
	// the URL must be host-reachable; its own gateway is). AILERON_URL inside the
	// container is host.docker.internal-based and is NOT what the daemon can
	// fetch, and nothing expands a `${AILERON_URL}` placeholder, so we resolve a
	// real URL here and bake it into the prompt.
	daemonWasRunning := daemonRunning(e.aileronBin)
	daemonURL, err := daemonStart(e.aileronBin)
	if err != nil {
		return fmt.Errorf("starting aileron daemon for R10: %w", err)
	}
	if !daemonWasRunning {
		defer daemonStop(e.aileronBin)
	}
	healthURL := strings.TrimRight(daemonURL, "/") + "/healthz"
	logf("%s scenario: daemon at %s (started-by-test=%v)", cfg.Name, daemonURL, !daemonWasRunning)

	// --- CLI->daemon auth smoke (audit list) -------------------------------
	// Exercise an authenticated CLI->daemon call end-to-end against the live
	// daemon before the launch. `aileron audit list` hits /v1/audit, which the
	// daemon's local-auth middleware 401s unless the CLI attaches the bearer
	// token. The R10 assertion below reads the audit JSONL file directly, so it
	// never covers this auth path — which is how a regression that dropped the
	// Authorization header from the audit fetchers shipped unnoticed.
	if err := assertAuditListReachable(e.aileronBin); err != nil {
		return err
	}

	// Unlock the daemon vault before backgrounding the launch. A freshly-started
	// daemon comes up with a locked vault, and `aileron launch` needs it unlocked
	// (the agent credential is vault-backed). The launch runs in the background
	// with redirected stdio, so its own passphrase prompt cannot reach the
	// terminal; instead we touch the vault here in the FOREGROUND, where the
	// prompt reaches the operator's terminal (`aileron vault list` triggers the
	// run() vault state-machine — ensureVaultUnlocked — which prompts when locked
	// and is a no-op when already unlocked). The operator running by hand answers
	// the prompt; the launch then proceeds against an unlocked daemon.
	if err := ensureVaultUnlocked(e.aileronBin); err != nil {
		return err
	}

	prompt := systestlib.BuildPrompt(cfg, sentinel, healthURL)
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
		// Graceful stop first, mirroring the bash trap's `kill` (SIGTERM): the
		// launcher installs a SIGINT/SIGTERM handler that runs `docker stop` on
		// the `docker run --rm` sandbox, so a graceful signal is what tears the
		// container down and lets R8.5 (probe_teardown: no `aileron-sbx-*`
		// survives) hold on the mid-probe abort path. A bare SIGKILL bypasses
		// that handler and can leave a stray container. os.Interrupt maps to
		// SIGINT on Unix; on Windows it cannot be delivered to another process,
		// so Signal returns an error and we fall straight through to Kill.
		// Either way we escalate to SIGKILL if the launch has not exited within
		// the grace window, so reap never blocks indefinitely.
		if err := cmd.Process.Signal(os.Interrupt); err != nil {
			_ = cmd.Process.Kill()
		} else {
			select {
			case <-waitDone:
			case <-time.After(reapGracePeriod):
				_ = cmd.Process.Kill()
			}
		}
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
		logf("launch log follows:")
		dumpFile(launchLog)
		return systestlib.Fail(fmt.Sprintf("never observed a running %s* container during the launch (see launch log above)", sbxPrefix))
	}

	// --- wait for the launch to finish, then assert its exit code (R8.6) ----
	// Wait for the launch to exit on its own here — do NOT reap()/kill it. The
	// agent must complete its forwarded exec (write the R9 sentinel into the
	// mounted workspace and perform the R10 http_request) before we read the
	// result, exactly as bash did with `wait "$LAUNCH_PID"`. Calling reap()
	// (which signals/kills the launch) on this normal path interrupts the agent
	// mid-exec, so the sentinel is never written and R9 fails with "agent did
	// not write it". The probes above already ran against the live container;
	// reap() remains the deferred / early-return safety net for the abnormal
	// path (a mid-probe failure still tears the launch down).
	<-waitDone
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

	// --- R10 round-trip audit: this run's session produced http_request_sent --
	// The session id is the container-name suffix (aileron-sbx-<sessionID>); the
	// launcher sets it as AILERON_SESSION_ID, which the daemon records on the
	// comms audit entry, so we can assert THIS run's record rather than any
	// unrelated event in the shared daily file.
	sessionID := strings.TrimPrefix(container, sbxPrefix)

	// R10 prerequisite — approve the agent's pending http_request via the CLI.
	// http_request is an approval-gated action: the daemon parks a pending
	// approval and its executor goroutine waits (effectively forever) for a
	// decision, then performs the outbound fetch and writes the
	// http_request_sent audit record. The agent's MCP client surfaces the
	// 202-pending as a tool failure and the agent moves on, but the daemon
	// goroutine is still waiting — so approving the pending request here, exactly
	// as a user would (`aileron approval approve <id>`), unblocks the round-trip
	// and produces the audit record R10 asserts.
	if err := approveSessionHTTPRequest(e.aileronBin, sessionID); err != nil {
		logf("launch log follows:")
		dumpFile(launchLog)
		return err
	}

	if err := assertAuditR10(e.stateDir, sessionID); err != nil {
		logf("launch log follows:")
		dumpFile(launchLog)
		return err
	}

	logf("%s scenario: all assertions passed (R7a, R8.1-8.6, R9, R10)", cfg.Name)
	return nil
}

// assertAuditR10 performs the session-scoped R10 audit assertion: when stateDir
// is set, poll today's audit JSONL for a "http_request_sent" record carrying
// this run's sessionID, proving the agent's forwarded http_request reached the
// daemon and was audited. When stateDir is unset, skip with a logged note. The
// audit filename is audit-YYYY-MM-DD.jsonl per internal/audit/local.go.
//
// The daemon writes the record from a background goroutine
// (handlers_comms.executeApprovedCommsHTTP), so it can land slightly after the
// launch exits; poll up to r10PollTimeout before failing.
func assertAuditR10(stateDir, sessionID string) error {
	if stateDir == "" {
		logf("R10 audit assertion skipped: AILERON_STATE_DIR unset (set it to the daemon state dir to enable)")
		return nil
	}
	auditFile := filepath.Join(stateDir, "audit", "audit-"+time.Now().Format("2006-01-02")+".jsonl")
	deadline := time.Now().Add(r10PollTimeout)
	for {
		data, readErr := os.ReadFile(auditFile)
		if readErr == nil {
			if n, _ := systestlib.CountAuditEventsForSession(data, r10Event, sessionID); n > 0 {
				logf("ok: R10 audit has %q for session %s (%s)", r10Event, sessionID, auditFile)
				return nil
			}
		}
		if time.Now().After(deadline) {
			// Final attempt: render the faithful diagnostic (also covers a
			// never-created audit file via the read error inside the assert).
			data, _ := os.ReadFile(auditFile)
			return systestlib.AssertAuditHasEventForSession(data, r10Event, sessionID, auditFile)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// daemonRunning reports whether `aileron daemon status` says a daemon is up.
func daemonRunning(bin string) bool {
	out, _ := exec.Command(bin, "daemon", "status").CombinedOutput()
	return systestlib.DaemonStatusRunning(string(out))
}

// daemonStart runs `aileron daemon start` (idempotent — returns the existing
// daemon's URL if one is already up) and parses the host URL from its output.
func daemonStart(bin string) (string, error) {
	out, err := exec.Command(bin, "daemon", "start").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("aileron daemon start: %v: %s", err, strings.TrimSpace(string(out)))
	}
	url := systestlib.ParseDaemonStartURL(string(out))
	if url == "" {
		return "", fmt.Errorf("aileron daemon start: could not parse daemon URL from output: %q", strings.TrimSpace(string(out)))
	}
	return url, nil
}

// assertAuditListReachable runs `aileron audit list` against the live daemon
// exactly as a user would and fails if it exits non-zero. This is the
// CLI->daemon authenticated round-trip: the audit endpoint is behind the
// local-auth middleware, so a CLI that forgets the bearer token gets a 401
// ("missing or invalid daemon token"), which the fetcher surfaces as an error
// and `audit list` reports with a non-zero exit. Exit status (not a substring
// scan of the rendered audit entries, which could legitimately contain "401")
// is the reliable signal; the auth-error text is detected only to enrich the
// failure diagnostic. Empty audit output on exit 0 is a pass — we assert
// reachability + auth, not contents.
func assertAuditListReachable(bin string) error {
	out, err := exec.Command(bin, "audit", "list").CombinedOutput()
	combined := strings.TrimSpace(string(out))
	if err != nil {
		if strings.Contains(combined, "401") || strings.Contains(strings.ToLower(combined), "missing or invalid daemon token") {
			return systestlib.Fail(fmt.Sprintf("`aileron audit list` rejected by daemon auth (CLI omitted the bearer token): %v: %s", err, combined))
		}
		return systestlib.Fail(fmt.Sprintf("`aileron audit list` failed (CLI->daemon round-trip): %v: %s", err, combined))
	}
	logf("ok: `aileron audit list` reached the daemon (CLI->daemon auth)")
	return nil
}

// daemonStop runs `aileron daemon stop`, best-effort (used in a defer for a
// daemon this run started; teardown failures must not mask the test result).
func daemonStop(bin string) {
	_ = exec.Command(bin, "daemon", "stop").Run()
}

// ensureVaultUnlocked touches the daemon vault in the FOREGROUND so its
// passphrase prompt (when locked) reaches the operator's terminal, before the
// backgrounded launch needs the vault. `aileron vault list` runs through run()'s
// vault state-machine (ensureVaultUnlocked): it prompts on /dev/tty when the
// vault is locked and is a silent pass-through when already unlocked. stdout is
// discarded (the secret listing is irrelevant); stdin and stderr stay attached
// to the terminal so the prompt is visible and answerable. The launch is
// backgrounded with redirected stdio and cannot prompt, so this must happen
// here.
func ensureVaultUnlocked(bin string) error {
	logf("ensuring the Aileron daemon vault is unlocked (enter your passphrase if prompted)...")
	cmd := exec.Command(bin, "vault", "list")
	cmd.Stdin = os.Stdin
	cmd.Stdout = io.Discard
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("unlocking the daemon vault (run `%s vault list` and enter the passphrase): %w", bin, err)
	}
	return nil
}

// approveSessionHTTPRequest approves the agent's pending http_request approval
// for sessionID via the CLI, exactly as a user would. http_request is
// approval-gated; the daemon parks a pending approval whose executor goroutine
// waits (effectively forever) for a decision, so the approval is still pending
// after the launch exits and approving it here produces the http_request_sent
// audit record. It polls `aileron approval list` briefly because the approval
// registers as the agent calls the tool. A missing approval is a hard error:
// R10 requires the round-trip, so none means the agent never called the tool.
func approveSessionHTTPRequest(bin, sessionID string) error {
	deadline := time.Now().Add(approvalPollTimeout)
	for {
		out, _ := exec.Command(bin, "approval", "list").CombinedOutput()
		if id := systestlib.ParseApprovalIDForSession(string(out), sessionID); id != "" {
			// Capture combined output so an approve failure surfaces the CLI's
			// own diagnostic (e.g. "server returned …") instead of a bare exit
			// status — being blind here cost a debugging round.
			if aout, aerr := exec.Command(bin, "approval", "approve", id).CombinedOutput(); aerr != nil {
				return fmt.Errorf("approving http_request approval %s: %v: %s", id, aerr, strings.TrimSpace(string(aout)))
			}
			logf("ok: approved pending http_request %s for session %s", id, sessionID)
			return nil
		}
		if time.Now().After(deadline) {
			return systestlib.Fail(fmt.Sprintf("R10 no pending http_request approval for session %s found via `aileron approval list` (the agent did not call the http_request tool)", sessionID))
		}
		time.Sleep(500 * time.Millisecond)
	}
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
	// authPath is an in-container POSIX path; use path.Dir (forward-slash), not
	// filepath.Dir, so the parent matches the docker-inspect mount destinations on
	// a Windows host (where filepath.Dir would emit backslashes).
	authDir := path.Dir(authPath)
	isWindows := runtime.GOOS == "windows"
	if err := systestlib.ProbeCredentials(authFileExists, mode, authPath, authDir, mounts, isWindows); err != nil {
		return err
	}
	if isWindows {
		// Docker Desktop on Windows does not project the host file's 0600 mode into
		// the bind mount, so the mode is not asserted (see ProbeCredentials).
		logf("ok: R8.3 auth file %s present, %s bind-mounted (mode not host-controlled on Windows)", authPath, authDir)
	} else {
		logf("ok: R8.3 auth file %s present, mode 0600, %s bind-mounted", authPath, authDir)
	}
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
