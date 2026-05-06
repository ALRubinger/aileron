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

	"github.com/ALRubinger/aileron/internal/approval"
	"github.com/ALRubinger/aileron/internal/audit"
	"github.com/ALRubinger/aileron/internal/comms"
	"github.com/ALRubinger/aileron/internal/daemon/spawn"
	launchpolicy "github.com/ALRubinger/aileron/internal/policy/launch"
	"github.com/ALRubinger/aileron/internal/vault"
)

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

	// Action-approval queue: under Step 8, the per-launch queue still
	// powers the local approval-socket and the comms server.
	// Step 9 (#454) moves the queue to daemon ownership and routes
	// per-session via HTTP. For now the queue is process-local.
	approvalQueue := approval.NewActionApprovalQueue(nil, nil)

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
	approvalSocket := filepath.Join(os.TempDir(), "ai-"+sessionID+".sock")
	commsSocket := filepath.Join(os.TempDir(), "ai-comms-"+sessionID+".sock")

	// LLM-endpoint env (e.g. ANTHROPIC_BASE_URL for Claude Code) now
	// points at the daemon. Agents that don't override their LLM
	// endpoint via env (Pi reads a settings file) bypass this; the
	// daemon-owned MCP path still reaches them via AILERON_URL.
	agentEnv := composeAgentEnv(config.Agent.Env(), config.Agent.LLMEndpointEnv(), daemonURL)
	env := buildEnv(config.ShellShim, config.Agent.Name(), sessionID, auditStateDir, envConfig, agentEnv)
	env = append(env, "AILERON_APPROVAL_SOCKET="+approvalSocket)
	env = append(env, "AILERON_COMMS_SOCKET="+commsSocket)

	// Agent-required args come first, then user-supplied args.
	allArgs := append(config.Agent.Args(), config.Args...)

	// Register aileron-mcp as an MCP server so the agent has access to
	// read_messages, draft_reply, etc. AILERON_URL points at the daemon
	// (was the embedded gateway pre-ADR-0012); aileron-mcp routes action
	// discovery (`tools/list`) and execution (`tools/call`) there.
	//
	// AILERON_APPROVAL_URL is the user-facing approval surface — the
	// daemon's `/approvals` page. The agent embeds this in templated
	// tool descriptions so the user knows where to approve gated actions.
	selfPath, _ := os.Executable()
	if mcpBin, err := resolveSibling(selfPath, "aileron-mcp"); err == nil {
		mcpEnv := map[string]string{
			"AILERON_COMMS_SOCKET": commsSocket,
			"AILERON_URL":          daemonURL,
			"AILERON_APPROVAL_URL": daemonURL + "/approvals",
		}
		envJSON, _ := json.Marshal(mcpEnv)
		mcpConfig := fmt.Sprintf(
			`{"mcpServers":{"aileron":{"command":%q,"env":%s}}}`,
			mcpBin, string(envJSON),
		)
		allArgs = append(allArgs, "--mcp-config", mcpConfig)
	}

	cmd := exec.CommandContext(ctx, agentPath, allArgs...)
	cmd.Env = env
	if config.Dir != "" {
		cmd.Dir = config.Dir
	}

	// Create the notification queue and prepare comms config.
	queue := NewNotifyQueue(100, nil)
	wireQuietHours(config.Dir, queue)
	sessionLog, closeSessionLog := openSessionLogger(config.Dir, config.LogLevel)
	defer closeSessionLog()
	fmt.Fprintf(os.Stderr, "aileron: session log → %s\n", sessionLogPath(config.Dir))

	sessionLog.Info("session started",
		"agent", config.Agent.Name(),
		"session_id", sessionID,
		"dir", config.Dir,
		"binary", agentPath,
		"daemon_url", daemonURL,
	)

	// Comms listeners (Slack, Discord) so incoming messages still
	// land in the NotifyQueue and are readable via the
	// `read_messages` MCP tool. Step 9 (#454) moves these to daemon
	// ownership; until then they stay per-launch.
	listeners := startCommsListeners(ctx, config.Dir, queue, auditStateDir, sessionID, sessionLog)
	defer stopCommsListeners(listeners)

	// Comms server: same socket the pre-#419 launch exposed to
	// aileron-mcp for `read_messages` etc. The send / draft / http
	// paths fail-closed under the new launch (no in-pty approval
	// surface; webapp wire-through pending) — see commsserver.go for
	// the regression detail.
	commsSrv, err := NewCommsServer(commsSocket, queue, listeners, auditStateDir, sessionID, approvalQueue)
	if err != nil {
		return LaunchResult{}, fmt.Errorf("starting comms server: %w", err)
	}
	defer commsSrv.Close()
	go commsSrv.Serve()

	// Approval socket: aileron-sh dials this when a shell command
	// matches an `ask:` policy rule (issue #427). Independent of the
	// daemon — Step 9 routes this through daemon HTTP per session ID.
	approvalSrv, err := NewApprovalSocketServer(approvalSocket, approvalQueue, sessionID, config.Dir, sessionLog.With("component", "approval-socket"))
	if err != nil {
		sessionLog.Warn("approval socket disabled", "error", err)
		fmt.Fprintf(os.Stderr, "aileron: approval socket unavailable: %v\n", err)
	} else {
		defer func() { _ = approvalSrv.Close() }()
		go approvalSrv.Serve(ctx)
	}

	// Banner: probe the daemon's vault state so we can include the
	// "open the URL to unlock" hint when needed. Failure is silent —
	// the banner just omits the hint.
	probeCtx, cancelProbe := context.WithTimeout(ctx, daemonHTTPTimeout)
	locked, ok := client.LocalVaultLocked(probeCtx)
	cancelProbe()
	printStartupBanner(os.Stderr, daemonURL, sessionID, sessionLogPath(config.Dir), ok && locked)

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
		PrintSessionSummary(os.Stderr, auditStateDir, sessionID)
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

