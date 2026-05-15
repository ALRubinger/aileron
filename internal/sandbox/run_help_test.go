package sandbox_test

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ALRubinger/aileron/internal/sandbox"
	"github.com/ALRubinger/aileron/internal/sandbox/sandboxtest"
)

func TestRunHelp_CapturesStdout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sandboxtest.FakeBinary is POSIX-only; Windows port pending")
	}
	dir := t.TempDir()
	fake := sandboxtest.FakeBinary{
		Stdout: "Usage: fake [command]\n\nCommands:\n  ping  Ping a host\n  pong  Reply\n",
	}
	path, err := fake.Write(dir, "fakecli")
	if err != nil {
		t.Fatalf("write fake binary: %v", err)
	}

	res, err := sandbox.RunHelp(context.Background(), path, dir)
	if err != nil {
		t.Fatalf("RunHelp: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("exit code: got %d, want 0", res.ExitCode)
	}
	if !strings.Contains(string(res.Stdout), "Commands:") {
		t.Errorf("stdout missing usage block: %q", string(res.Stdout))
	}
}

func TestRunHelp_CapturesStderrForNonZeroExit(t *testing.T) {
	// Some CLIs (curl is the canonical example) emit --help on
	// stderr and exit non-zero. The introspector reads whichever
	// stream carries the text; RunHelp must surface both regardless
	// of exit code so the parser can find it. The executor folds
	// ExitError into RunHelpResult.ExitCode rather than err, so
	// non-zero exit alone is a successful capture.
	if runtime.GOOS == "windows" {
		t.Skip("sandboxtest.FakeBinary is POSIX-only")
	}
	dir := t.TempDir()
	fake := sandboxtest.FakeBinary{
		Stderr:   "Usage: weird [command]\n",
		ExitCode: 2,
	}
	path, err := fake.Write(dir, "weirdcli")
	if err != nil {
		t.Fatalf("write fake binary: %v", err)
	}

	res, err := sandbox.RunHelp(context.Background(), path, dir)
	if err != nil {
		t.Fatalf("RunHelp: unexpected error %v", err)
	}
	if res.ExitCode != 2 {
		t.Errorf("exit code: got %d, want 2", res.ExitCode)
	}
	if !strings.Contains(string(res.Stderr), "Usage") {
		t.Errorf("stderr should carry help text: %q", string(res.Stderr))
	}
}

func TestRunHelp_RejectsRelativeProgramPath(t *testing.T) {
	_, err := sandbox.RunHelp(context.Background(), "fakecli", "/tmp")
	if err == nil {
		t.Fatal("expected error for relative program path")
	}
	if !strings.Contains(err.Error(), "absolute") {
		t.Errorf("error should mention absolute path requirement: %v", err)
	}
}

func TestRunHelp_RejectsRelativeCwd(t *testing.T) {
	_, err := sandbox.RunHelp(context.Background(), "/usr/bin/true", "tmp")
	if err == nil {
		t.Fatal("expected error for relative cwd")
	}
	if !strings.Contains(err.Error(), "absolute") {
		t.Errorf("error should mention absolute cwd: %v", err)
	}
}

func TestRunHelp_RejectsEmptyCwd(t *testing.T) {
	_, err := sandbox.RunHelp(context.Background(), "/usr/bin/true", "")
	if err == nil {
		t.Fatal("expected error for empty cwd")
	}
}

func TestRunHelp_RejectsWhenSandboxUnavailable(t *testing.T) {
	// On supported platforms SandboxAvailable returns nil, so this
	// path is hard to exercise without monkeypatching. Verify the
	// contract on platforms where the fallback already denies.
	if runtime.GOOS == "linux" || runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
		t.Skip("supported platform; SandboxAvailable returns nil")
	}
	_, err := sandbox.RunHelp(context.Background(), "/usr/bin/true", "/tmp")
	if !errors.Is(err, sandbox.ErrSpawnUnavailable) {
		t.Errorf("expected wrapped ErrSpawnUnavailable, got %v", err)
	}
}

func TestRunHelp_AppliesHelpTimeout(t *testing.T) {
	// HelpTimeout is documented as the upper bound the runner
	// installs internally. Verify it's positive and matches the
	// wrap-tool's existing 5s — the introspection path should keep
	// parity with the unsandboxed wrap.HelpRunnerExec deadline so
	// both surfaces fail in the same place on a hung help command.
	if sandbox.HelpTimeout <= 0 {
		t.Errorf("HelpTimeout must be positive, got %v", sandbox.HelpTimeout)
	}
	if sandbox.HelpTimeout != 5*time.Second {
		t.Errorf("HelpTimeout drift: got %v, want 5s to match wrap.HelpRunnerExec", sandbox.HelpTimeout)
	}
}
