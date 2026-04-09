package launch

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/ALRubinger/aileron/core/audit"
	"github.com/ALRubinger/aileron/core/comms"
	launchpolicy "github.com/ALRubinger/aileron/core/policy/launch"
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
	agentPath, err := ResolveBinary(config.Agent.BinaryNames())
	if err != nil {
		return LaunchResult{}, fmt.Errorf("agent %q: %w", config.Agent.Name(), err)
	}

	// Install a wrapper script at ~/.aileron/bash whose path contains "bash"
	// so that Claude Code accepts it as a valid shell.
	wrapperPath, err := InstallWrapper(config.ShellShim)
	if err != nil {
		return LaunchResult{}, fmt.Errorf("installing shell wrapper: %w", err)
	}

	sessionID := generateSessionID()
	auditLog := resolveAuditLog(config.Dir)
	envConfig := loadEnvConfig(config.Dir)

	env := buildEnv(config.ShellShim, wrapperPath, config.Agent.Name(), sessionID, auditLog, envConfig, config.Agent.Env())

	// Agent-required args come first, then user-supplied args.
	allArgs := append(config.Agent.Args(), config.Args...)
	cmd := exec.CommandContext(ctx, agentPath, allArgs...)
	cmd.Env = env
	if config.Dir != "" {
		cmd.Dir = config.Dir
	}

	// Create the notification queue and start any comms listeners.
	queue := NewNotifyQueue(100, nil)
	listeners := startCommsListeners(ctx, config.Dir, queue)
	defer stopCommsListeners(listeners)

	// If stdin is a terminal, use the pty proxy with status bar.
	var result LaunchResult
	if term.IsTerminal(int(os.Stdin.Fd())) {
		result, err = launchWithPty(cmd, config, queue)
	} else {
		result, err = launchDirect(cmd, config)
	}

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
func launchWithPty(cmd *exec.Cmd, config LaunchConfig, queue *NotifyQueue) (LaunchResult, error) {
	stdinFd := int(os.Stdin.Fd())

	cols, rows, err := term.GetSize(stdinFd)
	if err != nil {
		return LaunchResult{}, fmt.Errorf("getting terminal size: %w", err)
	}

	bar := NewStatusBar(rows, cols, "Flying ✈️ withaileron.ai ")
	agentRows := ComputeAgentRows(rows, bar.BarHeight())

	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{
		Rows: uint16(agentRows),
		Cols: uint16(cols),
	})
	if err != nil {
		return LaunchResult{}, fmt.Errorf("failed to start %s in pty: %w", config.Agent.Name(), err)
	}
	defer ptmx.Close()

	oldState, err := term.MakeRaw(stdinFd)
	if err != nil {
		return LaunchResult{}, fmt.Errorf("setting raw mode: %w", err)
	}
	defer term.Restore(stdinFd, oldState)

	// Set up the overlay and intelligent I/O routing.
	bar.SetQueue(queue)

	outputCopier := NewOutputCopier(ptmx, os.Stdout, nil)
	overlay := NewOverlay(queue, outputCopier, os.Stdout, rows, cols, nil)
	outputCopier.SetOverlay(overlay)
	router := NewKeyRouter(os.Stdin, ptmx, overlay)
	overlay.onDismiss = router.DeactivateOverlay

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

	go router.Run()
	go outputCopier.Run()

	err = cmd.Wait()
	signal.Stop(sigCh)
	close(sigCh)

	CleanupTerminalScreen(os.Stdout, rows)

	return exitResult(err)
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
// No scroll region is set — the agent's pty is sized to agentRows, so its
// cursor positioning stays within the agent area naturally. The status bar
// sits below the agent's viewport. If the agent's scrolling output
// overwrites the bar, it will be re-rendered on the next resize.
func SetupTerminalScreen(w io.Writer, agentRows int, bar *StatusBar) {
	fmt.Fprintf(w, "\033[2J")
	bar.Render(w)
	fmt.Fprintf(w, "\033[%d;1H", agentRows)
}

// CleanupTerminalScreen clears the status bar area.
func CleanupTerminalScreen(w io.Writer, totalRows int) {
	fmt.Fprintf(w, "\033[%d;1H\033[J", totalRows-1)
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
//   - Sets CLAUDE_CODE_SHELL to the wrapper path (whose name contains "bash")
//   - Sets AILERON_REAL_SHELL to the original SHELL value
//   - Merges any agent-specific env vars
func buildEnv(shimPath, wrapperPath, agentName, sessionID, auditLog string, envConfig *launchpolicy.EnvConfig, agentEnv map[string]string) []string {
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
		"CLAUDE_CODE_SHELL":  true,
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
	if wrapperPath != "" {
		filtered = append(filtered, "CLAUDE_CODE_SHELL="+wrapperPath)
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
		if envGlobMatch(p, key) {
			return false
		}
	}
	for _, s := range cfg.Scrub {
		if envGlobMatch(s, key) {
			return true
		}
	}
	return false
}

// envGlobMatch matches an env var name against a pattern with * wildcards.
// Supports prefix (AWS_*), suffix (*_SECRET), and exact match.
func envGlobMatch(pattern, name string) bool {
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

// startCommsListeners reads the notification config from the policy file,
// creates listeners, and starts them.
func startCommsListeners(ctx context.Context, dir string, queue *NotifyQueue) []comms.Listener {
	if dir == "" {
		dir, _ = os.Getwd()
	}
	policyPath := FindPolicyFile(dir)
	if policyPath == "" {
		return nil
	}
	pf := loadPolicyFileFrom(policyPath)
	if pf.Notifications == nil {
		return nil
	}

	var created []comms.Listener
	if cfg := pf.Notifications.Slack; cfg != nil && cfg.AppToken != "" && cfg.BotToken != "" {
		channels := make([]string, 0, len(cfg.Channels))
		for _, ch := range cfg.Channels {
			channels = append(channels, ch.Name)
		}
		created = append(created, comms.NewSlackListener(cfg.AppToken, cfg.BotToken, channels, cfg.Ignore))
	}

	return StartListeners(ctx, created, queue, os.Stderr)
}

// StartListeners connects and starts each listener, bridging incoming
// messages to the NotifyQueue. Returns the successfully started
// listeners. Errors are written to w.
func StartListeners(ctx context.Context, listeners []comms.Listener, queue *NotifyQueue, w io.Writer) []comms.Listener {
	var started []comms.Listener
	for _, l := range listeners {
		if err := l.Connect(ctx); err != nil {
			fmt.Fprintf(w, "aileron: %s connect failed: %v\n", l.Service(), err)
			continue
		}
		msgs, err := l.Listen(ctx)
		if err != nil {
			fmt.Fprintf(w, "aileron: %s listen failed: %v\n", l.Service(), err)
			continue
		}
		started = append(started, l)
		go BridgeMessages(msgs, queue)
	}
	return started
}

// BridgeMessages reads from a comms listener channel and pushes messages
// into the NotifyQueue. Exported for testing.
func BridgeMessages(msgs <-chan comms.IncomingMessage, queue *NotifyQueue) {
	for msg := range msgs {
		preview := msg.Body
		if len(preview) > 80 {
			preview = preview[:77] + "..."
		}
		queue.Push(Message{
			ID:        msg.ID,
			Source:    msg.Service,
			Channel:   msg.Channel,
			Author:    msg.Author,
			Preview:   preview,
			Body:      msg.Body,
			Timestamp: msg.Timestamp,
		})
	}
}

// stopCommsListeners shuts down all running listeners.
func stopCommsListeners(listeners []comms.Listener) {
	for _, l := range listeners {
		l.Close()
	}
}
