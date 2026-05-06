package launch

import (
	"context"
	"crypto/rand"
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

	"github.com/ALRubinger/aileron/internal/approval"
	"github.com/ALRubinger/aileron/internal/audit"
	"github.com/ALRubinger/aileron/internal/comms"
	launchpolicy "github.com/ALRubinger/aileron/internal/policy/launch"
	"github.com/ALRubinger/aileron/internal/vault"
	"golang.org/x/term"
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
// When stdin is a terminal, the agent runs inside a pty with a status bar
// rendered in the bottom 2 rows. Otherwise, it falls back to direct I/O.
func Launch(ctx context.Context, config LaunchConfig) (LaunchResult, error) {
	// Per ADR-0011, the daemon needs the local credential vault for
	// credential resolution at action-execution time. Two prompt
	// surfaces are supported:
	//
	//   - "webapp" (default under launch, #429): the daemon starts
	//     vault-locked; the user opens the webapp URL printed in the
	//     startup banner and submits the passphrase via the modal.
	//     Vault-needing endpoints return 423 until the webapp POSTs
	//     /v1/vault/unlock.
	//
	//   - "stderr" (legacy / fallback): EnsureVault prompts on stderr
	//     before the agent runs. Selected automatically when the
	//     vault file is missing (the webapp modal does not yet handle
	//     first-launch creation), or when AILERON_VAULT_PROMPT=stderr.
	//
	// The mode selection happens before StartGateway so the gateway
	// is built with the correct (locked or pre-unlocked) vault state.
	mode, unlockedVault, err := resolveVaultPromptMode(DefaultVaultPath(), os.Getenv("AILERON_VAULT_PROMPT"), term.IsTerminal(int(os.Stdin.Fd())))
	if err != nil {
		return LaunchResult{}, fmt.Errorf("vault: %w", err)
	}

	// Construct the action-approval queue once and share it across
	// the embedded gateway and the CommsServer. Both register
	// pending entries here; the webapp's `/approvals` page renders a
	// single SSE stream for all kinds (action / comms-send /
	// comms-draft / http-request) — see #428.
	approvalQueue := approval.NewActionApprovalQueue(nil, nil)

	// Start the embedded Aileron gateway when the agent supports
	// LLM-endpoint override via env var. The gateway either shares
	// the user's just-unlocked vault (stderr mode) or runs in the
	// vault-locked-pending-webapp-unlock mode (webapp mode).
	// Agents whose endpoint is configured via a settings file
	// (see [Pi.LLMEndpointEnv]) bypass this — adding gateway
	// support for those agents requires extending [Agent.ConfigureShell].
	var gateway *Gateway
	if config.Agent.LLMEndpointEnv() != "" {
		gw, err := StartGateway(ctx, gatewayConfig{
			Vault:           unlockedVault,
			LocalVaultPath:  vaultPathForGateway(mode),
			ActionApprovals: approvalQueue,
			Log:             slog.Default(),
		})
		if err != nil {
			return LaunchResult{}, fmt.Errorf("starting gateway: %w", err)
		}
		gateway = gw
		defer func() { _ = gateway.Close(context.Background()) }()
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

	sessionID := generateSessionID()
	auditStateDir := resolveAuditStateDir()
	envConfig := loadEnvConfig(config.Dir)
	approvalSocket := filepath.Join(os.TempDir(), "ai-"+sessionID+".sock")
	commsSocket := filepath.Join(os.TempDir(), "ai-comms-"+sessionID+".sock")

	agentEnv := composeAgentEnv(config.Agent.Env(), config.Agent.LLMEndpointEnv(), gatewayURL(gateway))
	env := buildEnv(config.ShellShim, config.Agent.Name(), sessionID, auditStateDir, envConfig, agentEnv)
	env = append(env, "AILERON_APPROVAL_SOCKET="+approvalSocket)
	env = append(env, "AILERON_COMMS_SOCKET="+commsSocket)

	// Agent-required args come first, then user-supplied args.
	allArgs := append(config.Agent.Args(), config.Args...)

	// Register aileron-mcp as an MCP server so the agent has access to
	// read_messages, draft_reply, etc. — and, when the embedded gateway
	// is up, AILERON_URL so aileron-mcp can route action discovery
	// (`tools/list`) and execution (`tools/call`) to the same daemon
	// the agent's LLM calls flow through.
	//
	// AILERON_APPROVAL_URL is the user-facing approval surface. Until the
	// webapp ships (#418), this points at the standalone server's API; the
	// agent uses it in templated tool descriptions to tell the user
	// exactly where to approve gated actions. Without launch setting it,
	// aileron-mcp falls back to a generic "check the Aileron webapp"
	// phrasing, which still works but is less actionable.
	selfPath, _ := os.Executable()
	if mcpBin, err := resolveSibling(selfPath, "aileron-mcp"); err == nil {
		mcpEnv := map[string]string{
			"AILERON_COMMS_SOCKET": commsSocket,
		}
		if gateway != nil {
			mcpEnv["AILERON_URL"] = gateway.URL
			mcpEnv["AILERON_APPROVAL_URL"] = gateway.URL + "/approvals"
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
	)

	// Comms listeners (Slack, Discord) so incoming messages still
	// land in the NotifyQueue and are readable via the
	// `read_messages` MCP tool. The webapp surface for surfacing
	// these messages is a future addition (#418's followups);
	// today the agent reads them via `aileron-mcp`.
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
	// matches an `ask:` policy rule (issue #427). The server registers
	// a shell-kind entry on the same approval queue the gateway
	// exposes to the webapp, blocks on the user's verdict, and
	// replies with the four-option decision string aileron-sh
	// expects. When the listener can't be created (e.g. tmpdir is
	// read-only in a hostile environment), aileron-sh's dial fails
	// and the policy-enforced shell falls back to its built-in
	// deny — agents stay blocked from running gated commands without
	// explicit approval.
	approvalSrv, err := NewApprovalSocketServer(approvalSocket, approvalQueue, sessionID, config.Dir, sessionLog.With("component", "approval-socket"))
	if err != nil {
		sessionLog.Warn("approval socket disabled", "error", err)
		fmt.Fprintf(os.Stderr, "aileron: approval socket unavailable: %v\n", err)
	} else {
		defer func() { _ = approvalSrv.Close() }()
		go approvalSrv.Serve(ctx)
	}

	// Print the startup banner once on stderr before exec'ing the
	// agent. The agent inherits this terminal — Claude Code, Pi,
	// and others own the terminal completely under the new launch
	// path. The banner is the only thing Aileron ever writes here.
	printStartupBanner(os.Stderr, gateway, sessionID, sessionLogPath(config.Dir), unlockedVault == nil && mode == vaultPromptModeWebapp)

	result, err := launchDirect(cmd, config)

	sessionLog.Info("session ended", "exit_code", result.ExitCode)
	if auditStateDir != "" {
		PrintSessionSummary(os.Stderr, auditStateDir, sessionID)
	}
	return result, err
}

// printStartupBanner writes a single line on stderr before exec'ing
// the agent, naming the webapp URL and session id. Replaces the in-
// pty StatusBar from the pre-#419 launch path. Output is fenced so
// agents that don't render ANSI gracefully (or terminals that wrap
// long lines) still parse the URL cleanly.
//
// When the daemon starts vault-locked (webapp mode, #429) and the
// gateway is up, a follow-up line points the user at the unlock
// surface — vault-needing tool calls fail with 423 until the user
// types their passphrase into the modal.
func printStartupBanner(w io.Writer, gateway *Gateway, sessionID, logPath string, vaultLocked bool) {
	url := ""
	if gateway != nil {
		url = gateway.URL
	}
	if url == "" {
		fmt.Fprintf(w, "✈️  Aileron — session %s — log %s\n", sessionID, logPath)
		return
	}
	fmt.Fprintf(w, "✈️  Aileron — webapp %s — session %s — log %s\n", url, sessionID, logPath)
	if vaultLocked {
		fmt.Fprintf(w, "✈️  Vault locked — open %s and enter your passphrase to unlock.\n", url)
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

// generateSessionID returns a short random hex string for correlating
// audit entries within a single aileron launch session.
func generateSessionID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return fmt.Sprintf("%x", b)
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
