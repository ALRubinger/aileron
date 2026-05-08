package launch

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/ALRubinger/aileron/internal/audit"
	"github.com/ALRubinger/aileron/internal/daemon/spawn"
	launchpolicy "github.com/ALRubinger/aileron/internal/policy/launch"
)

// MCPServerName is the name Aileron registers itself under in
// agents' MCP-server configs. The agent surfaces Aileron's tools as
// `mcp__<MCPServerName>__<tool-name>`. Agent definitions reference
// this when they wire host-level allowlists (e.g. Claude Code's
// `--allowedTools mcp__aileron`) so Aileron remains the trust surface
// per ADR-0009/0010 and the host doesn't double-prompt for tools the
// daemon already gates.
const MCPServerName = "aileron"

// LaunchConfig holds the configuration for launching an agent.
type LaunchConfig struct {
	// Agent is the agent to launch.
	Agent Agent
	// ShellShim is the absolute path to the aileron-sh binary.
	ShellShim string
	// Args are extra arguments to pass to the agent binary.
	Args []string
	// Dir is the working directory for the agent. If empty, the current
	// directory is used.
	Dir string
	// LogLevel sets the session log verbosity (e.g. slog.LevelDebug).
	LogLevel slog.Level
}

// LaunchResult holds the outcome of a launched agent process.
type LaunchResult struct {
	ExitCode int
}

// ResolveBinary searches PATH for the first matching binary name from the
// given candidates. Returns the absolute path or an error.
func ResolveBinary(names []string) (string, error) {
	for _, name := range names {
		path, err := exec.LookPath(name)
		if err == nil {
			return path, nil
		}
	}
	return "", fmt.Errorf("could not find any of %v on PATH", names)
}

// resolveSibling looks for a binary next to the given executable path,
// then falls back to searching PATH.
func resolveSibling(selfPath, name string) (string, error) {
	dir := filepath.Dir(selfPath)
	candidate := filepath.Join(dir, name)
	if _, err := os.Stat(candidate); err == nil {
		return filepath.Abs(candidate)
	}
	return exec.LookPath(name)
}

// ResolveShim looks for the aileron-sh binary next to the given executable
// path, then falls back to searching PATH.
func ResolveShim(selfPath string) (string, error) {
	dir := filepath.Dir(selfPath)
	candidate := filepath.Join(dir, "aileron-sh")
	if _, err := os.Stat(candidate); err == nil {
		abs, err := filepath.Abs(candidate)
		if err != nil {
			return "", fmt.Errorf("resolving shim path: %w", err)
		}
		return abs, nil
	}
	path, err := exec.LookPath("aileron-sh")
	if err != nil {
		return "", fmt.Errorf("aileron-sh not found next to %s or on PATH", selfPath)
	}
	return path, nil
}

