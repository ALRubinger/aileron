package launch

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"

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

	env := buildEnv(config.ShellShim, config.Agent.Env())

	// Set up agent-specific hooks for policy enforcement.
	hookArgs, cleanup, err := config.Agent.SetupHooks(config.ShellShim)
	if err != nil {
		return LaunchResult{}, fmt.Errorf("setting up hooks for %s: %w", config.Agent.Name(), err)
	}
	if cleanup != nil {
		defer cleanup()
	}

	// Prepend hook args, then append user args.
	allArgs := append(hookArgs, config.Args...)

	cmd := exec.CommandContext(ctx, agentPath, allArgs...)
	cmd.Env = env
	if config.Dir != "" {
		cmd.Dir = config.Dir
	}

	// If stdin is a terminal, use the pty proxy with status bar.
	if term.IsTerminal(int(os.Stdin.Fd())) {
		return launchWithPty(cmd, config)
	}
	return launchDirect(cmd, config)
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
func launchWithPty(cmd *exec.Cmd, config LaunchConfig) (LaunchResult, error) {
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

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGWINCH)
	go func() {
		for sig := range sigCh {
			switch sig {
			case syscall.SIGWINCH:
				HandleResize(os.Stdout, stdinFd, ptmx, bar)
			default:
				if cmd.Process != nil {
					_ = cmd.Process.Signal(sig)
				}
			}
		}
	}()

	go func() { io.Copy(ptmx, os.Stdin) }()
	go func() { io.Copy(os.Stdout, ptmx) }()

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
//   - Sets AILERON_REAL_SHELL to the original SHELL value
//   - Merges any agent-specific env vars
func buildEnv(shimPath string, agentEnv map[string]string) []string {
	origShell := os.Getenv("SHELL")
	if origShell == "" {
		origShell = "/bin/sh"
	}

	env := os.Environ()
	filtered := make([]string, 0, len(env)+len(agentEnv)+2)
	for _, e := range env {
		if len(e) >= 6 && e[:6] == "SHELL=" {
			continue
		}
		if len(e) >= 19 && e[:19] == "AILERON_REAL_SHELL=" {
			continue
		}
		filtered = append(filtered, e)
	}

	filtered = append(filtered, "SHELL="+shimPath)
	filtered = append(filtered, "AILERON_REAL_SHELL="+origShell)

	for k, v := range agentEnv {
		filtered = append(filtered, k+"="+v)
	}

	return filtered
}
