package launch

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/ALRubinger/aileron/internal/daemon/spawn"
)

// MCPServerName is the name Aileron registers itself under in agents'
// MCP-server configs. The agent surfaces Aileron's tools as
// `mcp__<MCPServerName>__<tool-name>`. Agent definitions reference this
// when they wire host-level allowlists (e.g. Claude Code's
// `--allowedTools mcp__aileron`) so the host CLI does not double-prompt
// for tools the daemon already gates (ADR-0009 / ADR-0010).
const MCPServerName = "aileron"

// LaunchConfig holds the configuration for launching an agent.
type LaunchConfig struct {
	// Agent is the agent to launch.
	Agent Agent
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

// ResolveBinary searches PATH for the first matching binary name from
// the given candidates. Returns the absolute path or an error.
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

// resolveMCPBinary locates aileron-mcp. The MCP server is a required
// runtime dependency of `aileron launch` per ADR-0015 — without it the
// agent has no path to Aileron's tools, the vault, or the action-
// approval surface. Missing aileron-mcp is a hard error with a
// remediation hint, not a silent skip.
//
// Lookup order: next to the running aileron binary first (the shape
// the Homebrew cask, deb/rpm/apk packages, and `task build:cli + task
// build:mcp` all produce), then $PATH (covers `go install` users who
// installed only the CLI or unusual layouts).
func resolveMCPBinary(selfPath string) (string, error) {
	mcpBin, err := resolveSibling(selfPath, "aileron-mcp")
	if err == nil {
		return mcpBin, nil
	}
	siblingDir := "<unknown>"
	if selfPath != "" {
		siblingDir = filepath.Dir(selfPath)
	}
	return "", fmt.Errorf(
		"aileron-mcp not found next to %s/aileron or on PATH; reinstall aileron from the official packages, or build and place aileron-mcp alongside aileron (task build:mcp; cp build/aileron-mcp %s/)",
		siblingDir, siblingDir,
	)
}

// Launch starts the agent as a child process under Aileron's daemon.
//
// Per ADR-0015 the launcher is the daemon-connection + MCP-registration
// + gateway-routing step. It does not replace $SHELL, install wrapper
// scripts, or audit shell commands the agent runs locally.
//
// Per ADR-0012 launch is a thin client of the user-scoped local daemon.
// It resolves (and auto-spawns) the daemon URL, registers the session,
// asks the agent to wire up aileron-mcp (Claude/Pi pass `--mcp-config`;
// Codex/Goose/OpenCode write config files), points the agent's
// LLM-endpoint env at the daemon if applicable, then execs the agent.
func Launch(ctx context.Context, config LaunchConfig) (LaunchResult, error) {
	stateDir, err := resolveStateDir()
	if err != nil {
		return LaunchResult{}, fmt.Errorf("state dir: %w", err)
	}
	opts := spawn.Options{StateDir: stateDir}
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

	agentPath, err := ResolveBinary(config.Agent.BinaryNames())
	if err != nil {
		return LaunchResult{}, fmt.Errorf("agent %q: %w", config.Agent.Name(), err)
	}

	// Resolve aileron-mcp up front. Per ADR-0015 the MCP server is the
	// runtime path for every Aileron tool the agent calls; running
	// without it isn't running. Resolve before session registration so
	// a missing binary fails before the daemon has anything to clean up.
	selfPath, _ := os.Executable()
	mcpBin, err := resolveMCPBinary(selfPath)
	if err != nil {
		return LaunchResult{}, err
	}

	regCtx, cancelReg := context.WithTimeout(ctx, daemonHTTPTimeout)
	sessionID, err := client.RegisterSession(regCtx, config.Agent.Name(), config.Dir)
	cancelReg()
	if err != nil {
		return LaunchResult{}, fmt.Errorf("register session: %w", err)
	}

	agentEnv := composeAgentEnv(config.Agent.Env(), config.Agent.LLMEndpointEnv(), daemonURL)
	env := buildEnv(agentEnv)

	allArgs := append(config.Agent.Args(), config.Args...)

	// MCP registration. The agent decides the wire shape: CLI flag
	// (Claude, Pi) or config-file (Codex, Goose, OpenCode). The env
	// passed here ends up inside the MCP server process so it can
	// reach the daemon's session-scoped surfaces.
	mcpEnv := map[string]string{
		"AILERON_URL":          daemonURL,
		"AILERON_COMMS_URL":    daemonURL,
		"AILERON_SESSION_ID":   sessionID,
		"AILERON_APPROVAL_URL": daemonURL + "/approvals",
	}
	extraArgs, mcpErr := config.Agent.ConfigureMCP(mcpBin, mcpEnv, config.Dir)
	if mcpErr != nil {
		return LaunchResult{}, fmt.Errorf("configuring MCP for %s: %w", config.Agent.Name(), mcpErr)
	}
	allArgs = append(allArgs, extraArgs...)

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
		"daemon_log", filepath.Join(stateDir, "daemon.log"),
	)

	probeCtx, cancelProbe := context.WithTimeout(ctx, daemonHTTPTimeout)
	locked, ok := client.LocalVaultLocked(probeCtx)
	cancelProbe()
	printStartupBanner(os.Stderr, daemonURL, sessionID, SessionLogPath(config.Dir), ok && locked)

	result, runErr := launchDirect(cmd, config)

	endCtx, cancelEnd := context.WithTimeout(context.Background(), daemonHTTPTimeout)
	exit := result.ExitCode
	if endErr := client.EndSession(endCtx, sessionID, &exit); endErr != nil {
		sessionLog.Warn("end session", "error", endErr)
	}
	cancelEnd()

	sessionLog.Info("session ended", "exit_code", result.ExitCode)
	return result, runErr
}

// composeAgentEnv merges the agent's env map with the LLM-endpoint
// override (e.g. ANTHROPIC_BASE_URL → daemonURL). Empty endpointEnv or
// empty url skips the override entirely.
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
// publishes its discovery files into.
func resolveStateDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".aileron"), nil
}

// resolveDaemonBinary locates the daemon binary — a sibling of the
// running aileron binary named "server" — and falls back to PATH.
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

// printStartupBanner writes a single line on stderr before exec'ing the
// agent, naming the daemon URL and session id. When vaultLocked is
// true, a follow-up line points the user at the unlock surface.
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

// buildEnv creates the environment for the child process: the parent
// environment plus the agent's extra env vars. Per ADR-0015 no shim env
// vars (SHELL, AILERON_REAL_SHELL, AILERON_AGENT, etc.) are injected.
func buildEnv(agentEnv map[string]string) []string {
	managed := make(map[string]bool, len(agentEnv))
	for k := range agentEnv {
		managed[k] = true
	}
	env := os.Environ()
	filtered := make([]string, 0, len(env)+len(agentEnv))
	for _, e := range env {
		eqIdx := indexByte(e, '=')
		if eqIdx < 0 {
			filtered = append(filtered, e)
			continue
		}
		if managed[e[:eqIdx]] {
			continue
		}
		filtered = append(filtered, e)
	}
	for k, v := range agentEnv {
		filtered = append(filtered, k+"="+v)
	}
	return filtered
}

// indexByte is strings.IndexByte without the import.
func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

// SessionLogPath returns `<dir>/.aileron/session.log`. The session log
// captures the launched agent's stdio and is per-project. Distinct from
// the daemon's user-scoped audit store (ADR-0010), which records
// actions Aileron executes — not the agent's local stdio.
//
// Exported so `aileron sessions watch <id>` can resolve the same path
// the launcher writes to.
func SessionLogPath(dir string) string {
	if dir == "" {
		dir, _ = os.Getwd()
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