// Launch starts the agent as a child process with the modified environment.
//
// Under ADR-0012 launch is a thin client of the user-scoped local
// daemon: it resolves (and auto-spawns if needed) the daemon URL,
// registers the session, sets the agent's LLM-endpoint env to point
// at the daemon, runs the agent, and ends the session on exit. The
// daemon owns the gateway, the vault, and the action-approval surface.
func Launch(ctx context.Context, config LaunchConfig) (LaunchResult, error) {
	// Resolve (or auto-spawn) the daemon. spawn.Resolve honors
	// AILERON_API_URL for tests/dev and falls back to fork-execing the
	// daemon binary; the URL is stable across launches in a daemon's
	// lifetime, so the agent's ANTHROPIC_BASE_URL doesn't churn.
	stateDir, err := resolveStateDir()
	if err != nil {
		return LaunchResult{}, fmt.Errorf("state dir: %w", err)
	}
	opts := spawn.Options{StateDir: stateDir}
	// Binary lookup is only needed when spawn would actually have to
	// fork-exec the daemon. AILERON_API_URL bypasses spawn entirely
	// (test/dev), so skip the binary check there — otherwise tests
	// without a `server` binary on PATH would fail to even start.
	if os.Getenv("AILERON_API_URL") == "" {
		binary, err := resolveDaemonBinary()
		if err != nil {
			return LaunchResult{}, fmt.Errorf("locate daemon binary: %w", err)
		}
		opts.Binary = binary
	}
	resolveCtx, cancelResolve := context.WithTimeout(ctx, 10*time.Second)
	daemonURL, err := spawn.Resolve(resolveCtx, opts)
	cancelResolve()
	if err != nil {
		return LaunchResult{}, fmt.Errorf("daemon: %w", err)
	}
	daemonURL = trimTrailingSlash(daemonURL)
	client := newDaemonClient(daemonURL)

	// Register the session: the daemon mints the ULID and stamps
	// StartedAt. Pass the working directory so `aileron sessions list`
	// can render it.
	regCtx, cancelReg := context.WithTimeout(ctx, daemonHTTPTimeout)
	sessionID, err := client.RegisterSession(regCtx, config.Agent.Name(), config.Dir)
	cancelReg()
	if err != nil {
		return LaunchResult{}, fmt.Errorf("register session: %w", err)
	}

	agentPath, err := ResolveBinary(config.Agent.BinaryNames())
	if err != nil {
		return LaunchResult{}, fmt.Errorf("agent %q: %w", config.Agent.Name(), err)
	}

	// Let the agent perform any agent-specific shell configuration.
	// Claude installs a wrapper script; Pi writes .pi/settings.json.
	if err := config.Agent.ConfigureShell(config.ShellShim, config.Dir); err != nil {
		return LaunchResult{}, fmt.Errorf("configuring shell for %s: %w", config.Agent.Name(), err)
	}

	auditStateDir := resolveAuditStateDir()
	envConfig := loadEnvConfig(config.Dir)

	// LLM-endpoint env (e.g. ANTHROPIC_BASE_URL for Claude Code) now
	// points at the daemon. Agents that don't override their LLM
	// endpoint via env (Pi reads a settings file) bypass this; the
	// daemon-owned MCP path still reaches them via AILERON_URL.
	agentEnv := composeAgentEnv(config.Agent.Env(), config.Agent.LLMEndpointEnv(), daemonURL)
	env := buildEnv(config.ShellShim, config.Agent.Name(), sessionID, auditStateDir, envConfig, agentEnv)
	// AILERON_APPROVAL_URL + AILERON_SESSION_ID are what aileron-sh
	// uses to POST shell-approval requests to the daemon (Step 9A of
	// #454, replacing the pre-9A unix socket).
	env = append(env, "AILERON_APPROVAL_URL="+daemonURL)

	// Agent-required args come first, then user-supplied args.
	allArgs := append(config.Agent.Args(), config.Args...)

	// Register aileron-mcp as an MCP server so the agent has access to
	// read_messages, draft_reply, etc. AILERON_URL points at the daemon
	// (was the embedded gateway pre-ADR-0012); aileron-mcp routes action
	// discovery (`tools/list`) and execution (`tools/call`) there.
	//
	// AILERON_COMMS_URL + AILERON_SESSION_ID are how aileron-mcp's
	// comms tools (`read_messages`, `send_message`, `draft_reply`,
	// `http_request`) reach the daemon-owned comms surface — Step
	// 9B-2 of #454 replaced the pre-9B per-session unix socket
	// (/tmp/ai-comms-{sessionID}.sock) with HTTP long-poll handlers
	// matching the 9A shell-approval pattern.
	//
	// AILERON_APPROVAL_URL is the user-facing approval surface — the
	// daemon's `/approvals` page. The agent embeds this in templated
	// tool descriptions so the user knows where to approve gated actions.
	selfPath, _ := os.Executable()
	if mcpBin, err := resolveSibling(selfPath, "aileron-mcp"); err == nil {
		mcpEnv := map[string]string{
			"AILERON_URL":          daemonURL,
			"AILERON_COMMS_URL":    daemonURL,
			"AILERON_SESSION_ID":   sessionID,
			"AILERON_APPROVAL_URL": daemonURL + "/approvals",
		}
		envJSON, _ := json.Marshal(mcpEnv)
		mcpConfig := fmt.Sprintf(
			`{"mcpServers":{%q:{"command":%q,"env":%s}}}`,
			MCPServerName, mcpBin, string(envJSON),
		)
		allArgs = append(allArgs, "--mcp-config", mcpConfig)
	}

	cmd := exec.CommandContext(ctx, agentPath, allArgs...)
	cmd.Env = env
	if config.Dir != "" {
		cmd.Dir = config.Dir
	}

	sessionLog, closeSessionLog := openSessionLogger(config.Dir, config.LogLevel)
	defer closeSessionLog()
	fmt.Fprintf(os.Stderr, "aileron: session log → %s\n", SessionLogPath(config.Dir))

	sessionLog.Info("session started",
		"agent", config.Agent.Name(),
		"session_id", sessionID,
		"dir", config.Dir,
		"binary", agentPath,
		"daemon_url", daemonURL,
		"audit_dir", auditStateDir,
		"daemon_log", filepath.Join(stateDir, "daemon.log"),
	)

	// Banner: probe the daemon's vault state so we can include the
	// "open the URL to unlock" hint when needed. Failure is silent —
	// the banner just omits the hint.
	probeCtx, cancelProbe := context.WithTimeout(ctx, daemonHTTPTimeout)
	locked, ok := client.LocalVaultLocked(probeCtx)
	cancelProbe()
	printStartupBanner(os.Stderr, daemonURL, sessionID, SessionLogPath(config.Dir), ok && locked)

	result, runErr := launchDirect(cmd, config)

	// End the session: stamp EndedAt + ExitCode on the daemon's record.
	// Best-effort — failure here logs but doesn't override the agent's
	// exit code. The orphan-reaper handles the worst case (daemon dies
	// while we're mid-launch).
	endCtx, cancelEnd := context.WithTimeout(context.Background(), daemonHTTPTimeout)
	exit := result.ExitCode
	if endErr := client.EndSession(endCtx, sessionID, &exit); endErr != nil {
		sessionLog.Warn("end session", "error", endErr)
	}
	cancelEnd()

	sessionLog.Info("session ended", "exit_code", result.ExitCode)
	if auditStateDir != "" {
		// audit-*.jsonl files live in <auditStateDir>/audit/, not at
		// the top level — pass the subdir so ReadShellEntriesFiltered
		// finds the daily-rotated files.
		auditDir := audit.DailyDir(auditStateDir)
		shellSummary := summarizeSessionShell(auditDir, sessionID)
		logSessionShellSummary(sessionLog, shellSummary)
		PrintSessionSummary(os.Stderr, auditDir, sessionID)
	}
	return result, runErr
}

