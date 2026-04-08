package launch

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
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
// It blocks until the child exits, forwarding SIGINT and SIGTERM.
func Launch(ctx context.Context, config LaunchConfig) (LaunchResult, error) {
	agentPath, err := ResolveBinary(config.Agent.BinaryNames())
	if err != nil {
		return LaunchResult{}, fmt.Errorf("agent %q: %w", config.Agent.Name(), err)
	}

	env := buildEnv(config.ShellShim, config.Agent.Env())

	cmd := exec.CommandContext(ctx, agentPath, config.Args...)
	cmd.Env = env
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if config.Dir != "" {
		cmd.Dir = config.Dir
	}

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

	err = cmd.Wait()
	signal.Stop(sigCh)
	close(sigCh)

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
		// Remove existing SHELL and AILERON_REAL_SHELL entries
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