// PrintSessionSummary reads the audit log for the given session and
// prints a summary of what happened.
func PrintSessionSummary(w io.Writer, auditPath, sessionID string) {
	entries, err := audit.ReadShellEntriesFiltered(auditPath, audit.ShellFilter{
		SessionID: sessionID,
	})
	if err != nil || len(entries) == 0 {
		return
	}

	var allowed, denied, approved, userDenied int
	for _, e := range entries {
		switch e.Disposition {
		case "allow":
			allowed++
		case "deny":
			denied++
		case "ask_approved":
			approved++
		case "ask_denied":
			userDenied++
		}
	}

	fmt.Fprintf(w, "\n\033[1maileron session summary:\033[0m\n")
	if allowed > 0 {
		fmt.Fprintf(w, "  %d command(s) allowed by policy\n", allowed)
	}
	if denied > 0 {
		fmt.Fprintf(w, "  %d command(s) denied by policy\n", denied)
	}
	if approved > 0 {
		fmt.Fprintf(w, "  %d command(s) approved by user\n", approved)
	}
	if userDenied > 0 {
		fmt.Fprintf(w, "  %d command(s) denied by user\n", userDenied)
	}
}

// wireQuietHours reads the QuietHours config from the policy file and
// attaches it to the notification queue.
func wireQuietHours(dir string, queue *NotifyQueue) {
	if dir == "" {
		dir, _ = os.Getwd()
	}
	policyPath := FindPolicyFile(dir)
	if policyPath == "" {
		return
	}
	pf := loadPolicyFileFrom(policyPath)
	if pf.Notifications != nil && pf.Notifications.QuietHours != nil {
		queue.SetQuietHours(pf.Notifications.QuietHours)
	}
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

// commsSetup holds the parsed notification config from the policy file,
// ready for vault resolution and listener creation.
type commsSetup struct {
	pf         *launchpolicy.PolicyFile
	tokenRefs  []string
	needsVault bool
	sessionLog *slog.Logger
}

// prepareCommsConfig reads the notification policy and validates token
// refs. Returns a commsSetup indicating whether a vault is needed. Does
// NOT open the vault or start listeners — that is deferred until after
// the PTY is set up so we can show a Panel-based passphrase prompt.
func prepareCommsConfig(dir string, sessionLog *slog.Logger) *commsSetup {
	if dir == "" {
		dir, _ = os.Getwd()
	}
	policyPath := FindPolicyFile(dir)
	if policyPath == "" {
		sessionLog.Debug("no policy file found, skipping comms listeners")
		return nil
	}
	pf := loadPolicyFileFrom(policyPath)
	if pf.Notifications == nil {
		sessionLog.Debug("no notifications configured in policy file")
		return nil
	}

	// Validate that token fields use vault references, not plaintext.
	if cfg := pf.Notifications.Slack; cfg != nil {
		if err := ValidateTokenRef("slack.app_token", cfg.AppToken); err != nil {
			sessionLog.Warn("slack token validation failed", "error", err)
			fmt.Fprintf(os.Stderr, "aileron: %v\n", err)
			return nil
		}
		if err := ValidateTokenRef("slack.bot_token", cfg.BotToken); err != nil {
			sessionLog.Warn("slack token validation failed", "error", err)
			fmt.Fprintf(os.Stderr, "aileron: %v\n", err)
			return nil
		}
	}
	if cfg := pf.Notifications.Discord; cfg != nil {
		if err := ValidateTokenRef("discord.bot_token", cfg.BotToken); err != nil {
			sessionLog.Warn("discord token validation failed", "error", err)
			fmt.Fprintf(os.Stderr, "aileron: %v\n", err)
			return nil
		}
	}

	if cfg := pf.Notifications.Slack; cfg != nil && cfg.UserToken != "" {
		if err := ValidateTokenRef("slack.user_token", cfg.UserToken); err != nil {
			sessionLog.Warn("slack token validation failed", "error", err)
			fmt.Fprintf(os.Stderr, "aileron: %v\n", err)
			return nil
		}
	}

	var tokenRefs []string
	if cfg := pf.Notifications.Slack; cfg != nil {
		tokenRefs = append(tokenRefs, cfg.AppToken, cfg.BotToken)
		if cfg.UserToken != "" {
			tokenRefs = append(tokenRefs, cfg.UserToken)
		}
	}
	if cfg := pf.Notifications.Discord; cfg != nil {
		tokenRefs = append(tokenRefs, cfg.BotToken)
	}

	needsVault := false
	for _, ref := range tokenRefs {
		if IsVaultRef(ref) {
			needsVault = true
			break
		}
	}

	return &commsSetup{
		pf:         pf,
		tokenRefs:  tokenRefs,
		needsVault: needsVault,
		sessionLog: sessionLog,
	}
}

// startCommsWithVault resolves tokens (using the provided vault, which
// may be nil), creates listeners, and starts them.
func startCommsWithVault(ctx context.Context, setup *commsSetup, v vault.Vault, queue *NotifyQueue, auditStateDir, sessionID string) []comms.Listener {
	if setup == nil {
		return nil
	}
	sessionLog := setup.sessionLog

	resolved, err := ResolveTokens(setup.tokenRefs, v)
	if err != nil {
		sessionLog.Warn("token resolution failed", "error", err)
		fmt.Fprintf(os.Stderr, "aileron: %v\n", err)
		if strings.Contains(err.Error(), "decryption failed") {
			fmt.Fprintln(os.Stderr, "aileron: wrong vault passphrase — notifications will not be available this session")
		} else {
			fmt.Fprintln(os.Stderr, "aileron: notifications will not be available this session")
		}
		return nil
	}

	// Map resolved tokens back to config positions.
	idx := 0
	autoDraft := make(map[string]bool)
	priority := make(map[string]string)
	var created []comms.Listener
	if cfg := setup.pf.Notifications.Slack; cfg != nil && cfg.AppToken != "" && cfg.BotToken != "" {
		appToken, botToken := resolved[idx], resolved[idx+1]
		idx += 2
		var userToken string
		if cfg.UserToken != "" {
			userToken = resolved[idx]
			idx++
		}
		channels := make([]string, 0, len(cfg.Channels))
		for _, ch := range cfg.Channels {
			channels = append(channels, ch.Name)
			if ch.AutoDraft {
				autoDraft[ch.Name] = true
			}
			if ch.Priority != "" {
				priority[ch.Name] = ch.Priority
			}
		}
		sessionLog.Info("slack listener configured",
			"channels", channels,
			"ignore", cfg.Ignore,
			"user_token", userToken != "",
		)
		sl := comms.NewSlackListener(appToken, botToken, userToken, channels, cfg.Ignore, sessionLog.With("component", "slack"))
		created = append(created, sl)
	}
	if cfg := setup.pf.Notifications.Discord; cfg != nil && cfg.BotToken != "" {
		botToken := resolved[idx]
		channels := make([]string, 0, len(cfg.Channels))
		for _, ch := range cfg.Channels {
			channels = append(channels, ch.Name)
			if ch.Priority != "" {
				priority[ch.Name] = ch.Priority
			}
		}
		sessionLog.Info("discord listener configured",
			"channels", channels,
			"ignore", cfg.Ignore,
		)
		dl := comms.NewDiscordListener(botToken, channels, cfg.Ignore, sessionLog.With("component", "discord"))
		created = append(created, dl)
	}

	return StartListeners(ctx, created, queue, os.Stderr, autoDraft, priority, auditStateDir, sessionID, sessionLog)
}

// startCommsListeners is the legacy convenience wrapper that prepares
// config, opens the vault via the old tty prompt, and starts listeners.
// Used by the non-pty (direct) launch path.
func startCommsListeners(ctx context.Context, dir string, queue *NotifyQueue, auditStateDir, sessionID string, sessionLog *slog.Logger) []comms.Listener {
	setup := prepareCommsConfig(dir, sessionLog)
	if setup == nil {
		return nil
	}

	var v vault.Vault
	if setup.needsVault {
		var err error
		v, err = OpenVaultFunc(os.Stderr)
		if err != nil {
			sessionLog.Warn("vault open failed", "error", err)
			fmt.Fprintf(os.Stderr, "aileron: vault: %v\n", err)
			return nil
		}
	}

	return startCommsWithVault(ctx, setup, v, queue, auditStateDir, sessionID)
}

// StartListeners connects and starts each listener, bridging incoming
// messages to the NotifyQueue. The autoDraft map controls which channels
// trigger automatic draft replies. The priority map controls the
// priority level ("normal", "high") per channel. Returns the
// successfully started listeners. Errors are written to w.
func StartListeners(ctx context.Context, listeners []comms.Listener, queue *NotifyQueue, w io.Writer, autoDraft map[string]bool, priority map[string]string, auditStateDir, sessionID string, log *slog.Logger) []comms.Listener {
	var started []comms.Listener
	for _, l := range listeners {
		if err := l.Connect(ctx); err != nil {
			fmt.Fprintf(w, "aileron: %s connect failed: %v\n", l.Service(), err)
			log.Warn("listener connect failed", "service", l.Service(), "error", err)
			continue
		}
		msgs, err := l.Listen(ctx)
		if err != nil {
			fmt.Fprintf(w, "aileron: %s listen failed: %v\n", l.Service(), err)
			log.Warn("listener listen failed", "service", l.Service(), "error", err)
			continue
		}
		log.Info("listener started", "service", l.Service())
		started = append(started, l)
		go BridgeMessages(msgs, queue, autoDraft, priority, auditStateDir, sessionID, log)
	}
	return started
}

// BridgeMessages reads from a comms listener channel and pushes messages
// into the NotifyQueue. The autoDraft map controls which channels trigger
// automatic draft replies. The priority map sets the priority level per
// channel. Exported for testing.
func BridgeMessages(msgs <-chan comms.IncomingMessage, queue *NotifyQueue, autoDraft map[string]bool, priority map[string]string, auditStateDir, sessionID string, log *slog.Logger) {
	for msg := range msgs {
		preview := msg.Body
		if len(preview) > 80 {
			preview = preview[:77] + "..."
		}
		pri := priority[msg.Channel]
		if pri == "" {
			pri = "normal"
		}
		log.Debug("message received",
			"service", msg.Service,
			"channel", msg.Channel,
			"author", msg.Author,
			"priority", pri,
			"preview", preview,
		)
		queue.Push(Message{
			ID:        msg.ID,
			Source:    msg.Service,
			Channel:   msg.Channel,
			Author:    msg.Author,
			Preview:   preview,
			Body:      msg.Body,
			Timestamp: msg.Timestamp,
			AutoDraft: autoDraft[msg.Channel],
			Priority:  pri,
		})
		if auditStateDir != "" {
			audit.AppendMessageEntry(audit.DailyPath(auditStateDir), audit.MessageEntry{
				Timestamp: msg.Timestamp,
				SessionID: sessionID,
				Event:     "message_received",
				Service:   msg.Service,
				Channel:   msg.Channel,
				Author:    msg.Author,
				Body:      msg.Body,
			})
		}
	}
}

// stopCommsListeners shuts down all running listeners.
func stopCommsListeners(listeners []comms.Listener) {
	for _, l := range listeners {
		l.Close()
	}
}

// sessionLogPath returns the path to the session log file,
// located alongside the audit log in .aileron/.
func sessionLogPath(dir string) string {
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
	path := sessionLogPath(dir)
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