// composeAgentEnv merges the agent's env map with the LLM-endpoint
// override (e.g. ANTHROPIC_BASE_URL → daemonURL). Empty endpointEnv
// or empty url skips the override entirely.
func composeAgentEnv(agentEnv map[string]string, endpointEnv, url string) map[string]string {
	merged := make(map[string]string, len(agentEnv)+1)
	for k, v := range agentEnv {
		merged[k] = v
	}
	if endpointEnv != "" && url != "" {
		merged[endpointEnv] = url
	}
	return merged
}

// resolveStateDir returns ~/.aileron, the state directory the daemon
// publishes its discovery files into. Launch passes this to
// spawn.Resolve and to the audit log writers.
func resolveStateDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".aileron"), nil
}

// resolveDaemonBinary locates the daemon binary — a sibling of the
// running aileron binary named "server" — and falls back to PATH.
// Mirrors the helper in cmd/aileron; duplicated to keep launch
// self-contained without a circular import on the cmd module.
func resolveDaemonBinary() (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	candidate := filepath.Join(filepath.Dir(self), "server")
	if _, err := os.Stat(candidate); err == nil {
		return filepath.Abs(candidate)
	}
	return exec.LookPath("server")
}

// printStartupBanner writes a single line on stderr before exec'ing
// the agent, naming the daemon URL and session id. Replaces the in-
// pty StatusBar from the pre-#419 launch path. Output is fenced so
// agents that don't render ANSI gracefully (or terminals that wrap
// long lines) still parse the URL cleanly.
//
// daemonURL is the URL spawn.Resolve handed back — stable across
// every launch in the daemon's lifetime, so users can bookmark it.
// When vaultLocked is true, a follow-up line points the user at the
// unlock surface — vault-needing tool calls fail with 423 until the
// user types their passphrase into the webapp modal.
func printStartupBanner(w io.Writer, daemonURL, sessionID, logPath string, vaultLocked bool) {
	if daemonURL == "" {
		fmt.Fprintf(w, "✈️  Aileron — session %s — log %s\n", sessionID, logPath)
		return
	}
	fmt.Fprintf(w, "✈️  Aileron — webapp %s — session %s — log %s\n", daemonURL, sessionID, logPath)
	if vaultLocked {
		fmt.Fprintf(w, "✈️  Vault locked — open %s and enter your passphrase to unlock.\n", daemonURL)
	}
}

