package launch

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/ALRubinger/aileron/internal/audit"
	"github.com/ALRubinger/aileron/internal/comms"
	launchpolicy "github.com/ALRubinger/aileron/internal/policy/launch"
	"github.com/ALRubinger/aileron/internal/vault"
	"github.com/creack/pty/v2"
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
	// Per ADR-0011, ensure the local credential vault is unlocked
	// before any agent runs. On first launch (no vault file) we
	// prompt to create one; on subsequent launches we prompt for
	// the passphrase with retry. The KEK lives in this process for
	// the lifetime of the launch.
	//
	// We only do this when stdin is a terminal — non-TTY contexts
	// (tests, CI, headless integrations) fall through to the lazy
	// on-demand path via OpenVaultFunc, preserving backward
	// compatibility with the existing test surface.
	if term.IsTerminal(int(os.Stdin.Fd())) {
		if _, err := EnsureVault(DefaultVaultPath(), nil, os.Stderr, 3); err != nil {
			return LaunchResult{}, fmt.Errorf("vault: %w", err)
		}
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
	auditLog := resolveAuditLog(config.Dir)
	envConfig := loadEnvConfig(config.Dir)
	approvalSocket := filepath.Join(os.TempDir(), "ai-"+sessionID+".sock")
	commsSocket := filepath.Join(os.TempDir(), "ai-comms-"+sessionID+".sock")

	env := buildEnv(config.ShellShim, config.Agent.Name(), sessionID, auditLog, envConfig, config.Agent.Env())
	env = append(env, "AILERON_APPROVAL_SOCKET="+approvalSocket)
	env = append(env, "AILERON_COMMS_SOCKET="+commsSocket)

	// Agent-required args come first, then user-supplied args.
	allArgs := append(config.Agent.Args(), config.Args...)

	// If the comms socket is set, register aileron-mcp as an MCP server
	// so the agent has access to read_messages, draft_reply, etc.
	selfPath, _ := os.Executable()
	if mcpBin, err := resolveSibling(selfPath, "aileron-mcp"); err == nil {
		mcpConfig := fmt.Sprintf(
			`{"mcpServers":{"aileron":{"command":%q,"env":{"AILERON_COMMS_SOCKET":%q}}}}`,
			mcpBin, commsSocket,
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

	commsConf := prepareCommsConfig(config.Dir, sessionLog)

	// If stdin is a terminal, use the pty proxy with status bar.
	// The pty path defers vault opening so it can show a Panel prompt.
	var result LaunchResult
	var listeners []comms.Listener
	if term.IsTerminal(int(os.Stdin.Fd())) {
		result, listeners, err = launchWithPty(ctx, cmd, config, queue, commsConf, approvalSocket, commsSocket, auditLog, sessionID)
	} else {
		// Non-pty path: use legacy tty-based vault prompt.
		listeners = startCommsListeners(ctx, config.Dir, queue, auditLog, sessionID, sessionLog)
		result, err = launchDirect(cmd, config)
	}
	defer stopCommsListeners(listeners)

	sessionLog.Info("session ended", "exit_code", result.ExitCode)
	if auditLog != "" {
		PrintSessionSummary(os.Stderr, auditLog, sessionID)
	}
	return result, err
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

// launchWithPty runs the agent inside a pty with a status bar at the bottom.
func launchWithPty(ctx context.Context, cmd *exec.Cmd, config LaunchConfig, queue *NotifyQueue, commsConf *commsSetup, approvalSocket, commsSocket, auditLog, sessionID string) (LaunchResult, []comms.Listener, error) {
	stdinFd := int(os.Stdin.Fd())

	cols, rows, err := term.GetSize(stdinFd)
	if err != nil {
		return LaunchResult{}, nil, fmt.Errorf("getting terminal size: %w", err)
	}

	bar := NewStatusBar(rows, cols, "Flying ✈️ withaileron.ai ")
	agentRows := ComputeAgentRows(rows, bar.BarHeight())

	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{
		Rows: uint16(agentRows),
		Cols: uint16(cols),
	})
	if err != nil {
		return LaunchResult{}, nil, fmt.Errorf("failed to start %s in pty: %w", config.Agent.Name(), err)
	}
	defer ptmx.Close()

	oldState, err := term.MakeRaw(stdinFd)
	if err != nil {
		return LaunchResult{}, nil, fmt.Errorf("setting raw mode: %w", err)
	}
	defer term.Restore(stdinFd, oldState)

	SetupTerminalScreen(os.Stdout, agentRows, bar)

	// Set up the overlay and intelligent I/O routing.
	bar.SetQueue(queue)

	outputCopier := NewOutputCopier(ptmx, os.Stdout, nil)
	outputCopier.SetOnIdle(func() { bar.Render(os.Stdout) })
	overlay := NewOverlay(queue, outputCopier, os.Stdout, rows, cols, nil)
	outputCopier.SetOverlay(overlay)
	router := NewKeyRouter(os.Stdin, ptmx, overlay)
	overlay.onDismiss = router.DeactivateOverlay

	WireDraftInjection(ptmx, overlay, queue)

	// Start the router early so vault prompt can steal input.
	go router.Run()
	go outputCopier.Run()

	// Now that the PTY, router, and copier are up, open the vault
	// (with Panel-based retry prompt) and start comms listeners.
	var listeners []comms.Listener
	if commsConf != nil {
		var v vault.Vault
		if commsConf.needsVault {
			v = PromptVaultWithPanel(outputCopier, router, bar, ptmx)
		}
		listeners = startCommsWithVault(ctx, commsConf, v, queue, auditLog, sessionID)
	}

	// Start the approval server so aileron-sh can request user approvals
	// on the real terminal (not the pty).
	approvalSrv, err := NewApprovalServer(approvalSocket, bar, outputCopier, router, ptmx)
	if err != nil {
		return LaunchResult{}, listeners, fmt.Errorf("starting approval server: %w", err)
	}
	defer approvalSrv.Close()
	go approvalSrv.Serve()

	// Start the comms server so aileron-mcp can read/send messages.
	commsSrv, err := NewCommsServer(commsSocket, queue, listeners, bar, outputCopier, router, auditLog, sessionID)
	if err != nil {
		return LaunchResult{}, listeners, fmt.Errorf("starting comms server: %w", err)
	}
	defer commsSrv.Close()
	go commsSrv.Serve()

	WireReply(overlay, commsSrv)
	WireDraftSend(overlay, commsSrv)

	// Wire onChange to re-render the status bar when notifications arrive.
	queue.onChange = func() { bar.Render(os.Stdout) }

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGWINCH)
	go func() {
		for sig := range sigCh {
			switch sig {
			case syscall.SIGWINCH:
				HandleResize(os.Stdout, stdinFd, ptmx, bar)
				newCols, newRows, _ := term.GetSize(stdinFd)
				overlay.Resize(newRows, newCols)
			default:
				if cmd.Process != nil {
					_ = cmd.Process.Signal(sig)
				}
			}
		}
	}()

	err = cmd.Wait()
	signal.Stop(sigCh)
	close(sigCh)

	// Print learned rules before the deferred Close clears the server.
	if learned := approvalSrv.LearnedRules(); len(learned) > 0 {
		PrintLearnedRulesSummary(os.Stderr, learned)
	}

	CleanupTerminalScreen(os.Stdout, rows)

	r, exitErr := exitResult(err)
	return r, listeners, exitErr
}

// WireDraftInjection connects the overlay's draft-request and converse
// actions to a DraftInjector that writes prompts into the agent's pty
// stdin. Auto-draft messages are NOT injected automatically — the user
// must explicitly request a draft from the overlay. Exported for testing.
func WireDraftInjection(ptmx io.Writer, overlay *Overlay, queue *NotifyQueue) {
	injector := NewDraftInjector(ptmx)
	overlay.OnDraftRequest = func(msg Message) {
		injector.Inject(msg)
	}
	overlay.OnDraftConverse = func(msg Message, feedback string) {
		injector.InjectRevision(msg, feedback)
	}
	// No queue.SetOnAutoDraft — user must open the overlay and press 'a'.
}

// WireReply connects the overlay's reply callback to the CommsServer's
// DirectSend method. No approval prompt is shown since the user authored
// the reply text themselves. Exported for testing.
func WireReply(overlay *Overlay, commsSrv *CommsServer) {
	overlay.OnReply = func(msg Message, reply string) {
		commsSrv.DirectSend(msg.Source, msg.Channel, reply)
	}
}

// WireDraftSend connects the overlay's draft-approve and draft-edit
// callbacks to send directly via the CommsServer. The agent drafts the
// text; Aileron sends it after user approval. Exported for testing.
func WireDraftSend(overlay *Overlay, commsSrv *CommsServer) {
	overlay.OnDraftApprove = func(msg Message) {
		commsSrv.DirectSend(msg.Source, msg.Channel, msg.Draft)
	}
	overlay.OnDraftEdit = func(msg Message, edited string) {
		commsSrv.DirectSend(msg.Source, msg.Channel, edited)
	}
}

// ComputeAgentRows returns the number of rows available for the agent,
// reserving space for the status bar.
func ComputeAgentRows(totalRows, barHeight int) int {
	rows := totalRows - barHeight
	if rows < 1 {
		return 1
	}
	return rows
}

// SetupTerminalScreen clears the screen and renders the status bar.
func SetupTerminalScreen(w io.Writer, agentRows int, bar *StatusBar) {
	fmt.Fprintf(w, "\033[2J\033[1;1H")
	bar.Render(w)
	fmt.Fprintf(w, "\033[1;1H")
}

// CleanupTerminalScreen clears the status bar area (3 rows).
func CleanupTerminalScreen(w io.Writer, totalRows int) {
	fmt.Fprintf(w, "\033[%d;1H\033[J", totalRows-2)
}

// HandleResize updates the pty size and re-renders the status bar after a
// terminal resize. The fd parameter is the file descriptor to query for the
// new terminal size.
func HandleResize(w io.Writer, fd int, ptmx *os.File, bar *StatusBar) {
	newCols, newRows, err := term.GetSize(fd)
	if err != nil {
		return
	}
	newAgentRows := ComputeAgentRows(newRows, bar.BarHeight())
	_ = pty.Setsize(ptmx, &pty.Winsize{
		Rows: uint16(newAgentRows),
		Cols: uint16(newCols),
	})
	bar.Resize(w, newRows, newCols)
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
func buildEnv(shimPath, agentName, sessionID, auditLog string, envConfig *launchpolicy.EnvConfig, agentEnv map[string]string) []string {
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
		"AILERON_AUDIT_LOG":  true,
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
	if auditLog != "" {
		filtered = append(filtered, "AILERON_AUDIT_LOG="+auditLog)
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

// ResolveAuditLogFromCwd resolves the audit log path from the current
// working directory.
func ResolveAuditLogFromCwd() string {
	return resolveAuditLog("")
}

// resolveAuditLog determines the audit log path. It looks for
// aileron.yaml in the given directory (or cwd) and reads its
// Settings.AuditLog field. Falls back to .aileron/audit.jsonl
// relative to the policy file's directory.
func resolveAuditLog(dir string) string {
	if dir == "" {
		dir, _ = os.Getwd()
	}
	policyPath := FindPolicyFile(dir)
	if policyPath == "" {
		// No policy file — use cwd/.aileron/audit.jsonl.
		return filepath.Join(dir, ".aileron", "audit.jsonl")
	}

	policyDir := filepath.Dir(policyPath)

	pf := loadPolicyFileFrom(policyPath)
	if pf.Settings != nil && pf.Settings.AuditLog != "" {
		p := pf.Settings.AuditLog
		if !filepath.IsAbs(p) {
			p = filepath.Join(policyDir, p)
		}
		return p
	}
	return filepath.Join(policyDir, ".aileron", "audit.jsonl")
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

// PrintLearnedRulesSummary prints the rules the user chose to persist
// during the session (via "allow for project" or "allow for me").
func PrintLearnedRulesSummary(w io.Writer, rules []LearnedRule) {
	fmt.Fprintf(w, "  %d rule(s) learned this session:\n", len(rules))
	for _, r := range rules {
		fmt.Fprintf(w, "    %s  -> %s (%s)\n", r.Pattern, r.File, r.Scope)
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
func startCommsWithVault(ctx context.Context, setup *commsSetup, v vault.Vault, queue *NotifyQueue, auditLog, sessionID string) []comms.Listener {
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

	return StartListeners(ctx, created, queue, os.Stderr, autoDraft, priority, auditLog, sessionID, sessionLog)
}

// startCommsListeners is the legacy convenience wrapper that prepares
// config, opens the vault via the old tty prompt, and starts listeners.
// Used by the non-pty (direct) launch path.
func startCommsListeners(ctx context.Context, dir string, queue *NotifyQueue, auditLog, sessionID string, sessionLog *slog.Logger) []comms.Listener {
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

	return startCommsWithVault(ctx, setup, v, queue, auditLog, sessionID)
}

// StartListeners connects and starts each listener, bridging incoming
// messages to the NotifyQueue. The autoDraft map controls which channels
// trigger automatic draft replies. The priority map controls the
// priority level ("normal", "high") per channel. Returns the
// successfully started listeners. Errors are written to w.
func StartListeners(ctx context.Context, listeners []comms.Listener, queue *NotifyQueue, w io.Writer, autoDraft map[string]bool, priority map[string]string, auditLog, sessionID string, log *slog.Logger) []comms.Listener {
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
		go BridgeMessages(msgs, queue, autoDraft, priority, auditLog, sessionID, log)
	}
	return started
}

// BridgeMessages reads from a comms listener channel and pushes messages
// into the NotifyQueue. The autoDraft map controls which channels trigger
// automatic draft replies. The priority map sets the priority level per
// channel. Exported for testing.
func BridgeMessages(msgs <-chan comms.IncomingMessage, queue *NotifyQueue, autoDraft map[string]bool, priority map[string]string, auditLog, sessionID string, log *slog.Logger) {
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
		if auditLog != "" {
			audit.AppendMessageEntry(auditLog, audit.MessageEntry{
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