// launchDirect runs the agent with direct stdin/stdout/stderr passthrough.
// Used when stdin is not a terminal (piped input, CI, etc.).
func launchDirect(cmd *exec.Cmd, config LaunchConfig) (LaunchResult, error) {
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return LaunchResult{}, fmt.Errorf("failed to start %s: %w", config.Agent.Name(), err)
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		for sig := range sigCh {
			_ = cmd.Process.Signal(sig)
		}
	}()

	err := cmd.Wait()
	signal.Stop(sigCh)
	close(sigCh)

	return exitResult(err)
}


// exitResult extracts an exit code from a cmd.Wait error.
func exitResult(err error) (LaunchResult, error) {
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return LaunchResult{ExitCode: exitErr.ExitCode()}, nil
		}
		return LaunchResult{}, err
	}
	return LaunchResult{ExitCode: 0}, nil
}

// buildEnv creates the environment for the child process:
//   - Copies the current environment
//   - Replaces SHELL with the shim path
//   - Sets AILERON_REAL_SHELL to the original SHELL value
//   - Merges any agent-specific env vars (including agent-specific vars like CLAUDE_CODE_SHELL)
func buildEnv(shimPath, agentName, sessionID, auditStateDir string, envConfig *launchpolicy.EnvConfig, agentEnv map[string]string) []string {
	origShell := os.Getenv("SHELL")
	if origShell == "" {
		origShell = "/bin/sh"
	}

	// Agent can override AILERON_REAL_SHELL (e.g. Claude Code needs /bin/bash
	// because its command templates use bash builtins like shopt).
	realShell := origShell
	if v, ok := agentEnv["AILERON_REAL_SHELL"]; ok {
		realShell = v
	}

	// Build a set of keys managed by buildEnv + agent overrides so we can
	// strip them from the inherited environment in a single pass.
	managed := map[string]bool{
		"SHELL":              true,
		"AILERON_REAL_SHELL": true,
		"AILERON_AGENT":      true,
		"AILERON_SESSION_ID": true,
		"AILERON_AUDIT_DIR":  true,
	}
	for k := range agentEnv {
		managed[k] = true
	}

	env := os.Environ()
	filtered := make([]string, 0, len(env)+len(agentEnv)+3)
	for _, e := range env {
		eqIdx := strings.IndexByte(e, '=')
		if eqIdx < 0 {
			filtered = append(filtered, e)
			continue
		}
		key := e[:eqIdx]
		if managed[key] {
			continue
		}
		if shouldScrub(key, envConfig) {
			continue
		}
		filtered = append(filtered, e)
	}

	filtered = append(filtered, "SHELL="+shimPath)
	filtered = append(filtered, "AILERON_REAL_SHELL="+realShell)
	filtered = append(filtered, "AILERON_AGENT="+agentName)
	filtered = append(filtered, "AILERON_SESSION_ID="+sessionID)
	if auditStateDir != "" {
		filtered = append(filtered, "AILERON_AUDIT_DIR="+auditStateDir)
	}
	for k, v := range agentEnv {
		if k == "AILERON_REAL_SHELL" {
			continue // already handled above
		}
		filtered = append(filtered, k+"="+v)
	}

	return filtered
}

// ResolveAuditLogFromCwd returns the audit subdirectory the
// daily-rotated JSONL files live in (`~/.aileron/audit`). Pre-ADR-0012
// this resolved per-project under the working directory and honored an
// `aileron.yaml` `Settings.AuditLog` override; ADR-0012 centralizes
// audit at user scope so all callers get the same path.
//
// The function name is kept for callers (`aileron policy save` etc.)
// that read the audit log via the file system; under ADR-0012 it is
// effectively a constant. Read helpers like
// [audit.ReadShellEntriesFiltered] accept a directory and scan every
// `audit-*.jsonl` file inside.
func ResolveAuditLogFromCwd() string {
	return audit.DailyDir(resolveAuditStateDir())
}

// resolveAuditStateDir returns `~/.aileron`. The audit JSONL files
// themselves live under `audit/audit-YYYY-MM-DD.jsonl` inside.
func resolveAuditStateDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".aileron"
	}
	return filepath.Join(home, ".aileron")
}

// SessionShellSummary is the aggregate count of shell-policy
// dispositions emitted by aileron-sh for one launch session. Empty
// (zero counts, Total == 0) means either the session ran no shell
// commands or its audit entries are unreadable — the difference does
// not matter to the operator opening the session log.
type SessionShellSummary struct {
	Total      int
	Allowed    int // policy: allow
	Denied     int // policy: deny
	Approved   int // ask, user approved
	UserDenied int // ask, user denied
}

// summarizeSessionShell reads the audit log entries for sessionID and
// returns aggregate counts. Errors and missing files are flattened to
// a zero summary — this powers operator-visible output where partial
// data (or no data) is the normal case for early-exit launches.
func summarizeSessionShell(auditPath, sessionID string) SessionShellSummary {
	entries, err := audit.ReadShellEntriesFiltered(auditPath, audit.ShellFilter{
		SessionID: sessionID,
	})
	if err != nil || len(entries) == 0 {
		return SessionShellSummary{}
	}
	var s SessionShellSummary
	for _, e := range entries {
		s.Total++
		switch e.Disposition {
		case "allow":
			s.Allowed++
		case "deny":
			s.Denied++
		case "ask_approved":
			s.Approved++
		case "ask_denied":
			s.UserDenied++
		}
	}
	return s
}

// PrintSessionSummary reads the audit log for the given session and
// prints a colorized summary of what happened. Used at end-of-launch
// to give the operator a one-glance receipt on stderr; the structured
// equivalent lands in the session log via [logSessionShellSummary].
func PrintSessionSummary(w io.Writer, auditPath, sessionID string) {
	s := summarizeSessionShell(auditPath, sessionID)
	if s.Total == 0 {
		return
	}
	fmt.Fprintf(w, "\n\033[1maileron session summary:\033[0m\n")
	if s.Allowed > 0 {
		fmt.Fprintf(w, "  %d command(s) allowed by policy\n", s.Allowed)
	}
	if s.Denied > 0 {
		fmt.Fprintf(w, "  %d command(s) denied by policy\n", s.Denied)
	}
	if s.Approved > 0 {
		fmt.Fprintf(w, "  %d command(s) approved by user\n", s.Approved)
	}
	if s.UserDenied > 0 {
		fmt.Fprintf(w, "  %d command(s) denied by user\n", s.UserDenied)
	}
}

// logSessionShellSummary writes the structured shell-summary as a
// single slog event so it lands in the per-session log alongside
// "session started" / "session ended" — the operator's first stop
// when a launch goes wrong. Always emits an event (including when
// Total == 0) so the absence of activity is itself recorded.
func logSessionShellSummary(log *slog.Logger, s SessionShellSummary) {
	log.Info("session shell summary",
		"total", s.Total,
		"allowed", s.Allowed,
		"denied", s.Denied,
		"approved", s.Approved,
		"user_denied", s.UserDenied,
	)
}

// loadEnvConfig loads the merged policy's EnvConfig for env scrubbing.
func loadEnvConfig(dir string) *launchpolicy.EnvConfig {
	if dir == "" {
		dir, _ = os.Getwd()
	}
	policyPath := FindPolicyFile(dir)
	if policyPath == "" {
		return nil
	}
	pf := loadPolicyFileFrom(policyPath)
	return pf.Env
}

// shouldScrub returns true if the env var key matches a scrub pattern
// and is not in the passthrough list.
func shouldScrub(key string, cfg *launchpolicy.EnvConfig) bool {
	if cfg == nil {
		return false
	}
	// Passthrough beats scrub.
	for _, p := range cfg.Passthrough {
		if EnvGlobMatch(p, key) {
			return false
		}
	}
	for _, s := range cfg.Scrub {
		if EnvGlobMatch(s, key) {
			return true
		}
	}
	return false
}

// EnvGlobMatch matches an env var name against a pattern with * wildcards.
// Supports prefix (AWS_*), suffix (*_SECRET), contains (*TOKEN*), wildcard
// (*), and exact match.
func EnvGlobMatch(pattern, name string) bool {
	if pattern == "*" {
		return true
	}
	if strings.HasPrefix(pattern, "*") && strings.HasSuffix(pattern, "*") {
		return strings.Contains(name, pattern[1:len(pattern)-1])
	}
	if strings.HasPrefix(pattern, "*") {
		return strings.HasSuffix(name, pattern[1:])
	}
	if strings.HasSuffix(pattern, "*") {
		return strings.HasPrefix(name, pattern[:len(pattern)-1])
	}
	return pattern == name
}

// SessionLogPath returns the per-project session log path: the
// .aileron/session.log next to the policy file (walking up from dir if
// necessary) or, if no policy file exists, .aileron/session.log under
// dir itself. The session log captures the launched agent's stdio and
// is intentionally project-scoped — distinct from the audit log, which
// is user-scoped at ~/.aileron/audit/.
//
// Exported so `aileron sessions watch <id>` can resolve the same path
// the launcher writes to, without re-implementing the policy-file
// walk.
func SessionLogPath(dir string) string {
	if dir == "" {
		dir, _ = os.Getwd()
	}
	policyPath := FindPolicyFile(dir)
	if policyPath != "" {
		return filepath.Join(filepath.Dir(policyPath), ".aileron", "session.log")
	}
	return filepath.Join(dir, ".aileron", "session.log")
}

// openSessionLogger creates an slog.Logger that writes to
// .aileron/session.log at the given level. Returns the logger and a
// cleanup function to close the file. If the file cannot be created,
// returns a discard logger so callers never receive nil.
func openSessionLogger(dir string, level slog.Level) (*slog.Logger, func()) {
	path := SessionLogPath(dir)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return slog.New(slog.NewTextHandler(io.Discard, nil)), func() {}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return slog.New(slog.NewTextHandler(io.Discard, nil)), func() {}
	}
	logger := slog.New(slog.NewTextHandler(f, &slog.HandlerOptions{
		Level: level,
	}))
	return logger, func() { f.Close() }
}
