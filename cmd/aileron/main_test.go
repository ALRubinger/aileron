package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ALRubinger/aileron/internal/cstore"
	"github.com/ALRubinger/aileron/internal/daemon/discovery"
	"github.com/ALRubinger/aileron/internal/launch"
	"github.com/ALRubinger/aileron/internal/launch/agents"
	sandboxcomposition "github.com/ALRubinger/aileron/internal/sandbox/composition"
	sandboxcontainer "github.com/ALRubinger/aileron/internal/sandbox/container"
)

// init bypasses the CLI vault state machine for the bulk of tests.
// The state machine prompts for a passphrase and tries to spawn a
// daemon — neither is available in the test process, and almost no
// existing test exercises that flow. Tests for vault_state.go itself
// call ensureVaultUnlocked directly (not through the seam) so this
// override doesn't suppress them.
func init() {
	ensureVaultUnlockedFn = func(string, io.Writer) error { return nil }
	// Force the non-interactive path by default so launch tests that don't
	// pass --claude-auth never block on the first-run prompt. Tests that
	// exercise the prompt override isTTYFn locally.
	isTTYFn = func() bool { return false }
}

func newTestRegistry() *launch.Registry {
	r := launch.NewRegistry()
	r.Register(agents.Claude{})
	r.Register(agents.Pi{})
	return r
}

func TestRun_NoArgs(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run(nil, newTestRegistry(), &stdout, &stderr)
	if code != 1 {
		t.Errorf("expected exit code 1, got %d", code)
	}
	if !strings.Contains(stdout.String(), "aileron launch") {
		t.Error("expected usage in stdout")
	}
}

func TestRun_Help(t *testing.T) {
	for _, arg := range []string{"help", "--help", "-h"} {
		var stdout, stderr bytes.Buffer
		code := run([]string{arg}, newTestRegistry(), &stdout, &stderr)
		if code != 0 {
			t.Errorf("help (%s): expected exit code 0, got %d", arg, code)
		}
		if !strings.Contains(stdout.String(), "aileron launch") {
			t.Errorf("help (%s): expected usage output", arg)
		}
	}
}

func TestRun_Version(t *testing.T) {
	for _, arg := range []string{"version", "--version", "-v"} {
		var stdout, stderr bytes.Buffer
		code := run([]string{arg}, newTestRegistry(), &stdout, &stderr)
		if code != 0 {
			t.Errorf("version (%s): expected exit code 0, got %d", arg, code)
		}
		if !strings.Contains(stdout.String(), "aileron") {
			t.Errorf("version (%s): expected version output", arg)
		}
	}
}

func TestRun_UnknownCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"bogus"}, newTestRegistry(), &stdout, &stderr)
	if code != 1 {
		t.Errorf("expected exit code 1, got %d", code)
	}
	if !strings.Contains(stderr.String(), `unknown command: "bogus"`) {
		t.Errorf("expected unknown command error, got %q", stderr.String())
	}
}

func TestRun_LaunchNoAgent(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"launch"}, newTestRegistry(), &stdout, &stderr)
	if code != 1 {
		t.Errorf("expected exit code 1, got %d", code)
	}
	if !strings.Contains(stderr.String(), "usage: aileron launch") {
		t.Error("expected launch usage in stderr")
	}
}

func TestRun_LaunchUnknownAgent(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"launch", "bogus-agent"}, newTestRegistry(), &stdout, &stderr)
	if code != 1 {
		t.Errorf("expected exit code 1, got %d", code)
	}
	if !strings.Contains(stderr.String(), "unknown agent") {
		t.Error("expected unknown agent error in stderr")
	}
	if !strings.Contains(stderr.String(), "claude") {
		t.Error("expected available agents list in stderr")
	}
}

// Regression test for issue #492 item 9: the CLI must populate
// LaunchConfig.Dir from os.Getwd() so the daemon's session record carries
// working_dir. Before the fix, Dir was the empty string at the call site,
// and every `aileron sessions list` row showed cwd="".
func TestRun_LaunchPopulatesWorkingDir(t *testing.T) {
	origLaunch := launchFn
	t.Cleanup(func() {
		launchFn = origLaunch
	})

	var captured launch.LaunchConfig
	launchFn = func(_ context.Context, cfg launch.LaunchConfig) (launch.LaunchResult, error) {
		captured = cfg
		return launch.LaunchResult{ExitCode: 0}, nil
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"launch", "claude"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit code 0, got %d (stderr=%q)", code, stderr.String())
	}

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd: %v", err)
	}
	if captured.Dir != cwd {
		t.Errorf("LaunchConfig.Dir = %q, want %q (cwd)", captured.Dir, cwd)
	}
}

func TestRun_LaunchPassesSandboxOptions(t *testing.T) {
	origLaunch := launchFn
	t.Cleanup(func() {
		launchFn = origLaunch
	})

	var captured launch.LaunchConfig
	launchFn = func(_ context.Context, cfg launch.LaunchConfig) (launch.LaunchResult, error) {
		captured = cfg
		return launch.LaunchResult{ExitCode: 0}, nil
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"launch", "--sandbox=docker", "--sandbox-build=never", "claude"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit code 0, got %d (stderr=%q)", code, stderr.String())
	}
	if captured.SandboxRuntime != "docker" {
		t.Errorf("LaunchConfig.SandboxRuntime = %q, want docker", captured.SandboxRuntime)
	}
	if captured.SandboxBuildPolicy != "never" {
		t.Errorf("LaunchConfig.SandboxBuildPolicy = %q, want never", captured.SandboxBuildPolicy)
	}
	// --sandbox-proxy defaults to "auto" so the launcher's resolver can
	// pick the docker default-on policy.
	if captured.SandboxProxy != "auto" {
		t.Errorf("LaunchConfig.SandboxProxy = %q, want auto (default)", captured.SandboxProxy)
	}
	// --host-login defaults to "auto" so the launcher's resolver enables
	// host-side acquisition when a binding declares one.
	if captured.HostLogin != "auto" {
		t.Errorf("LaunchConfig.HostLogin = %q, want auto (default)", captured.HostLogin)
	}
}

// TestRun_LaunchPropagatesHostLoginFlag verifies the CLI threads the
// --host-login flag through to LaunchConfig.HostLogin in both the
// before-agent and trailing forms.
func TestRun_LaunchPropagatesHostLoginFlag(t *testing.T) {
	origLaunch := launchFn
	t.Cleanup(func() { launchFn = origLaunch })

	cases := []struct {
		name string
		args []string
		want string
	}{
		{"before agent", []string{"launch", "--host-login=off", "claude"}, "off"},
		{"trailing eq form", []string{"launch", "claude", "--host-login=off"}, "off"},
		{"trailing space form", []string{"launch", "claude", "--host-login", "on"}, "on"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var captured launch.LaunchConfig
			launchFn = func(_ context.Context, cfg launch.LaunchConfig) (launch.LaunchResult, error) {
				captured = cfg
				return launch.LaunchResult{ExitCode: 0}, nil
			}
			var stdout, stderr bytes.Buffer
			code := run(tc.args, newTestRegistry(), &stdout, &stderr)
			if code != 0 {
				t.Fatalf("exit = %d (stderr=%q)", code, stderr.String())
			}
			if captured.HostLogin != tc.want {
				t.Errorf("LaunchConfig.HostLogin = %q, want %q", captured.HostLogin, tc.want)
			}
			if len(captured.Args) != 0 {
				t.Errorf("host-login flag leaked to agent args: %v", captured.Args)
			}
		})
	}
}

// TestRun_LaunchPropagatesSandboxProxyFlag verifies the CLI threads
// the --sandbox-proxy flag through to LaunchConfig.SandboxProxy so the
// launcher's resolver sees what the user typed.
func TestRun_LaunchPropagatesSandboxProxyFlag(t *testing.T) {
	origLaunch := launchFn
	t.Cleanup(func() {
		launchFn = origLaunch
	})

	for _, want := range []string{"on", "off", "auto"} {
		t.Run(want, func(t *testing.T) {
			var captured launch.LaunchConfig
			launchFn = func(_ context.Context, cfg launch.LaunchConfig) (launch.LaunchResult, error) {
				captured = cfg
				return launch.LaunchResult{ExitCode: 0}, nil
			}
			var stdout, stderr bytes.Buffer
			code := run([]string{"launch", "--sandbox=docker", "--sandbox-proxy=" + want, "claude"}, newTestRegistry(), &stdout, &stderr)
			if code != 0 {
				t.Fatalf("exit = %d (stderr=%q)", code, stderr.String())
			}
			if captured.SandboxProxy != want {
				t.Errorf("LaunchConfig.SandboxProxy = %q, want %q", captured.SandboxProxy, want)
			}
		})
	}
}

// TestRun_LaunchFlagsAfterAgentName locks the contract that aileron's
// own launch flags are position-independent: they resolve the same
// whether they appear before or after the agent name, they are NOT
// forwarded to the agent, genuinely-unknown args still forward, and a
// standalone "--" forwards an aileron-named flag through to the agent.
// Regression for the cryptic `unknown option '--sandbox=docker'` the
// agent CLI emitted when the flag landed after the agent name.
func TestRun_LaunchFlagsAfterAgentName(t *testing.T) {
	origLaunch := launchFn
	t.Cleanup(func() { launchFn = origLaunch })

	cases := []struct {
		name        string
		args        []string
		wantRuntime string
		wantBuild   string
		wantArgs    string // agent passthrough args, space-joined
	}{
		{"eq form after agent", []string{"launch", "claude", "--sandbox=docker"}, "docker", "auto", ""},
		{"space form after agent", []string{"launch", "claude", "--sandbox", "docker"}, "docker", "auto", ""},
		{"before agent still works", []string{"launch", "--sandbox=docker", "claude"}, "docker", "auto", ""},
		{"split before and after", []string{"launch", "--sandbox=docker", "claude", "--sandbox-build=never"}, "docker", "never", ""},
		{"unknown agent args forward", []string{"launch", "claude", "--model", "opus", "--sandbox=docker"}, "docker", "auto", "--model opus"},
		// After "--" the --sandbox=docker forwards to the agent, so aileron
		// never sees it; the default Docker sandbox runtime still applies.
		{"double dash forwards through", []string{"launch", "claude", "--", "--sandbox=docker"}, "docker", "auto", "--sandbox=docker"},
		{"bare dash forwards to agent", []string{"launch", "claude", "-"}, "docker", "auto", "-"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var captured launch.LaunchConfig
			launchFn = func(_ context.Context, cfg launch.LaunchConfig) (launch.LaunchResult, error) {
				captured = cfg
				return launch.LaunchResult{ExitCode: 0}, nil
			}
			var stdout, stderr bytes.Buffer
			code := run(tc.args, newTestRegistry(), &stdout, &stderr)
			if code != 0 {
				t.Fatalf("exit = %d (stderr=%q)", code, stderr.String())
			}
			if captured.SandboxRuntime != tc.wantRuntime {
				t.Errorf("SandboxRuntime = %q, want %q", captured.SandboxRuntime, tc.wantRuntime)
			}
			if captured.SandboxBuildPolicy != tc.wantBuild {
				t.Errorf("SandboxBuildPolicy = %q, want %q", captured.SandboxBuildPolicy, tc.wantBuild)
			}
			if got := strings.Join(captured.Args, " "); got != tc.wantArgs {
				t.Errorf("agent Args = %q, want %q", got, tc.wantArgs)
			}
		})
	}
}

// TestRun_LaunchTrailingFlagMissingValue verifies that an aileron launch
// flag placed after the agent name with no value fails fast with a clear
// message instead of launching with a bogus value.
func TestRun_LaunchTrailingFlagMissingValue(t *testing.T) {
	origLaunch := launchFn
	t.Cleanup(func() { launchFn = origLaunch })
	launchFn = func(_ context.Context, _ launch.LaunchConfig) (launch.LaunchResult, error) {
		t.Fatal("launch should not run when a trailing flag is missing its value")
		return launch.LaunchResult{}, nil
	}
	var stdout, stderr bytes.Buffer
	code := run([]string{"launch", "claude", "--sandbox"}, newTestRegistry(), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "needs a value") {
		t.Errorf("stderr = %q, want it to mention the missing value", stderr.String())
	}
}

// TestRun_LaunchDefaultsToDockerSandbox locks the contract that omitting
// every sandbox flag selects the Docker sandbox runtime. This is the flip
// from the old host-by-default behavior.
func TestRun_LaunchDefaultsToDockerSandbox(t *testing.T) {
	origLaunch := launchFn
	t.Cleanup(func() { launchFn = origLaunch })

	var captured launch.LaunchConfig
	launchFn = func(_ context.Context, cfg launch.LaunchConfig) (launch.LaunchResult, error) {
		captured = cfg
		return launch.LaunchResult{ExitCode: 0}, nil
	}
	var stdout, stderr bytes.Buffer
	code := run([]string{"launch", "claude"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d (stderr=%q)", code, stderr.String())
	}
	if captured.SandboxRuntime != "docker" {
		t.Errorf("SandboxRuntime = %q, want docker (default)", captured.SandboxRuntime)
	}
}

// TestRun_LaunchLocalForcesHostLaunch verifies --local maps to the "off"
// runtime (host launch) in both the before-agent and trailing forms, and
// that the bool flag is never leaked to the agent.
func TestRun_LaunchLocalForcesHostLaunch(t *testing.T) {
	origLaunch := launchFn
	t.Cleanup(func() { launchFn = origLaunch })

	cases := []struct {
		name string
		args []string
	}{
		{"before agent", []string{"launch", "--local", "claude"}},
		{"trailing bare form", []string{"launch", "claude", "--local"}},
		{"trailing inline form", []string{"launch", "claude", "--local=true"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var captured launch.LaunchConfig
			launchFn = func(_ context.Context, cfg launch.LaunchConfig) (launch.LaunchResult, error) {
				captured = cfg
				return launch.LaunchResult{ExitCode: 0}, nil
			}
			var stdout, stderr bytes.Buffer
			code := run(tc.args, newTestRegistry(), &stdout, &stderr)
			if code != 0 {
				t.Fatalf("exit = %d (stderr=%q)", code, stderr.String())
			}
			if captured.SandboxRuntime != "off" {
				t.Errorf("SandboxRuntime = %q, want off (host launch)", captured.SandboxRuntime)
			}
			if len(captured.Args) != 0 {
				t.Errorf("--local leaked to agent args: %v", captured.Args)
			}
		})
	}
}

// TestRun_LaunchLocalSandboxConflict verifies that combining --local with
// an explicit --sandbox fails fast rather than silently picking one. The
// conflict holds whether the flags appear before or after the agent name.
func TestRun_LaunchLocalSandboxConflict(t *testing.T) {
	origLaunch := launchFn
	t.Cleanup(func() { launchFn = origLaunch })
	launchFn = func(_ context.Context, _ launch.LaunchConfig) (launch.LaunchResult, error) {
		t.Fatal("launch should not run when --local conflicts with --sandbox")
		return launch.LaunchResult{}, nil
	}

	cases := [][]string{
		{"launch", "--local", "--sandbox=docker", "claude"},
		{"launch", "--sandbox=auto", "--local", "claude"},
		{"launch", "claude", "--local", "--sandbox=docker"},
	}
	for _, args := range cases {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(args, newTestRegistry(), &stdout, &stderr)
			if code != 1 {
				t.Fatalf("exit = %d, want 1", code)
			}
			if !strings.Contains(stderr.String(), "--local conflicts with --sandbox") {
				t.Errorf("stderr = %q, want it to mention the --local/--sandbox conflict", stderr.String())
			}
		})
	}
}

// TestParseLaunchFlagToken covers the dash/flag token classifier across
// non-flags, bare dashes, and both inline-value and value-less flag forms.
func TestParseLaunchFlagToken(t *testing.T) {
	cases := []struct {
		arg, name, value string
		hasValue         bool
	}{
		{"claude", "", "", false},
		{"-", "", "", false},
		{"--", "", "", false},
		{"--sandbox", "sandbox", "", false},
		{"--sandbox=docker", "sandbox", "docker", true},
		{"-sandbox=docker", "sandbox", "docker", true},
		{"--log-level=debug", "log-level", "debug", true},
	}
	for _, tc := range cases {
		name, value, hasValue := parseLaunchFlagToken(tc.arg)
		if name != tc.name || value != tc.value || hasValue != tc.hasValue {
			t.Errorf("parseLaunchFlagToken(%q) = (%q, %q, %v), want (%q, %q, %v)",
				tc.arg, name, value, hasValue, tc.name, tc.value, tc.hasValue)
		}
	}
}

func TestRun_LaunchLogLevelNoAgent(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"launch", "--log-level=debug"}, newTestRegistry(), &stdout, &stderr)
	if code != 1 {
		t.Errorf("expected exit code 1, got %d", code)
	}
	if !strings.Contains(stderr.String(), "usage: aileron launch") {
		t.Error("expected launch usage in stderr")
	}
}

func TestRunSandboxInitCreatesDevcontainerScaffold(t *testing.T) {
	dir := t.TempDir()
	oldWd, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWd) })

	var stdout, stderr bytes.Buffer
	code := run([]string{"sandbox", "init"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit code 0, got %d; stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), ".devcontainer/devcontainer.json") {
		t.Fatalf("stdout = %q", stdout.String())
	}
	if _, err := os.Stat(filepath.Join(dir, ".devcontainer", "devcontainer.json")); err != nil {
		t.Fatalf("devcontainer.json not created: %v", err)
	}
	// init scaffolds a Feature-composing devcontainer.json only; it no longer
	// writes a per-agent Dockerfile.
	if _, err := os.Stat(filepath.Join(dir, ".devcontainer", "Dockerfile")); !os.IsNotExist(err) {
		t.Fatalf("Dockerfile should not be created: stat err = %v", err)
	}
}

func TestRunSandboxPlanReportsTier(t *testing.T) {
	dir := t.TempDir()
	oldWd, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWd) })

	var stdout, stderr bytes.Buffer
	code := run([]string{"sandbox", "plan"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit code 0, got %d; stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "tier: base") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestRunSandboxRejectsUnknownCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"sandbox", "bogus"}, newTestRegistry(), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected exit code 1, got %d", code)
	}
	if !strings.Contains(stderr.String(), `unknown sandbox command: "bogus"`) {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestRunSandboxRequiresSubcommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"sandbox"}, newTestRegistry(), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected exit code 1, got %d", code)
	}
	if !strings.Contains(stderr.String(), "usage: aileron sandbox") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestRunSandboxInitRejectsAgentFlag(t *testing.T) {
	// The --agent flag was removed (no deprecation). The standard flag parser
	// rejects the unknown flag and surfaces the new usage text without writing
	// any scaffold.
	for _, arg := range []string{"--agent=claude", "--agent=anything"} {
		dir := t.TempDir()
		oldWd, _ := os.Getwd()
		if err := os.Chdir(dir); err != nil {
			t.Fatalf("chdir: %v", err)
		}
		t.Cleanup(func() { _ = os.Chdir(oldWd) })

		var stdout, stderr bytes.Buffer
		code := run([]string{"sandbox", "init", arg}, newTestRegistry(), &stdout, &stderr)
		if code != 1 {
			t.Fatalf("%s: expected exit code 1, got %d; stderr=%q", arg, code, stderr.String())
		}
		if !strings.Contains(stderr.String(), "flag provided but not defined: -agent") {
			t.Fatalf("%s: stderr missing flag error: %q", arg, stderr.String())
		}
		if _, err := os.Stat(filepath.Join(dir, ".devcontainer", "devcontainer.json")); !os.IsNotExist(err) {
			t.Fatalf("%s: scaffold should not be written when the flag is rejected: stat err = %v", arg, err)
		}
	}
}

func TestRunSandboxInitRejectsExtraArgs(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"sandbox", "init", "extra"}, newTestRegistry(), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected exit code 1, got %d", code)
	}
	if !strings.Contains(stderr.String(), "usage: aileron sandbox init") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestRunSandboxInitSurfacesExistingFiles(t *testing.T) {
	dir := t.TempDir()
	oldWd, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWd) })

	var stdout, stderr bytes.Buffer
	if code := run([]string{"sandbox", "init"}, newTestRegistry(), &stdout, &stderr); code != 0 {
		t.Fatalf("first init failed: %d stderr=%q", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	code := run([]string{"sandbox", "init"}, newTestRegistry(), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected exit code 1, got %d", code)
	}
	if !strings.Contains(stderr.String(), "--force") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestRunSandboxPlanRejectsExtraArgs(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"sandbox", "plan", "extra"}, newTestRegistry(), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected exit code 1, got %d", code)
	}
	if !strings.Contains(stderr.String(), "usage: aileron sandbox plan") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestRunSandboxCheckValidatesAgentCommand(t *testing.T) {
	dir := t.TempDir()
	oldWd, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWd) })
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}

	origBuild := sandboxCheckBuildFn
	origValidate := sandboxCheckValidateFn
	t.Cleanup(func() {
		sandboxCheckBuildFn = origBuild
		sandboxCheckValidateFn = origValidate
	})

	var capturedPolicy string
	sandboxCheckBuildFn = func(_ context.Context, runtimeName string, _, _ io.Writer, opts sandboxcontainer.BuildOptions) (sandboxcontainer.BuildResult, error) {
		if runtimeName != "docker" {
			t.Fatalf("runtimeName = %q, want docker", runtimeName)
		}
		capturedPolicy = opts.Policy
		return sandboxcontainer.BuildResult{
			Runtime: "docker",
			Image:   "ghcr.io/acme/agent:latest",
			Tier:    sandboxcomposition.TierBYOImage,
		}, nil
	}
	var capturedCommand string
	sandboxCheckValidateFn = func(_ context.Context, runtimeName, workDir, image, command string, _ bool) error {
		if runtimeName != "docker" || workDir != cwd || image != "ghcr.io/acme/agent:latest" {
			t.Fatalf("validate args = %q %q %q", runtimeName, workDir, image)
		}
		capturedCommand = command
		return nil
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"sandbox", "check", "--runtime=docker", "--build=never", "claude"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit code 0, got %d stderr=%q", code, stderr.String())
	}
	if capturedPolicy != "never" {
		t.Fatalf("build policy = %q, want never", capturedPolicy)
	}
	if capturedCommand != "claude" {
		t.Fatalf("command = %q, want claude", capturedCommand)
	}
	for _, want := range []string{
		"tier: byo_image",
		"runtime: docker",
		"image: ghcr.io/acme/agent:latest",
		"agent: claude",
		"support: ok",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("stdout = %q, want %q", stdout.String(), want)
		}
	}
}

func TestRunSandboxCheckRequiresAgentCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"sandbox", "check"}, newTestRegistry(), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected exit code 1, got %d", code)
	}
	if !strings.Contains(stderr.String(), "usage: aileron sandbox check") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestRunSandboxCheckRewritesBuildPolicyHint(t *testing.T) {
	dir := t.TempDir()
	oldWd, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWd) })

	origBuild := sandboxCheckBuildFn
	origValidate := sandboxCheckValidateFn
	t.Cleanup(func() {
		sandboxCheckBuildFn = origBuild
		sandboxCheckValidateFn = origValidate
	})
	sandboxCheckBuildFn = func(context.Context, string, io.Writer, io.Writer, sandboxcontainer.BuildOptions) (sandboxcontainer.BuildResult, error) {
		return sandboxcontainer.BuildResult{}, errors.New("use --sandbox-build=auto")
	}
	sandboxCheckValidateFn = func(context.Context, string, string, string, string, bool) error {
		t.Fatal("validate should not run after build error")
		return nil
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"sandbox", "check", "--agent=claude"}, newTestRegistry(), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected exit code 1, got %d", code)
	}
	if !strings.Contains(stderr.String(), "--build=auto") || strings.Contains(stderr.String(), "--sandbox-build") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestRunSandboxBuildRejectsExtraArgs(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"sandbox", "build", "extra"}, newTestRegistry(), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected exit code 1, got %d", code)
	}
	if !strings.Contains(stderr.String(), "usage: aileron sandbox build") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestRunSandboxBuildRejectsInvalidFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"sandbox", "build", "--bogus"}, newTestRegistry(), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected exit code 1, got %d", code)
	}
	if !strings.Contains(stderr.String(), "flag provided but not defined") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestRunSandboxBuildReportsComplete(t *testing.T) {
	dir := t.TempDir()
	oldWd, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWd) })

	origBuild := sandboxBuildFn
	t.Cleanup(func() { sandboxBuildFn = origBuild })
	sandboxBuildFn = func(_ context.Context, runtimeName string, buildStdout, _ io.Writer, opts sandboxcontainer.BuildOptions) (sandboxcontainer.BuildResult, error) {
		if runtimeName != "docker" {
			t.Fatalf("runtimeName = %q, want docker", runtimeName)
		}
		if opts.Tag != "ghcr.io/acme/sandbox:test" {
			t.Fatalf("Tag = %q", opts.Tag)
		}
		if opts.Plan.Tier != "base" {
			t.Fatalf("Plan.Tier = %q, want base", opts.Plan.Tier)
		}
		if _, ok := buildStdout.(*bytes.Buffer); !ok {
			t.Fatalf("stdout writer = %T, want *bytes.Buffer", buildStdout)
		}
		return sandboxcontainer.BuildResult{
			Runtime: runtimeName,
			Image:   opts.Tag,
			Built:   true,
			Tier:    opts.Plan.Tier,
		}, nil
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"sandbox", "build", "--runtime=docker", "--tag=ghcr.io/acme/sandbox:test"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit code 0, got %d; stderr=%q", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"tier: base", "runtime: docker", "image: ghcr.io/acme/sandbox:test", "build: complete"} {
		if !strings.Contains(out, want) {
			t.Fatalf("stdout = %q, missing %q", out, want)
		}
	}
}

func TestRunSandboxBuildReportsNoBuildRequired(t *testing.T) {
	dir := t.TempDir()
	oldWd, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWd) })
	if err := os.MkdirAll(filepath.Join(dir, ".devcontainer"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".devcontainer", "devcontainer.json"), []byte(`{"customizations":{"aileron":{"image":"ghcr.io/acme/agent:latest"}}}`), 0o644); err != nil {
		t.Fatalf("write devcontainer: %v", err)
	}

	origBuild := sandboxBuildFn
	t.Cleanup(func() { sandboxBuildFn = origBuild })
	sandboxBuildFn = func(_ context.Context, _ string, _, _ io.Writer, opts sandboxcontainer.BuildOptions) (sandboxcontainer.BuildResult, error) {
		return sandboxcontainer.BuildResult{
			Image: opts.Plan.Image,
			Tier:  opts.Plan.Tier,
		}, sandboxcontainer.ErrNoBuildRequired
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"sandbox", "build"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit code 0, got %d; stderr=%q", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "tier: byo_image") || !strings.Contains(out, "image: ghcr.io/acme/agent:latest") || !strings.Contains(out, "build: not required") {
		t.Fatalf("stdout = %q", out)
	}
}

func TestRunSandboxBuildWithDefaultBuilderReportsNoBuildRequired(t *testing.T) {
	dir := t.TempDir()
	oldWd, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWd) })
	if err := os.MkdirAll(filepath.Join(dir, ".devcontainer"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".devcontainer", "devcontainer.json"), []byte(`{"customizations":{"aileron":{"image":"ghcr.io/acme/agent:latest"}}}`), 0o644); err != nil {
		t.Fatalf("write devcontainer: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"sandbox", "build"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit code 0, got %d; stderr=%q", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "tier: byo_image") || !strings.Contains(out, "image: ghcr.io/acme/agent:latest") || !strings.Contains(out, "build: not required") {
		t.Fatalf("stdout = %q", out)
	}
}

func TestRunSandboxBuildSurfacesBuildError(t *testing.T) {
	dir := t.TempDir()
	oldWd, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWd) })

	origBuild := sandboxBuildFn
	t.Cleanup(func() { sandboxBuildFn = origBuild })
	sandboxBuildFn = func(context.Context, string, io.Writer, io.Writer, sandboxcontainer.BuildOptions) (sandboxcontainer.BuildResult, error) {
		return sandboxcontainer.BuildResult{}, errors.New("runtime unavailable")
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"sandbox", "build"}, newTestRegistry(), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected exit code 1, got %d", code)
	}
	if !strings.Contains(stderr.String(), "runtime unavailable") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestRunSandboxBuildSurfacesParseError(t *testing.T) {
	dir := t.TempDir()
	oldWd, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWd) })
	if err := os.MkdirAll(filepath.Join(dir, ".devcontainer"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".devcontainer", "devcontainer.json"), []byte(`{"customizations":{"aileron":{"approval_surface":"sms"}}}`), 0o644); err != nil {
		t.Fatalf("write devcontainer: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"sandbox", "build"}, newTestRegistry(), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected exit code 1, got %d", code)
	}
	if !strings.Contains(stderr.String(), "approval_surface") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestRunSandboxPlanSurfacesParseError(t *testing.T) {
	dir := t.TempDir()
	oldWd, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWd) })
	if err := os.MkdirAll(filepath.Join(dir, ".devcontainer"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".devcontainer", "devcontainer.json"), []byte(`{"customizations":{"aileron":{"approval_surface":"sms"}}}`), 0o644); err != nil {
		t.Fatalf("write devcontainer: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"sandbox", "plan"}, newTestRegistry(), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected exit code 1, got %d", code)
	}
	if !strings.Contains(stderr.String(), "approval_surface") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestRunStatus_All(t *testing.T) {
	dir := setTestHome(t)
	oldWd, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(oldWd)

	var stdout, stderr bytes.Buffer
	code := run([]string{"status"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Errorf("expected exit code 0, got %d; stderr: %s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "Runtime") {
		t.Error("expected Runtime section")
	}
	if !strings.Contains(out, "Notifications") {
		t.Error("expected Notifications section")
	}
	if !strings.Contains(out, "Vault") {
		t.Error("expected Vault section")
	}
}

func TestRunStatus_Notifications(t *testing.T) {
	dir := setTestHome(t)
	configDir := filepath.Join(dir, ".aileron")
	os.MkdirAll(configDir, 0o755)
	os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte(`
notifications:
  quiet_hours:
    start: "22:00"
    end: "06:00"
    timezone: America/New_York
`), 0o644)

	oldWd, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(oldWd)

	var stdout, stderr bytes.Buffer
	code := run([]string{"status", "notifications"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Errorf("expected exit code 0, got %d", code)
	}
	out := stdout.String()
	if !strings.Contains(out, "Quiet hours") {
		t.Error("expected Quiet hours section")
	}
	if !strings.Contains(out, "22:00") {
		t.Error("expected quiet hours start time")
	}
}

func TestRunStatus_Vault(t *testing.T) {
	setTestHome(t)

	var stdout, stderr bytes.Buffer
	code := run([]string{"status", "vault"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Errorf("expected exit code 0, got %d", code)
	}
	out := stdout.String()
	if !strings.Contains(out, "not created") {
		t.Error("expected 'not created' for missing vault")
	}
}

func TestRunStatus_VaultWithSecrets(t *testing.T) {
	dir := setTestHome(t)

	vaultPath := filepath.Join(dir, ".aileron", "secrets.json")
	os.MkdirAll(filepath.Dir(vaultPath), 0o700)
	os.WriteFile(vaultPath, []byte(`{"salt":"AAAA","secrets":{"slack_bot":{"value":"ZW5j","metadata":{"type":"secret"}}}}`), 0o600)

	var stdout, stderr bytes.Buffer
	code := run([]string{"status", "vault"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Errorf("expected exit code 0, got %d", code)
	}
	out := stdout.String()
	if !strings.Contains(out, "1 stored") {
		t.Error("expected '1 stored'")
	}
	if !strings.Contains(out, "slack_bot") {
		t.Error("expected secret name")
	}
}

func TestRunStatus_UnknownSection(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"status", "bogus"}, newTestRegistry(), &stdout, &stderr)
	if code != 1 {
		t.Errorf("expected exit code 1, got %d", code)
	}
	if !strings.Contains(stderr.String(), "unknown status section") {
		t.Error("expected unknown section error")
	}
}

func TestRunStatus_InHelp(t *testing.T) {
	var stdout bytes.Buffer
	run([]string{"help"}, newTestRegistry(), &stdout, &bytes.Buffer{})
	if !strings.Contains(stdout.String(), "aileron status") {
		t.Error("expected 'aileron status' in help output")
	}
}

// TestRunStatus_RuntimeUnreachable covers the offline path: with no
// daemon listening, `aileron status runtime` must exit cleanly and
// surface a "(not reachable)" line that names the underlying error.
// The CLI is a thin client; users run it from arbitrary shells where
// the daemon may or may not be up.
//
// Locks in the post-#489 behavior: the unreachable line carries the
// fetcher's error verbatim, not a hardcoded URL like the legacy
// `http://localhost:8721/v1` fallback that `bindingAPIBaseURL` used
// to return. The legacy port has no meaning under ADR-0012.
func TestRunStatus_RuntimeUnreachable(t *testing.T) {
	// Force the fetcher to fail so the test doesn't depend on the host
	// having an actual server listening on the default port.
	prev := runtimeStatusFetcher
	runtimeStatusFetcher = func() (*runtimeStatus, error) {
		return nil, fmt.Errorf("daemon binary missing")
	}
	defer func() { runtimeStatusFetcher = prev }()

	var stdout, stderr bytes.Buffer
	code := run([]string{"status", "runtime"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Errorf("expected exit code 0, got %d; stderr: %s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "Runtime") {
		t.Error("expected Runtime header")
	}
	if !strings.Contains(out, "not reachable") {
		t.Errorf("expected 'not reachable' hint when daemon is down; got: %s", out)
	}
	if !strings.Contains(out, "daemon binary missing") {
		t.Errorf("expected fetcher error to surface in output; got: %s", out)
	}
	if strings.Contains(out, "8721") {
		t.Errorf("output should not name the legacy port 8721; got: %s", out)
	}
	if strings.Contains(out, "aileron serve") {
		t.Errorf("output should not suggest 'aileron serve' (deprecated under ADR-0012); got: %s", out)
	}
}

// TestRunStatus_RuntimeReachable asserts the happy path: when the
// daemon answers /v1/status, the CLI surfaces version + counts +
// vault state. This is the primary `aileron status` deliverable from
// ADR-0004 (operational primitives).
func TestRunStatus_RuntimeReachable(t *testing.T) {
	commit := "abc1234"
	listen := "127.0.0.1:8721"
	gw := "http://127.0.0.1:54321"
	prev := runtimeStatusFetcher
	runtimeStatusFetcher = func() (*runtimeStatus, error) {
		return &runtimeStatus{
			Version:        "v0.0.42",
			Commit:         &commit,
			ListenAddr:     &listen,
			ActionCount:    3,
			ConnectorCount: 2,
			BindingCount:   5,
			VaultState:     "unlocked",
			GatewayUrl:     &gw,
		}, nil
	}
	defer func() { runtimeStatusFetcher = prev }()

	var stdout, stderr bytes.Buffer
	code := run([]string{"status", "runtime"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Errorf("expected exit code 0, got %d; stderr: %s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"v0.0.42", "abc1234", "127.0.0.1:8721", "http://127.0.0.1:54321", "unlocked", "3 installed", "2 installed", "5 active"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in runtime output; got:\n%s", want, out)
		}
	}
}

// TestRunStatus_RuntimeIncludedInDefault asserts the bare `aileron
// status` (no section) renders the Runtime header alongside the
// existing sections. Operators reach for `aileron status` first; the
// daemon snapshot must be there without an extra subcommand.
func TestRunStatus_RuntimeIncludedInDefault(t *testing.T) {
	dir := setTestHome(t)
	oldWd, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(oldWd)

	prev := runtimeStatusFetcher
	runtimeStatusFetcher = func() (*runtimeStatus, error) {
		return &runtimeStatus{Version: "v0.0.42", VaultState: "missing"}, nil
	}
	defer func() { runtimeStatusFetcher = prev }()

	var stdout, stderr bytes.Buffer
	code := run([]string{"status"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Errorf("expected exit code 0, got %d; stderr: %s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "Runtime") {
		t.Error("expected Runtime section in default status output")
	}
	if !strings.Contains(out, "v0.0.42") {
		t.Error("expected daemon version in default status output")
	}
}

func TestRunSecret_NoSubcommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"secret"}, newTestRegistry(), &stdout, &stderr)
	if code != 1 {
		t.Errorf("expected exit code 1, got %d", code)
	}
	if !strings.Contains(stderr.String(), "usage: aileron secret") {
		t.Errorf("expected usage message, got: %s", stderr.String())
	}
}

func TestRunSecret_UnknownSubcommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"secret", "bogus"}, newTestRegistry(), &stdout, &stderr)
	if code != 1 {
		t.Errorf("expected exit code 1, got %d", code)
	}
	if !strings.Contains(stderr.String(), `unknown secret command: "bogus"`) {
		t.Errorf("expected unknown command error, got: %s", stderr.String())
	}
}

func TestRunSecret_SetNoName(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"secret", "set"}, newTestRegistry(), &stdout, &stderr)
	if code != 1 {
		t.Errorf("expected exit code 1, got %d", code)
	}
	if !strings.Contains(stderr.String(), "usage: aileron secret set") {
		t.Errorf("expected usage message, got: %s", stderr.String())
	}
}

func TestRunSecret_ListEmpty(t *testing.T) {
	setTestHome(t)

	var stdout, stderr bytes.Buffer
	code := run([]string{"secret", "list"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Errorf("expected exit code 0, got %d; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "No secrets stored") {
		t.Errorf("expected 'No secrets stored', got: %s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "aileron secret set") {
		t.Errorf("expected next-step hint mentioning `aileron secret set`, got: %s", stdout.String())
	}
}

func TestRunSecret_ListWithSecrets(t *testing.T) {
	dir := setTestHome(t)

	// Pre-populate the vault file directly (skip encryption for test).
	vaultPath := filepath.Join(dir, ".aileron", "secrets.json")
	os.MkdirAll(filepath.Dir(vaultPath), 0o700)
	os.WriteFile(vaultPath, []byte(`{"salt":"AAAA","secrets":{"slack_bot_token":{"value":"ZW5j","metadata":{"type":"secret"}},"discord_token":{"value":"ZW5j","metadata":{"type":"secret"}}}}`), 0o600)

	var stdout, stderr bytes.Buffer
	code := run([]string{"secret", "list"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Errorf("expected exit code 0, got %d; stderr: %s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "slack_bot_token") {
		t.Errorf("expected 'slack_bot_token' in output, got: %s", out)
	}
	if !strings.Contains(out, "discord_token") {
		t.Errorf("expected 'discord_token' in output, got: %s", out)
	}
}

// TestRunSecret_ListJSON_Empty: --json on an empty vault emits `[]`,
// not the human-targeted prose. Lets scripts detect "nothing here yet"
// without grepping for "No secrets stored".
func TestRunSecret_ListJSON_Empty(t *testing.T) {
	setTestHome(t)

	var stdout, stderr bytes.Buffer
	code := run([]string{"secret", "list", "--json"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	if got := strings.TrimRight(stdout.String(), "\n"); got != "[]" {
		t.Errorf("stdout = %q, want %q", got, "[]")
	}
}

// TestRunSecret_ListJSON_NDJSON: --json with secrets emits one
// JSON-encoded name per line.
func TestRunSecret_ListJSON_NDJSON(t *testing.T) {
	dir := setTestHome(t)

	vaultPath := filepath.Join(dir, ".aileron", "secrets.json")
	if err := os.MkdirAll(filepath.Dir(vaultPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(vaultPath, []byte(`{"salt":"AAAA","secrets":{"a":{"value":"ZW5j","metadata":{}},"b":{"value":"ZW5j","metadata":{}}}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"secret", "list", "--json"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	lines := strings.Split(strings.TrimRight(stdout.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2: %q", len(lines), stdout.String())
	}
	got := map[string]bool{}
	for _, line := range lines {
		var name string
		if err := json.Unmarshal([]byte(line), &name); err != nil {
			t.Errorf("line %q is not JSON: %v", line, err)
		}
		got[name] = true
	}
	for _, want := range []string{"a", "b"} {
		if !got[want] {
			t.Errorf("missing name %q in: %v", want, got)
		}
	}
}

// `aileron binding list` reads vault metadata without unlocking, per
// ADR-0011 acceptance: metadata is plaintext on disk, so the user
// can inspect what's bound before paying the passphrase prompt.

// fakeBindingServer stands up an in-process httptest.NewServer that
// implements just enough of the /v1/bindings surface to drive the
// CLI tests. Each test scopes AILERON_API_URL to this server's URL.
func fakeBindingServer(t *testing.T, fn http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.StripPrefix("/v1", fn))
	t.Cleanup(srv.Close)
	t.Setenv("AILERON_API_URL", srv.URL+"/v1")
	return srv
}

func TestRunBinding_ListEmpty(t *testing.T) {
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/bindings" {
			t.Errorf("path = %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"items":[]}`)
	})
	var stdout, stderr bytes.Buffer
	code := run([]string{"binding", "list"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Errorf("exit code = %d; stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "No bindings configured.") {
		t.Errorf("output = %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), "aileron binding setup") {
		t.Errorf("expected next-step hint mentioning `aileron binding setup`, got: %s", stdout.String())
	}
}

// TestRunBinding_ListJSON_Empty: --json on the empty case emits `[]`,
// not the human "No bindings configured." text. Scripts can detect the
// empty set with a JSON parser instead of grepping prose.
func TestRunBinding_ListJSON_Empty(t *testing.T) {
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"items":[]}`)
	})
	var stdout, stderr bytes.Buffer
	code := run([]string{"binding", "list", "--json"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	if got := strings.TrimRight(stdout.String(), "\n"); got != "[]" {
		t.Errorf("stdout = %q, want %q", got, "[]")
	}
}

// TestRunBinding_ListJSON_NDJSON: --json with rows emits one
// JSON-encoded binding per line, round-trippable through json.Decode.
func TestRunBinding_ListJSON_NDJSON(t *testing.T) {
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"items":[
			{"name":"api_key/linear/team","kind":"api_key","service":"linear","identity":"team","connector_fqn":"github://aileron/linear","status":"active","created_at":"2024-01-01T00:00:00Z"},
			{"name":"oauth2/slack/work","kind":"oauth2","service":"slack","identity":"work","connector_fqn":"github://aileron/slack","status":"active","created_at":"2024-01-01T00:00:00Z"}
		]}`)
	})
	var stdout, stderr bytes.Buffer
	code := run([]string{"binding", "list", "--json"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	lines := strings.Split(strings.TrimRight(stdout.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2: %q", len(lines), stdout.String())
	}
	for _, line := range lines {
		var b bindingRow
		if err := json.Unmarshal([]byte(line), &b); err != nil {
			t.Errorf("line %q is not JSON: %v", line, err)
		}
	}
}

// TestRunBinding_UsageAdvertisesJSON: bindingUsage gained `[--json]`
// in #492 item 2; the user-discovery path is `aileron binding` with no
// subcommand, which prints the usage to stderr. Pins the contract that
// the flag is documented in that surface.
func TestRunBinding_UsageAdvertisesJSON(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"binding"}, newTestRegistry(), &stdout, &stderr)
	if code == 0 {
		t.Error("expected nonzero exit for no subcommand")
	}
	if !strings.Contains(stderr.String(), "--json") {
		t.Errorf("usage missing `--json`:\n%s", stderr.String())
	}
}

func TestRunBinding_ListShowsTable(t *testing.T) {
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"items":[
			{"name":"api_key/linear/team","kind":"api_key","service":"linear","identity":"team","connector_fqn":"github://aileron/linear","status":"active","created_at":"2024-01-01T00:00:00Z"},
			{"name":"oauth2/slack/work","kind":"oauth2","service":"slack","identity":"work","connector_fqn":"github://aileron/slack","status":"active","created_at":"2024-01-01T00:00:00Z"}
		]}`)
	})
	var stdout, stderr bytes.Buffer
	code := run([]string{"binding", "list"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Errorf("exit code = %d; stderr = %s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"NAME", "KIND", "STATUS",
		"api_key/linear/team", "oauth2/slack/work", "github://aileron/linear",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestRunBinding_ListPropagatesFilters(t *testing.T) {
	var seenQuery url.Values
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		seenQuery = r.URL.Query()
		_, _ = io.WriteString(w, `{"items":[]}`)
	})
	var stdout, stderr bytes.Buffer
	run([]string{"binding", "list", "--connector", "github://aileron/linear", "--kind", "api_key"},
		newTestRegistry(), &stdout, &stderr)
	if got := seenQuery.Get("connector_fqn"); got != "github://aileron/linear" {
		t.Errorf("connector_fqn = %q", got)
	}
	if got := seenQuery.Get("kind"); got != "api_key" {
		t.Errorf("kind = %q", got)
	}
}

func TestRunBinding_InspectPrintsDetails(t *testing.T) {
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/bindings/api_key/linear/team" {
			t.Errorf("path = %q", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{
			"name":"api_key/linear/team","kind":"api_key","service":"linear","identity":"team",
			"connector_fqn":"github://aileron/linear","scope":"issues:write","account":"alr@x",
			"created_at":"2024-01-01T12:00:00Z","status":"active"
		}`)
	})
	var stdout, stderr bytes.Buffer
	code := run([]string{"binding", "inspect", "api_key/linear/team"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Errorf("exit code = %d; stderr = %s", code, stderr.String())
	}
	for _, want := range []string{
		"Name:       api_key/linear/team",
		"Kind:       api_key",
		"Connector:  github://aileron/linear",
		"Account:    alr@x",
		"Scope:      issues:write",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("output missing %q:\n%s", want, stdout.String())
		}
	}
}

func TestRunBinding_InspectNotFoundIsExit1(t *testing.T) {
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":{"code":"not_found"}}`)
	})
	var stdout, stderr bytes.Buffer
	code := run([]string{"binding", "inspect", "api_key/missing/x"}, newTestRegistry(), &stdout, &stderr)
	if code == 0 {
		t.Error("expected nonzero exit code")
	}
}

func TestRunBinding_SetupSendsAPIKeyBody(t *testing.T) {
	var got struct {
		ConnectorFQN string `json:"connector_fqn"`
		Bindings     []struct {
			Identity string `json:"identity"`
			Source   struct {
				Kind  string `json:"kind"`
				Value string `json:"value"`
			} `json:"source"`
		} `json:"bindings"`
	}
	// CLI now probes oauth2/init first; the connector under test is
	// api_key, so the server returns 422 not_oauth2 and the CLI falls
	// through to the api_key flow.
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/bindings/setup/oauth2/init":
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = io.WriteString(w, `{"error":{"code":"not_oauth2","message":"connector declares api_key"}}`)
		case "/bindings/setup":
			if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
				t.Fatalf("decode: %v", err)
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"created":[{"name":"api_key/linear/team","kind":"api_key","service":"linear","identity":"team","connector_fqn":"github://aileron/linear","created_at":"2024-01-01T00:00:00Z"}]}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	})
	stdin := strings.NewReader("team\nlin-secret\n")
	var stdout, stderr bytes.Buffer
	code := runBinding([]string{"setup", "github://aileron/linear"}, stdin, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d; stderr = %s", code, stderr.String())
	}
	if got.ConnectorFQN != "github://aileron/linear" {
		t.Errorf("connector_fqn = %q", got.ConnectorFQN)
	}
	if len(got.Bindings) != 1 {
		t.Fatalf("len(bindings) = %d", len(got.Bindings))
	}
	if got.Bindings[0].Identity != "team" || got.Bindings[0].Source.Kind != "api_key" ||
		got.Bindings[0].Source.Value != "lin-secret" {
		t.Errorf("body = %+v", got)
	}
	if !strings.Contains(stdout.String(), "Created: api_key/linear/team") {
		t.Errorf("stdout missing created line: %s", stdout.String())
	}
}

func TestRunBinding_SetupRejectsEmptyValue(t *testing.T) {
	// init returns 422 not_oauth2 → CLI falls through and rejects
	// blank api_key value before hitting /bindings/setup.
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/bindings/setup/oauth2/init" {
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = io.WriteString(w, `{"error":{"code":"not_oauth2"}}`)
			return
		}
		t.Error("/bindings/setup should not be hit when value is empty")
	})
	stdin := strings.NewReader("team\n\n") // identity provided, value empty
	var stdout, stderr bytes.Buffer
	code := runBinding([]string{"setup", "github://aileron/linear"}, stdin, &stdout, &stderr)
	if code == 0 {
		t.Error("expected nonzero exit when value is blank")
	}
}

func TestRunBinding_RebindPostsValue(t *testing.T) {
	var seenBody []byte
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/bindings/api_key/linear/team/rebind" {
			t.Errorf("path = %s", r.URL.Path)
		}
		seenBody, _ = io.ReadAll(r.Body)
		_, _ = io.WriteString(w, `{"name":"api_key/linear/team","kind":"api_key","service":"linear","identity":"team","connector_fqn":"github://aileron/linear","created_at":"2024-01-01T00:00:00Z"}`)
	})
	stdin := strings.NewReader("new-secret\n")
	var stdout, stderr bytes.Buffer
	code := runBinding([]string{"rebind", "api_key/linear/team"}, stdin, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d; stderr = %s", code, stderr.String())
	}
	if !strings.Contains(string(seenBody), `"value":"new-secret"`) {
		t.Errorf("body = %s", seenBody)
	}
	if !strings.Contains(stdout.String(), "Rebound: api_key/linear/team") {
		t.Errorf("stdout: %s", stdout.String())
	}
}

func TestRunBinding_RevokeRequiresConfirmation(t *testing.T) {
	hits := 0
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusNoContent)
	})
	// Cancel.
	var stdout, stderr bytes.Buffer
	code := runBinding([]string{"revoke", "api_key/linear/team"}, strings.NewReader("n\n"), &stdout, &stderr)
	if code != 0 {
		t.Errorf("cancel exit = %d", code)
	}
	if hits != 0 {
		t.Errorf("server called %d times on cancel", hits)
	}
	if !strings.Contains(stdout.String(), "cancelled") {
		t.Errorf("missing cancel message: %s", stdout.String())
	}

	// Confirm.
	stdout.Reset()
	stderr.Reset()
	code = runBinding([]string{"revoke", "api_key/linear/team"}, strings.NewReader("y\n"), &stdout, &stderr)
	if code != 0 {
		t.Errorf("confirm exit = %d; stderr = %s", code, stderr.String())
	}
	if hits != 1 {
		t.Errorf("server called %d times on confirm", hits)
	}
	if !strings.Contains(stdout.String(), "Revoked: api_key/linear/team") {
		t.Errorf("missing revoked line: %s", stdout.String())
	}
}

func TestRunBinding_ListServerErrorIsExit1(t *testing.T) {
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":{"code":"oops"}}`)
	})
	var stdout, stderr bytes.Buffer
	code := run([]string{"binding", "list"}, newTestRegistry(), &stdout, &stderr)
	if code == 0 {
		t.Errorf("expected nonzero exit; stdout = %s", stdout.String())
	}
}

func TestRunBinding_SetupServerErrorIsExit1(t *testing.T) {
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"code":"oauth_setup_not_yet_supported"}}`)
	})
	var stdout, stderr bytes.Buffer
	code := runBinding([]string{"setup", "github://aileron/x"},
		strings.NewReader("work\nval\n"), &stdout, &stderr)
	if code == 0 {
		t.Errorf("expected nonzero exit; stderr = %s", stderr.String())
	}
}

func TestRunBinding_RebindNotFoundIsExit1(t *testing.T) {
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{}`)
	})
	var stdout, stderr bytes.Buffer
	code := runBinding([]string{"rebind", "api_key/x/y"},
		strings.NewReader("v\n"), &stdout, &stderr)
	if code == 0 {
		t.Errorf("expected nonzero exit; stderr = %s", stderr.String())
	}
}

func TestRunBinding_RevokeServerErrorIsExit1(t *testing.T) {
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{}`)
	})
	var stdout, stderr bytes.Buffer
	code := runBinding([]string{"revoke", "api_key/x/y"},
		strings.NewReader("y\n"), &stdout, &stderr)
	if code == 0 {
		t.Errorf("expected nonzero exit; stderr = %s", stderr.String())
	}
}

func TestRunBinding_RevokeNotFoundIsExit1(t *testing.T) {
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{}`)
	})
	var stdout, stderr bytes.Buffer
	code := runBinding([]string{"revoke", "api_key/x/y"},
		strings.NewReader("y\n"), &stdout, &stderr)
	if code == 0 {
		t.Error("expected nonzero exit on 404")
	}
}

func TestRunBinding_SetupRequiresIdentity(t *testing.T) {
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("server should not be hit when CLI rejects empty identity")
	})
	var stdout, stderr bytes.Buffer
	code := runBinding([]string{"setup", "github://aileron/x"},
		strings.NewReader("\n"), &stdout, &stderr)
	if code == 0 {
		t.Error("expected nonzero exit for empty identity")
	}
}

func TestRunBinding_RebindRequiresValue(t *testing.T) {
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("server should not be hit when CLI rejects empty value")
	})
	var stdout, stderr bytes.Buffer
	code := runBinding([]string{"rebind", "api_key/x/y"},
		strings.NewReader("\n"), &stdout, &stderr)
	if code == 0 {
		t.Error("expected nonzero exit for empty value")
	}
}

func TestRunBinding_InspectRequiresExactlyOneArg(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runBinding([]string{"inspect"}, strings.NewReader(""), &stdout, &stderr)
	if code == 0 {
		t.Error("inspect with no name accepted")
	}
}

func TestRunBinding_SetupRequiresExactlyOneArg(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runBinding([]string{"setup"}, strings.NewReader(""), &stdout, &stderr)
	if code == 0 {
		t.Error("setup with no FQN accepted")
	}
}

func TestRunBinding_RebindRequiresExactlyOneArg(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runBinding([]string{"rebind"}, strings.NewReader(""), &stdout, &stderr)
	if code == 0 {
		t.Error("rebind with no name accepted")
	}
}

func TestRunBinding_RevokeRequiresExactlyOneArg(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runBinding([]string{"revoke"}, strings.NewReader(""), &stdout, &stderr)
	if code == 0 {
		t.Error("revoke with no name accepted")
	}
}

func TestRunBinding_TransportErrorIsExit1(t *testing.T) {
	// Point at a closed listener so the HTTP request fails at dial.
	t.Setenv("AILERON_API_URL", "http://127.0.0.1:1/v1")
	for _, args := range [][]string{
		{"binding", "list"},
		{"binding", "inspect", "x"},
	} {
		var stdout, stderr bytes.Buffer
		code := run(args, newTestRegistry(), &stdout, &stderr)
		if code == 0 {
			t.Errorf("%v: expected nonzero exit on transport error", args)
		}
	}
}

func TestRunBinding_ListBadFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runBinding([]string{"list", "--bogus"}, strings.NewReader(""), &stdout, &stderr)
	if code == 0 {
		t.Error("expected nonzero exit on bad flag")
	}
}

func TestRunBinding_InvalidJSONFromServerIsExit1(t *testing.T) {
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "not json")
	})
	for _, args := range [][]string{
		{"binding", "list"},
		{"binding", "inspect", "x"},
	} {
		var stdout, stderr bytes.Buffer
		code := run(args, newTestRegistry(), &stdout, &stderr)
		if code == 0 {
			t.Errorf("%v: expected nonzero exit on bad JSON", args)
		}
	}
}

func TestRunBinding_SetupParsesPartialResponse(t *testing.T) {
	// Server returns 201 but with garbage body — CLI should exit 1
	// rather than crash trying to render the response.
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, "not json")
	})
	stdin := strings.NewReader("team\nval\n")
	var stdout, stderr bytes.Buffer
	code := runBinding([]string{"setup", "github://aileron/x"}, stdin, &stdout, &stderr)
	if code == 0 {
		t.Error("expected nonzero exit on bad response body")
	}
}

func TestBindingAPIBaseURL_OverrideTrimsTrailingSlash(t *testing.T) {
	t.Setenv("AILERON_API_URL", "https://example.com/v1/")
	got, err := bindingAPIBaseURL()
	if err != nil {
		t.Fatalf("override should not error: %v", err)
	}
	if got != "https://example.com/v1" {
		t.Errorf("override = %q, want trimmed", got)
	}
}

func TestDaemonAuthTokenPrefersEnvironment(t *testing.T) {
	setTestHome(t)
	t.Setenv("AILERON_TOKEN", "tok_env")
	if got := daemonAuthToken(); got != "tok_env" {
		t.Fatalf("daemonAuthToken = %q, want tok_env", got)
	}
}

func TestDaemonAuthTokenReadsDiscovery(t *testing.T) {
	home := setTestHome(t)
	stateDir := filepath.Join(home, ".aileron")
	if err := discovery.Write(stateDir, discovery.Info{
		URL:   "http://127.0.0.1:8721",
		Token: "tok_file",
	}); err != nil {
		t.Fatalf("write discovery: %v", err)
	}
	if got := daemonAuthToken(); got != "tok_file" {
		t.Fatalf("daemonAuthToken = %q, want tok_file", got)
	}
}

func TestSetDaemonAuthorization(t *testing.T) {
	setTestHome(t)
	t.Setenv("AILERON_TOKEN", "tok_req")
	req := httptest.NewRequest(http.MethodGet, "http://daemon.test/v1/status", nil)

	setDaemonAuthorization(req)

	if got := req.Header.Get("Authorization"); got != "Bearer tok_req" {
		t.Fatalf("Authorization = %q, want bearer token", got)
	}
}

func TestBindingDoRequestSendsDaemonAuthorization(t *testing.T) {
	t.Setenv("AILERON_TOKEN", "tok_binding")
	var sawAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	t.Setenv("AILERON_API_URL", srv.URL+"/v1")

	status, _, err := bindingDoRequest(http.MethodGet, "/bindings", nil)
	if err != nil {
		t.Fatalf("bindingDoRequest: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if sawAuth != "Bearer tok_binding" {
		t.Fatalf("Authorization = %q, want bearer token", sawAuth)
	}
}

// TestBindingAPIBaseURL_PropagatesSpawnErrorThroughCallers locks in
// the post-#490 contract: every helper that previously dialed the
// hardcoded `localhost:8721` fallback now propagates the spawn
// error from bindingAPIBaseURL instead. Stubs bindingAPIBaseURL to
// return a sentinel error, then calls each helper and asserts the
// sentinel reaches the caller.
//
// One test covers seven helpers because the error-propagation code
// added at each call site is mechanical (early-return on
// bindingAPIBaseURL error). Testing the contract once per helper
// catches a future regression where someone forgets to plumb the
// error through a new call site.
func TestBindingAPIBaseURL_PropagatesSpawnErrorThroughCallers(t *testing.T) {
	sentinel := errors.New("spawn unavailable for test")

	// Stub the URL helper for the duration of the test. Restore on
	// cleanup so other parallel-package tests aren't affected.
	orig := bindingAPIBaseURL
	bindingAPIBaseURL = func() (string, error) { return "", sentinel }
	t.Cleanup(func() { bindingAPIBaseURL = orig })

	cases := []struct {
		name string
		call func() error
	}{
		{
			name: "bindingDoRequest",
			call: func() error {
				_, _, err := bindingDoRequest(http.MethodGet, "/bindings", nil)
				return err
			},
		},
		{
			name: "fetchRuntimeStatus",
			call: func() error {
				_, err := fetchRuntimeStatus()
				return err
			},
		},
		{
			name: "fetchConnectorCheck",
			call: func() error {
				_, err := fetchConnectorCheck(false)
				return err
			},
		},
		{
			name: "postSyncRequest",
			call: func() error {
				_, err := postSyncRequest(false)
				return err
			},
		},
		{
			name: "fetchAuditList",
			call: func() error {
				_, err := fetchAuditList(auditListQuery{})
				return err
			},
		},
		{
			name: "fetchAuditGet",
			call: func() error {
				_, _, err := fetchAuditGet("audit-id")
				return err
			},
		},
		{
			name: "approvalDoRequest",
			call: func() error {
				_, err := approvalDoRequest(http.MethodGet, "/approvals", nil)
				return err
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			if err == nil {
				t.Fatal("expected error from spawn failure to propagate")
			}
			if !errors.Is(err, sentinel) {
				t.Errorf("got %v, want sentinel %v wrapped or returned", err, sentinel)
			}
		})
	}
}

func TestPromptLine_ReusesBufferedReader(t *testing.T) {
	// promptLine must accept an existing *bufio.Reader without
	// double-buffering, so two consecutive prompts read distinct
	// lines from the same source.
	r := bufio.NewReader(strings.NewReader("first\nsecond\n"))
	var out bytes.Buffer
	if got := promptLine(r, &out, "a: "); got != "first" {
		t.Errorf("first = %q", got)
	}
	if got := promptLine(r, &out, "b: "); got != "second" {
		t.Errorf("second = %q", got)
	}
}

func TestRunBinding_NoSubcommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"binding"}, newTestRegistry(), &stdout, &stderr)
	if code == 0 {
		t.Error("expected nonzero exit code with no subcommand")
	}
	if !strings.Contains(stderr.String(), "usage:") {
		t.Errorf("stderr missing usage hint: %s", stderr.String())
	}
}

func TestRunBinding_UnknownSubcommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"binding", "wibble"}, newTestRegistry(), &stdout, &stderr)
	if code == 0 {
		t.Error("expected nonzero exit code for unknown subcommand")
	}
}

func TestRunSecret_InHelp(t *testing.T) {
	var stdout bytes.Buffer
	run([]string{"help"}, newTestRegistry(), &stdout, &bytes.Buffer{})
	if !strings.Contains(stdout.String(), "aileron secret set") {
		t.Error("expected 'aileron secret set' in help output")
	}
	if !strings.Contains(stdout.String(), "aileron secret list") {
		t.Error("expected 'aileron secret list' in help output")
	}
}

func mockPromptPassphrase(responses []string) func() {
	calls := 0
	old := promptPassphrase
	promptPassphrase = func(prompt string, w io.Writer) (string, error) {
		if calls >= len(responses) {
			return "", fmt.Errorf("unexpected prompt call %d", calls)
		}
		r := responses[calls]
		calls++
		return r, nil
	}
	return func() { promptPassphrase = old }
}

func TestRunSecretSet_NewVault(t *testing.T) {
	dir := t.TempDir()
	origDefault := launch.DefaultVaultPath
	launch.DefaultVaultPath = func() string { return filepath.Join(dir, "secrets.json") }
	defer func() { launch.DefaultVaultPath = origDefault }()

	// Passphrase, confirm, secret value.
	restore := mockPromptPassphrase([]string{"mypass", "mypass", "secret-value"})
	defer restore()

	var stdout, stderr bytes.Buffer
	code := runSecretSet([]string{"test_token"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Stored secret") {
		t.Error("expected success message")
	}
	if !strings.Contains(stderr.String(), "Creating a new Aileron vault") {
		t.Error("expected new vault warning")
	}
}

func TestRunSecretSet_WrongPassphrase(t *testing.T) {
	dir := t.TempDir()
	vaultPath := filepath.Join(dir, "secrets.json")
	origDefault := launch.DefaultVaultPath
	launch.DefaultVaultPath = func() string { return vaultPath }
	defer func() { launch.DefaultVaultPath = origDefault }()

	// Create a vault with one secret.
	restore := mockPromptPassphrase([]string{"correct", "correct", "val"})
	var discard bytes.Buffer
	runSecretSet([]string{"existing"}, &discard, &discard)
	restore()

	// Now try to add with wrong passphrase.
	restore = mockPromptPassphrase([]string{"wrong", "also-wrong"})
	defer restore()

	var stdout, stderr bytes.Buffer
	code := runSecretSet([]string{"new_token"}, &stdout, &stderr)
	if code == 0 {
		t.Error("expected non-zero exit for wrong passphrase")
	}
}

func TestRunSecretSet_MismatchedConfirmation(t *testing.T) {
	dir := t.TempDir()
	origDefault := launch.DefaultVaultPath
	launch.DefaultVaultPath = func() string { return filepath.Join(dir, "secrets.json") }
	defer func() { launch.DefaultVaultPath = origDefault }()

	// Passphrase and confirm don't match.
	restore := mockPromptPassphrase([]string{"pass1", "pass2"})
	defer restore()

	var stdout, stderr bytes.Buffer
	code := runSecretSet([]string{"test_token"}, &stdout, &stderr)
	if code == 0 {
		t.Error("expected non-zero exit for mismatched passphrases")
	}
	if !strings.Contains(stderr.String(), "do not match") {
		t.Error("expected mismatch error")
	}
}

// --- Connector + action install tests (#366) ---

// previewBody returns a canonical preview JSON payload used by tests
// that don't care about the preview-specific contents (capabilities,
// already_installed). For tests that DO care, write the payload
// inline.
func previewBody(fqn, version, hash string) string {
	return `{"fqn":"` + fqn + `","version":"` + version + `","hash":"` + hash + `","publisher":"test","signature_status":"verified","already_installed":false,"capabilities":{}}`
}

// connectorInstallServer routes preview + install requests to two
// separate handler functions. Mirrors the two-endpoint consent flow
// the CLI now drives. Either handler may be nil to default to a
// simple OK response.
func connectorInstallServer(t *testing.T, onPreview, onInstall http.HandlerFunc) *httptest.Server {
	return fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/connectors/preview":
			if onPreview != nil {
				onPreview(w, r)
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, previewBody("github://acme/x", "1.0.0", "sha256:abc"))
		case "/connectors/install":
			if onInstall != nil {
				onInstall(w, r)
				return
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"fqn":"github://acme/x","version":"1.0.0","hash":"sha256:abc","entry_dir":"/path"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

func TestRunConnector_InstallHappyPath(t *testing.T) {
	var seenInstallBody []byte
	var seenInstallPath string
	connectorInstallServer(t, nil, func(w http.ResponseWriter, r *http.Request) {
		seenInstallPath = r.URL.Path
		seenInstallBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{
			"fqn":"github://acme/x",
			"version":"1.0.0",
			"hash":"sha256:abc",
			"entry_dir":"/path/to/entry"
		}`)
	})
	var stdout, stderr bytes.Buffer
	code := run([]string{"connector", "install", "github://acme/x", "--version=1.0.0", "--yes"},
		newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Errorf("exit = %d, body=%s", code, stderr.String())
	}
	if seenInstallPath != "/connectors/install" {
		t.Errorf("path = %q", seenInstallPath)
	}
	if !strings.Contains(string(seenInstallBody), `"fqn":"github://acme/x"`) ||
		!strings.Contains(string(seenInstallBody), `"version":"1.0.0"`) {
		t.Errorf("body = %s", seenInstallBody)
	}
	for _, want := range []string{"Connector install preview", "Installed:", "github://acme/x@1.0.0", "sha256:abc", "/path/to/entry"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("stdout missing %q:\n%s", want, stdout.String())
		}
	}
}

func TestRunConnector_InstallAcceptsAtVersionInFQN(t *testing.T) {
	var seenInstallBody []byte
	connectorInstallServer(t, nil, func(w http.ResponseWriter, r *http.Request) {
		seenInstallBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"fqn":"github://acme/x","version":"1.0.0","hash":"x","entry_dir":"y"}`)
	})
	var stdout, stderr bytes.Buffer
	code := run([]string{"connector", "install", "github://acme/x@1.0.0", "--yes"},
		newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Errorf("exit = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(string(seenInstallBody), `"version":"1.0.0"`) {
		t.Errorf("body = %s", seenInstallBody)
	}
}

// TestRunConnector_InstallAlreadyInstalledShortCircuits asserts the
// preview-driven short-circuit: when the preview reports
// already_installed=true, the CLI skips the prompt AND the install
// endpoint, rendering "Already installed" directly. The install
// handler must not be hit.
func TestRunConnector_InstallAlreadyInstalledShortCircuits(t *testing.T) {
	installCalled := false
	connectorInstallServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"fqn":"github://acme/x","version":"1.0.0","hash":"sha256:abc","publisher":"test","signature_status":"verified","already_installed":true,"capabilities":{}}`)
	}, func(w http.ResponseWriter, r *http.Request) {
		installCalled = true
		w.WriteHeader(http.StatusOK)
	})
	var stdout, stderr bytes.Buffer
	code := run([]string{"connector", "install", "github://acme/x@1.0.0"},
		newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Errorf("exit = %d, stderr=%s", code, stderr.String())
	}
	if installCalled {
		t.Error("install endpoint was called even though preview reported already_installed=true")
	}
	if !strings.Contains(stdout.String(), "Already installed") {
		t.Errorf("expected 'Already installed' in output: %s", stdout.String())
	}
}

func TestRunConnector_InstallMissingVersionRejected(t *testing.T) {
	connectorInstallServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("server should not be called when version is missing")
	}, func(w http.ResponseWriter, r *http.Request) {
		t.Error("server should not be called when version is missing")
	})
	var stdout, stderr bytes.Buffer
	code := run([]string{"connector", "install", "github://acme/x"},
		newTestRegistry(), &stdout, &stderr)
	if code == 0 {
		t.Error("expected nonzero exit when version is missing")
	}
}

func TestRunConnector_InstallVersionConflictRejected(t *testing.T) {
	connectorInstallServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("server should not be called when version conflicts")
	}, func(w http.ResponseWriter, r *http.Request) {
		t.Error("server should not be called when version conflicts")
	})
	var stdout, stderr bytes.Buffer
	code := run([]string{"connector", "install", "github://acme/x@1.0.0", "--version=2.0.0"},
		newTestRegistry(), &stdout, &stderr)
	if code == 0 {
		t.Error("expected nonzero exit when --version conflicts with @<v> in FQN")
	}
}

// TestRunConnector_InstallSignatureFailureExits1 asserts ADR-0007's
// "signature failure is a hard fail" contract from the CLI side: the
// preview endpoint returns 422, the CLI exits 1 BEFORE prompting,
// and the install endpoint is never called.
func TestRunConnector_InstallSignatureFailureExits1(t *testing.T) {
	installCalled := false
	connectorInstallServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = io.WriteString(w, `{"error":{"code":"signature_failure","message":"signature_failure"}}`)
	}, func(w http.ResponseWriter, r *http.Request) {
		installCalled = true
	})
	var stdout, stderr bytes.Buffer
	code := run([]string{"connector", "install", "github://acme/x@1.0.0", "--yes"},
		newTestRegistry(), &stdout, &stderr)
	if code == 0 {
		t.Errorf("expected nonzero exit; stderr=%s", stderr.String())
	}
	if installCalled {
		t.Error("install endpoint was called after preview signature_failure; --yes must NOT bypass")
	}
}

func TestRunConnector_NoSubcommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"connector"}, newTestRegistry(), &stdout, &stderr)
	if code == 0 {
		t.Error("expected nonzero exit with no subcommand")
	}
}

func TestRunConnector_UnknownSubcommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"connector", "wibble"}, newTestRegistry(), &stdout, &stderr)
	if code == 0 {
		t.Error("expected nonzero exit on unknown subcommand")
	}
}

// TestRunConnector_InstallExpectedHashPropagated asserts the
// --hash flag flows through both the preview and install request
// bodies. The preview path matters because that's where signature
// verification happens — the hash check needs to fire there too.
func TestRunConnector_InstallExpectedHashPropagated(t *testing.T) {
	var previewBody, installBody []byte
	connectorInstallServer(t, func(w http.ResponseWriter, r *http.Request) {
		previewBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"fqn":"github://acme/x","version":"1.0.0","hash":"sha256:abcd","publisher":"test","signature_status":"verified","already_installed":false,"capabilities":{}}`)
	}, func(w http.ResponseWriter, r *http.Request) {
		installBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"fqn":"x","version":"1.0.0","hash":"h","entry_dir":"d"}`)
	})
	var stdout, stderr bytes.Buffer
	run([]string{"connector", "install", "github://acme/x@1.0.0", "--hash=sha256:abcd", "--yes"},
		newTestRegistry(), &stdout, &stderr)
	for _, body := range [][]byte{previewBody, installBody} {
		if !strings.Contains(string(body), `"expected_hash":"sha256:abcd"`) {
			t.Errorf("body should include expected_hash: %s", body)
		}
	}
}

// TestRunConnector_InstallPromptYesProceeds asserts the operator's
// happy-path: preview shown, "y\n" piped, install called. This is
// the canonical interactive flow per ADR-0007.
func TestRunConnector_InstallPromptYesProceeds(t *testing.T) {
	installCalled := false
	connectorInstallServer(t, nil, func(w http.ResponseWriter, r *http.Request) {
		installCalled = true
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"fqn":"github://acme/x","version":"1.0.0","hash":"sha256:abc","entry_dir":"/path"}`)
	})

	r := newTestRegistry()
	var stdout, stderr bytes.Buffer
	stdin := strings.NewReader("y\n")

	// We can't drive run() with a custom stdin because run() takes
	// os.Stdin via `runConnector`. Reach into runConnectorInstall
	// directly with our stdin reader.
	code := runConnectorInstall(
		[]string{"github://acme/x@1.0.0"},
		stdin, &stdout, &stderr,
	)
	if code != 0 {
		t.Errorf("exit = %d, stderr=%s", code, stderr.String())
	}
	if !installCalled {
		t.Error("install endpoint not called after 'y' confirmation")
	}
	if !strings.Contains(stdout.String(), "Install? [y/n]:") {
		t.Errorf("expected prompt in stdout; got: %s", stdout.String())
	}
	_ = r // silence unused if compiler complains
}

// TestRunConnector_InstallPromptNoCancels asserts the cancel path:
// when the operator types anything other than y/yes, the install
// endpoint is not called and the CLI prints "Cancelled."
func TestRunConnector_InstallPromptNoCancels(t *testing.T) {
	installCalled := false
	connectorInstallServer(t, nil, func(w http.ResponseWriter, r *http.Request) {
		installCalled = true
		w.WriteHeader(http.StatusCreated)
	})

	var stdout, stderr bytes.Buffer
	stdin := strings.NewReader("n\n")
	code := runConnectorInstall(
		[]string{"github://acme/x@1.0.0"},
		stdin, &stdout, &stderr,
	)
	if code != 0 {
		t.Errorf("exit = %d, stderr=%s", code, stderr.String())
	}
	if installCalled {
		t.Error("install endpoint was called after operator typed 'n'")
	}
	if !strings.Contains(stdout.String(), "Cancelled.") {
		t.Errorf("expected 'Cancelled.' in output; got: %s", stdout.String())
	}
}

// TestRunConnector_InstallPreviewRendersCapabilities asserts the
// consent prompt renders network hosts and credential kind/scope
// from the preview response. This is the load-bearing UX assertion
// — operators decide whether to install based on what's rendered
// here.
func TestRunConnector_InstallPreviewRendersCapabilities(t *testing.T) {
	connectorInstallServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{
			"fqn":"github://aileron/slack",
			"version":"1.2.0",
			"hash":"sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			"publisher":"Aileron Engineering",
			"signature_status":"verified",
			"already_installed":false,
			"capabilities":{
				"network_hosts":["slack.com:443"],
				"credential":{"kind":"api_key","scope":"Send Slack messages"}
			}
		}`)
	}, nil)

	var stdout, stderr bytes.Buffer
	stdin := strings.NewReader("n\n") // cancel — we just want the rendering
	runConnectorInstall(
		[]string{"github://aileron/slack@1.2.0"},
		stdin, &stdout, &stderr,
	)
	out := stdout.String()
	for _, want := range []string{
		"Connector install preview",
		"github://aileron/slack",
		"1.2.0",
		"sha256:0123456789ab",
		"Aileron Engineering",
		"verified",
		"Network access:",
		"slack.com:443",
		"Credential required:",
		"api_key",
		"Send Slack messages",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("preview output missing %q:\n%s", want, out)
		}
	}
}

// TestRunConnector_InstallPreviewServerErrorExits1 asserts that a
// preview-side failure (e.g. fetch_failed because the source is
// down) exits 1 with the error visible. The install endpoint must
// not be reached.
func TestRunConnector_InstallPreviewServerErrorExits1(t *testing.T) {
	installCalled := false
	connectorInstallServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = io.WriteString(w, `{"error":{"code":"fetch_failed"}}`)
	}, func(w http.ResponseWriter, r *http.Request) {
		installCalled = true
	})
	var stdout, stderr bytes.Buffer
	code := run([]string{"connector", "install", "github://acme/x@1.0.0", "--yes"},
		newTestRegistry(), &stdout, &stderr)
	if code == 0 {
		t.Error("expected nonzero exit when preview fails")
	}
	if installCalled {
		t.Error("install endpoint was called after preview failed")
	}
}

// TestRunConnectorCheck_Empty asserts that with nothing installed the
// CLI prints a useful "no connectors installed" line and exits 0 —
// the bare-server path operators see when they run check before any
// install.
func TestRunConnectorCheck_Empty(t *testing.T) {
	prev := connectorCheckFetcher
	connectorCheckFetcher = func(includePrerelease bool) (*connectorsCheckResponse, error) {
		return &connectorsCheckResponse{Results: []connectorCheckResult{}}, nil
	}
	defer func() { connectorCheckFetcher = prev }()

	var stdout, stderr bytes.Buffer
	code := run([]string{"connector", "check"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "No connectors installed") {
		t.Errorf("expected 'No connectors installed' in output; got: %s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "aileron connector install") {
		t.Errorf("expected next-step hint mentioning `aileron connector install`, got: %s", stdout.String())
	}
}

// TestRunConnectorCheck_JSON_Empty: --json on an empty install set emits
// `[]`, so scripts can detect the bare-server case without grepping.
func TestRunConnectorCheck_JSON_Empty(t *testing.T) {
	prev := connectorCheckFetcher
	connectorCheckFetcher = func(includePrerelease bool) (*connectorsCheckResponse, error) {
		return &connectorsCheckResponse{Results: []connectorCheckResult{}}, nil
	}
	defer func() { connectorCheckFetcher = prev }()

	var stdout, stderr bytes.Buffer
	code := run([]string{"connector", "check", "--json"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	if got := strings.TrimRight(stdout.String(), "\n"); got != "[]" {
		t.Errorf("stdout = %q, want %q", got, "[]")
	}
}

// TestRunConnectorCheck_JSON_NDJSON: --json with results emits one
// JSON-encoded connectorCheckResult per line.
func TestRunConnectorCheck_JSON_NDJSON(t *testing.T) {
	latest := "0.0.6"
	prev := connectorCheckFetcher
	connectorCheckFetcher = func(includePrerelease bool) (*connectorsCheckResponse, error) {
		return &connectorsCheckResponse{Results: []connectorCheckResult{
			{Fqn: "github://x/a", CurrentVersion: "0.0.5", LatestVersion: &latest, UpdateAvailable: true},
			{Fqn: "github://x/b", CurrentVersion: "0.1.0", LatestVersion: &latest, UpdateAvailable: false},
		}}, nil
	}
	defer func() { connectorCheckFetcher = prev }()

	var stdout, stderr bytes.Buffer
	code := run([]string{"connector", "check", "--json"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	lines := strings.Split(strings.TrimRight(stdout.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2: %q", len(lines), stdout.String())
	}
	for _, line := range lines {
		var r connectorCheckResult
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Errorf("line %q is not JSON: %v", line, err)
		}
	}
}

// TestRunConnector_UsageAdvertisesJSON: connectorUsage gained `[--json]`
// for `connector check` in #492 item 2. Pins that the flag is
// documented in the surface a user reaches via `aileron connector`
// with no subcommand.
func TestRunConnector_UsageAdvertisesJSON(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"connector"}, newTestRegistry(), &stdout, &stderr)
	if code == 0 {
		t.Error("expected nonzero exit for no subcommand")
	}
	if !strings.Contains(stderr.String(), "--json") {
		t.Errorf("usage missing `--json`:\n%s", stderr.String())
	}
}

// TestRunConnectorCheck_UpdateAvailable asserts the primary signal:
// when the daemon reports update_available=true, the CLI renders the
// row with the latest version and a summary line counting updates.
func TestRunConnectorCheck_UpdateAvailable(t *testing.T) {
	latest := "0.0.6"
	prev := connectorCheckFetcher
	connectorCheckFetcher = func(includePrerelease bool) (*connectorsCheckResponse, error) {
		return &connectorsCheckResponse{Results: []connectorCheckResult{
			{
				Fqn:               "github://ALRubinger/aileron-connector-google",
				CurrentVersion:    "0.0.5",
				UpdateAvailable:   true,
				LatestVersion:     &latest,
				AvailableVersions: []string{"0.0.6"},
			},
		}}, nil
	}
	defer func() { connectorCheckFetcher = prev }()

	var stdout, stderr bytes.Buffer
	code := run([]string{"connector", "check"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"github://ALRubinger/aileron-connector-google", "0.0.5", "0.0.6", "update available", "1 update(s) available"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in output; got: %s", want, out)
		}
	}
}

// TestRunConnectorCheck_UpToDate asserts the "all good" path: every
// connector is on its latest version, summary says all up to date.
func TestRunConnectorCheck_UpToDate(t *testing.T) {
	latest := "1.0.0"
	prev := connectorCheckFetcher
	connectorCheckFetcher = func(includePrerelease bool) (*connectorsCheckResponse, error) {
		return &connectorsCheckResponse{Results: []connectorCheckResult{
			{Fqn: "github://acme/x", CurrentVersion: "1.0.0", UpdateAvailable: false, LatestVersion: &latest},
		}}, nil
	}
	defer func() { connectorCheckFetcher = prev }()

	var stdout, stderr bytes.Buffer
	run([]string{"connector", "check"}, newTestRegistry(), &stdout, &stderr)
	out := stdout.String()
	if !strings.Contains(out, "up to date") {
		t.Errorf("expected 'up to date' in output; got: %s", out)
	}
	if !strings.Contains(out, "All 1 connector(s) are up to date") {
		t.Errorf("expected summary line; got: %s", out)
	}
}

// TestRunConnectorCheck_PerConnectorError asserts the "offline-failing
// per connector" surface: a row with an error renders a "check failed"
// line, but other rows still render and the command exits 0.
func TestRunConnectorCheck_PerConnectorError(t *testing.T) {
	errMsg := "dial tcp: connection refused"
	latest := "1.1.0"
	prev := connectorCheckFetcher
	connectorCheckFetcher = func(includePrerelease bool) (*connectorsCheckResponse, error) {
		return &connectorsCheckResponse{Results: []connectorCheckResult{
			{Fqn: "github://acme/down", CurrentVersion: "1.0.0", Error: &errMsg},
			{Fqn: "github://acme/up", CurrentVersion: "1.0.0", UpdateAvailable: true, LatestVersion: &latest},
		}}, nil
	}
	defer func() { connectorCheckFetcher = prev }()

	var stdout, stderr bytes.Buffer
	code := run([]string{"connector", "check"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (per-connector errors don't fail the command)", code)
	}
	out := stdout.String()
	if !strings.Contains(out, "check failed") {
		t.Errorf("expected 'check failed' for the down connector; got: %s", out)
	}
	if !strings.Contains(out, "github://acme/up") {
		t.Errorf("expected the working connector to still render; got: %s", out)
	}
}

// TestRunConnectorCheck_IncludePrereleaseFlag asserts the
// --include-prerelease flag flows through to the fetcher (and thence
// to the daemon's query parameter). Without this propagation the flag
// would be silently dropped.
func TestRunConnectorCheck_IncludePrereleaseFlag(t *testing.T) {
	var sawInclude bool
	prev := connectorCheckFetcher
	connectorCheckFetcher = func(includePrerelease bool) (*connectorsCheckResponse, error) {
		sawInclude = includePrerelease
		return &connectorsCheckResponse{}, nil
	}
	defer func() { connectorCheckFetcher = prev }()

	var stdout, stderr bytes.Buffer
	run([]string{"connector", "check", "--include-prerelease"}, newTestRegistry(), &stdout, &stderr)
	if !sawInclude {
		t.Error("expected fetcher to be called with includePrerelease=true")
	}
}

// TestRunConnectorCheck_FetcherError asserts that a daemon error
// (network, 500, etc.) exits with a non-zero status and prints to
// stderr. Operators can wire the exit code into shell scripts.
func TestRunConnectorCheck_FetcherError(t *testing.T) {
	prev := connectorCheckFetcher
	connectorCheckFetcher = func(includePrerelease bool) (*connectorsCheckResponse, error) {
		return nil, fmt.Errorf("connection refused")
	}
	defer func() { connectorCheckFetcher = prev }()

	var stdout, stderr bytes.Buffer
	code := run([]string{"connector", "check"}, newTestRegistry(), &stdout, &stderr)
	if code == 0 {
		t.Error("expected nonzero exit when fetcher fails")
	}
	if !strings.Contains(stderr.String(), "connection refused") {
		t.Errorf("expected error message in stderr; got: %s", stderr.String())
	}
}

// TestRunConnectorCheck_FetcherSendsQueryParam asserts the production
// fetcher (not the stub) builds the right URL when include-prerelease
// is requested. This is the only test that touches the actual HTTP
// call path.
func TestRunConnectorCheck_FetcherSendsQueryParam(t *testing.T) {
	var sawQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"results":[]}`)
	}))
	defer srv.Close()
	t.Setenv("AILERON_API_URL", srv.URL+"/v1")

	if _, err := fetchConnectorCheck(true); err != nil {
		t.Fatalf("fetchConnectorCheck: %v", err)
	}
	if sawQuery != "include_prerelease=true" {
		t.Errorf("query = %q, want include_prerelease=true", sawQuery)
	}
}

// TestRunSync_Empty asserts the empty-state path: no actions, no
// requireds, no installs. The CLI prints a "no dependencies" line
// and exits 0 — what an operator running sync on a fresh project
// sees before installing anything.
func TestRunSync_Empty(t *testing.T) {
	prev := syncFetcher
	syncFetcher = func(autoInstall bool) (*syncResult, error) {
		return &syncResult{}, nil
	}
	defer func() { syncFetcher = prev }()

	var stdout, stderr bytes.Buffer
	code := run([]string{"sync"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "No connector dependencies") {
		t.Errorf("expected empty-state message; got: %s", stdout.String())
	}
}

// TestRunSync_InstallsAndUnbound asserts the canonical happy path:
// one action declares a connector, sync installs it, and reports an
// unbound capability so the operator knows to run `aileron binding
// setup <FQN>` next.
func TestRunSync_InstallsAndUnbound(t *testing.T) {
	scope := "test scope"
	prev := syncFetcher
	syncFetcher = func(autoInstall bool) (*syncResult, error) {
		return &syncResult{
			ActionsSeen: 1,
			Required:    []connectorRefWire{{Fqn: "github://acme/widget", Version: "1.0.0"}},
			Installed: []installedConnectorWire{
				{Fqn: "github://acme/widget", Version: "1.0.0", Hash: "sha256:abc"},
			},
			Unbound: []unboundCapabilityWire{
				{ConnectorFqn: "github://acme/widget", Kind: "api_key", Scope: &scope},
			},
		}, nil
	}
	defer func() { syncFetcher = prev }()

	var stdout, stderr bytes.Buffer
	code := run([]string{"sync"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"Walked 1 action", "github://acme/widget", "Installed 1", "1 unbound", "api_key", "test scope", "aileron binding setup"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in output; got: %s", want, out)
		}
	}
}

// TestRunSync_AlreadyInstalledIdempotent asserts the second-run path:
// every connector already present, no failures, no unbound. A clean
// idempotent re-run should print "already installed" and exit 0.
func TestRunSync_AlreadyInstalledIdempotent(t *testing.T) {
	prev := syncFetcher
	syncFetcher = func(autoInstall bool) (*syncResult, error) {
		return &syncResult{
			ActionsSeen:      1,
			Required:         []connectorRefWire{{Fqn: "github://acme/widget", Version: "1.0.0"}},
			AlreadyInstalled: []connectorRefWire{{Fqn: "github://acme/widget", Version: "1.0.0"}},
		}, nil
	}
	defer func() { syncFetcher = prev }()

	var stdout, stderr bytes.Buffer
	code := run([]string{"sync"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "already installed") {
		t.Errorf("expected 'already installed'; got: %s", out)
	}
	if strings.Contains(out, "Installed 1") {
		t.Errorf("did not expect 'Installed 1' line on idempotent re-run; got: %s", out)
	}
}

// TestRunSync_InstallFailureExits1 asserts the script-friendly path:
// any install failure exits 1 so operators wiring sync into CI / Make
// targets can branch on the exit code.
func TestRunSync_InstallFailureExits1(t *testing.T) {
	prev := syncFetcher
	syncFetcher = func(autoInstall bool) (*syncResult, error) {
		return &syncResult{
			ActionsSeen: 1,
			Required:    []connectorRefWire{{Fqn: "github://acme/widget", Version: "1.0.0"}},
			InstallFailures: []connectorFailureWire{
				{Fqn: "github://acme/widget", Version: "1.0.0", Error: "connection refused"},
			},
		}, nil
	}
	defer func() { syncFetcher = prev }()

	var stdout, stderr bytes.Buffer
	code := run([]string{"sync"}, newTestRegistry(), &stdout, &stderr)
	if code != 1 {
		t.Errorf("exit = %d, want 1 on install failure", code)
	}
	out := stdout.String()
	if !strings.Contains(out, "1 install failure") {
		t.Errorf("expected '1 install failure'; got: %s", out)
	}
	if !strings.Contains(out, "connection refused") {
		t.Errorf("expected error message in output; got: %s", out)
	}
}

// TestRunSync_BindAllNoUnboundIsNoop asserts that --bind-all with no
// unbound capabilities is a clean no-op: no prompts, no summary line,
// no stderr noise. The earlier "not yet implemented" stderr warning
// is gone.
func TestRunSync_BindAllNoUnboundIsNoop(t *testing.T) {
	prev := syncFetcher
	syncFetcher = func(autoInstall bool) (*syncResult, error) {
		return &syncResult{ActionsSeen: 1}, nil
	}
	defer func() { syncFetcher = prev }()

	stdin := strings.NewReader("")
	var stdout, stderr bytes.Buffer
	code := runSync([]string{"--bind-all"}, stdin, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	if strings.Contains(stderr.String(), "not yet implemented") {
		t.Errorf("legacy 'not yet implemented' warning leaked into stderr: %s", stderr.String())
	}
	if strings.Contains(stdout.String(), "Binding ") {
		t.Errorf("did not expect binding loop output with no unbound; got: %s", stdout.String())
	}
}

// TestRunSync_BindAllAPIKeyHappyPath asserts the canonical flow: one
// unbound api_key capability → operator types identity + key → CLI
// POSTs /v1/bindings/setup with the right body → CLI prints
// "Created" and a summary, exits 0. Mocks the daemon at the HTTP
// boundary via httptest so the test runs without a real server.
func TestRunSync_BindAllAPIKeyHappyPath(t *testing.T) {
	scope := "secret_key"
	prev := syncFetcher
	syncFetcher = func(autoInstall bool) (*syncResult, error) {
		return &syncResult{
			Unbound: []unboundCapabilityWire{
				{ConnectorFqn: "github://acme/widget", Kind: "api_key", Scope: &scope},
			},
		}, nil
	}
	defer func() { syncFetcher = prev }()

	var seenBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/bindings/setup" || r.Method != http.MethodPost {
			t.Errorf("unexpected daemon call: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		seenBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"created":[{"name":"github://acme/widget/work"}],"skipped":[]}`)
	}))
	defer srv.Close()
	t.Setenv("AILERON_API_URL", srv.URL+"/v1")

	stdin := strings.NewReader("work\nsk-live-1234\n")
	var stdout, stderr bytes.Buffer
	code := runSync([]string{"--bind-all"}, stdin, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
	}

	out := stdout.String()
	for _, want := range []string{
		"github://acme/widget [api_key]",
		"secret_key",
		"Created: github://acme/widget/work",
		"Bound 1 of 1",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in stdout; got: %s", want, out)
		}
	}

	// Wire body must carry FQN + the identity + value the user typed.
	body := string(seenBody)
	for _, want := range []string{
		`"connector_fqn":"github://acme/widget"`,
		`"identity":"work"`,
		`"kind":"api_key"`,
		`"value":"sk-live-1234"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("expected %q in POST body; got: %s", want, body)
		}
	}
}

// TestRunSync_BindAllOAuth2InitPosted asserts that an oauth2 unbound
// entry triggers POST /v1/bindings/setup/oauth2/init with the right
// body. The full dance (browser open, callback listener, finish POST)
// is out of scope for a unit test; this pins the boundary the issue
// asks for ("OAuth path mocked at the binding-setup-oauth2 boundary").
// The test makes the init endpoint return an error so the loop
// terminates without trying to bind a real loopback listener.
func TestRunSync_BindAllOAuth2InitPosted(t *testing.T) {
	prev := syncFetcher
	syncFetcher = func(autoInstall bool) (*syncResult, error) {
		return &syncResult{
			Unbound: []unboundCapabilityWire{
				{ConnectorFqn: "github://acme/oauth-widget", Kind: "oauth2"},
			},
		}, nil
	}
	defer func() { syncFetcher = prev }()

	var seenPath string
	var seenBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		seenBody, _ = io.ReadAll(r.Body)
		// Surface as a 500 so the loop counts a failure and exits
		// cleanly without trying to listen for a callback.
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":"upstream provider down"}`)
	}))
	defer srv.Close()
	t.Setenv("AILERON_API_URL", srv.URL+"/v1")

	stdin := strings.NewReader("personal\n")
	var stdout, stderr bytes.Buffer
	code := runSync([]string{"--bind-all"}, stdin, &stdout, &stderr)
	if code != 1 {
		t.Errorf("exit = %d, want 1 on bind failure", code)
	}

	if seenPath != "/v1/bindings/setup/oauth2/init" {
		t.Errorf("path = %q, want /v1/bindings/setup/oauth2/init", seenPath)
	}
	body := string(seenBody)
	for _, want := range []string{
		`"connector_fqn":"github://acme/oauth-widget"`,
		`"identity":"personal"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("expected %q in init body; got: %s", want, body)
		}
	}

	if !strings.Contains(stdout.String(), "Bound 0 of 1 capability(s) (1 failed)") {
		t.Errorf("expected '0 of 1 (1 failed)' summary; got: %s", stdout.String())
	}
}

// TestRunSync_BindAllPartialFailureContinues asserts that a single
// failing binding doesn't abort the loop: the next unbound entry is
// still attempted, and the summary line tallies correctly.
func TestRunSync_BindAllPartialFailureContinues(t *testing.T) {
	prev := syncFetcher
	syncFetcher = func(autoInstall bool) (*syncResult, error) {
		return &syncResult{
			Unbound: []unboundCapabilityWire{
				{ConnectorFqn: "github://acme/first", Kind: "api_key"},
				{ConnectorFqn: "github://acme/second", Kind: "api_key"},
			},
		}, nil
	}
	defer func() { syncFetcher = prev }()

	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			// First binding fails on the daemon side.
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = io.WriteString(w, `{"error":"vault locked"}`)
			return
		}
		// Second binding succeeds.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"created":[{"name":"github://acme/second/work"}]}`)
	}))
	defer srv.Close()
	t.Setenv("AILERON_API_URL", srv.URL+"/v1")

	stdin := strings.NewReader("work\nbad-key\nwork\ngood-key\n")
	var stdout, stderr bytes.Buffer
	code := runSync([]string{"--bind-all"}, stdin, &stdout, &stderr)
	if code != 1 {
		t.Errorf("exit = %d, want 1 (partial failure)", code)
	}
	if calls != 2 {
		t.Errorf("daemon calls = %d, want 2 (loop must continue past the first failure)", calls)
	}
	out := stdout.String()
	if !strings.Contains(out, "Bound 1 of 2 capability(s) (1 failed)") {
		t.Errorf("expected '1 of 2 (1 failed)' summary; got: %s", out)
	}
}

// TestRunSync_BindAllUnsupportedKindContinues asserts that an
// unrecognized credential kind logs to stderr and is counted as a
// failure, but doesn't crash or short-circuit the loop.
func TestRunSync_BindAllUnsupportedKindContinues(t *testing.T) {
	prev := syncFetcher
	syncFetcher = func(autoInstall bool) (*syncResult, error) {
		return &syncResult{
			Unbound: []unboundCapabilityWire{
				{ConnectorFqn: "github://acme/weird", Kind: "future_kind"},
			},
		}, nil
	}
	defer func() { syncFetcher = prev }()

	stdin := strings.NewReader("work\n")
	var stdout, stderr bytes.Buffer
	code := runSync([]string{"--bind-all"}, stdin, &stdout, &stderr)
	if code != 1 {
		t.Errorf("exit = %d, want 1 on unsupported kind", code)
	}
	if !strings.Contains(stderr.String(), `unsupported credential kind "future_kind"`) {
		t.Errorf("expected unsupported-kind message in stderr; got: %s", stderr.String())
	}
	if !strings.Contains(stdout.String(), "Bound 0 of 1 capability(s) (1 failed)") {
		t.Errorf("expected '0 of 1 (1 failed)' summary; got: %s", stdout.String())
	}
}

// TestRunSync_BindAllEmptyIdentitySkips asserts that pressing return
// at the identity prompt skips that entry (counted as failed) and
// the loop continues to the next.
func TestRunSync_BindAllEmptyIdentitySkips(t *testing.T) {
	prev := syncFetcher
	syncFetcher = func(autoInstall bool) (*syncResult, error) {
		return &syncResult{
			Unbound: []unboundCapabilityWire{
				{ConnectorFqn: "github://acme/widget", Kind: "api_key"},
			},
		}, nil
	}
	defer func() { syncFetcher = prev }()

	// Empty identity (just a newline).
	stdin := strings.NewReader("\n")
	var stdout, stderr bytes.Buffer
	code := runSync([]string{"--bind-all"}, stdin, &stdout, &stderr)
	if code != 1 {
		t.Errorf("exit = %d, want 1 when binding skipped", code)
	}
	if !strings.Contains(stderr.String(), "identity is required") {
		t.Errorf("expected 'identity is required' in stderr; got: %s", stderr.String())
	}
}

// TestRunSync_YesFlagPropagated asserts the --yes flag flows through
// to the fetcher (and thence to the daemon's auto_install body
// field). Without this propagation the flag would be silently dropped.
func TestRunSync_YesFlagPropagated(t *testing.T) {
	var sawAutoInstall bool
	prev := syncFetcher
	syncFetcher = func(autoInstall bool) (*syncResult, error) {
		sawAutoInstall = autoInstall
		return &syncResult{}, nil
	}
	defer func() { syncFetcher = prev }()

	var stdout, stderr bytes.Buffer
	run([]string{"sync", "--yes"}, newTestRegistry(), &stdout, &stderr)
	if !sawAutoInstall {
		t.Error("expected fetcher to be called with autoInstall=true")
	}
}

// TestRunSync_FetcherError asserts the daemon-down path: a network
// or 5xx error from the daemon exits 1 and prints the error to stderr.
func TestRunSync_FetcherError(t *testing.T) {
	prev := syncFetcher
	syncFetcher = func(autoInstall bool) (*syncResult, error) {
		return nil, fmt.Errorf("connection refused")
	}
	defer func() { syncFetcher = prev }()

	var stdout, stderr bytes.Buffer
	code := run([]string{"sync"}, newTestRegistry(), &stdout, &stderr)
	if code == 0 {
		t.Error("expected nonzero exit on fetcher error")
	}
	if !strings.Contains(stderr.String(), "connection refused") {
		t.Errorf("expected error in stderr; got: %s", stderr.String())
	}
}

// TestPostSyncRequest_HappyPath asserts the actual HTTP fetcher (not
// the stub) hits the right URL, sends the right method + headers, and
// decodes the daemon's response into syncResult correctly. The other
// runSync tests stub the fetcher; this is the only path that
// exercises the real network code.
func TestPostSyncRequest_HappyPath(t *testing.T) {
	var sawMethod, sawPath, sawContentType string
	var sawBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawMethod = r.Method
		sawPath = r.URL.Path
		sawContentType = r.Header.Get("Content-Type")
		sawBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"actions_seen": 2,
			"required": [{"fqn":"github://acme/x","version":"1.0.0"}],
			"installed": [],
			"already_installed": [{"fqn":"github://acme/x","version":"1.0.0"}],
			"install_failures": [],
			"unbound": []
		}`)
	}))
	defer srv.Close()
	t.Setenv("AILERON_API_URL", srv.URL+"/v1")

	got, err := postSyncRequest(false)
	if err != nil {
		t.Fatalf("postSyncRequest: %v", err)
	}
	if sawMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", sawMethod)
	}
	if sawPath != "/v1/sync" {
		t.Errorf("path = %q, want /v1/sync", sawPath)
	}
	if sawContentType != "application/json" {
		t.Errorf("content-type = %q, want application/json", sawContentType)
	}
	if !strings.Contains(string(sawBody), `"auto_install":false`) {
		t.Errorf("body should contain auto_install=false; got: %s", sawBody)
	}
	if got.ActionsSeen != 2 {
		t.Errorf("ActionsSeen = %d, want 2", got.ActionsSeen)
	}
	if len(got.Required) != 1 || got.Required[0].Fqn != "github://acme/x" {
		t.Errorf("Required = %v, want [github://acme/x]", got.Required)
	}
	if len(got.AlreadyInstalled) != 1 {
		t.Errorf("AlreadyInstalled = %v, want 1", got.AlreadyInstalled)
	}
}

// TestPostSyncRequest_AutoInstallTrueInBody asserts the autoInstall
// argument flows into the JSON body. Mirrors --yes propagation
// from the runSync test, but checks the wire shape directly.
func TestPostSyncRequest_AutoInstallTrueInBody(t *testing.T) {
	var sawBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"actions_seen":0,"required":[],"installed":[],"already_installed":[],"install_failures":[],"unbound":[]}`)
	}))
	defer srv.Close()
	t.Setenv("AILERON_API_URL", srv.URL+"/v1")

	if _, err := postSyncRequest(true); err != nil {
		t.Fatalf("postSyncRequest: %v", err)
	}
	if !strings.Contains(string(sawBody), `"auto_install":true`) {
		t.Errorf("body should contain auto_install=true; got: %s", sawBody)
	}
}

// TestPostSyncRequest_Non200ReturnsError asserts daemon-side errors
// (e.g. 401 unauthorized, 500 internal) surface as a Go error with
// the status code embedded in the message. Operators wiring sync
// into shell scripts can grep for the code if needed.
func TestPostSyncRequest_Non200ReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":{"code":"internal_error","message":"oops"}}`)
	}))
	defer srv.Close()
	t.Setenv("AILERON_API_URL", srv.URL+"/v1")

	_, err := postSyncRequest(false)
	if err == nil {
		t.Fatal("expected error on 500")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error should mention 500; got: %v", err)
	}
	if !strings.Contains(err.Error(), "oops") {
		t.Errorf("error should include response body; got: %v", err)
	}
}

// TestPostSyncRequest_MalformedJSONReturnsError asserts that a
// truncated or otherwise unparseable response surfaces as a wrapped
// "decoding response" error. Without this, a daemon bug producing
// bad JSON would crash the CLI rather than reporting cleanly.
func TestPostSyncRequest_MalformedJSONReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"actions_seen": not_a_number}`)
	}))
	defer srv.Close()
	t.Setenv("AILERON_API_URL", srv.URL+"/v1")

	_, err := postSyncRequest(false)
	if err == nil {
		t.Fatal("expected error on malformed JSON")
	}
	if !strings.Contains(err.Error(), "decoding response") {
		t.Errorf("error should mention 'decoding response'; got: %v", err)
	}
}

// TestPostSyncRequest_NetworkErrorReturnsError asserts a closed
// listener (server already shut down) surfaces as a Go error rather
// than a panic. Same robustness contract as fetchRuntimeStatus and
// fetchConnectorCheck.
func TestPostSyncRequest_NetworkErrorReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	addr := srv.URL
	srv.Close() // shut down listener immediately
	t.Setenv("AILERON_API_URL", addr+"/v1")

	_, err := postSyncRequest(false)
	if err == nil {
		t.Fatal("expected error on closed listener")
	}
}

func TestRunAction_AddHappyPath(t *testing.T) {
	var seenInstallBody []byte
	var seenInstallPath string
	actionInstallServer(t, nil, func(w http.ResponseWriter, r *http.Request) {
		seenInstallPath = r.URL.Path
		seenInstallBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{
			"name":"list-recent-prs",
			"fqn":"github://acme/x/actions/list-recent-prs",
			"version":"0.1.0",
			"source":"github://acme/x/actions/list-recent-prs@0.1.0",
			"path":"/path/list-recent-prs.md"
		}`)
	})
	var stdout, stderr bytes.Buffer
	code := run([]string{"action", "add", "github://acme/x/actions/list-recent-prs", "--version=0.1.0", "--yes"},
		newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Errorf("exit = %d, stderr=%s", code, stderr.String())
	}
	if seenInstallPath != "/actions/install" {
		t.Errorf("install path = %q", seenInstallPath)
	}
	if !strings.Contains(string(seenInstallBody), `"version":"0.1.0"`) {
		t.Errorf("install body = %s", seenInstallBody)
	}
	for _, want := range []string{"Action install preview", "Added:", "list-recent-prs", "/path/list-recent-prs.md"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("stdout missing %q:\n%s", want, stdout.String())
		}
	}
}

func TestRunAction_AddForceFlagPropagated(t *testing.T) {
	var seenInstallBody []byte
	actionInstallServer(t, nil, func(w http.ResponseWriter, r *http.Request) {
		seenInstallBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"name":"x","fqn":"y","version":"1.0.0","source":"y","path":"z"}`)
	})
	var stdout, stderr bytes.Buffer
	run([]string{"action", "add", "github://acme/x/actions/y@0.1.0", "--force", "--yes"},
		newTestRegistry(), &stdout, &stderr)
	if !strings.Contains(string(seenInstallBody), `"force":true`) {
		t.Errorf("force flag not propagated: %s", seenInstallBody)
	}
}

func TestRunAction_AddAlreadyInstalledIs200(t *testing.T) {
	actionInstallServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"fqn":"y","version":"1","hash":"sha256:a","name":"x","already_installed":true,"connector_deps":[]}`)
	}, func(w http.ResponseWriter, r *http.Request) {
		t.Error("install endpoint must not be hit when preview reports already_installed=true")
	})
	var stdout, stderr bytes.Buffer
	code := run([]string{"action", "add", "github://acme/x/actions/y@0.1.0"},
		newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Errorf("exit = %d", code)
	}
	if !strings.Contains(stdout.String(), "Already installed") {
		t.Errorf("output: %s", stdout.String())
	}
}

func TestRunAction_AddMissingVersionRejected(t *testing.T) {
	actionInstallServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("server should not be called")
	}, func(w http.ResponseWriter, r *http.Request) {
		t.Error("server should not be called")
	})
	var stdout, stderr bytes.Buffer
	code := run([]string{"action", "add", "github://acme/x/actions/y"},
		newTestRegistry(), &stdout, &stderr)
	if code == 0 {
		t.Error("expected nonzero exit when version missing")
	}
}

func TestRunAction_AddServerErrorIsExit1(t *testing.T) {
	actionInstallServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = io.WriteString(w, `{}`)
	}, nil)
	var stdout, stderr bytes.Buffer
	code := run([]string{"action", "add", "github://acme/x/actions/y@0.1.0", "--yes"},
		newTestRegistry(), &stdout, &stderr)
	if code == 0 {
		t.Error("expected nonzero exit")
	}
}

func TestRunAction_NoSubcommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"action"}, newTestRegistry(), &stdout, &stderr)
	if code == 0 {
		t.Error("expected nonzero exit")
	}
}

func TestRunAction_UnknownSubcommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"action", "wibble"}, newTestRegistry(), &stdout, &stderr)
	if code == 0 {
		t.Error("expected nonzero exit")
	}
}

func TestSplitFQNVersion(t *testing.T) {
	cases := []struct {
		fqn         string
		flag        string
		wantFQN     string
		wantVersion string
		wantErr     bool
	}{
		{"github://x/y", "1.0.0", "github://x/y", "1.0.0", false},
		{"github://x/y@1.0.0", "", "github://x/y", "1.0.0", false},
		{"github://x/y@1.0.0", "1.0.0", "github://x/y", "1.0.0", false},
		{"github://x/y@1.0.0", "2.0.0", "", "", true},
		{"github://x/y", "", "", "", true},
		{"github://x/y@", "", "", "", true},
	}
	for _, c := range cases {
		fqn, ver, err := splitFQNVersion(c.fqn, c.flag)
		if c.wantErr {
			if err == nil {
				t.Errorf("splitFQNVersion(%q, %q) = no error; want error", c.fqn, c.flag)
			}
			continue
		}
		if err != nil {
			t.Errorf("splitFQNVersion(%q, %q): %v", c.fqn, c.flag, err)
			continue
		}
		if fqn != c.wantFQN || ver != c.wantVersion {
			t.Errorf("splitFQNVersion(%q, %q) = (%q, %q); want (%q, %q)",
				c.fqn, c.flag, fqn, ver, c.wantFQN, c.wantVersion)
		}
	}
}

// fakeBrowser captures URLs the CLI would have asked the system to
// open, without actually spawning a process. Tests that exercise the
// OAuth dance swap bindingBrowser to one of these so `go test`
// doesn't pop real browser windows.
type fakeBrowser struct{ opened []string }

func (f *fakeBrowser) Open(url string) error { f.opened = append(f.opened, url); return nil }

// withFakeBrowser swaps bindingBrowser to a fake for the duration of
// a test, restoring the default on cleanup.
func withFakeBrowser(t *testing.T) *fakeBrowser {
	t.Helper()
	prev := bindingBrowser
	fb := &fakeBrowser{}
	bindingBrowser = fb
	t.Cleanup(func() { bindingBrowser = prev })
	return fb
}

// TestRunBinding_SetupOAuth2DriveDance exercises the full
// server-driven OAuth dance from the CLI's perspective: init returns
// an authorize URL pointing at a fake provider, CLI spins up its
// loopback listener, a goroutine simulates the user's browser
// landing on the redirect URI with `code` + `state`, CLI POSTs
// finish, prints the bound name.
func TestRunBinding_SetupOAuth2DriveDance(t *testing.T) {
	fb := withFakeBrowser(t)
	_ = fb // verified at the end
	var seenInitBody []byte
	var seenFinishBody []byte
	var redirectURI string
	var sessionID = "test-session-id"
	var state = "test-state-abc"

	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/bindings/setup/oauth2/init":
			seenInitBody, _ = io.ReadAll(r.Body)
			// Pick a free port for the redirect_uri the CLI will use.
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatalf("port pick: %v", err)
			}
			port := ln.Addr().(*net.TCPAddr).Port
			_ = ln.Close()
			redirectURI = fmt.Sprintf("http://127.0.0.1:%d/callback", port)
			authorizeURL := fmt.Sprintf("http://provider.test/authorize?state=%s&redirect_uri=%s", state, redirectURI)
			_, _ = io.WriteString(w, fmt.Sprintf(`{"session_id":%q,"authorize_url":%q,"redirect_uri":%q}`,
				sessionID, authorizeURL, redirectURI))

			// Simulate the user clicking through the OAuth screen
			// and the provider redirecting to the loopback callback.
			// Small delay so the CLI has time to bind its listener.
			go func() {
				time.Sleep(100 * time.Millisecond)
				_, _ = http.Get(redirectURI + "?code=auth-code&state=" + state)
			}()
		case "/bindings/setup/oauth2/finish":
			seenFinishBody, _ = io.ReadAll(r.Body)
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{
				"name":"oauth2/google/work","kind":"oauth2","service":"google","identity":"work",
				"connector_fqn":"github://acme/aileron-connector-google","created_at":"2024-01-01T00:00:00Z"
			}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	})

	// Stub the browser-open so tests don't pop a real browser.
	// (The CLI calls SystemBrowser{}.Open which would invoke `open`/`xdg-open`/`start`;
	// we tolerate the failure since the simulated callback fires regardless.)

	stdin := strings.NewReader("work\n")
	var stdout, stderr bytes.Buffer
	code := runBinding([]string{"setup", "github://acme/aileron-connector-google"}, stdin, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(string(seenInitBody), `"connector_fqn":"github://acme/aileron-connector-google"`) {
		t.Errorf("init body missing connector_fqn: %s", seenInitBody)
	}
	if !strings.Contains(string(seenInitBody), `"identity":"work"`) {
		t.Errorf("init body missing identity: %s", seenInitBody)
	}
	if !strings.Contains(string(seenFinishBody), `"session_id":"`+sessionID+`"`) {
		t.Errorf("finish body missing session_id: %s", seenFinishBody)
	}
	if !strings.Contains(string(seenFinishBody), `"code":"auth-code"`) {
		t.Errorf("finish body missing code: %s", seenFinishBody)
	}
	if !strings.Contains(string(seenFinishBody), `"state":"`+state+`"`) {
		t.Errorf("finish body missing state: %s", seenFinishBody)
	}
	if !strings.Contains(stdout.String(), "Bound: oauth2/google/work") {
		t.Errorf("stdout missing bound line: %s", stdout.String())
	}
	// Confirm the CLI asked the (fake) browser to open the authorize URL.
	if len(fb.opened) != 1 {
		t.Errorf("fake browser open count = %d, want 1", len(fb.opened))
	}
}

func TestRunBinding_SetupOAuth2_InitErrorIsExit1(t *testing.T) {
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":"oops"}`)
	})
	stdin := strings.NewReader("work\n")
	var stdout, stderr bytes.Buffer
	code := runBinding([]string{"setup", "github://acme/x"}, stdin, &stdout, &stderr)
	if code == 0 {
		t.Errorf("expected nonzero exit; stderr = %s", stderr.String())
	}
}

func TestPortFromRedirectURI(t *testing.T) {
	cases := map[string]int{
		"http://127.0.0.1:54321/callback":  54321,
		"http://localhost:8080/x":          8080,
		"http://127.0.0.1/callback":        0, // no port
		"https://accounts.google.com/auth": 0, // standard port omitted
		"":                                 0,
		"not a url":                        0,
	}
	for in, want := range cases {
		if got := portFromRedirectURI(in); got != want {
			t.Errorf("portFromRedirectURI(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestRunAction_AddAutoPromptsForUnboundCapabilities(t *testing.T) {
	// After install, the server returned `unbound_capabilities`; the
	// CLI prompts the user, drops into binding setup for each yes,
	// and runs the api_key flow when the connector declares api_key.
	withSeededKeyring(t, "github://x/y")
	var initHits, setupHits int
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/actions/preview":
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"fqn":"github://x/y/actions/echo","version":"1.0.0","hash":"sha256:a","name":"echo","signature_status":"verified","connector_deps":[]}`)
		case "/actions/install":
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{
				"name":"echo","fqn":"github://x/y/actions/echo",
				"version":"1.0.0","source":"x","path":"/p",
				"unbound_capabilities":[{"connector_fqn":"github://x/conn-a","kind":"api_key","scope":"read"}]
			}`)
		case "/bindings/setup/oauth2/init":
			initHits++
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = io.WriteString(w, `{"error":{"code":"not_oauth2"}}`)
		case "/bindings/setup":
			setupHits++
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"created":[{"name":"api_key/conn-a/work","kind":"api_key","service":"conn-a","identity":"work","connector_fqn":"github://x/conn-a","created_at":"2024-01-01T00:00:00Z"}]}`)
		case "/hub/action-install-decision":
			// Action isn't Hub-listed in this fixture; force the
			// composite flow to fall through to the legacy path so this
			// test continues to exercise its intended surface.
			w.WriteHeader(http.StatusNotFound)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	})
	// Action-add consent "y", unbound-binding prompt "y", identity "work", api_key value "k".
	stdin := strings.NewReader("y\ny\nwork\nk\n")
	var stdout, stderr bytes.Buffer
	code := runAction([]string{"add", "github://x/y/actions/echo@1.0.0"},
		stdin, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	if initHits != 1 {
		t.Errorf("oauth2/init hits = %d, want 1", initHits)
	}
	if setupHits != 1 {
		t.Errorf("bindings/setup hits = %d, want 1", setupHits)
	}
	if !strings.Contains(stdout.String(), "github://x/conn-a") {
		t.Errorf("stdout should mention the connector FQN: %s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "Created: api_key/conn-a/work") {
		t.Errorf("stdout missing binding-created line: %s", stdout.String())
	}
}

func TestRunAction_AddDeclineDoesNotBindButPrintsHint(t *testing.T) {
	withSeededKeyring(t, "github://x/y")
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/actions/preview":
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"fqn":"x","version":"1.0.0","hash":"sha256:a","name":"echo","signature_status":"verified","connector_deps":[]}`)
		case "/actions/install":
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{
				"name":"echo","fqn":"x","version":"1.0.0","source":"x","path":"/p",
				"unbound_capabilities":[{"connector_fqn":"github://x/conn-a","kind":"oauth2"}]
			}`)
		case "/hub/action-install-decision":
			w.WriteHeader(http.StatusNotFound)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	})
	// Action-add consent "y", then "n" to the binding-setup prompt.
	stdin := strings.NewReader("y\nn\n")
	var stdout, stderr bytes.Buffer
	code := runAction([]string{"add", "github://x/y/actions/echo@1.0.0"},
		stdin, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Skipped") {
		t.Errorf("stdout should mention skipped: %s", stdout.String())
	}
}

func TestRunAction_AddNoBindFlagSkipsPrompt(t *testing.T) {
	withSeededKeyring(t, "github://x/y")
	hits := map[string]int{}
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		hits[r.URL.Path]++
		switch r.URL.Path {
		case "/actions/preview":
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"fqn":"x","version":"1.0.0","hash":"sha256:a","name":"echo","signature_status":"verified","connector_deps":[]}`)
		case "/actions/install":
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{
				"name":"echo","fqn":"x","version":"1.0.0","source":"x","path":"/p",
				"unbound_capabilities":[{"connector_fqn":"github://x/conn-a","kind":"api_key"}]
			}`)
		case "/hub/action-install-decision":
			w.WriteHeader(http.StatusNotFound)
		default:
			t.Errorf("unexpected path with --no-bind: %s", r.URL.Path)
		}
	})
	var stdout, stderr bytes.Buffer
	code := run([]string{"action", "add", "github://x/y/actions/echo@1.0.0", "--no-bind", "--yes"},
		newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	if hits["/bindings/setup/oauth2/init"] != 0 {
		t.Errorf("--no-bind should not call binding setup")
	}
	if !strings.Contains(stdout.String(), "Run `aileron binding setup") {
		t.Errorf("stdout should print the manual-binding hint with --no-bind: %s", stdout.String())
	}
}

func TestRunBinding_SetupOAuth2_MalformedInitResponse(t *testing.T) {
	withFakeBrowser(t)
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		// Server claims 200 but body isn't valid JSON.
		_, _ = io.WriteString(w, `{not json`)
	})
	stdin := strings.NewReader("work\n")
	var stdout, stderr bytes.Buffer
	code := runBinding([]string{"setup", "github://x/y"}, stdin, &stdout, &stderr)
	if code == 0 {
		t.Errorf("expected nonzero exit on malformed init response; stderr=%s", stderr.String())
	}
}

func TestRunBinding_SetupOAuth2_BadRedirectURIRejected(t *testing.T) {
	withFakeBrowser(t)
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"session_id":"s","authorize_url":"http://x/auth","redirect_uri":"not-a-url"}`)
	})
	stdin := strings.NewReader("work\n")
	var stdout, stderr bytes.Buffer
	code := runBinding([]string{"setup", "github://x/y"}, stdin, &stdout, &stderr)
	if code == 0 {
		t.Errorf("expected nonzero exit when redirect_uri has no port; stderr=%s", stderr.String())
	}
}

func TestRunBinding_SetupOAuth2_FinishErrorIsExit1(t *testing.T) {
	withFakeBrowser(t)
	var redirectURI string
	state := "test-state"
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/bindings/setup/oauth2/init":
			ln, _ := net.Listen("tcp", "127.0.0.1:0")
			port := ln.Addr().(*net.TCPAddr).Port
			_ = ln.Close()
			redirectURI = fmt.Sprintf("http://127.0.0.1:%d/callback", port)
			authorizeURL := fmt.Sprintf("http://provider.test/auth?state=%s&redirect_uri=%s", state, redirectURI)
			_, _ = io.WriteString(w, fmt.Sprintf(`{"session_id":"s","authorize_url":%q,"redirect_uri":%q}`,
				authorizeURL, redirectURI))
			go func() {
				time.Sleep(50 * time.Millisecond)
				_, _ = http.Get(redirectURI + "?code=c&state=" + state)
			}()
		case "/bindings/setup/oauth2/finish":
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = io.WriteString(w, `{"error":{"code":"token_exchange_failed"}}`)
		}
	})
	stdin := strings.NewReader("work\n")
	var stdout, stderr bytes.Buffer
	code := runBinding([]string{"setup", "github://x/y"}, stdin, &stdout, &stderr)
	if code == 0 {
		t.Errorf("expected nonzero exit when finish fails; stderr=%s", stderr.String())
	}
}

func TestRunBinding_SetupOAuth2_PortBindFailsWhenSamePortInUse(t *testing.T) {
	withFakeBrowser(t)
	// Bind a port and HOLD it; the CLI's listener.NewListener call
	// for the same port will fail.
	holder, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("seed listener: %v", err)
	}
	t.Cleanup(func() { _ = holder.Close() })
	heldPort := holder.Addr().(*net.TCPAddr).Port
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		// Return the held port as the redirect URI; CLI's bind fails.
		_, _ = io.WriteString(w, fmt.Sprintf(
			`{"session_id":"s","authorize_url":"http://x/auth?state=s","redirect_uri":"http://127.0.0.1:%d/callback"}`,
			heldPort))
	})
	stdin := strings.NewReader("work\n")
	var stdout, stderr bytes.Buffer
	code := runBinding([]string{"setup", "github://x/y"}, stdin, &stdout, &stderr)
	if code == 0 {
		t.Error("expected nonzero exit when listener bind fails")
	}
}

// --- runBindingReauthorize: refresh OAuth scopes after drift (#726) ---

// TestRunBinding_ReauthorizeDrivesOAuthDance walks the full flow:
//  1. GET the existing binding to discover kind=oauth2 + identity +
//     connector_fqn + service + account.
//  2. POST init with purpose=reauthorize.
//  3. Simulated browser callback.
//  4. POST finish; server returns 200 (reauthorize upsert).
//  5. CLI prints "Reauthorized: <name>".
func TestRunBinding_ReauthorizeDrivesOAuthDance(t *testing.T) {
	withFakeBrowser(t)
	var seenInitBody []byte
	var seenFinishBody []byte
	state := "rstate-1"

	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/bindings/oauth2/google/work":
			_, _ = io.WriteString(w, `{
				"name":"oauth2/google/work","kind":"oauth2","service":"google","identity":"work",
				"connector_fqn":"github://acme/aileron-connector-google","account":"alr@x.com",
				"status":"stale","stale_reason":"scope_drift",
				"missing_scopes":["https://www.googleapis.com/auth/drive"],
				"created_at":"2024-01-01T00:00:00Z"
			}`)
		case "/bindings/setup/oauth2/init":
			seenInitBody, _ = io.ReadAll(r.Body)
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatalf("port pick: %v", err)
			}
			port := ln.Addr().(*net.TCPAddr).Port
			_ = ln.Close()
			redirectURI := fmt.Sprintf("http://127.0.0.1:%d/callback", port)
			authorizeURL := fmt.Sprintf("http://provider.test/auth?state=%s&redirect_uri=%s",
				state, redirectURI)
			_, _ = io.WriteString(w, fmt.Sprintf(
				`{"session_id":"sess-1","authorize_url":%q,"redirect_uri":%q}`,
				authorizeURL, redirectURI))
			go func() {
				time.Sleep(50 * time.Millisecond)
				_, _ = http.Get(redirectURI + "?code=fresh-code&state=" + state)
			}()
		case "/bindings/setup/oauth2/finish":
			seenFinishBody, _ = io.ReadAll(r.Body)
			// Reauthorize returns 200, not 201.
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{
				"name":"oauth2/google/work","kind":"oauth2","service":"google","identity":"work",
				"connector_fqn":"github://acme/aileron-connector-google","status":"active",
				"created_at":"2024-01-01T00:00:00Z"
			}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	})

	var stdout, stderr bytes.Buffer
	code := runBinding([]string{"reauthorize", "oauth2/google/work"}, strings.NewReader(""),
		&stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	// Init body must declare purpose=reauthorize so the server upserts.
	for _, want := range []string{
		`"purpose":"reauthorize"`,
		`"identity":"work"`,
		`"service":"google"`,
		`"account":"alr@x.com"`,
		`"connector_fqn":"github://acme/aileron-connector-google"`,
	} {
		if !strings.Contains(string(seenInitBody), want) {
			t.Errorf("init body missing %s: %s", want, seenInitBody)
		}
	}
	if !strings.Contains(string(seenFinishBody), `"code":"fresh-code"`) {
		t.Errorf("finish body missing code: %s", seenFinishBody)
	}
	if !strings.Contains(stdout.String(), "Reauthorized: oauth2/google/work") {
		t.Errorf("stdout missing reauthorized line: %s", stdout.String())
	}
}

// TestRunBinding_ReauthorizeRejectsAPIKey: the OAuth dance only makes
// sense for OAuth bindings. For api_key, the CLI surfaces a pointer to
// `rebind` rather than silently rebroadcasting a non-OAuth init that
// would 422.
func TestRunBinding_ReauthorizeRejectsAPIKey(t *testing.T) {
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{
			"name":"api_key/linear/team","kind":"api_key","service":"linear","identity":"team",
			"connector_fqn":"github://aileron/linear","status":"active",
			"created_at":"2024-01-01T00:00:00Z"
		}`)
	})
	var stdout, stderr bytes.Buffer
	code := runBinding([]string{"reauthorize", "api_key/linear/team"}, strings.NewReader(""),
		&stdout, &stderr)
	if code == 0 {
		t.Error("expected nonzero exit when reauthorizing an api_key binding")
	}
	if !strings.Contains(stderr.String(), "binding rebind") {
		t.Errorf("stderr should point at `binding rebind`: %s", stderr.String())
	}
}

func TestRunBinding_ReauthorizeNotFound(t *testing.T) {
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":{"code":"not_found"}}`)
	})
	var stdout, stderr bytes.Buffer
	code := runBinding([]string{"reauthorize", "oauth2/missing/x"}, strings.NewReader(""),
		&stdout, &stderr)
	if code == 0 {
		t.Error("expected nonzero exit when binding not found")
	}
}

func TestRunBinding_ReauthorizeRequiresExactlyOneArg(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runBinding([]string{"reauthorize"}, strings.NewReader(""), &stdout, &stderr)
	if code == 0 {
		t.Error("expected nonzero exit when name argument is missing")
	}
}

// TestRunBinding_ReauthorizeServerErrorOnGetIsExit1: when the GET of
// the existing binding returns a non-404 server error, the CLI must
// surface it as a nonzero exit so scripts notice rather than
// proceeding to a useless init.
func TestRunBinding_ReauthorizeServerErrorOnGetIsExit1(t *testing.T) {
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":"boom"}`)
	})
	var stdout, stderr bytes.Buffer
	code := runBinding([]string{"reauthorize", "oauth2/google/work"}, strings.NewReader(""),
		&stdout, &stderr)
	if code == 0 {
		t.Error("expected nonzero exit when GET binding returns 500")
	}
}

// TestRunBinding_ReauthorizeMalformedBindingResponseIsExit1: a
// successful GET with a body that doesn't parse as a binding must
// fail rather than panic.
func TestRunBinding_ReauthorizeMalformedBindingResponseIsExit1(t *testing.T) {
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{not json`)
	})
	var stdout, stderr bytes.Buffer
	code := runBinding([]string{"reauthorize", "oauth2/google/work"}, strings.NewReader(""),
		&stdout, &stderr)
	if code == 0 {
		t.Error("expected nonzero exit on malformed binding JSON")
	}
}

// TestRunBinding_ReauthorizeInitErrorIsExit1: server returns a non-200
// on init (e.g. session quota / 500). CLI propagates as nonzero exit.
func TestRunBinding_ReauthorizeInitErrorIsExit1(t *testing.T) {
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/bindings/oauth2/google/work":
			_, _ = io.WriteString(w, `{
				"name":"oauth2/google/work","kind":"oauth2","service":"google","identity":"work",
				"connector_fqn":"github://acme/g","created_at":"2024-01-01T00:00:00Z"
			}`)
		case "/bindings/setup/oauth2/init":
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":"boom"}`)
		}
	})
	var stdout, stderr bytes.Buffer
	code := runBinding([]string{"reauthorize", "oauth2/google/work"}, strings.NewReader(""),
		&stdout, &stderr)
	if code == 0 {
		t.Error("expected nonzero exit when init returns 500")
	}
}

// TestRunBinding_ReauthorizeMalformedInitResponseIsExit1: init returns
// 200 but with a body that doesn't parse as the expected envelope.
func TestRunBinding_ReauthorizeMalformedInitResponseIsExit1(t *testing.T) {
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/bindings/oauth2/google/work":
			_, _ = io.WriteString(w, `{
				"name":"oauth2/google/work","kind":"oauth2","service":"google","identity":"work",
				"connector_fqn":"github://acme/g","created_at":"2024-01-01T00:00:00Z"
			}`)
		case "/bindings/setup/oauth2/init":
			_, _ = io.WriteString(w, `{not json`)
		}
	})
	withFakeBrowser(t)
	var stdout, stderr bytes.Buffer
	code := runBinding([]string{"reauthorize", "oauth2/google/work"}, strings.NewReader(""),
		&stdout, &stderr)
	if code == 0 {
		t.Error("expected nonzero exit on malformed init response")
	}
}

// TestRunBinding_ReauthorizeBadRedirectURIRejected: defensive — if
// the server sends an init response with a redirect_uri lacking a
// port, the CLI can't bind a listener and must fail loudly.
func TestRunBinding_ReauthorizeBadRedirectURIRejected(t *testing.T) {
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/bindings/oauth2/google/work":
			_, _ = io.WriteString(w, `{
				"name":"oauth2/google/work","kind":"oauth2","service":"google","identity":"work",
				"connector_fqn":"github://acme/g","created_at":"2024-01-01T00:00:00Z"
			}`)
		case "/bindings/setup/oauth2/init":
			_, _ = io.WriteString(w,
				`{"session_id":"s","authorize_url":"http://x/auth","redirect_uri":"not-a-url"}`)
		}
	})
	var stdout, stderr bytes.Buffer
	code := runBinding([]string{"reauthorize", "oauth2/google/work"}, strings.NewReader(""),
		&stdout, &stderr)
	if code == 0 {
		t.Error("expected nonzero exit when redirect_uri has no port")
	}
}

// TestRunBinding_ReauthorizeFinishErrorIsExit1: finish returns a
// non-2xx (e.g. token exchange failed at the provider). Exit 1.
func TestRunBinding_ReauthorizeFinishErrorIsExit1(t *testing.T) {
	withFakeBrowser(t)
	var redirectURI string
	state := "rstate"
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/bindings/oauth2/google/work":
			_, _ = io.WriteString(w, `{
				"name":"oauth2/google/work","kind":"oauth2","service":"google","identity":"work",
				"connector_fqn":"github://acme/g","created_at":"2024-01-01T00:00:00Z"
			}`)
		case "/bindings/setup/oauth2/init":
			ln, _ := net.Listen("tcp", "127.0.0.1:0")
			port := ln.Addr().(*net.TCPAddr).Port
			_ = ln.Close()
			redirectURI = fmt.Sprintf("http://127.0.0.1:%d/callback", port)
			authorizeURL := fmt.Sprintf("http://provider.test/auth?state=%s&redirect_uri=%s",
				state, redirectURI)
			_, _ = io.WriteString(w, fmt.Sprintf(
				`{"session_id":"s","authorize_url":%q,"redirect_uri":%q}`,
				authorizeURL, redirectURI))
			go func() {
				time.Sleep(50 * time.Millisecond)
				_, _ = http.Get(redirectURI + "?code=c&state=" + state)
			}()
		case "/bindings/setup/oauth2/finish":
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = io.WriteString(w, `{"error":{"code":"token_exchange_failed"}}`)
		}
	})
	var stdout, stderr bytes.Buffer
	code := runBinding([]string{"reauthorize", "oauth2/google/work"}, strings.NewReader(""),
		&stdout, &stderr)
	if code == 0 {
		t.Error("expected nonzero exit when finish returns 422")
	}
}

// TestRunBinding_NameWithSlashesRoutesThroughParameterizedPath is a
// regression test for the reauthorize "error parsing response: invalid
// character '<' looking for beginning of value" bug: binding names are
// `<kind>/<service>/<identity>` and contain literal slashes, so the
// CLI must percent-encode them when building `/v1/bindings/{name}`
// URLs. Without encoding, Go's net/http ServeMux treats each slash as
// a new path segment, `/v1/bindings/{name}` (which matches one
// segment) does not match, and the request falls through to the
// webapp's catch-all handler — which returns HTML the CLI then fails
// to decode as JSON.
//
// The fakeBindingServer fixture matches on `r.URL.Path` (which Go
// percent-decodes), so it cannot detect this — the test must mount a
// real ServeMux with the `{name}` pattern AND a catch-all that
// returns HTML, then assert the catch-all is never reached.
func TestRunBinding_NameWithSlashesRoutesThroughParameterizedPath(t *testing.T) {
	withFakeBrowser(t)
	cases := []struct {
		verb       string
		args       []string
		method     string
		path       string // path the {name} route serves
		respStatus int
		respBody   string
		stdin      string
	}{
		{
			verb:       "inspect",
			args:       []string{"inspect", "oauth2/google/work"},
			method:     http.MethodGet,
			path:       "GET /v1/bindings/{name}",
			respStatus: http.StatusOK,
			respBody: `{"name":"oauth2/google/work","kind":"oauth2","service":"google","identity":"work",
				"connector_fqn":"github://acme/g","created_at":"2024-01-01T00:00:00Z"}`,
		},
		{
			verb:       "reauthorize-get",
			args:       []string{"reauthorize", "oauth2/google/work"},
			method:     http.MethodGet,
			path:       "GET /v1/bindings/{name}",
			respStatus: http.StatusNotFound, // 404 short-circuits before init.
			respBody:   `{"error":{"code":"not_found"}}`,
		},
		{
			verb:       "revoke",
			args:       []string{"revoke", "oauth2/google/work"},
			method:     http.MethodDelete,
			path:       "DELETE /v1/bindings/{name}",
			respStatus: http.StatusNoContent,
			respBody:   "",
			stdin:      "y\n",
		},
		{
			verb:       "rebind",
			args:       []string{"rebind", "api_key/linear/team"},
			method:     http.MethodPost,
			path:       "POST /v1/bindings/{name}/rebind",
			respStatus: http.StatusOK,
			respBody: `{"name":"api_key/linear/team","kind":"api_key","service":"linear","identity":"team",
				"connector_fqn":"github://acme/l","created_at":"2024-01-01T00:00:00Z"}`,
			stdin: "new-key\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.verb, func(t *testing.T) {
			var matchedName string
			var fallbackHit bool
			mux := http.NewServeMux()
			mux.HandleFunc(tc.path, func(w http.ResponseWriter, r *http.Request) {
				matchedName = r.PathValue("name")
				if tc.respStatus != 0 && tc.respStatus != http.StatusOK {
					w.WriteHeader(tc.respStatus)
				}
				if tc.respBody != "" {
					_, _ = io.WriteString(w, tc.respBody)
				}
			})
			// Mirrors the daemon's `mux.Handle("/", webappHandler())`:
			// any unmatched path serves HTML, which is exactly what
			// blows up the JSON decoder in the bug being regressed.
			mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
				fallbackHit = true
				w.Header().Set("Content-Type", "text/html")
				_, _ = io.WriteString(w, "<!DOCTYPE html><html><body>webapp</body></html>")
			})
			srv := httptest.NewServer(mux)
			t.Cleanup(srv.Close)
			t.Setenv("AILERON_API_URL", srv.URL+"/v1")

			var stdout, stderr bytes.Buffer
			_ = runBinding(tc.args, strings.NewReader(tc.stdin), &stdout, &stderr)
			if fallbackHit {
				t.Fatalf("request fell through to webapp catch-all; "+
					"binding name was not percent-encoded. stderr=%s", stderr.String())
			}
			wantName := tc.args[1]
			if matchedName != wantName {
				t.Errorf("PathValue(name) = %q, want %q (raw path: not encoded)",
					matchedName, wantName)
			}
		})
	}
}

// TestRunBinding_ListSurfacesStaleReason: when a binding is `stale`,
// the list view appends its reason inline so the user sees what to do
// without needing `--json`.
func TestRunBinding_ListSurfacesStaleReason(t *testing.T) {
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"items":[
			{"name":"oauth2/google/work","kind":"oauth2","service":"google","identity":"work",
			 "connector_fqn":"github://acme/aileron-connector-google",
			 "status":"stale","stale_reason":"scope_drift",
			 "missing_scopes":["https://www.googleapis.com/auth/drive"],
			 "created_at":"2024-01-01T00:00:00Z"}
		]}`)
	})
	var stdout, stderr bytes.Buffer
	code := run([]string{"binding", "list"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "stale (scope_drift)") {
		t.Errorf("list output should show stale reason inline; got:\n%s", stdout.String())
	}
}

// TestRunBinding_InspectSurfacesStaleDetails: the inspect view shows
// the missing scopes and a copy-paste reauthorize command so the user
// fixes the drift without paging through docs.
func TestRunBinding_InspectSurfacesStaleDetails(t *testing.T) {
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{
			"name":"oauth2/google/work","kind":"oauth2","service":"google","identity":"work",
			"connector_fqn":"github://acme/aileron-connector-google",
			"status":"stale","stale_reason":"scope_drift",
			"missing_scopes":["https://www.googleapis.com/auth/drive","https://www.googleapis.com/auth/documents"],
			"created_at":"2024-01-01T00:00:00Z"
		}`)
	})
	var stdout, stderr bytes.Buffer
	code := run([]string{"binding", "inspect", "oauth2/google/work"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	for _, want := range []string{
		"Stale:      scope_drift",
		"Missing:",
		"https://www.googleapis.com/auth/drive",
		"Resolve:    aileron binding reauthorize oauth2/google/work",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("inspect output missing %q:\n%s", want, stdout.String())
		}
	}
}

// --- runActionAdd: auto-install missing connectors (issue #413) ---

// withSeededKeyring isolates $HOME to a temp dir and pre-populates
// `~/.aileron/keyring.json` with a fake key per supplied authority.
// Used by action add / connector install tests so the auto-trust
// prompt added in #563 is a no-op for the test fixture's authority.
func withSeededKeyring(t *testing.T, authorities ...string) {
	t.Helper()
	withTempHome(t)
	path := cstore.DefaultKeyringPath()
	kr, err := cstore.LoadKeyring(path)
	if err != nil {
		t.Fatalf("withSeededKeyring: load: %v", err)
	}
	for _, auth := range authorities {
		pub, _ := genTestKey(t)
		kr.Add(auth, pub)
	}
	if err := kr.SaveKeyring(path); err != nil {
		t.Fatalf("withSeededKeyring: save: %v", err)
	}
}

// trustTestAuthority adds a fake key for `authority` to the keyring
// at the default path (callers must have isolated $HOME first via
// withSeededKeyring or withTempHome). Used to extend the seeded set
// after the test fixture is up.
func trustTestAuthority(t *testing.T, authority string) {
	t.Helper()
	path := cstore.DefaultKeyringPath()
	kr, err := cstore.LoadKeyring(path)
	if err != nil {
		t.Fatalf("trustTestAuthority: load: %v", err)
	}
	pub, _ := genTestKey(t)
	kr.Add(authority, pub)
	if err := kr.SaveKeyring(path); err != nil {
		t.Fatalf("trustTestAuthority: save: %v", err)
	}
}

// actionInstallServer routes preview + install requests to two
// separate handler functions. Mirrors connectorInstallServer.
// Either handler may be nil to default to a simple OK response.
//
// Per issue #563, runActionAdd now ensures the action's authority is
// trusted before posting to /actions/preview. The fixture pre-seeds
// the keyring with keys for the authorities the action add tests in
// this file actually exercise — `github://acme/x` and
// `github://acme/conn` — so the trust check is a silent no-op and
// these tests stay focused on the install flow rather than the
// trust prompt. Tests that exercise additional authorities (e.g.
// connector deps from a different publisher) call trustTestAuthority
// to extend the seed; tests that assert the trust prompt itself
// fires call withTempHome directly with no seed.
func actionInstallServer(t *testing.T, onPreview, onInstall http.HandlerFunc) *httptest.Server {
	withSeededKeyring(t, "github://acme/x", "github://acme/conn")
	return fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/actions/preview":
			if onPreview != nil {
				onPreview(w, r)
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"fqn":"github://acme/conn/actions/run","version":"0.1.0","hash":"sha256:abc","name":"my-action","signature_status":"verified","connector_deps":[]}`)
		case "/actions/install":
			if onInstall != nil {
				onInstall(w, r)
				return
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"name":"my-action","fqn":"github://acme/conn/actions/run","version":"0.1.0","source":"github://acme/conn/actions/run@0.1.0","path":"/tmp/my-action.md"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

// TestRunActionAdd_PreviewThenInstallOnYes asserts the canonical
// consent flow: POST /actions/preview, render preview, prompt y/N,
// then POST /actions/install with auto_install_connectors=true on
// "y". This is the path operators see for any new action with
// connector deps.
func TestRunActionAdd_PreviewThenInstallOnYes(t *testing.T) {
	var previewCalled, installCalled bool
	var installBody []byte
	actionInstallServer(t, func(w http.ResponseWriter, r *http.Request) {
		previewCalled = true
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{
			"fqn":"github://acme/conn/actions/run",
			"version":"0.1.0",
			"hash":"sha256:abc",
			"name":"my-action",
			"intent":"do the thing",
			"signature_status":"verified",
			"connector_deps":[
				{"fqn":"github://acme/conn","version":"1.0.0","hash":"sha256:depabc","capabilities":["op_a"],"already_installed":false}
			]
		}`)
	}, func(w http.ResponseWriter, r *http.Request) {
		installCalled = true
		installBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"name":"my-action","fqn":"github://acme/conn/actions/run","version":"0.1.0","source":"github://acme/conn/actions/run@0.1.0","path":"/tmp/my-action.md"}`)
	})

	stdin := strings.NewReader("y\n")
	var stdout, stderr bytes.Buffer
	code := runActionAdd([]string{"github://acme/conn/actions/run@0.1.0"}, stdin, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d; stderr = %s", code, stderr.String())
	}
	if !previewCalled {
		t.Error("preview endpoint not called")
	}
	if !installCalled {
		t.Error("install endpoint not called after 'y' confirmation")
	}
	if !strings.Contains(string(installBody), `"auto_install_connectors":true`) {
		t.Errorf("install body should set auto_install_connectors=true: %s", installBody)
	}
	out := stdout.String()
	for _, want := range []string{
		"Action install preview",
		"my-action",
		"do the thing",
		"verified",
		"Connectors that will be installed",
		"github://acme/conn@1.0.0",
		"op_a",
		"Install? [y/n]:",
		"Added: my-action",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

// TestRunActionAdd_PromptNoCancels asserts the cancel path: the
// operator types "n", install endpoint is not called, CLI prints
// "Cancelled." and exits 0.
func TestRunActionAdd_PromptNoCancels(t *testing.T) {
	installCalled := false
	actionInstallServer(t, nil, func(w http.ResponseWriter, r *http.Request) {
		installCalled = true
	})

	stdin := strings.NewReader("n\n")
	var stdout, stderr bytes.Buffer
	code := runActionAdd([]string{"github://acme/conn/actions/run@0.1.0"}, stdin, &stdout, &stderr)
	if code != 0 {
		t.Errorf("exit = %d, stderr=%s", code, stderr.String())
	}
	if installCalled {
		t.Error("install endpoint was called after operator typed 'n'")
	}
	if !strings.Contains(stdout.String(), "Cancelled.") {
		t.Errorf("expected 'Cancelled.' in output; got: %s", stdout.String())
	}
}

// TestRunActionAdd_YesFlagSkipsPrompt asserts the --yes flag: skip
// the prompt, go directly to install. Useful for scripts.
func TestRunActionAdd_YesFlagSkipsPrompt(t *testing.T) {
	installCalled := false
	actionInstallServer(t, nil, func(w http.ResponseWriter, r *http.Request) {
		installCalled = true
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"name":"my-action","fqn":"x","version":"0.1.0","source":"x","path":"/p"}`)
	})

	// Empty stdin — if the prompt fires we'd hit EOF. --yes
	// suppresses the prompt entirely.
	var stdout, stderr bytes.Buffer
	code := runActionAdd(
		[]string{"github://acme/conn/actions/run@0.1.0", "--yes"},
		strings.NewReader(""), &stdout, &stderr,
	)
	if code != 0 {
		t.Errorf("exit = %d, stderr=%s", code, stderr.String())
	}
	if !installCalled {
		t.Error("install endpoint not called with --yes")
	}
}

// TestRunActionAdd_AlreadyInstalledShortCircuits: when preview
// reports already_installed=true, the CLI prints "Already installed"
// and exits without prompting or hitting install.
func TestRunActionAdd_AlreadyInstalledShortCircuits(t *testing.T) {
	installCalled := false
	actionInstallServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"fqn":"github://acme/conn/actions/run","version":"0.1.0","hash":"sha256:abc","name":"my-action","already_installed":true,"connector_deps":[]}`)
	}, func(w http.ResponseWriter, r *http.Request) {
		installCalled = true
	})

	var stdout, stderr bytes.Buffer
	code := runActionAdd(
		[]string{"github://acme/conn/actions/run@0.1.0"},
		strings.NewReader(""), &stdout, &stderr,
	)
	if code != 0 {
		t.Errorf("exit = %d, stderr=%s", code, stderr.String())
	}
	if installCalled {
		t.Error("install endpoint was called after preview reported already_installed=true")
	}
	if !strings.Contains(stdout.String(), "Already installed") {
		t.Errorf("expected 'Already installed' in output; got: %s", stdout.String())
	}
}

// TestRunActionAdd_UpgradePromptAcceptForcesInstall asserts the
// upgrade-candidate path: preview reports `existing` (same name on
// disk with different bytes — typically a previous version), the CLI
// renders the upgrade banner with old→new versions, and on "y" sends
// install with force=true so the server overwrites the prior file
// instead of returning 409. Without this prompt, suite installs hit
// `action_exists` whenever the user already has any version of the
// action installed.
func TestRunActionAdd_UpgradePromptAcceptForcesInstall(t *testing.T) {
	var installBody []byte
	actionInstallServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{
			"fqn":"github://acme/conn/actions/run",
			"version":"0.2.0",
			"hash":"sha256:newbytes",
			"name":"my-action",
			"signature_status":"verified",
			"existing":{
				"version":"0.1.0",
				"hash":"sha256:oldbytes",
				"source":"github://acme/conn/actions/run@0.1.0",
				"path":"/Users/alr/.aileron/actions/my-action.md"
			},
			"connector_deps":[]
		}`)
	}, func(w http.ResponseWriter, r *http.Request) {
		installBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"name":"my-action","fqn":"github://acme/conn/actions/run","version":"0.2.0","source":"github://acme/conn/actions/run@0.2.0","path":"/Users/alr/.aileron/actions/my-action.md"}`)
	})

	stdin := strings.NewReader("y\n")
	var stdout, stderr bytes.Buffer
	code := runActionAdd([]string{"github://acme/conn/actions/run@0.2.0"}, stdin, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(string(installBody), `"force":true`) {
		t.Errorf("install body should set force=true on upgrade accept: %s", installBody)
	}
	out := stdout.String()
	for _, want := range []string{
		"Upgrade required",
		"Installed:  v0.1.0",
		"Requested:  v0.2.0",
		"Replace v0.1.0 with v0.2.0?",
		"Upgraded: my-action",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

// TestRunActionAdd_UpgradePromptDeclineSkipsInstall: declining the
// upgrade prompt does NOT call /actions/install and exits 0. The
// summary message should make clear the existing version was kept,
// not that the install failed.
func TestRunActionAdd_UpgradePromptDeclineSkipsInstall(t *testing.T) {
	installCalled := false
	actionInstallServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{
			"fqn":"github://acme/conn/actions/run",
			"version":"0.2.0",
			"hash":"sha256:newbytes",
			"name":"my-action",
			"signature_status":"verified",
			"existing":{
				"version":"0.1.0",
				"hash":"sha256:oldbytes",
				"source":"github://acme/conn/actions/run@0.1.0",
				"path":"/Users/alr/.aileron/actions/my-action.md"
			},
			"connector_deps":[]
		}`)
	}, func(w http.ResponseWriter, r *http.Request) {
		installCalled = true
	})

	stdin := strings.NewReader("n\n")
	var stdout, stderr bytes.Buffer
	code := runActionAdd([]string{"github://acme/conn/actions/run@0.2.0"}, stdin, &stdout, &stderr)
	if code != 0 {
		t.Errorf("decline should exit 0, got %d; stderr=%s", code, stderr.String())
	}
	if installCalled {
		t.Error("install endpoint must not be called when operator declines upgrade")
	}
	if !strings.Contains(stdout.String(), "Skipped: my-action (kept v0.1.0)") {
		t.Errorf("expected skip-with-kept-version message; got: %s", stdout.String())
	}
}

// TestRunActionAdd_UpgradeYesFlagForcesInstall: --yes implies consent
// to upgrade just as it implies consent to fresh install — so a suite
// with --yes can run unattended even when actions are at older
// versions on disk.
func TestRunActionAdd_UpgradeYesFlagForcesInstall(t *testing.T) {
	var installBody []byte
	actionInstallServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{
			"fqn":"github://acme/conn/actions/run",
			"version":"0.2.0",
			"hash":"sha256:newbytes",
			"name":"my-action",
			"signature_status":"verified",
			"existing":{
				"version":"0.1.0",
				"hash":"sha256:oldbytes",
				"source":"github://acme/conn/actions/run@0.1.0",
				"path":"/Users/alr/.aileron/actions/my-action.md"
			},
			"connector_deps":[]
		}`)
	}, func(w http.ResponseWriter, r *http.Request) {
		installBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"name":"my-action","fqn":"github://acme/conn/actions/run","version":"0.2.0","source":"x","path":"/p"}`)
	})

	var stdout, stderr bytes.Buffer
	code := runActionAdd(
		[]string{"github://acme/conn/actions/run@0.2.0", "--yes"},
		strings.NewReader(""), &stdout, &stderr,
	)
	if code != 0 {
		t.Errorf("exit = %d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(string(installBody), `"force":true`) {
		t.Errorf("--yes on upgrade should send force=true: %s", installBody)
	}
}

// TestRunActionAdd_PreviewSignatureFailureExits1 asserts ADR-0007's
// "signature failure is a hard fail" contract from the action
// path: preview returns 422, CLI exits 1 BEFORE prompting, install
// endpoint is never called.
func TestRunActionAdd_PreviewSignatureFailureExits1(t *testing.T) {
	installCalled := false
	actionInstallServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = io.WriteString(w, `{"error":{"code":"signature_failure","message":"signature_failure"}}`)
	}, func(w http.ResponseWriter, r *http.Request) {
		installCalled = true
	})
	var stdout, stderr bytes.Buffer
	code := runActionAdd(
		[]string{"github://acme/conn/actions/run@0.1.0", "--yes"},
		strings.NewReader(""), &stdout, &stderr,
	)
	if code == 0 {
		t.Errorf("expected nonzero exit; stderr=%s", stderr.String())
	}
	if installCalled {
		t.Error("install endpoint was called after preview signature_failure; --yes must NOT bypass")
	}
}

// TestRunActionAdd_PreviewRendersAlreadyInstalledDeps asserts the
// rendering split: deps with already_installed=true render under
// "already installed" (informational), deps with =false render
// under "will be installed". Lets the operator see exactly which
// connectors are new vs. existing.
func TestRunActionAdd_PreviewRendersAlreadyInstalledDeps(t *testing.T) {
	actionInstallServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{
			"fqn":"github://acme/conn/actions/run",
			"version":"0.1.0",
			"hash":"sha256:abc",
			"name":"my-action",
			"signature_status":"verified",
			"connector_deps":[
				{"fqn":"github://acme/conn-new","version":"1.0.0","hash":"sha256:new","already_installed":false},
				{"fqn":"github://acme/conn-existing","version":"2.0.0","hash":"sha256:exists","already_installed":true}
			]
		}`)
	}, nil)
	// The new (not-yet-installed) dep authority is checked for trust
	// before the install consent prompt; pre-trust it so this test
	// stays focused on the rendering split.
	trustTestAuthority(t, "github://acme/conn-new")

	stdin := strings.NewReader("n\n") // cancel — we just want the rendering
	var stdout, stderr bytes.Buffer
	runActionAdd([]string{"github://acme/conn/actions/run@0.1.0"}, stdin, &stdout, &stderr)

	out := stdout.String()
	for _, want := range []string{
		"will be installed",
		"github://acme/conn-new",
		"already installed",
		"github://acme/conn-existing",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

// --- runActionAdd: auto-trust publisher (issue #563) ---

// TestRunActionAdd_AutoTrustPromptsAndInstalls is the headline #563
// test: from a fresh machine (empty keyring, mock publisher.pub on
// the convention path), `aileron action add <FQN>` walks the user
// through the trust prompt → preview → install in one command. Both
// the keyring entry and the install must land.
func TestRunActionAdd_AutoTrustPromptsAndInstalls(t *testing.T) {
	home := withTempHome(t)
	pub, pemBytes := genTestKey(t)
	withMockGitHubRaw(t, map[string][]byte{
		"/acme/conn/HEAD/keys/publisher.pub": pemBytes,
	})

	var previewCalled, installCalled bool
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/actions/preview":
			previewCalled = true
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{
				"fqn":"github://acme/conn/actions/run",
				"version":"0.1.0",
				"hash":"sha256:abc",
				"name":"my-action",
				"signature_status":"verified",
				"connector_deps":[]
			}`)
		case "/actions/install":
			installCalled = true
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"name":"my-action","fqn":"github://acme/conn/actions/run","version":"0.1.0","source":"x","path":"/p"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	// Two sequential prompts share one bufio.Reader; runAction wraps
	// stdin so each prompt picks up where the previous left off.
	stdin := bufio.NewReader(strings.NewReader("y\ny\n")) // trust y, install y
	var stdout, stderr bytes.Buffer
	code := runActionAdd(
		[]string{"github://acme/conn/actions/run@0.1.0"},
		stdin, &stdout, &stderr,
	)
	if code != 0 {
		t.Fatalf("exit = %d; stderr = %s", code, stderr.String())
	}
	if !previewCalled {
		t.Error("preview endpoint should have been called after trust prompt accepted")
	}
	if !installCalled {
		t.Error("install endpoint should have been called after both prompts accepted")
	}
	out := stdout.String()
	for _, want := range []string{
		"Publisher github://acme/conn is not yet trusted",
		"Trust publisher github://acme/conn?",
		"✓ Trusted publisher github://acme\n",
		"Action install preview",
		"Install? [y/n]:",
		"Added: my-action",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	// Trust must persist across runs; the grant is owner-level so it
	// covers every connector this publisher ships (ADR-0013).
	kr, err := cstore.LoadKeyring(filepath.Join(home, ".aileron", "keyring.json"))
	if err != nil {
		t.Fatalf("LoadKeyring: %v", err)
	}
	if !kr.HasOwnerKey("github://acme", pub) {
		t.Error("keyring should contain owner-level trusted key after auto-trust")
	}
}

// TestRunActionAdd_HubCompositeAcceptTrustsAuthoritiesAndInstalls is
// the headline #709 acceptance test: when the action is Hub-listed,
// the CLI renders a single composite trust panel covering the action
// plus every connector authority it depends on. One "y" trusts every
// surfaced authority and proceeds to preview + install. Per-authority
// trust prompts are suppressed.
func TestRunActionAdd_HubCompositeAcceptTrustsAuthoritiesAndInstalls(t *testing.T) {
	home := withTempHome(t)
	pub, pemBytes := genTestKey(t)
	withMockGitHubRaw(t, map[string][]byte{
		"/acme/conn/HEAD/keys/publisher.pub": pemBytes,
	})

	var hubCalled, previewCalled, installCalled bool
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/hub/action-install-decision":
			hubCalled = true
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{
				"kind":"action",
				"fqn":"github://acme/conn/actions/run",
				"description":"do the thing",
				"publisher_github":"acme",
				"connector_fqn":"github://acme/conn",
				"authorities":[{
					"fqn":"github://acme/conn",
					"publisher_github":"acme",
					"fingerprint":"sha256:i4l2kuD8q++d5b9v8/LLI1",
					"trust_state":"unknown",
					"publisher_footprint":[],
					"risk_indicators":["First connector by this publisher you've installed"]
				}]
			}`)
		case "/actions/preview":
			previewCalled = true
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{
				"fqn":"github://acme/conn/actions/run","version":"0.1.0",
				"hash":"sha256:abc","name":"my-action","signature_status":"verified",
				"connector_deps":[]
			}`)
		case "/actions/install":
			installCalled = true
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"name":"my-action","fqn":"github://acme/conn/actions/run","version":"0.1.0","source":"x","path":"/p"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	// One y at the composite prompt; one y at the per-action install
	// prompt. The per-authority trust prompts that the legacy flow
	// would fire are suppressed because trustState records every
	// composite authority as already trusted.
	stdin := bufio.NewReader(strings.NewReader("y\ny\n"))
	var stdout, stderr bytes.Buffer
	code := runActionAdd(
		[]string{"github://acme/conn/actions/run@0.1.0"},
		stdin, &stdout, &stderr,
	)
	if code != 0 {
		t.Fatalf("exit = %d; stderr = %s", code, stderr.String())
	}
	if !hubCalled || !previewCalled || !installCalled {
		t.Errorf("expected all three endpoints to fire; hub=%v preview=%v install=%v", hubCalled, previewCalled, installCalled)
	}
	out := stdout.String()
	for _, want := range []string{
		"Hub install-decision (action)",
		"Action:    github://acme/conn/actions/run",
		"Summary:   do the thing",
		"Connector: github://acme/conn",
		"sha256:i4l2kuD8q++d5b9v8/LLI1",
		"Trust these publishers and install?",
		"First connector by this publisher",
		"Added: my-action",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	// Per-authority "Publisher X is not yet trusted" lines MUST NOT
	// fire — the composite consent already covered them. Their absence
	// is the load-bearing UX promise of the composite flow.
	if strings.Contains(out, "Publisher github://acme/conn is not yet trusted.\nAileron will fetch") {
		t.Errorf("per-authority trust prompt fired after composite accept:\n%s", out)
	}
	// Trust persists post-install — composite y also wrote the keyring.
	kr, err := cstore.LoadKeyring(filepath.Join(home, ".aileron", "keyring.json"))
	if err != nil {
		t.Fatalf("LoadKeyring: %v", err)
	}
	if !kr.HasOwnerKey("github://acme", pub) {
		t.Error("keyring should contain owner-level trusted key after composite accept")
	}
}

// TestRunActionAdd_HubCompositeDeclineAbortsBeforePreview: declining at
// the composite prompt aborts cleanly (exit 0, no preview/install
// calls, keyring stays empty). Mirrors the legacy decline path but at
// the new consent surface.
func TestRunActionAdd_HubCompositeDeclineAbortsBeforePreview(t *testing.T) {
	home := withTempHome(t)
	_, pemBytes := genTestKey(t)
	withMockGitHubRaw(t, map[string][]byte{
		"/acme/conn/HEAD/keys/publisher.pub": pemBytes,
	})

	previewCalled := false
	installCalled := false
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/hub/action-install-decision":
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{
				"kind":"action","fqn":"github://acme/conn/actions/run",
				"description":"x","publisher_github":"acme",
				"connector_fqn":"github://acme/conn",
				"authorities":[{
					"fqn":"github://acme/conn","publisher_github":"acme",
					"fingerprint":"sha256:any","trust_state":"unknown",
					"publisher_footprint":[],"risk_indicators":["x"]
				}]
			}`)
		case "/actions/preview":
			previewCalled = true
		case "/actions/install":
			installCalled = true
		}
	})

	stdin := strings.NewReader("n\n")
	var stdout, stderr bytes.Buffer
	code := runActionAdd(
		[]string{"github://acme/conn/actions/run@0.1.0"},
		stdin, &stdout, &stderr,
	)
	// Composite decline is a clean cancel: exit 0, no install pipeline
	// runs. The legacy decline-at-trust path historically exited
	// nonzero; the action-first amendment treats decline as the
	// operator's normal "no thanks" interaction.
	if code != 0 {
		t.Errorf("expected clean cancel exit 0, got %d; stderr=%s", code, stderr.String())
	}
	if previewCalled || installCalled {
		t.Errorf("preview/install should not fire on decline; preview=%v install=%v", previewCalled, installCalled)
	}
	if !strings.Contains(stdout.String(), "Cancelled.") {
		t.Errorf("expected 'Cancelled.' in output:\n%s", stdout.String())
	}
	kr, _ := cstore.LoadKeyring(filepath.Join(home, ".aileron", "keyring.json"))
	if kr != nil && len(kr.Keys("github://acme/conn")) != 0 {
		t.Error("keyring should remain empty when composite declined")
	}
}

// TestRunActionAdd_HubCompositeYesFlagAutoAccepts: --yes short-circuits
// the composite prompt the same way it short-circuits the legacy
// trust + install prompts. CI scripts and agent-driven installs stay
// non-interactive.
func TestRunActionAdd_HubCompositeYesFlagAutoAccepts(t *testing.T) {
	withTempHome(t)
	_, pemBytes := genTestKey(t)
	withMockGitHubRaw(t, map[string][]byte{
		"/acme/conn/HEAD/keys/publisher.pub": pemBytes,
	})

	hubCalled := 0
	installCalled := false
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/hub/action-install-decision":
			hubCalled++
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{
				"kind":"action","fqn":"github://acme/conn/actions/run",
				"description":"x","publisher_github":"acme",
				"connector_fqn":"github://acme/conn",
				"authorities":[{
					"fqn":"github://acme/conn","publisher_github":"acme",
					"fingerprint":"sha256:any","trust_state":"unknown",
					"publisher_footprint":[],"risk_indicators":["x"]
				}]
			}`)
		case "/actions/preview":
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"fqn":"github://acme/conn/actions/run","version":"0.1.0","hash":"sha256:abc","name":"my-action","signature_status":"verified","connector_deps":[]}`)
		case "/actions/install":
			installCalled = true
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"name":"my-action","fqn":"github://acme/conn/actions/run","version":"0.1.0","source":"x","path":"/p"}`)
		}
	})

	// No stdin reads — --yes must skip every prompt.
	var stdout, stderr bytes.Buffer
	code := runActionAdd(
		[]string{"github://acme/conn/actions/run@0.1.0", "--yes"},
		strings.NewReader(""), &stdout, &stderr,
	)
	if code != 0 {
		t.Fatalf("exit = %d; stderr=%s", code, stderr.String())
	}
	if hubCalled != 1 || !installCalled {
		t.Errorf("expected hub composite to fire once and install to run; hub=%d install=%v", hubCalled, installCalled)
	}
	// Composite panel must NOT be rendered when --yes is in effect —
	// that would surface a wall of trust info before silent install,
	// confusing for the script consumer.
	if strings.Contains(stdout.String(), "Trust these publishers and install?") {
		t.Errorf("composite prompt fired despite --yes:\n%s", stdout.String())
	}
}

// TestRunActionAdd_HubCompositeAllAlreadyTrustedAutoAccepts: when every
// authority in the action composite carries trust_state
// "already_trusted", the y/n/d prompt is skipped, but the composite
// panel still renders for transparency and the install proceeds.
// Regression for feedback #1150.
func TestRunActionAdd_HubCompositeAllAlreadyTrustedAutoAccepts(t *testing.T) {
	withTempHome(t)
	_, pemBytes := genTestKey(t)
	withMockGitHubRaw(t, map[string][]byte{
		"/acme/conn/HEAD/keys/publisher.pub": pemBytes,
	})

	hubCalled := 0
	installCalled := false
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/hub/action-install-decision":
			hubCalled++
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{
				"kind":"action","fqn":"github://acme/conn/actions/run",
				"description":"x","publisher_github":"acme",
				"connector_fqn":"github://acme/conn",
				"authorities":[{
					"fqn":"github://acme/conn","publisher_github":"acme",
					"fingerprint":"sha256:any","trust_state":"already_trusted",
					"publisher_footprint":[],"risk_indicators":[]
				}]
			}`)
		case "/actions/preview":
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"fqn":"github://acme/conn/actions/run","version":"0.1.0","hash":"sha256:abc","name":"my-action","signature_status":"verified","connector_deps":[]}`)
		case "/actions/install":
			installCalled = true
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"name":"my-action","fqn":"github://acme/conn/actions/run","version":"0.1.0","source":"x","path":"/p"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	// No "y" for the composite prompt — it auto-accepts. The single "y"
	// answers the downstream per-action install consent.
	stdin := bufio.NewReader(strings.NewReader("y\n"))
	var stdout, stderr bytes.Buffer
	code := runActionAdd(
		[]string{"github://acme/conn/actions/run@0.1.0"},
		stdin, &stdout, &stderr,
	)
	if code != 0 {
		t.Fatalf("exit = %d; stderr=%s", code, stderr.String())
	}
	if hubCalled != 1 || !installCalled {
		t.Errorf("expected hub composite to fire once and install to run; hub=%d install=%v", hubCalled, installCalled)
	}
	out := stdout.String()
	// The y/n/d prompt MUST NOT render when every authority is already
	// trusted.
	if strings.Contains(out, "Trust these publishers and install?") {
		t.Errorf("composite prompt fired despite all authorities already trusted:\n%s", out)
	}
	// The composite panel still renders for transparency.
	for _, want := range []string{
		"Hub install-decision (action)",
		"Action:    github://acme/conn/actions/run",
		"already trusted",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

// TestRunActionAdd_HubCompositeMixedTrustStillPrompts: when the action
// composite mixes an already_trusted authority with an unknown one, the
// y/n/d prompt still fires. Auto-accept only applies when every
// authority is already trusted. Regression for feedback #1150.
func TestRunActionAdd_HubCompositeMixedTrustStillPrompts(t *testing.T) {
	withTempHome(t)
	_, pemBytes := genTestKey(t)
	withMockGitHubRaw(t, map[string][]byte{
		"/acme/conn/HEAD/keys/publisher.pub": pemBytes,
	})

	hubCalled := 0
	previewCalled := false
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/hub/action-install-decision":
			hubCalled++
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{
				"kind":"action","fqn":"github://acme/conn/actions/run",
				"description":"x","publisher_github":"acme",
				"connector_fqn":"github://acme/conn",
				"authorities":[
					{
						"fqn":"github://acme/conn","publisher_github":"acme",
						"fingerprint":"sha256:any","trust_state":"already_trusted",
						"publisher_footprint":[],"risk_indicators":[]
					},
					{
						"fqn":"github://other/dep","publisher_github":"other",
						"fingerprint":"sha256:other","trust_state":"unknown",
						"publisher_footprint":[],"risk_indicators":["x"]
					}
				]
			}`)
		case "/actions/preview":
			previewCalled = true
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"fqn":"github://acme/conn/actions/run","version":"0.1.0","hash":"sha256:abc","name":"my-action","signature_status":"verified","connector_deps":[]}`)
		case "/actions/install":
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"name":"my-action","fqn":"github://acme/conn/actions/run","version":"0.1.0","source":"x","path":"/p"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	// Decline at the composite prompt to prove it fired.
	stdin := bufio.NewReader(strings.NewReader("n\n"))
	var stdout, stderr bytes.Buffer
	code := runActionAdd(
		[]string{"github://acme/conn/actions/run@0.1.0"},
		stdin, &stdout, &stderr,
	)
	if code != 0 {
		t.Fatalf("exit = %d; stderr=%s", code, stderr.String())
	}
	if hubCalled != 1 {
		t.Errorf("composite should fire exactly once, got %d", hubCalled)
	}
	if previewCalled {
		t.Error("declining the composite should abort before preview")
	}
	out := stdout.String()
	// The y/n/d prompt MUST fire when the set mixes trust states.
	if !strings.Contains(out, "Trust these publishers and install?") {
		t.Errorf("composite prompt should fire on mixed trust states:\n%s", out)
	}
	if !strings.Contains(out, "Cancelled.") {
		t.Errorf("expected decline to cancel:\n%s", out)
	}
}

// TestRunActionAdd_HubComposite422FallsThroughToLegacy: when the
// composite endpoint can't precompute the trust panel (e.g. action's
// connector_fqn isn't Hub-listed), the CLI falls through to the
// legacy per-authority trust + preview flow. Operators with Hub
// configuration gaps still get their installs done.
func TestRunActionAdd_HubComposite422FallsThroughToLegacy(t *testing.T) {
	withTempHome(t)
	_, pemBytes := genTestKey(t)
	withMockGitHubRaw(t, map[string][]byte{
		"/acme/conn/HEAD/keys/publisher.pub": pemBytes,
	})

	hubCalled, previewCalled, installCalled := false, false, false
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/hub/action-install-decision":
			hubCalled = true
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = io.WriteString(w, `{"error":{"code":"missing_connector"}}`)
		case "/actions/preview":
			previewCalled = true
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"fqn":"github://acme/conn/actions/run","version":"0.1.0","hash":"sha256:abc","name":"my-action","signature_status":"verified","connector_deps":[]}`)
		case "/actions/install":
			installCalled = true
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"name":"my-action","fqn":"github://acme/conn/actions/run","version":"0.1.0","source":"x","path":"/p"}`)
		}
	})

	stdin := bufio.NewReader(strings.NewReader("y\ny\n"))
	var stdout, stderr bytes.Buffer
	code := runActionAdd(
		[]string{"github://acme/conn/actions/run@0.1.0"},
		stdin, &stdout, &stderr,
	)
	if code != 0 {
		t.Fatalf("exit = %d; stderr=%s", code, stderr.String())
	}
	if !hubCalled || !previewCalled || !installCalled {
		t.Errorf("expected fall-through to legacy flow; hub=%v preview=%v install=%v", hubCalled, previewCalled, installCalled)
	}
	// Composite panel must NOT render when the endpoint 422s.
	if strings.Contains(stdout.String(), "Hub install-decision (action)") {
		t.Errorf("composite panel rendered on 422 fall-through:\n%s", stdout.String())
	}
	// Legacy per-authority trust prompt MUST render (operator typed y
	// to accept it).
	if !strings.Contains(stdout.String(), "Publisher github://acme/conn is not yet trusted") {
		t.Errorf("legacy trust prompt should fire on fall-through:\n%s", stdout.String())
	}
}

// TestRunActionAdd_HubCompositeDetailsExpandsAndAccepts: typing "d"
// at the composite prompt expands the per-authority publisher_footprint
// view; a subsequent "y" accepts. Validates the d=details → y two-step.
func TestRunActionAdd_HubCompositeDetailsExpandsAndAccepts(t *testing.T) {
	withTempHome(t)
	_, pemBytes := genTestKey(t)
	withMockGitHubRaw(t, map[string][]byte{
		"/acme/conn/HEAD/keys/publisher.pub": pemBytes,
	})

	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/hub/action-install-decision":
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{
				"kind":"action","fqn":"github://acme/conn/actions/run",
				"description":"x","publisher_github":"acme",
				"connector_fqn":"github://acme/conn",
				"authorities":[{
					"fqn":"github://acme/conn","publisher_github":"acme",
					"fingerprint":"sha256:any","trust_state":"unknown",
					"publisher_footprint":["github://acme/other-conn","github://acme/third-conn"],
					"risk_indicators":["First connector by this publisher you've installed"]
				}]
			}`)
		case "/actions/preview":
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"fqn":"github://acme/conn/actions/run","version":"0.1.0","hash":"sha256:abc","name":"my-action","signature_status":"verified","connector_deps":[]}`)
		case "/actions/install":
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"name":"my-action","fqn":"github://acme/conn/actions/run","version":"0.1.0","source":"x","path":"/p"}`)
		}
	})

	// d → details, y → accept composite, y → accept the action install
	stdin := bufio.NewReader(strings.NewReader("d\ny\ny\n"))
	var stdout, stderr bytes.Buffer
	code := runActionAdd(
		[]string{"github://acme/conn/actions/run@0.1.0"},
		stdin, &stdout, &stderr,
	)
	if code != 0 {
		t.Fatalf("exit = %d; stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	// Details expansion surfaces the publisher_footprint.
	for _, want := range []string{
		"Other connectors by this publisher",
		"github://acme/other-conn",
		"github://acme/third-conn",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("details mode missing %q:\n%s", want, out)
		}
	}
}

// TestRunActionAdd_HubComposite503FallsThroughToLegacyWithNote: 503
// from the daemon's composite endpoint surfaces a one-line stderr
// note and falls through to the legacy trust flow. Operators with
// broken daemon-side Hub config still install actions they've pre-
// trusted.
func TestRunActionAdd_HubComposite503FallsThroughToLegacyWithNote(t *testing.T) {
	withTempHome(t)
	_, pemBytes := genTestKey(t)
	withMockGitHubRaw(t, map[string][]byte{
		"/acme/conn/HEAD/keys/publisher.pub": pemBytes,
	})

	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/hub/action-install-decision":
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"error":{"code":"hub_unreachable"}}`)
		case "/actions/preview":
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"fqn":"github://acme/conn/actions/run","version":"0.1.0","hash":"sha256:abc","name":"my-action","signature_status":"verified","connector_deps":[]}`)
		case "/actions/install":
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"name":"my-action","fqn":"github://acme/conn/actions/run","version":"0.1.0","source":"x","path":"/p"}`)
		}
	})

	stdin := bufio.NewReader(strings.NewReader("y\ny\n"))
	var stdout, stderr bytes.Buffer
	code := runActionAdd(
		[]string{"github://acme/conn/actions/run@0.1.0"},
		stdin, &stdout, &stderr,
	)
	if code != 0 {
		t.Fatalf("exit = %d; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "hub install-decision returned 503") {
		t.Errorf("expected one-line stderr note on 503; got:\n%s", stderr.String())
	}
	// Legacy flow must still produce its trust prompt.
	if !strings.Contains(stdout.String(), "Publisher github://acme/conn is not yet trusted") {
		t.Errorf("legacy trust prompt should still fire after 503 fall-through:\n%s", stdout.String())
	}
}

// TestRunActionAdd_HubCompositeMalformedJSONFallsThroughWithNote: 200
// with un-parseable body surfaces a note and falls through. Guards
// against a daemon shipping a wire-incompatible payload silently
// blocking installs that the legacy path can complete.
func TestRunActionAdd_HubCompositeMalformedJSONFallsThroughWithNote(t *testing.T) {
	withTempHome(t)
	_, pemBytes := genTestKey(t)
	withMockGitHubRaw(t, map[string][]byte{
		"/acme/conn/HEAD/keys/publisher.pub": pemBytes,
	})

	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/hub/action-install-decision":
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `not valid json`)
		case "/actions/preview":
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"fqn":"github://acme/conn/actions/run","version":"0.1.0","hash":"sha256:abc","name":"my-action","signature_status":"verified","connector_deps":[]}`)
		case "/actions/install":
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"name":"my-action","fqn":"github://acme/conn/actions/run","version":"0.1.0","source":"x","path":"/p"}`)
		}
	})

	stdin := bufio.NewReader(strings.NewReader("y\ny\n"))
	var stdout, stderr bytes.Buffer
	code := runActionAdd(
		[]string{"github://acme/conn/actions/run@0.1.0"},
		stdin, &stdout, &stderr,
	)
	if code != 0 {
		t.Fatalf("exit = %d; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "could not parse hub install-decision") {
		t.Errorf("expected parse-failure note; got:\n%s", stderr.String())
	}
}

// TestRunActionAdd_HubCompositeDetailsTwiceTreatsAsDecline: typing `d`
// twice expands the panel then declines (since the panel was already
// in details mode, a second `d` is meaningless and counts as cancel).
// Mirrors the connector-install composite contract.
func TestRunActionAdd_HubCompositeDetailsTwiceTreatsAsDecline(t *testing.T) {
	withTempHome(t)
	_, pemBytes := genTestKey(t)
	withMockGitHubRaw(t, map[string][]byte{
		"/acme/conn/HEAD/keys/publisher.pub": pemBytes,
	})

	previewCalled := false
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/hub/action-install-decision":
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{
				"kind":"action","fqn":"github://acme/conn/actions/run",
				"description":"x","publisher_github":"acme",
				"connector_fqn":"github://acme/conn",
				"authorities":[{
					"fqn":"github://acme/conn","publisher_github":"acme",
					"fingerprint":"sha256:any","trust_state":"unknown",
					"publisher_footprint":[],"risk_indicators":["x"]
				}]
			}`)
		case "/actions/preview":
			previewCalled = true
		}
	})

	stdin := bufio.NewReader(strings.NewReader("d\nd\n"))
	var stdout, stderr bytes.Buffer
	code := runActionAdd(
		[]string{"github://acme/conn/actions/run@0.1.0"},
		stdin, &stdout, &stderr,
	)
	if code != 0 {
		t.Errorf("expected clean cancel exit 0, got %d", code)
	}
	if previewCalled {
		t.Error("preview should not fire after second `d` treated as decline")
	}
	if !strings.Contains(stdout.String(), "Cancelled.") {
		t.Errorf("expected 'Cancelled.' in output:\n%s", stdout.String())
	}
}

// TestRunActionAdd_TrustDeclineAbortsBeforePreview asserts the cancel
// path: typing "n" at the trust prompt aborts with a nonzero exit
// code and never calls /actions/preview. The keyring stays empty.
func TestRunActionAdd_TrustDeclineAbortsBeforePreview(t *testing.T) {
	home := withTempHome(t)
	_, pemBytes := genTestKey(t)
	withMockGitHubRaw(t, map[string][]byte{
		"/acme/conn/HEAD/keys/publisher.pub": pemBytes,
	})

	previewCalled := false
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/actions/preview" {
			previewCalled = true
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{}`)
	})

	stdin := strings.NewReader("n\n")
	var stdout, stderr bytes.Buffer
	code := runActionAdd(
		[]string{"github://acme/conn/actions/run@0.1.0"},
		stdin, &stdout, &stderr,
	)
	if code == 0 {
		t.Errorf("expected nonzero exit when trust declined; stderr=%s", stderr.String())
	}
	if previewCalled {
		t.Error("preview endpoint should NOT be called when trust declined")
	}
	// Keyring should still be empty.
	kr, _ := cstore.LoadKeyring(filepath.Join(home, ".aileron", "keyring.json"))
	if kr != nil && len(kr.Keys("github://acme/conn")) != 0 {
		t.Error("keyring should remain empty when user declined trust")
	}
}

// TestRunActionAdd_YesFlagAutoTrusts asserts the non-interactive
// surface from #563 acceptance criteria: with --yes, the trust prompt
// is auto-accepted (key fetched + persisted) and the install proceeds
// without ever prompting. Suitable for CI scripts and agent-driven
// installs.
func TestRunActionAdd_YesFlagAutoTrusts(t *testing.T) {
	home := withTempHome(t)
	pub, pemBytes := genTestKey(t)
	withMockGitHubRaw(t, map[string][]byte{
		"/acme/conn/HEAD/keys/publisher.pub": pemBytes,
	})

	installCalled := false
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/actions/preview":
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{
				"fqn":"github://acme/conn/actions/run","version":"0.1.0",
				"hash":"sha256:abc","name":"my-action",
				"signature_status":"verified","connector_deps":[]
			}`)
		case "/actions/install":
			installCalled = true
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"name":"my-action","fqn":"x","version":"0.1.0","source":"x","path":"/p"}`)
		}
	})

	var stdout, stderr bytes.Buffer
	code := runActionAdd(
		[]string{"github://acme/conn/actions/run@0.1.0", "--yes"},
		strings.NewReader(""), // empty stdin: prompts would EOF
		&stdout, &stderr,
	)
	if code != 0 {
		t.Fatalf("exit = %d; stderr = %s", code, stderr.String())
	}
	if !installCalled {
		t.Error("install endpoint not called with --yes")
	}
	if strings.Contains(stdout.String(), "Trust publisher github://acme/conn?") {
		t.Errorf("--yes should suppress the trust prompt; got: %s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "✓ Trusted publisher github://acme\n") {
		t.Errorf("expected owner-level key-added confirmation line; got: %s", stdout.String())
	}
	kr, err := cstore.LoadKeyring(filepath.Join(home, ".aileron", "keyring.json"))
	if err != nil {
		t.Fatalf("LoadKeyring: %v", err)
	}
	if !kr.HasOwnerKey("github://acme", pub) {
		t.Error("keyring should contain owner-level trusted key after --yes auto-trust")
	}
}

// TestRunActionAdd_AlreadyTrustedAuthorityIsSilent asserts the no-op
// re-run path from #563 acceptance criteria: when the authority is
// already trusted, runActionAdd does not render the trust prompt or
// the "Trusted publisher" line — the user sees only the existing
// preview / consent flow.
func TestRunActionAdd_AlreadyTrustedAuthorityIsSilent(t *testing.T) {
	actionInstallServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{
			"fqn":"github://acme/conn/actions/run","version":"0.1.0",
			"hash":"sha256:abc","name":"my-action",
			"signature_status":"verified","connector_deps":[]
		}`)
	}, nil)

	stdin := strings.NewReader("y\n") // only the install consent
	var stdout, stderr bytes.Buffer
	code := runActionAdd(
		[]string{"github://acme/conn/actions/run@0.1.0"},
		stdin, &stdout, &stderr,
	)
	if code != 0 {
		t.Fatalf("exit = %d; stderr = %s", code, stderr.String())
	}
	out := stdout.String()
	if strings.Contains(out, "Trust publisher") {
		t.Errorf("trust prompt should not render when authority already trusted; got: %s", out)
	}
	if strings.Contains(out, "is not yet trusted") {
		t.Errorf("trust banner should not render when authority already trusted; got: %s", out)
	}
}

// TestRunActionAdd_CrossAuthorityDepPromptsTrust asserts the
// connector-dep branch from the #563 acceptance criteria: when the
// action's preview lists a not-yet-installed connector dep from a
// *different* publisher, the CLI prompts to trust that publisher
// before posting to /actions/install.
func TestRunActionAdd_CrossAuthorityDepPromptsTrust(t *testing.T) {
	home := withTempHome(t)
	depPub, depPem := genTestKey(t)
	withMockGitHubRaw(t, map[string][]byte{
		"/acme/conn-dep/HEAD/keys/publisher.pub": depPem,
	})
	// Trust the action's authority but NOT the dep's authority.
	trustTestAuthority(t, "github://acme/conn")

	installCalled := false
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/actions/preview":
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{
				"fqn":"github://acme/conn/actions/run","version":"0.1.0",
				"hash":"sha256:abc","name":"my-action",
				"signature_status":"verified",
				"connector_deps":[
					{"fqn":"github://acme/conn-dep","version":"1.0.0","hash":"sha256:dep","already_installed":false}
				]
			}`)
		case "/actions/install":
			installCalled = true
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"name":"my-action","fqn":"x","version":"0.1.0","source":"x","path":"/p"}`)
		}
	})

	stdin := bufio.NewReader(strings.NewReader("y\ny\n")) // trust dep, install
	var stdout, stderr bytes.Buffer
	code := runActionAdd(
		[]string{"github://acme/conn/actions/run@0.1.0"},
		stdin, &stdout, &stderr,
	)
	if code != 0 {
		t.Fatalf("exit = %d; stderr = %s", code, stderr.String())
	}
	if !installCalled {
		t.Error("install should fire after trust + consent")
	}
	if !strings.Contains(stdout.String(), "Trust publisher github://acme/conn-dep?") {
		t.Errorf("expected trust prompt for cross-authority dep; got: %s", stdout.String())
	}
	kr, _ := cstore.LoadKeyring(filepath.Join(home, ".aileron", "keyring.json"))
	if !kr.HasOwnerKey("github://acme", depPub) {
		t.Error("dep authority should be trusted (owner-level) after auto-trust")
	}
}

// TestRunActionAdd_AlreadyInstalledDepDoesNotPromptTrust asserts a
// no-op no-op: when a connector dep is already_installed (per the
// preview) the CLI does NOT prompt to trust its authority — the
// connector pipeline already verified that signature when the dep
// was first installed; re-prompting would be noise.
func TestRunActionAdd_AlreadyInstalledDepDoesNotPromptTrust(t *testing.T) {
	withSeededKeyring(t, "github://acme/conn") // action authority only

	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/actions/preview":
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{
				"fqn":"github://acme/conn/actions/run","version":"0.1.0",
				"hash":"sha256:abc","name":"my-action",
				"signature_status":"verified",
				"connector_deps":[
					{"fqn":"github://acme/some-other-pub","version":"1.0.0","hash":"sha256:dep","already_installed":true}
				]
			}`)
		case "/actions/install":
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"name":"my-action","fqn":"x","version":"0.1.0","source":"x","path":"/p"}`)
		}
	})

	stdin := strings.NewReader("y\n") // just the install consent
	var stdout, stderr bytes.Buffer
	code := runActionAdd(
		[]string{"github://acme/conn/actions/run@0.1.0"},
		stdin, &stdout, &stderr,
	)
	if code != 0 {
		t.Fatalf("exit = %d; stderr = %s", code, stderr.String())
	}
	if strings.Contains(stdout.String(), "Trust publisher github://acme/some-other-pub?") {
		t.Errorf("should not prompt to trust an already-installed dep authority; got: %s", stdout.String())
	}
}

// TestRunActionAdd_PublisherKeyFetchFailureExits1 asserts the failure
// surface when the publisher hasn't committed `keys/publisher.pub`:
// the CLI surfaces the fetch error and exits nonzero. The user is
// not blocked staring at a quiet terminal — the trust step fails
// loudly.
func TestRunActionAdd_PublisherKeyFetchFailureExits1(t *testing.T) {
	withTempHome(t)
	// Mock GitHub raw with NO entry for the convention path → 404.
	withMockGitHubRaw(t, map[string][]byte{})

	previewCalled := false
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/actions/preview" {
			previewCalled = true
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{}`)
	})

	var stdout, stderr bytes.Buffer
	code := runActionAdd(
		[]string{"github://acme/conn/actions/run@0.1.0", "--yes"},
		strings.NewReader(""), &stdout, &stderr,
	)
	if code == 0 {
		t.Errorf("expected nonzero exit; stderr=%s", stderr.String())
	}
	if previewCalled {
		t.Error("preview should not be called after a failed key fetch")
	}
	if !strings.Contains(stderr.String(), "fetch publisher key") {
		t.Errorf("expected fetch error in stderr; got: %s", stderr.String())
	}
}

// --- runActionAddSuite: -f manifest install (issue #564) ---

// writeSuiteManifest writes a TOML suite manifest to a temp file
// and returns the path. The caller can then point `aileron action
// add -f` at it. Inline TOML keeps the test self-contained.
func writeSuiteManifest(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "suite.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write suite manifest: %v", err)
	}
	return path
}

// suiteInstallServer is the suite-test analogue of
// actionInstallServer. It seeds the keyring and stands up a fake
// daemon that records (preview, install) hits per FQN so tests can
// assert which actions made it through. Each handler may be nil to
// fall through to a default success response.
//
// per-FQN handlers receive the action FQN extracted from the
// request body (without @<version>) so the test can match on a
// specific entry.
func suiteInstallServer(
	t *testing.T,
	authorities []string,
	onPreview func(fqn string, w http.ResponseWriter, r *http.Request),
	onInstall func(fqn string, w http.ResponseWriter, r *http.Request),
) *httptest.Server {
	t.Helper()
	withSeededKeyring(t, authorities...)
	return fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			FQN string `json:"fqn"`
		}
		_ = json.Unmarshal(body, &req)
		// Restore body so handlers can re-read if they want.
		r.Body = io.NopCloser(strings.NewReader(string(body)))
		switch r.URL.Path {
		case "/actions/preview":
			if onPreview != nil {
				onPreview(req.FQN, w, r)
				return
			}
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, `{"fqn":%q,"version":"0.1.0","hash":"sha256:abc","name":%q,"signature_status":"verified","connector_deps":[]}`,
				req.FQN, lastSegment(req.FQN))
		case "/actions/install":
			if onInstall != nil {
				onInstall(req.FQN, w, r)
				return
			}
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"name":%q,"fqn":%q,"version":"0.1.0","source":%q,"path":"/p"}`,
				lastSegment(req.FQN), req.FQN, req.FQN+"@0.1.0")
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

// lastSegment returns the slug after the last `/` in an FQN,
// suitable as a synthetic action `name` in test responses.
func lastSegment(fqn string) string {
	if i := strings.LastIndex(fqn, "/"); i >= 0 {
		return fqn[i+1:]
	}
	return fqn
}

// TestRunActionAddSuite_HappyPathInstallsEveryAction is the headline
// #564 test: a local v2 manifest with three actions installs all
// three and the final summary lists them as added.
func TestRunActionAddSuite_HappyPathInstallsEveryAction(t *testing.T) {
	manifestPath := writeSuiteManifest(t, `
name = "test-suite"
description = "A test suite"

actions = [
  "github://acme/conn/actions/foo@0.1.0",
  "github://acme/conn/actions/bar@0.1.0",
  "github://acme/conn/actions/baz@0.1.0",
]
`)
	installed := map[string]int{}
	suiteInstallServer(t, []string{"github://acme/conn"}, nil, func(fqn string, w http.ResponseWriter, r *http.Request) {
		installed[fqn]++
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"name":%q,"fqn":%q,"version":"0.1.0","source":"x","path":"/p"}`,
			lastSegment(fqn), fqn)
	})

	var stdout, stderr bytes.Buffer
	code := runActionAddSuite([]string{"--yes", manifestPath}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d; stderr = %s", code, stderr.String())
	}
	for _, want := range []string{
		"github://acme/conn/actions/foo",
		"github://acme/conn/actions/bar",
		"github://acme/conn/actions/baz",
	} {
		if installed[want] != 1 {
			t.Errorf("install for %s: hits = %d, want 1", want, installed[want])
		}
	}
	out := stdout.String()
	for _, want := range []string{
		"Suite: test-suite",
		"3 action(s) to install",
		"Suite install summary:",
		"3 action(s): 3 added.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

// TestRunActionAddSuite_SharesTrustPromptAcrossActions: when every
// action in the suite shares an authority, the trust prompt fires
// at most once per run.
func TestRunActionAddSuite_SharesTrustPromptAcrossActions(t *testing.T) {
	withTempHome(t)
	_, pemBytes := genTestKey(t)
	withMockGitHubRaw(t, map[string][]byte{
		"/acme/conn/HEAD/keys/publisher.pub": pemBytes,
	})

	manifestPath := writeSuiteManifest(t, `
name = "shared-trust"
description = "Three actions, one publisher"

actions = [
  "github://acme/conn/actions/foo@0.1.0",
  "github://acme/conn/actions/bar@0.1.0",
  "github://acme/conn/actions/baz@0.1.0",
]
`)
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			FQN string `json:"fqn"`
		}
		_ = json.Unmarshal(body, &req)
		switch r.URL.Path {
		case "/actions/preview":
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, `{"fqn":%q,"version":"0.1.0","hash":"sha256:abc","name":%q,"signature_status":"verified","connector_deps":[]}`,
				req.FQN, lastSegment(req.FQN))
		case "/actions/install":
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"name":%q,"fqn":%q,"version":"0.1.0","source":"x","path":"/p"}`,
				lastSegment(req.FQN), req.FQN)
		}
	})

	var stdout, stderr bytes.Buffer
	code := runActionAddSuite([]string{"--yes", manifestPath}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d; stderr = %s", code, stderr.String())
	}
	count := strings.Count(stdout.String(), "Publisher github://acme/conn is not yet trusted")
	if count != 1 {
		t.Errorf("trust banner appeared %d times across the suite; want 1\nstdout:\n%s", count, stdout.String())
	}
}

// TestRunActionAddSuite_FailureSoftContinuesAfterMidStreamFailure:
// per-action failure → loop continues; summary lists each outcome;
// nonzero exit.
func TestRunActionAddSuite_FailureSoftContinuesAfterMidStreamFailure(t *testing.T) {
	manifestPath := writeSuiteManifest(t, `
name = "mixed"
description = "First and third succeed; second fails."

actions = [
  "github://acme/conn/actions/foo@0.1.0",
  "github://acme/conn/actions/bar@0.1.0",
  "github://acme/conn/actions/baz@0.1.0",
]
`)
	suiteInstallServer(t, []string{"github://acme/conn"},
		func(fqn string, w http.ResponseWriter, r *http.Request) {
			if strings.Contains(fqn, "/bar") {
				w.WriteHeader(http.StatusUnprocessableEntity)
				_, _ = io.WriteString(w, `{"error":{"code":"signature_failure","message":"bad signature"}}`)
				return
			}
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, `{"fqn":%q,"version":"0.1.0","hash":"sha256:abc","name":%q,"signature_status":"verified","connector_deps":[]}`,
				fqn, lastSegment(fqn))
		},
		nil,
	)

	var stdout, stderr bytes.Buffer
	code := runActionAddSuite([]string{"--yes", manifestPath}, strings.NewReader(""), &stdout, &stderr)
	if code == 0 {
		t.Errorf("expected nonzero exit when an action fails; stderr=%s", stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		"github://acme/conn/actions/foo@0.1.0",
		"github://acme/conn/actions/bar@0.1.0",
		"github://acme/conn/actions/baz@0.1.0",
		"3 action(s): 2 added, 1 failed.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "actions/bar@0.1.0 (exit 1)") {
		t.Errorf("bar entry should render with (exit N) suffix:\n%s", out)
	}
}

// TestRunActionAddSuite_AlreadyInstalledShortCircuits: when every
// action in the suite is already installed, the consent prompt is
// suppressed and the run exits 0 with a single all-already-installed
// message. Per #640, the suite-level consent screen is only shown
// when there is install work to commit.
func TestRunActionAddSuite_AlreadyInstalledShortCircuits(t *testing.T) {
	manifestPath := writeSuiteManifest(t, `
name = "rerun"
description = "Re-running with everything already installed."

actions = [
  "github://acme/conn/actions/foo@0.1.0",
  "github://acme/conn/actions/bar@0.1.0",
]
`)
	suiteInstallServer(t, []string{"github://acme/conn"},
		func(fqn string, w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, `{"fqn":%q,"version":"0.1.0","hash":"sha256:abc","name":%q,"already_installed":true,"connector_deps":[]}`,
				fqn, lastSegment(fqn))
		},
		func(fqn string, w http.ResponseWriter, r *http.Request) {
			t.Errorf("install endpoint should not be hit for already_installed entries (fqn=%s)", fqn)
		},
	)

	// Empty stdin: the suite must not prompt — if the consent screen
	// fired, the read would race and the test would be flaky.
	var stdout, stderr bytes.Buffer
	code := runActionAddSuite([]string{manifestPath}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d; stderr = %s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "All 2 action(s) already installed") {
		t.Errorf("expected all-already-installed short-circuit: %s", out)
	}
	// The interactive consent screen must not have rendered.
	if strings.Contains(out, "Install?") {
		t.Errorf("consent prompt should be suppressed when nothing to do:\n%s", out)
	}
}

// TestRunActionAddSuite_MixedBucketSummary asserts the v2 summary
// shape: each entry lands in one of {added, upgraded, already
// installed} and the footer aggregates the buckets. Regression for
// the issue surfaced during the Getting Started walkthrough where a
// 4-of-6 already-installed suite ran successfully but rendered the
// already-installed entries as failures.
func TestRunActionAddSuite_MixedBucketSummary(t *testing.T) {
	manifestPath := writeSuiteManifest(t, `
name = "mixed-buckets"
description = "One added, one upgraded, one already installed."

actions = [
  "github://acme/conn/actions/foo@0.2.0",
  "github://acme/conn/actions/bar@0.2.0",
  "github://acme/conn/actions/baz@0.2.0",
]
`)
	suiteInstallServer(t, []string{"github://acme/conn"},
		func(fqn string, w http.ResponseWriter, r *http.Request) {
			name := lastSegment(fqn)
			switch name {
			case "foo": // already installed at the same hash
				w.WriteHeader(http.StatusOK)
				fmt.Fprintf(w, `{"fqn":%q,"version":"0.2.0","hash":"sha256:foo","name":%q,"already_installed":true,"connector_deps":[]}`,
					fqn, name)
			case "bar": // upgrade from v0.1.0 to v0.2.0
				w.WriteHeader(http.StatusOK)
				fmt.Fprintf(w, `{"fqn":%q,"version":"0.2.0","hash":"sha256:barnew","name":%q,"signature_status":"verified","existing":{"version":"0.1.0","hash":"sha256:barold","source":"github://acme/conn/actions/bar@0.1.0","path":"/p/bar.md"},"connector_deps":[]}`,
					fqn, name)
			case "baz": // fresh install
				w.WriteHeader(http.StatusOK)
				fmt.Fprintf(w, `{"fqn":%q,"version":"0.2.0","hash":"sha256:baz","name":%q,"signature_status":"verified","connector_deps":[]}`,
					fqn, name)
			}
		},
		func(fqn string, w http.ResponseWriter, r *http.Request) {
			name := lastSegment(fqn)
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"name":%q,"fqn":%q,"version":"0.2.0","source":"x","path":"/p"}`, name, fqn)
		},
	)

	var stdout, stderr bytes.Buffer
	code := runActionAddSuite([]string{"--yes", manifestPath}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d (mixed buckets must not be a failure); stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		"Suite install summary:",
		"3 action(s): 1 added, 1 upgraded, 1 already installed.",
		"actions/foo@0.2.0 (already installed)",
		"actions/bar@0.2.0 (upgraded)",
		"actions/baz@0.2.0 (added)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

// TestRunActionAddSuite_DeclineCancelsEveryEntry: per #640, the
// suite is atomic at the consent layer. One Y/N prompt covers the
// whole suite (whether the entries are fresh, upgrades, or a mix);
// "n" cancels every entry, no install endpoint is called, and the
// run exits 0 with a summary that bucketed every entry as cancelled.
//
// Replaces the pre-#640 per-action upgrade-decline behavior that
// allowed partial install when one entry of a multi-entry suite was
// declined.
func TestRunActionAddSuite_DeclineCancelsEveryEntry(t *testing.T) {
	manifestPath := writeSuiteManifest(t, `
name = "decline-suite"
description = "Decline the suite; nothing installs."

actions = [
  "github://acme/conn/actions/foo@0.2.0",
  "github://acme/conn/actions/bar@0.2.0",
]
`)
	installCalls := map[string]int{}
	suiteInstallServer(t, []string{"github://acme/conn"},
		func(fqn string, w http.ResponseWriter, r *http.Request) {
			name := lastSegment(fqn)
			if name == "bar" {
				w.WriteHeader(http.StatusOK)
				fmt.Fprintf(w, `{"fqn":%q,"version":"0.2.0","hash":"sha256:new","name":%q,"signature_status":"verified","existing":{"version":"0.1.0","hash":"sha256:old","source":"github://acme/conn/actions/bar@0.1.0","path":"/p/bar.md"},"connector_deps":[]}`,
					fqn, name)
				return
			}
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, `{"fqn":%q,"version":"0.2.0","hash":"sha256:foo","name":%q,"signature_status":"verified","connector_deps":[]}`,
				fqn, name)
		},
		func(fqn string, w http.ResponseWriter, r *http.Request) {
			installCalls[fqn]++
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"name":%q,"fqn":%q,"version":"0.2.0","source":"x","path":"/p"}`, lastSegment(fqn), fqn)
		},
	)

	// One suite-level prompt; "n" cancels everything.
	stdin := strings.NewReader("n\n")
	var stdout, stderr bytes.Buffer
	code := runActionAddSuite([]string{manifestPath}, stdin, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("decline must not be a failure; exit=%d stderr=%s", code, stderr.String())
	}
	if len(installCalls) > 0 {
		t.Errorf("install endpoint must not be hit when suite is declined; calls=%+v", installCalls)
	}
	out := stdout.String()
	// Exactly one consent prompt, regardless of fresh + upgrade mix.
	if got := strings.Count(out, "Install? [y/n]: "); got != 1 {
		t.Errorf("expected exactly 1 suite-level prompt, got %d:\n%s", got, out)
	}
	for _, want := range []string{
		"Suite install preview",
		"actions/foo@0.2.0 (cancelled)",
		"actions/bar@0.2.0 (cancelled)",
		"2 action(s): 2 cancelled.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

// TestRunActionAddSuite_SingleConsentPromptForManyActions: the
// headline #640 contract. A suite with N actions surfaces exactly one
// "Install?" prompt under interactive mode. Pre-#640, each action got
// its own prompt; for the Gmail+Calendar suite that was 6 prompts in
// a row.
func TestRunActionAddSuite_SingleConsentPromptForManyActions(t *testing.T) {
	manifestPath := writeSuiteManifest(t, `
name = "many"
description = "Six actions; one prompt."

actions = [
  "github://acme/conn/actions/a1@0.1.0",
  "github://acme/conn/actions/a2@0.1.0",
  "github://acme/conn/actions/a3@0.1.0",
  "github://acme/conn/actions/a4@0.1.0",
  "github://acme/conn/actions/a5@0.1.0",
  "github://acme/conn/actions/a6@0.1.0",
]
`)
	installCalls := map[string]int{}
	suiteInstallServer(t, []string{"github://acme/conn"},
		func(fqn string, w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, `{"fqn":%q,"version":"0.1.0","hash":"sha256:abc","name":%q,"signature_status":"verified","connector_deps":[]}`,
				fqn, lastSegment(fqn))
		},
		func(fqn string, w http.ResponseWriter, r *http.Request) {
			installCalls[fqn]++
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"name":%q,"fqn":%q,"version":"0.1.0","source":"x","path":"/p"}`, lastSegment(fqn), fqn)
		},
	)

	stdin := strings.NewReader("y\n")
	var stdout, stderr bytes.Buffer
	code := runActionAddSuite([]string{manifestPath}, stdin, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d; stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	if got := strings.Count(out, "Install? [y/n]: "); got != 1 {
		t.Errorf("expected exactly 1 suite-level prompt for 6 actions, got %d:\n%s", got, out)
	}
	if len(installCalls) != 6 {
		t.Errorf("expected install endpoint hit 6 times, got %d:\n%+v", len(installCalls), installCalls)
	}
}

// TestRunActionAddSuite_MixedFreshAndUpgradeInPreview: the suite
// preview labels each entry by status — fresh (+), upgrade (↺),
// already installed (✓). Visual-regression test for the consent
// screen #640 introduces.
func TestRunActionAddSuite_MixedFreshAndUpgradeInPreview(t *testing.T) {
	manifestPath := writeSuiteManifest(t, `
name = "mixed-preview"
description = "Fresh + upgrade + already-installed in one preview."

actions = [
  "github://acme/conn/actions/foo@0.2.0",
  "github://acme/conn/actions/bar@0.2.0",
  "github://acme/conn/actions/baz@0.2.0",
]
`)
	suiteInstallServer(t, []string{"github://acme/conn"},
		func(fqn string, w http.ResponseWriter, r *http.Request) {
			name := lastSegment(fqn)
			switch name {
			case "foo":
				w.WriteHeader(http.StatusOK)
				fmt.Fprintf(w, `{"fqn":%q,"version":"0.2.0","hash":"sha256:foo","name":%q,"signature_status":"verified","connector_deps":[]}`,
					fqn, name)
			case "bar":
				w.WriteHeader(http.StatusOK)
				fmt.Fprintf(w, `{"fqn":%q,"version":"0.2.0","hash":"sha256:new","name":%q,"signature_status":"verified","existing":{"version":"0.1.0","hash":"sha256:old","source":"github://acme/conn/actions/bar@0.1.0","path":"/p/bar.md"},"connector_deps":[]}`,
					fqn, name)
			case "baz":
				w.WriteHeader(http.StatusOK)
				fmt.Fprintf(w, `{"fqn":%q,"version":"0.2.0","hash":"sha256:baz","name":%q,"already_installed":true,"connector_deps":[]}`,
					fqn, name)
			}
		},
		func(fqn string, w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"name":%q,"fqn":%q,"version":"0.2.0","source":"x","path":"/p"}`, lastSegment(fqn), fqn)
		},
	)

	stdin := strings.NewReader("n\n") // decline so we only assert preview rendering.
	var stdout, stderr bytes.Buffer
	code := runActionAddSuite([]string{manifestPath}, stdin, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d; stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		"Suite install preview",
		"Install 2 action(s) from this suite (1 already installed will be skipped):",
		"(new)",
		"(upgrade from v0.1.0)",
		"(already installed)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("preview missing %q:\n%s", want, out)
		}
	}
}

// TestRunActionAddSuite_PreviewFailureMarkedFailedInSummary: when
// the daemon refuses to preview one ref (e.g. signature_failure on a
// rotated key), the suite consent screen surfaces the failure with
// the ✗ marker, the user can still install the healthy entries, and
// the summary buckets the broken one as `failed` while reporting the
// rest as added. Preview-failed entries never reach the install
// endpoint.
func TestRunActionAddSuite_PreviewFailureMarkedFailedInSummary(t *testing.T) {
	manifestPath := writeSuiteManifest(t, `
name = "preview-fail"
description = "One bad apple; the rest install."

actions = [
  "github://acme/conn/actions/foo@0.1.0",
  "github://acme/conn/actions/bar@0.1.0",
  "github://acme/conn/actions/baz@0.1.0",
]
`)
	installCalls := map[string]int{}
	suiteInstallServer(t, []string{"github://acme/conn"},
		func(fqn string, w http.ResponseWriter, r *http.Request) {
			if strings.Contains(fqn, "/bar") {
				// Simulate the daemon refusing to preview this ref
				// (rotated signing key, missing keyring entry, etc.).
				w.WriteHeader(http.StatusUnprocessableEntity)
				_, _ = io.WriteString(w, `{"error":{"code":"signature_failure","message":"key not found"}}`)
				return
			}
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, `{"fqn":%q,"version":"0.1.0","hash":"sha256:abc","name":%q,"signature_status":"verified","connector_deps":[]}`,
				fqn, lastSegment(fqn))
		},
		func(fqn string, w http.ResponseWriter, r *http.Request) {
			installCalls[fqn]++
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"name":%q,"fqn":%q,"version":"0.1.0","source":"x","path":"/p"}`, lastSegment(fqn), fqn)
		},
	)

	stdin := strings.NewReader("y\n")
	var stdout, stderr bytes.Buffer
	code := runActionAddSuite([]string{manifestPath}, stdin, &stdout, &stderr)
	if code == 0 {
		t.Errorf("expected nonzero exit when a preview fails; stderr=%s", stderr.String())
	}
	// The two healthy refs install; the broken one is never sent to
	// /actions/install (it short-circuits as failed before the loop
	// even runs the entry). Install endpoint receives the bare FQN
	// (the @<version> rides in the request body, not the FQN field).
	if installCalls["github://acme/conn/actions/bar"] != 0 {
		t.Errorf("install endpoint hit for preview-failed ref; calls=%d",
			installCalls["github://acme/conn/actions/bar"])
	}
	if installCalls["github://acme/conn/actions/foo"] != 1 ||
		installCalls["github://acme/conn/actions/baz"] != 1 {
		t.Errorf("expected healthy refs to install; calls=%+v", installCalls)
	}
	out := stdout.String()
	for _, want := range []string{
		"Suite install preview",
		"(preview failed",
		"actions/bar@0.1.0",
		"actions/foo@0.1.0 (added)",
		"actions/baz@0.1.0 (added)",
		"3 action(s): 2 added, 1 failed.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

// TestRunActionAddSuite_YesFlagSkipsSuiteConsentPrompt: --yes is an
// explicit "skip every prompt including the suite-level one". Pin
// the contract: zero "Install?" prompts in the output regardless of
// how many actions are in the suite.
func TestRunActionAddSuite_YesFlagSkipsSuiteConsentPrompt(t *testing.T) {
	manifestPath := writeSuiteManifest(t, `
name = "yes-skips"
description = "Four actions; --yes; zero prompts."

actions = [
  "github://acme/conn/actions/a1@0.1.0",
  "github://acme/conn/actions/a2@0.1.0",
  "github://acme/conn/actions/a3@0.1.0",
  "github://acme/conn/actions/a4@0.1.0",
]
`)
	installCalls := map[string]int{}
	suiteInstallServer(t, []string{"github://acme/conn"}, nil,
		func(fqn string, w http.ResponseWriter, r *http.Request) {
			installCalls[fqn]++
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"name":%q,"fqn":%q,"version":"0.1.0","source":"x","path":"/p"}`, lastSegment(fqn), fqn)
		},
	)

	// Empty stdin: if a prompt fires, the test deadlocks (the daemon
	// is mocked but stdin is bounded). Asserting 0 prompts in stdout
	// is the explicit contract.
	var stdout, stderr bytes.Buffer
	code := runActionAddSuite([]string{"--yes", manifestPath}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d; stderr=%s", code, stderr.String())
	}
	if got := strings.Count(stdout.String(), "Install? [y/n]: "); got != 0 {
		t.Errorf("--yes must suppress every consent prompt, got %d:\n%s", got, stdout.String())
	}
	if got := strings.Count(stdout.String(), "Suite install preview"); got != 0 {
		t.Errorf("--yes must suppress the suite preview screen, got %d:\n%s", got, stdout.String())
	}
	if len(installCalls) != 4 {
		t.Errorf("expected 4 install calls under --yes; got %d (%+v)", len(installCalls), installCalls)
	}
}

// TestRunActionAddSuite_ConnectorDepsAggregatedAndDeduped: the suite
// preview's "Connectors:" section lists every distinct dep once. Two
// actions sharing a dep render that dep on one line; a third action
// declaring a different dep adds a second line. Exercises
// buildSuitePlan's dedup-by-FQN+version path.
func TestRunActionAddSuite_ConnectorDepsAggregatedAndDeduped(t *testing.T) {
	manifestPath := writeSuiteManifest(t, `
name = "shared-deps"
description = "Two share a connector dep; one is on its own."

actions = [
  "github://acme/conn/actions/foo@0.1.0",
  "github://acme/conn/actions/bar@0.1.0",
  "github://acme/conn/actions/baz@0.1.0",
]
`)
	suiteInstallServer(t, []string{"github://acme/conn"},
		func(fqn string, w http.ResponseWriter, r *http.Request) {
			name := lastSegment(fqn)
			// foo + bar share connector "google"; baz declares
			// "calendar". Both are not-already-installed.
			var deps string
			switch name {
			case "foo", "bar":
				deps = `[{"fqn":"github://acme/google","version":"0.0.7","hash":"sha256:g","capabilities":["network"],"already_installed":false}]`
			case "baz":
				deps = `[{"fqn":"github://acme/calendar","version":"0.0.3","hash":"sha256:c","capabilities":["network"],"already_installed":false}]`
			}
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, `{"fqn":%q,"version":"0.1.0","hash":"sha256:abc","name":%q,"signature_status":"verified","connector_deps":%s}`,
				fqn, name, deps)
		},
		func(fqn string, w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"name":%q,"fqn":%q,"version":"0.1.0","source":"x","path":"/p"}`, lastSegment(fqn), fqn)
		},
	)
	// Seed trust for both connector authorities so the preflight
	// trust pass doesn't prompt; the test is about preview rendering.
	withSeededKeyring(t, "github://acme/conn", "github://acme/google", "github://acme/calendar")

	stdin := strings.NewReader("n\n") // decline to focus on the rendered preview
	var stdout, stderr bytes.Buffer
	code := runActionAddSuite([]string{manifestPath}, stdin, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d; stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		"Connectors:",
		"github://acme/google@0.0.7",
		"github://acme/calendar@0.0.3",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("preview missing %q:\n%s", want, out)
		}
	}
	// The shared dep must appear exactly once, not once per action.
	if got := strings.Count(out, "github://acme/google@0.0.7"); got != 1 {
		t.Errorf("shared connector listed %d times, want exactly 1:\n%s", got, out)
	}
}

// TestRunActionAddSuite_MissingManifestFileExits1 surfaces a clear
// error when the local source path doesn't exist.
func TestRunActionAddSuite_MissingManifestFileExits1(t *testing.T) {
	withSeededKeyring(t)
	var stdout, stderr bytes.Buffer
	code := runActionAddSuite(
		[]string{"/no/such/file.toml"},
		strings.NewReader(""), &stdout, &stderr,
	)
	if code == 0 {
		t.Errorf("expected nonzero exit; stderr=%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "read suite manifest") {
		t.Errorf("expected read-error in stderr; got: %s", stderr.String())
	}
}

// TestRunActionAddSuite_RejectsExtraPositional: more than one
// source argument is a usage error.
func TestRunActionAddSuite_RejectsExtraPositional(t *testing.T) {
	manifestPath := writeSuiteManifest(t, `
name = "x"
description = "x"
actions = ["github://acme/conn/actions/foo@0.1.0"]
`)
	withSeededKeyring(t)
	var stdout, stderr bytes.Buffer
	code := runActionAddSuite(
		[]string{manifestPath, "github://acme/conn/actions/extra@0.1.0"},
		strings.NewReader(""), &stdout, &stderr,
	)
	if code == 0 {
		t.Errorf("expected nonzero exit with extra positional; stderr=%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "exactly one source") {
		t.Errorf("expected 'exactly one source' error; got: %s", stderr.String())
	}
}

// TestRunActionAddSuite_PathFormInLocalManifestErrors: a local
// manifest can't use path-form entries (no ref to inherit). Surface
// the error, name the entry.
func TestRunActionAddSuite_PathFormInLocalManifestErrors(t *testing.T) {
	manifestPath := writeSuiteManifest(t, `
name = "bad-local"
description = "Path-form requires a remote ref."

actions = [
  "actions/foo",
]
`)
	withSeededKeyring(t)
	var stdout, stderr bytes.Buffer
	code := runActionAddSuite([]string{manifestPath}, strings.NewReader(""), &stdout, &stderr)
	if code == 0 {
		t.Errorf("expected nonzero exit for path-form in local manifest; stderr=%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "path-form") {
		t.Errorf("error should explain path-form rule; got: %s", stderr.String())
	}
}

// TestRunActionAddSuite_TrustDeclineAbortsBeforeConsent: per #640's
// atomic-at-consent rule, trust prompts happen up front during the
// preflight phase. Declining trust for the suite's publisher aborts
// the run cleanly: no /actions/preview calls, no consent screen, no
// install attempt — and exits non-zero so a script that ran the
// suite install sees the failure.
func TestRunActionAddSuite_TrustDeclineAbortsBeforeConsent(t *testing.T) {
	withTempHome(t)
	_, pemBytes := genTestKey(t)
	withMockGitHubRaw(t, map[string][]byte{
		"/acme/conn/HEAD/keys/publisher.pub": pemBytes,
	})

	manifestPath := writeSuiteManifest(t, `
name = "decline-test"
description = "Decline trust; the run aborts before consent."

actions = [
  "github://acme/conn/actions/foo@0.1.0",
  "github://acme/conn/actions/bar@0.1.0",
  "github://acme/conn/actions/baz@0.1.0",
]
`)
	previewHits := 0
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/actions/preview" {
			previewHits++
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{}`)
	})

	stdin := bufio.NewReader(strings.NewReader("n\n"))
	var stdout, stderr bytes.Buffer
	code := runActionAddSuite([]string{manifestPath}, stdin, &stdout, &stderr)
	if code == 0 {
		t.Errorf("expected nonzero exit when trust declined; stderr=%s", stderr.String())
	}
	if previewHits != 0 {
		t.Errorf("preview hit %d times after trust decline; want 0", previewHits)
	}
	if strings.Count(stdout.String(), "Publisher github://acme/conn is not yet trusted") != 1 {
		t.Errorf("trust banner should fire once across the suite, not per action:\n%s", stdout.String())
	}
	// The interactive consent screen must not have rendered — the
	// run aborted before reaching it.
	if strings.Contains(stdout.String(), "Suite install preview") {
		t.Errorf("consent screen should not render after trust decline:\n%s", stdout.String())
	}
}

// --- runActionAddSuite: remote source (issue #564 phase 2) ---

// remoteSuiteServer stands up two httptest servers (GitHub API for
// resolveLatestRef + raw.githubusercontent.com for fetchSuiteTOML)
// and points the CLI's globals at them. Returns nothing — the
// fixtures register cleanup via t.Cleanup.
//
// suiteTOML is the body served at the raw URL; tagName is what
// the releases-latest endpoint returns (empty → 404 to simulate
// "no releases").
func remoteSuiteServer(t *testing.T, owner, repo, filePath, tagName, suiteTOML string) {
	t.Helper()
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		want := "/repos/" + owner + "/" + repo + "/releases/latest"
		if r.URL.Path != want {
			t.Errorf("unexpected api path: %s, want %s", r.URL.Path, want)
		}
		if tagName == "" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"tag_name":%q}`, tagName)
	}))
	t.Cleanup(apiSrv.Close)

	rawSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// raw.githubusercontent.com serves /<owner>/<repo>/<ref>/<path>.
		// Tests register at one specific ref path; anything else 404s.
		if !strings.HasSuffix(r.URL.Path, "/"+filePath) {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(suiteTOML))
	}))
	t.Cleanup(rawSrv.Close)

	prevAPI := githubAPIBase
	githubAPIBase = apiSrv.URL
	t.Cleanup(func() { githubAPIBase = prevAPI })

	prevRaw := rawGitHubBase
	rawGitHubBase = rawSrv.URL
	t.Cleanup(func() { rawGitHubBase = prevRaw })
}

// TestRunActionAddSuiteRemote_LatestResolvesAndInstalls is the
// headline remote test: @latest resolves via the releases API,
// the fetched manifest's path-form entries inherit the resolved
// release tag's bare SemVer as their install version.
func TestRunActionAddSuiteRemote_LatestResolvesAndInstalls(t *testing.T) {
	const suiteTOML = `
name = "remote-suite"
description = "Three actions, all path-form."

actions = [
  "actions/foo",
  "actions/bar",
  "actions/baz",
]
`
	remoteSuiteServer(t, "acme", "conn", "suite.toml", "v0.0.6", suiteTOML)

	installed := map[string]string{} // fqn -> version installed
	suiteInstallServer(t, []string{"github://acme/conn"},
		nil,
		func(fqn string, w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			var req struct {
				Version string `json:"version"`
			}
			_ = json.Unmarshal(body, &req)
			installed[fqn] = req.Version
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"name":%q,"fqn":%q,"version":%q,"source":"x","path":"/p"}`,
				lastSegment(fqn), fqn, req.Version)
		},
	)

	var stdout, stderr bytes.Buffer
	code := runActionAddSuite(
		[]string{"--yes", "github://acme/conn/suite.toml@latest"},
		strings.NewReader(""), &stdout, &stderr,
	)
	if code != 0 {
		t.Fatalf("exit = %d; stderr = %s", code, stderr.String())
	}
	for _, fqn := range []string{
		"github://acme/conn/actions/foo",
		"github://acme/conn/actions/bar",
		"github://acme/conn/actions/baz",
	} {
		if installed[fqn] != "0.0.6" {
			t.Errorf("install of %s: version = %q, want 0.0.6", fqn, installed[fqn])
		}
	}
	out := stdout.String()
	for _, want := range []string{
		"Resolving @latest",
		"→ v0.0.6",
		"Suite: remote-suite",
		"3 action(s): 3 added.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

// TestRunActionAddSuiteRemote_PinnedTagSkipsAPI: an explicit
// @<tag> ref doesn't hit the releases API.
func TestRunActionAddSuiteRemote_PinnedTagSkipsAPI(t *testing.T) {
	const suiteTOML = `
name = "pinned"
description = "Pinned to v0.0.6"

actions = [
  "actions/foo",
]
`
	apiHits := 0
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiHits++
		w.WriteHeader(http.StatusInternalServerError) // explode if hit
	}))
	defer apiSrv.Close()
	prevAPI := githubAPIBase
	githubAPIBase = apiSrv.URL
	t.Cleanup(func() { githubAPIBase = prevAPI })

	rawSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(suiteTOML))
	}))
	defer rawSrv.Close()
	prevRaw := rawGitHubBase
	rawGitHubBase = rawSrv.URL
	t.Cleanup(func() { rawGitHubBase = prevRaw })

	suiteInstallServer(t, []string{"github://acme/conn"}, nil, nil)

	var stdout, stderr bytes.Buffer
	code := runActionAddSuite(
		[]string{"--yes", "github://acme/conn/suite.toml@v0.0.6"},
		strings.NewReader(""), &stdout, &stderr,
	)
	if code != 0 {
		t.Fatalf("exit = %d; stderr = %s", code, stderr.String())
	}
	if apiHits != 0 {
		t.Errorf("releases API hit %d times for a pinned ref; want 0", apiHits)
	}
}

// TestRunActionAddSuiteRemote_NoReleasesFailsCleanly: @latest
// against a repo with no releases surfaces the actionable hint.
func TestRunActionAddSuiteRemote_NoReleasesFailsCleanly(t *testing.T) {
	remoteSuiteServer(t, "acme", "conn", "suite.toml", "", "")
	withSeededKeyring(t)

	var stdout, stderr bytes.Buffer
	code := runActionAddSuite(
		[]string{"github://acme/conn/suite.toml@latest"},
		strings.NewReader(""), &stdout, &stderr,
	)
	if code == 0 {
		t.Errorf("expected nonzero exit when no releases; stderr=%s", stderr.String())
	}
	for _, want := range []string{"@<release-tag>", "@<sha>"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr should mention %q; got: %s", want, stderr.String())
		}
	}
}

// TestRunActionAddSuiteRemote_RawFetch404NamesURL: when the suite
// manifest doesn't exist at the resolved ref, the user sees the
// URL that was tried.
func TestRunActionAddSuiteRemote_RawFetch404NamesURL(t *testing.T) {
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"tag_name":"v0.0.6"}`))
	}))
	defer apiSrv.Close()
	prevAPI := githubAPIBase
	githubAPIBase = apiSrv.URL
	t.Cleanup(func() { githubAPIBase = prevAPI })

	rawSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer rawSrv.Close()
	prevRaw := rawGitHubBase
	rawGitHubBase = rawSrv.URL
	t.Cleanup(func() { rawGitHubBase = prevRaw })

	withSeededKeyring(t)

	var stdout, stderr bytes.Buffer
	code := runActionAddSuite(
		[]string{"github://acme/conn/missing.toml@latest"},
		strings.NewReader(""), &stdout, &stderr,
	)
	if code == 0 {
		t.Errorf("expected nonzero exit; stderr=%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "missing.toml") {
		t.Errorf("stderr should name the missing file; got: %s", stderr.String())
	}
}

// TestRunActionAddSuiteRemote_MixedPathAndFQNEntries: a remote
// manifest with one path-form entry (inherits ref) and one FQN-form
// entry (explicit version) installs each at the right version.
func TestRunActionAddSuiteRemote_MixedPathAndFQNEntries(t *testing.T) {
	const suiteTOML = `
name = "mixed"
description = "Path-form inherits; FQN-form explicit."

actions = [
  "actions/foo",
  "github://other/repo/actions/bar@1.0.0",
]
`
	remoteSuiteServer(t, "acme", "conn", "suite.toml", "v0.0.6", suiteTOML)

	installedVersions := map[string]string{}
	suiteInstallServer(t, []string{"github://acme/conn", "github://other/repo"},
		nil,
		func(fqn string, w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			var req struct {
				Version string `json:"version"`
			}
			_ = json.Unmarshal(body, &req)
			installedVersions[fqn] = req.Version
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"name":%q,"fqn":%q,"version":%q,"source":"x","path":"/p"}`,
				lastSegment(fqn), fqn, req.Version)
		},
	)

	var stdout, stderr bytes.Buffer
	code := runActionAddSuite(
		[]string{"--yes", "github://acme/conn/suite.toml@latest"},
		strings.NewReader(""), &stdout, &stderr,
	)
	if code != 0 {
		t.Fatalf("exit = %d; stderr = %s", code, stderr.String())
	}
	if got := installedVersions["github://acme/conn/actions/foo"]; got != "0.0.6" {
		t.Errorf("path-form should inherit 0.0.6; got %q", got)
	}
	if got := installedVersions["github://other/repo/actions/bar"]; got != "1.0.0" {
		t.Errorf("FQN-form should use 1.0.0; got %q", got)
	}
}

// suiteAndKeyRawServer is a combined stand-in for raw.githubusercontent.com
// that serves BOTH the suite manifest (at the convention path) AND the
// publisher key (at /<owner>/<repo>/HEAD/keys/publisher.pub). The two
// existing helpers (`remoteSuiteServer` + `withMockGitHubRaw`) each
// rewrite `rawGitHubBase`, so combining them in the same test would
// race; the last one wins and the other endpoint 404s. This helper
// avoids that by routing both paths through one server.
//
// Also stands up the releases API mock at `/repos/<owner>/<repo>/releases/latest`
// returning the given tagName, so @latest resolves cleanly.
func suiteAndKeyRawServer(t *testing.T, owner, repo, filePath, tagName, suiteTOML string, publisherPEM []byte) {
	t.Helper()
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		want := "/repos/" + owner + "/" + repo + "/releases/latest"
		if r.URL.Path != want {
			t.Errorf("unexpected api path: %s, want %s", r.URL.Path, want)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"tag_name":%q}`, tagName)
	}))
	t.Cleanup(apiSrv.Close)

	keyPath := "/" + owner + "/" + repo + "/HEAD/keys/publisher.pub"
	rawSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == keyPath {
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(publisherPEM)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/"+filePath) {
			_, _ = w.Write([]byte(suiteTOML))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(rawSrv.Close)

	prevAPI := githubAPIBase
	githubAPIBase = apiSrv.URL
	t.Cleanup(func() { githubAPIBase = prevAPI })

	prevRaw := rawGitHubBase
	rawGitHubBase = rawSrv.URL
	t.Cleanup(func() { rawGitHubBase = prevRaw })
}

// --- suite composite install-decision (#709 / #725 sibling) ---

// TestRunActionAddSuiteRemote_HubCompositeAcceptSeedsTrust is the
// headline #709 suite test: when the suite's source maps to a Hub-
// listed suite FQN, the CLI renders one composite trust panel covering
// every connector authority in the dependency closure and one y/N. On
// accept, trust is seeded for every surfaced authority plus every
// member-action authority. The legacy per-authority preflight prompts
// in installSuiteEntries short-circuit because trustState already
// has them.
func TestRunActionAddSuiteRemote_HubCompositeAcceptSeedsTrust(t *testing.T) {
	home := withTempHome(t)
	pub, pemBytes := genTestKey(t)
	const suiteTOML = `
name = "hub-suite"
description = "Three actions"

actions = [
  "actions/foo",
  "actions/bar",
]
`
	suiteAndKeyRawServer(t, "acme", "conn", "suite.toml", "v0.0.6", suiteTOML, pemBytes)

	hubCalled := 0
	installed := map[string]int{}
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			FQN string `json:"fqn"`
		}
		_ = json.Unmarshal(body, &req)
		r.Body = io.NopCloser(strings.NewReader(string(body)))
		switch r.URL.Path {
		case "/hub/suite-install-decision":
			hubCalled++
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{
				"kind":"suite",
				"fqn":"github://acme/conn/suite",
				"description":"Three actions",
				"publisher_github":"acme",
				"member_actions":["github://acme/conn/actions/foo","github://acme/conn/actions/bar"],
				"authorities":[{
					"fqn":"github://acme/conn","publisher_github":"acme",
					"fingerprint":"sha256:any","trust_state":"unknown",
					"publisher_footprint":[],"risk_indicators":["First connector by this publisher you've installed"]
				}]
			}`)
		case "/actions/preview":
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, `{"fqn":%q,"version":"0.0.6","hash":"sha256:abc","name":%q,"signature_status":"verified","connector_deps":[]}`,
				req.FQN, lastSegment(req.FQN))
		case "/actions/install":
			installed[req.FQN]++
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"name":%q,"fqn":%q,"version":"0.0.6","source":"x","path":"/p"}`,
				lastSegment(req.FQN), req.FQN)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	// One y at the composite prompt, one y at the suite-install consent.
	// Legacy per-authority "Publisher is not yet trusted" prompts MUST
	// NOT fire — the composite covered them.
	stdin := bufio.NewReader(strings.NewReader("y\ny\n"))
	var stdout, stderr bytes.Buffer
	code := runActionAddSuite(
		[]string{"github://acme/conn/suite.toml@v0.0.6"},
		stdin, &stdout, &stderr,
	)
	if code != 0 {
		t.Fatalf("exit = %d; stderr=%s", code, stderr.String())
	}
	if hubCalled != 1 {
		t.Errorf("composite endpoint should fire exactly once, got %d", hubCalled)
	}
	if installed["github://acme/conn/actions/foo"] != 1 || installed["github://acme/conn/actions/bar"] != 1 {
		t.Errorf("expected both actions installed; got %v", installed)
	}
	out := stdout.String()
	for _, want := range []string{
		"Hub install-decision (suite)",
		"Suite:     github://acme/conn/suite",
		"Actions:   2",
		"github://acme/conn/actions/foo",
		"Trust these publishers and continue?",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	// Per-authority y/N trust prompts MUST NOT fire — the composite
	// covered them. The "Publisher X is not yet trusted" informational
	// line still prints (ensureAuthorityTrusted always announces what
	// it's about to do), but no interactive prompt follows because the
	// composite-accept seeds trust with autoYes=true.
	if strings.Contains(out, "Trust publisher github://acme/conn?") {
		t.Errorf("legacy per-authority y/N prompt fired after composite accept:\n%s", out)
	}
	// Trust must persist to the keyring — both the connector authority
	// and the action authorities (same authority in this same-publisher
	// suite) are now trusted via the composite-accept seed.
	kr, err := cstore.LoadKeyring(filepath.Join(home, ".aileron", "keyring.json"))
	if err != nil {
		t.Fatalf("LoadKeyring: %v", err)
	}
	if !kr.HasOwnerKey("github://acme", pub) {
		t.Error("keyring should contain owner-level trusted key after composite accept")
	}
}

// TestRunActionAddSuiteRemote_HubCompositeDeclineAborts: declining the
// composite trust prompt aborts the suite install cleanly (exit 0,
// no preview/install calls, keyring stays empty).
func TestRunActionAddSuiteRemote_HubCompositeDeclineAborts(t *testing.T) {
	home := withTempHome(t)
	_, pemBytes := genTestKey(t)
	const suiteTOML = `
name = "hub-suite"
description = "x"
actions = ["actions/foo"]
`
	suiteAndKeyRawServer(t, "acme", "conn", "suite.toml", "v0.0.6", suiteTOML, pemBytes)

	previewCalled, installCalled := false, false
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/hub/suite-install-decision":
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{
				"kind":"suite","fqn":"github://acme/conn/suite",
				"description":"x","publisher_github":"acme",
				"member_actions":["github://acme/conn/actions/foo"],
				"authorities":[{
					"fqn":"github://acme/conn","publisher_github":"acme",
					"fingerprint":"sha256:any","trust_state":"unknown",
					"publisher_footprint":[],"risk_indicators":["x"]
				}]
			}`)
		case "/actions/preview":
			previewCalled = true
		case "/actions/install":
			installCalled = true
		}
	})

	stdin := strings.NewReader("n\n")
	var stdout, stderr bytes.Buffer
	code := runActionAddSuite(
		[]string{"github://acme/conn/suite.toml@v0.0.6"},
		stdin, &stdout, &stderr,
	)
	if code != 0 {
		t.Errorf("expected clean cancel exit 0, got %d; stderr=%s", code, stderr.String())
	}
	if previewCalled || installCalled {
		t.Errorf("decline should short-circuit; preview=%v install=%v", previewCalled, installCalled)
	}
	if !strings.Contains(stdout.String(), "Cancelled.") {
		t.Errorf("expected 'Cancelled.':\n%s", stdout.String())
	}
	kr, _ := cstore.LoadKeyring(filepath.Join(home, ".aileron", "keyring.json"))
	if kr != nil && len(kr.Keys("github://acme/conn")) != 0 {
		t.Error("keyring should remain empty when composite declined")
	}
}

// TestRunActionAddSuiteRemote_HubComposite404FallsThroughToLegacy:
// when the suite isn't Hub-listed, the composite endpoint 404s and
// the legacy per-authority preflight runs as before.
func TestRunActionAddSuiteRemote_HubComposite404FallsThroughToLegacy(t *testing.T) {
	withTempHome(t)
	_, pemBytes := genTestKey(t)
	const suiteTOML = `
name = "legacy-suite"
description = "x"
actions = ["actions/foo"]
`
	suiteAndKeyRawServer(t, "acme", "conn", "suite.toml", "v0.0.6", suiteTOML, pemBytes)

	hubCalled, previewCalled, installCalled := false, false, false
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			FQN string `json:"fqn"`
		}
		_ = json.Unmarshal(body, &req)
		r.Body = io.NopCloser(strings.NewReader(string(body)))
		switch r.URL.Path {
		case "/hub/suite-install-decision":
			hubCalled = true
			w.WriteHeader(http.StatusNotFound)
		case "/actions/preview":
			previewCalled = true
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, `{"fqn":%q,"version":"0.0.6","hash":"sha256:abc","name":%q,"signature_status":"verified","connector_deps":[]}`,
				req.FQN, lastSegment(req.FQN))
		case "/actions/install":
			installCalled = true
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"name":%q,"fqn":%q,"version":"0.0.6","source":"x","path":"/p"}`,
				lastSegment(req.FQN), req.FQN)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	// One y to legacy trust prompt, one y to suite consent.
	stdin := bufio.NewReader(strings.NewReader("y\ny\n"))
	var stdout, stderr bytes.Buffer
	code := runActionAddSuite(
		[]string{"github://acme/conn/suite.toml@v0.0.6"},
		stdin, &stdout, &stderr,
	)
	if code != 0 {
		t.Fatalf("exit = %d; stderr=%s", code, stderr.String())
	}
	if !hubCalled || !previewCalled || !installCalled {
		t.Errorf("expected fall-through to legacy; hub=%v preview=%v install=%v", hubCalled, previewCalled, installCalled)
	}
	if strings.Contains(stdout.String(), "Hub install-decision (suite)") {
		t.Errorf("composite panel rendered on 404 fall-through:\n%s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "Publisher github://acme/conn is not yet trusted") {
		t.Errorf("legacy trust prompt should fire on fall-through:\n%s", stdout.String())
	}
}

// TestRunActionAddSuite_LocalSourceSkipsHubComposite: local file
// sources never carry a Hub suite FQN, so the composite endpoint is
// not consulted. The legacy preflight runs unchanged.
func TestRunActionAddSuite_LocalSourceSkipsHubComposite(t *testing.T) {
	manifestPath := writeSuiteManifest(t, `
name = "local-suite"
description = "x"

actions = [
  "github://acme/conn/actions/foo@0.1.0",
]
`)
	hubCalled := false
	suiteInstallServer(t, []string{"github://acme/conn"}, nil, nil)
	// Wrap the binding server with a sniffer that fails if the composite
	// endpoint fires. suiteInstallServer already attached a handler;
	// re-attach by replacing the AILERON_API_URL with a wrapper proxy
	// is too invasive — instead, assert through path-tracking on the
	// existing server by registering a custom handler.
	prevURL := os.Getenv("AILERON_API_URL")
	t.Setenv("AILERON_API_URL", prevURL) // no-op, just records intent
	// Replace the existing handler: re-create one that tracks hub calls.
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			FQN string `json:"fqn"`
		}
		_ = json.Unmarshal(body, &req)
		r.Body = io.NopCloser(strings.NewReader(string(body)))
		switch r.URL.Path {
		case "/hub/suite-install-decision", "/hub/action-install-decision":
			hubCalled = true
			w.WriteHeader(http.StatusNotFound)
		case "/actions/preview":
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, `{"fqn":%q,"version":"0.1.0","hash":"sha256:abc","name":%q,"signature_status":"verified","connector_deps":[]}`,
				req.FQN, lastSegment(req.FQN))
		case "/actions/install":
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"name":%q,"fqn":%q,"version":"0.1.0","source":"x","path":"/p"}`,
				lastSegment(req.FQN), req.FQN)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	var stdout, stderr bytes.Buffer
	code := runActionAddSuite([]string{"--yes", manifestPath}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d; stderr=%s", code, stderr.String())
	}
	if hubCalled {
		t.Error("local-source install should NOT consult the Hub suite-install-decision endpoint")
	}
}

// TestRunActionAddSuiteRemote_HubCompositeYesFlagAutoAccepts: --yes
// short-circuits the composite prompt, just like its action sibling.
func TestRunActionAddSuiteRemote_HubCompositeYesFlagAutoAccepts(t *testing.T) {
	withTempHome(t)
	_, pemBytes := genTestKey(t)
	const suiteTOML = `
name = "hub-suite"
description = "x"
actions = ["actions/foo"]
`
	suiteAndKeyRawServer(t, "acme", "conn", "suite.toml", "v0.0.6", suiteTOML, pemBytes)

	hubHits := 0
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			FQN string `json:"fqn"`
		}
		_ = json.Unmarshal(body, &req)
		r.Body = io.NopCloser(strings.NewReader(string(body)))
		switch r.URL.Path {
		case "/hub/suite-install-decision":
			hubHits++
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{
				"kind":"suite","fqn":"github://acme/conn/suite",
				"description":"x","publisher_github":"acme",
				"member_actions":["github://acme/conn/actions/foo"],
				"authorities":[{
					"fqn":"github://acme/conn","publisher_github":"acme",
					"fingerprint":"sha256:any","trust_state":"unknown",
					"publisher_footprint":[],"risk_indicators":["x"]
				}]
			}`)
		case "/actions/preview":
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, `{"fqn":%q,"version":"0.0.6","hash":"sha256:abc","name":%q,"signature_status":"verified","connector_deps":[]}`,
				req.FQN, lastSegment(req.FQN))
		case "/actions/install":
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"name":%q,"fqn":%q,"version":"0.0.6","source":"x","path":"/p"}`,
				lastSegment(req.FQN), req.FQN)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	var stdout, stderr bytes.Buffer
	code := runActionAddSuite(
		[]string{"--yes", "github://acme/conn/suite.toml@v0.0.6"},
		strings.NewReader(""), &stdout, &stderr,
	)
	if code != 0 {
		t.Fatalf("exit = %d; stderr=%s", code, stderr.String())
	}
	if hubHits != 1 {
		t.Errorf("composite should fire exactly once, got %d", hubHits)
	}
	// Composite prompt MUST NOT render when --yes is in effect.
	if strings.Contains(stdout.String(), "Trust these publishers and continue?") {
		t.Errorf("composite prompt fired despite --yes:\n%s", stdout.String())
	}
}

// TestRunActionAddSuiteRemote_HubCompositeAllAlreadyTrustedAutoAccepts:
// when every authority in the suite composite carries trust_state
// "already_trusted", the y/n/d prompt is skipped (the operator already
// trusts every publisher), but the composite panel still renders for
// transparency and the install proceeds. Regression for feedback #1150.
func TestRunActionAddSuiteRemote_HubCompositeAllAlreadyTrustedAutoAccepts(t *testing.T) {
	withTempHome(t)
	_, pemBytes := genTestKey(t)
	const suiteTOML = `
name = "hub-suite"
description = "x"
actions = ["actions/foo"]
`
	suiteAndKeyRawServer(t, "acme", "conn", "suite.toml", "v0.0.6", suiteTOML, pemBytes)

	hubHits := 0
	installed := map[string]int{}
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			FQN string `json:"fqn"`
		}
		_ = json.Unmarshal(body, &req)
		r.Body = io.NopCloser(strings.NewReader(string(body)))
		switch r.URL.Path {
		case "/hub/suite-install-decision":
			hubHits++
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{
				"kind":"suite","fqn":"github://acme/conn/suite",
				"description":"x","publisher_github":"acme",
				"member_actions":["github://acme/conn/actions/foo"],
				"authorities":[{
					"fqn":"github://acme/conn","publisher_github":"acme",
					"fingerprint":"sha256:any","trust_state":"already_trusted",
					"publisher_footprint":[],"risk_indicators":[]
				}]
			}`)
		case "/actions/preview":
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, `{"fqn":%q,"version":"0.0.6","hash":"sha256:abc","name":%q,"signature_status":"verified","connector_deps":[]}`,
				req.FQN, lastSegment(req.FQN))
		case "/actions/install":
			installed[req.FQN]++
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"name":%q,"fqn":%q,"version":"0.0.6","source":"x","path":"/p"}`,
				lastSegment(req.FQN), req.FQN)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	// No "y" for the composite prompt — it auto-accepts. The single "y"
	// answers the downstream suite-install consent.
	stdin := bufio.NewReader(strings.NewReader("y\n"))
	var stdout, stderr bytes.Buffer
	code := runActionAddSuite(
		[]string{"github://acme/conn/suite.toml@v0.0.6"},
		stdin, &stdout, &stderr,
	)
	if code != 0 {
		t.Fatalf("exit = %d; stderr=%s", code, stderr.String())
	}
	if hubHits != 1 {
		t.Errorf("composite should fire exactly once, got %d", hubHits)
	}
	if installed["github://acme/conn/actions/foo"] != 1 {
		t.Errorf("expected action installed; got %v", installed)
	}
	out := stdout.String()
	// The y/n/d prompt MUST NOT render when every authority is already
	// trusted — auto-accept is the whole point of the fix.
	if strings.Contains(out, "Trust these publishers and continue?") {
		t.Errorf("composite prompt fired despite all authorities already trusted:\n%s", out)
	}
	// The composite panel still renders for transparency.
	for _, want := range []string{
		"Hub install-decision (suite)",
		"Suite:     github://acme/conn/suite",
		"already trusted",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

// TestRunActionAddSuiteRemote_HubCompositeMixedTrustStillPrompts: when
// the suite composite mixes an already_trusted authority with an
// unknown one, the y/n/d prompt still fires — auto-accept only applies
// when every authority is already trusted. Regression for feedback #1150.
func TestRunActionAddSuiteRemote_HubCompositeMixedTrustStillPrompts(t *testing.T) {
	withTempHome(t)
	_, pemBytes := genTestKey(t)
	const suiteTOML = `
name = "hub-suite"
description = "x"
actions = ["actions/foo"]
`
	suiteAndKeyRawServer(t, "acme", "conn", "suite.toml", "v0.0.6", suiteTOML, pemBytes)

	hubHits := 0
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			FQN string `json:"fqn"`
		}
		_ = json.Unmarshal(body, &req)
		r.Body = io.NopCloser(strings.NewReader(string(body)))
		switch r.URL.Path {
		case "/hub/suite-install-decision":
			hubHits++
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{
				"kind":"suite","fqn":"github://acme/conn/suite",
				"description":"x","publisher_github":"acme",
				"member_actions":["github://acme/conn/actions/foo"],
				"authorities":[
					{
						"fqn":"github://acme/conn","publisher_github":"acme",
						"fingerprint":"sha256:any","trust_state":"already_trusted",
						"publisher_footprint":[],"risk_indicators":[]
					},
					{
						"fqn":"github://other/dep","publisher_github":"other",
						"fingerprint":"sha256:other","trust_state":"unknown",
						"publisher_footprint":[],"risk_indicators":["First connector by this publisher you've installed"]
					}
				]
			}`)
		case "/actions/preview":
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, `{"fqn":%q,"version":"0.0.6","hash":"sha256:abc","name":%q,"signature_status":"verified","connector_deps":[]}`,
				req.FQN, lastSegment(req.FQN))
		case "/actions/install":
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"name":%q,"fqn":%q,"version":"0.0.6","source":"x","path":"/p"}`,
				lastSegment(req.FQN), req.FQN)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	// Decline at the composite prompt to prove it fired. An "n" aborts
	// the suite install before preview/install run.
	stdin := bufio.NewReader(strings.NewReader("n\n"))
	var stdout, stderr bytes.Buffer
	code := runActionAddSuite(
		[]string{"github://acme/conn/suite.toml@v0.0.6"},
		stdin, &stdout, &stderr,
	)
	if code != 0 {
		t.Fatalf("exit = %d; stderr=%s", code, stderr.String())
	}
	if hubHits != 1 {
		t.Errorf("composite should fire exactly once, got %d", hubHits)
	}
	out := stdout.String()
	// The y/n/d prompt MUST fire when the set mixes trust states.
	if !strings.Contains(out, "Trust these publishers and continue?") {
		t.Errorf("composite prompt should fire on mixed trust states:\n%s", out)
	}
	if !strings.Contains(out, "Cancelled.") {
		t.Errorf("expected decline to cancel:\n%s", out)
	}
}

// TestRunActionAddSuiteRemote_HubCompositeMalformedJSONFallsThrough:
// malformed payload from the composite endpoint surfaces a one-line
// stderr note and proceeds with the legacy preflight.
func TestRunActionAddSuiteRemote_HubCompositeMalformedJSONFallsThrough(t *testing.T) {
	withTempHome(t)
	_, pemBytes := genTestKey(t)
	const suiteTOML = `
name = "legacy"
description = "x"
actions = ["actions/foo"]
`
	suiteAndKeyRawServer(t, "acme", "conn", "suite.toml", "v0.0.6", suiteTOML, pemBytes)

	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			FQN string `json:"fqn"`
		}
		_ = json.Unmarshal(body, &req)
		r.Body = io.NopCloser(strings.NewReader(string(body)))
		switch r.URL.Path {
		case "/hub/suite-install-decision":
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `not json`)
		case "/actions/preview":
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, `{"fqn":%q,"version":"0.0.6","hash":"sha256:abc","name":%q,"signature_status":"verified","connector_deps":[]}`,
				req.FQN, lastSegment(req.FQN))
		case "/actions/install":
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"name":%q,"fqn":%q,"version":"0.0.6","source":"x","path":"/p"}`,
				lastSegment(req.FQN), req.FQN)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	stdin := bufio.NewReader(strings.NewReader("y\ny\n"))
	var stdout, stderr bytes.Buffer
	code := runActionAddSuite(
		[]string{"github://acme/conn/suite.toml@v0.0.6"},
		stdin, &stdout, &stderr,
	)
	if code != 0 {
		t.Fatalf("exit = %d; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "could not parse hub install-decision") {
		t.Errorf("expected parse-failure note; stderr=%s", stderr.String())
	}
}

// TestRunActionAddSuiteRemote_HubComposite503FallsThrough: 503 from
// the composite endpoint surfaces a one-line stderr note and falls
// through to the legacy preflight.
func TestRunActionAddSuiteRemote_HubComposite503FallsThrough(t *testing.T) {
	withTempHome(t)
	_, pemBytes := genTestKey(t)
	const suiteTOML = `
name = "legacy"
description = "x"
actions = ["actions/foo"]
`
	suiteAndKeyRawServer(t, "acme", "conn", "suite.toml", "v0.0.6", suiteTOML, pemBytes)

	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			FQN string `json:"fqn"`
		}
		_ = json.Unmarshal(body, &req)
		r.Body = io.NopCloser(strings.NewReader(string(body)))
		switch r.URL.Path {
		case "/hub/suite-install-decision":
			w.WriteHeader(http.StatusServiceUnavailable)
		case "/actions/preview":
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, `{"fqn":%q,"version":"0.0.6","hash":"sha256:abc","name":%q,"signature_status":"verified","connector_deps":[]}`,
				req.FQN, lastSegment(req.FQN))
		case "/actions/install":
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"name":%q,"fqn":%q,"version":"0.0.6","source":"x","path":"/p"}`,
				lastSegment(req.FQN), req.FQN)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	stdin := bufio.NewReader(strings.NewReader("y\ny\n"))
	var stdout, stderr bytes.Buffer
	code := runActionAddSuite(
		[]string{"github://acme/conn/suite.toml@v0.0.6"},
		stdin, &stdout, &stderr,
	)
	if code != 0 {
		t.Fatalf("exit = %d; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "hub install-decision returned 503") {
		t.Errorf("expected 503 note; stderr=%s", stderr.String())
	}
}

// TestRunActionAddSuiteRemote_HubCompositeDetailsExpandsAndAccepts:
// d→details, then y accepts. Validates the d=details two-step on the
// suite-composite prompt.
func TestRunActionAddSuiteRemote_HubCompositeDetailsExpandsAndAccepts(t *testing.T) {
	withTempHome(t)
	_, pemBytes := genTestKey(t)
	const suiteTOML = `
name = "hub-suite"
description = "x"
actions = ["actions/foo"]
`
	suiteAndKeyRawServer(t, "acme", "conn", "suite.toml", "v0.0.6", suiteTOML, pemBytes)

	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			FQN string `json:"fqn"`
		}
		_ = json.Unmarshal(body, &req)
		r.Body = io.NopCloser(strings.NewReader(string(body)))
		switch r.URL.Path {
		case "/hub/suite-install-decision":
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{
				"kind":"suite","fqn":"github://acme/conn/suite",
				"description":"x","publisher_github":"acme",
				"member_actions":["github://acme/conn/actions/foo"],
				"authorities":[{
					"fqn":"github://acme/conn","publisher_github":"acme",
					"fingerprint":"sha256:any","trust_state":"unknown",
					"publisher_footprint":["github://acme/other-conn"],
					"risk_indicators":["First connector by this publisher"]
				}]
			}`)
		case "/actions/preview":
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, `{"fqn":%q,"version":"0.0.6","hash":"sha256:abc","name":%q,"signature_status":"verified","connector_deps":[]}`,
				req.FQN, lastSegment(req.FQN))
		case "/actions/install":
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"name":%q,"fqn":%q,"version":"0.0.6","source":"x","path":"/p"}`,
				lastSegment(req.FQN), req.FQN)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	// d → details, y → accept composite, y → accept the suite install.
	stdin := bufio.NewReader(strings.NewReader("d\ny\ny\n"))
	var stdout, stderr bytes.Buffer
	code := runActionAddSuite(
		[]string{"github://acme/conn/suite.toml@v0.0.6"},
		stdin, &stdout, &stderr,
	)
	if code != 0 {
		t.Fatalf("exit = %d; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Other connectors by this publisher") {
		t.Errorf("expected publisher_footprint in details mode:\n%s", stdout.String())
	}
}

// --- deriveHubSuiteFQN unit ---

func TestDeriveHubSuiteFQN(t *testing.T) {
	cases := []struct {
		name string
		in   cstore.FQN
		want string
	}{
		{
			name: "github single-suite repo",
			in:   cstore.FQN{Scheme: "github", Owner: "acme", Repo: "conn", Subpath: "suite.toml"},
			want: "github://acme/conn/suite",
		},
		{
			name: "github nested suite",
			in:   cstore.FQN{Scheme: "github", Owner: "acme", Repo: "conn", Subpath: "suites/gmail.toml"},
			want: "github://acme/conn/suites/gmail",
		},
		{
			name: "gitlab same shape",
			in:   cstore.FQN{Scheme: "gitlab", Owner: "acme", Repo: "conn", Subpath: "suite.toml"},
			want: "gitlab://acme/conn/suite",
		},
		{
			name: "hub scheme skipped (no .toml suffix story here)",
			in:   cstore.FQN{Scheme: "hub", Owner: "acme", Repo: "conn", Subpath: "suite.toml"},
			want: "",
		},
		{
			name: "missing subpath",
			in:   cstore.FQN{Scheme: "github", Owner: "acme", Repo: "conn"},
			want: "",
		},
		{
			name: "non-toml subpath",
			in:   cstore.FQN{Scheme: "github", Owner: "acme", Repo: "conn", Subpath: "config.yaml"},
			want: "",
		},
		{
			name: "bare .toml (would resolve to empty after strip)",
			in:   cstore.FQN{Scheme: "github", Owner: "acme", Repo: "conn", Subpath: ".toml"},
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := deriveHubSuiteFQN(tc.in); got != tc.want {
				t.Errorf("deriveHubSuiteFQN(%+v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// --- aileron audit ---

// TestRunAudit_EmptyPrintsNoEntries: with no events the CLI prints a
// useful message and exits 0. This is the bare-server path operators
// see before any audit-emitting actions have run.
func TestRunAudit_EmptyPrintsNoEntries(t *testing.T) {
	prev := auditListFetcher
	auditListFetcher = func(_ auditListQuery) (*auditListWire, error) {
		return &auditListWire{Events: []auditEventWire{}}, nil
	}
	defer func() { auditListFetcher = prev }()

	var stdout, stderr bytes.Buffer
	code := run([]string{"audit"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "No audit events") {
		t.Errorf("expected 'No audit events'; got: %s", stdout.String())
	}
}

// TestRunAudit_EmptyJSONEmitsArray: regression for #492 item 2 — `--json`
// on the empty case must emit `[]`, not the human "No audit events." line.
// Before the fix, the empty branch ignored the flag and broke any script
// that relied on parsing JSON from `aileron audit list --json`.
func TestRunAudit_EmptyJSONEmitsArray(t *testing.T) {
	prev := auditListFetcher
	auditListFetcher = func(_ auditListQuery) (*auditListWire, error) {
		return &auditListWire{Events: []auditEventWire{}}, nil
	}
	defer func() { auditListFetcher = prev }()

	var stdout, stderr bytes.Buffer
	code := run([]string{"audit", "list", "--json"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	if got := strings.TrimRight(stdout.String(), "\n"); got != "[]" {
		t.Errorf("stdout = %q, want %q", got, "[]")
	}
}

// TestRunAudit_TabularRendersTimestampIDTypeAndSummary: the default
// rendering is a one-line-per-event table whose columns are stable
// for shell pipelines.
func TestRunAudit_TabularRendersTimestampIDTypeAndSummary(t *testing.T) {
	prev := auditListFetcher
	auditListFetcher = func(_ auditListQuery) (*auditListWire, error) {
		return &auditListWire{Events: []auditEventWire{{
			AuditID:   "audit-xyz",
			EventType: "execution.failed",
			Timestamp: time.Date(2026, 5, 1, 12, 30, 0, 0, time.UTC),
			Payload: map[string]any{
				"aileron.failure.class":   "binding_required",
				"aileron.failure.details": map[string]any{"connector": "github://aileron/slack"},
			},
		}}}, nil
	}
	defer func() { auditListFetcher = prev }()

	var stdout, stderr bytes.Buffer
	code := run([]string{"audit", "list"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		"audit-xyz",
		"execution.failed",
		"class=binding_required",
		"connector=github://aileron/slack",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in output; got: %s", want, out)
		}
	}
}

// TestRunAudit_FlagsForwardedToFetcher: every documented filter flag
// ends up on the auditListQuery sent to the daemon. Drift between
// the CLI flags and the wire query is the most likely regression.
func TestRunAudit_FlagsForwardedToFetcher(t *testing.T) {
	var seen auditListQuery
	prev := auditListFetcher
	auditListFetcher = func(q auditListQuery) (*auditListWire, error) {
		seen = q
		return &auditListWire{}, nil
	}
	defer func() { auditListFetcher = prev }()

	var stdout, stderr bytes.Buffer
	args := []string{
		"audit", "list",
		"--since", "2026-05-01T00:00:00Z",
		"--audit-id", "audit-xyz",
		"--connector", "github://aileron/slack",
		"--class", "binding_required",
		"--limit", "50",
	}
	code := run(args, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	if seen.since != "2026-05-01T00:00:00Z" ||
		seen.auditID != "audit-xyz" ||
		seen.connector != "github://aileron/slack" ||
		seen.class != "binding_required" ||
		seen.limit != 50 {
		t.Errorf("query = %+v; some flags didn't reach fetcher", seen)
	}
}

// TestRunAudit_JSONRendersOnePerLine: --json switches to one
// JSON-encoded event per line. Round-trips back through json.Decode.
func TestRunAudit_JSONRendersOnePerLine(t *testing.T) {
	prev := auditListFetcher
	auditListFetcher = func(_ auditListQuery) (*auditListWire, error) {
		return &auditListWire{Events: []auditEventWire{
			{AuditID: "a1", EventType: "action.installed", Timestamp: time.Now()},
			{AuditID: "a2", EventType: "binding.created", Timestamp: time.Now()},
		}}, nil
	}
	defer func() { auditListFetcher = prev }()

	var stdout, stderr bytes.Buffer
	code := run([]string{"audit", "list", "--json"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	lines := strings.Split(strings.TrimRight(stdout.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2: %q", len(lines), stdout.String())
	}
	for _, line := range lines {
		var ev auditEventWire
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Errorf("line %q is not JSON: %v", line, err)
		}
	}
}

// TestRunAudit_FetcherErrorExitsNonZero: a daemon error (network,
// 500, etc.) exits non-zero with a useful stderr line. Operators can
// rely on the exit code in scripts.
func TestRunAudit_FetcherErrorExitsNonZero(t *testing.T) {
	prev := auditListFetcher
	auditListFetcher = func(_ auditListQuery) (*auditListWire, error) {
		return nil, fmt.Errorf("connection refused")
	}
	defer func() { auditListFetcher = prev }()

	var stdout, stderr bytes.Buffer
	code := run([]string{"audit", "list"}, newTestRegistry(), &stdout, &stderr)
	if code == 0 {
		t.Error("expected non-zero exit on fetcher error")
	}
	if !strings.Contains(stderr.String(), "connection refused") {
		t.Errorf("expected error in stderr; got: %s", stderr.String())
	}
}

// TestRunAuditShow_HappyPath: `aileron audit show <id>` prints the
// event as pretty JSON.
func TestRunAuditShow_HappyPath(t *testing.T) {
	prev := auditGetFetcher
	auditGetFetcher = func(id string) (*auditEventWire, int, error) {
		if id != "audit-xyz" {
			t.Errorf("fetcher saw id=%q", id)
		}
		return &auditEventWire{
			AuditID:   "audit-xyz",
			EventType: "action.installed",
			Timestamp: time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
			Payload:   map[string]any{"name": "ship-update"},
		}, http.StatusOK, nil
	}
	defer func() { auditGetFetcher = prev }()

	var stdout, stderr bytes.Buffer
	code := run([]string{"audit", "show", "audit-xyz"}, newTestRegistry(), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr.String())
	}
	var got auditEventWire
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v; out=%s", err, stdout.String())
	}
	if got.AuditID != "audit-xyz" || got.EventType != "action.installed" {
		t.Errorf("got = %+v", got)
	}
}

// TestRunAuditShow_NotFoundExitsNonZero: 404 from the daemon translates
// to a non-zero exit and a stderr line naming the missing id.
func TestRunAuditShow_NotFoundExitsNonZero(t *testing.T) {
	prev := auditGetFetcher
	auditGetFetcher = func(_ string) (*auditEventWire, int, error) {
		return nil, http.StatusNotFound, nil
	}
	defer func() { auditGetFetcher = prev }()

	var stdout, stderr bytes.Buffer
	code := run([]string{"audit", "show", "missing"}, newTestRegistry(), &stdout, &stderr)
	if code == 0 {
		t.Error("expected non-zero exit on 404")
	}
	if !strings.Contains(stderr.String(), "missing") {
		t.Errorf("stderr should name the id; got: %s", stderr.String())
	}
}

// TestRunAuditShow_RequiresAuditID: with no positional argument the
// CLI prints usage and exits non-zero; the fetcher is not called.
func TestRunAuditShow_RequiresAuditID(t *testing.T) {
	called := false
	prev := auditGetFetcher
	auditGetFetcher = func(_ string) (*auditEventWire, int, error) {
		called = true
		return nil, http.StatusOK, nil
	}
	defer func() { auditGetFetcher = prev }()

	var stdout, stderr bytes.Buffer
	code := run([]string{"audit", "show"}, newTestRegistry(), &stdout, &stderr)
	if code == 0 {
		t.Error("expected non-zero exit when audit-id is missing")
	}
	if called {
		t.Error("fetcher should not be called without an audit-id")
	}
	if !strings.Contains(stderr.String(), "audit-id") {
		t.Errorf("stderr should mention audit-id; got: %s", stderr.String())
	}
}

// TestRunAudit_UnknownSubcommand: unrecognized subcommand prints
// usage and exits non-zero.
func TestRunAudit_UnknownSubcommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"audit", "bogus"}, newTestRegistry(), &stdout, &stderr)
	if code == 0 {
		t.Error("expected non-zero exit on unknown subcommand")
	}
	if !strings.Contains(stderr.String(), "unknown audit command") {
		t.Errorf("stderr should explain the failure; got: %s", stderr.String())
	}
}

// TestFetchAuditGet_HitsCorrectURL: the production fetcher hits
// `/v1/audit/<id>` (with PathEscape), decodes the wire shape on 200,
// and returns http.StatusNotFound without an error on 404 so the
// caller can branch on status without parsing the error string.
func TestFetchAuditGet_HitsCorrectURL(t *testing.T) {
	var sawPath string
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/audit/", func(w http.ResponseWriter, r *http.Request) {
		sawPath = r.URL.Path
		if r.URL.Path == "/v1/audit/audit-known" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"audit_id":"audit-known","event_type":"action.installed","timestamp":"2026-05-01T00:00:00Z","actor":{"type":"human","id":"user"},"payload":{"name":"ship-update"}}`)
			return
		}
		http.NotFound(w, r)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	t.Setenv("AILERON_API_URL", srv.URL+"/v1")

	got, status, err := fetchAuditGet("audit-known")
	if err != nil || status != http.StatusOK {
		t.Fatalf("known: status=%d err=%v", status, err)
	}
	if got.AuditID != "audit-known" || got.EventType != "action.installed" {
		t.Errorf("got = %+v", got)
	}
	if sawPath != "/v1/audit/audit-known" {
		t.Errorf("path = %q", sawPath)
	}

	_, status, err = fetchAuditGet("missing")
	if err != nil {
		t.Errorf("404 should not surface as error; got %v", err)
	}
	if status != http.StatusNotFound {
		t.Errorf("status = %d, want 404", status)
	}
}

// TestAuditPayloadSummary_PerEventShape exercises the renderer
// branches the tabular tests don't reach: action-installed (action
// name + FQN), binding event (connector FQN only), and an event with
// no useful keys. Payload field names follow the OTel-namespaced
// audit schema (issue #390 Phase 6.5).
func TestAuditPayloadSummary_PerEventShape(t *testing.T) {
	cases := []struct {
		name string
		in   auditEventWire
		want string
	}{
		{
			name: "action-installed-with-fqn",
			in: auditEventWire{
				EventType: "action.installed",
				Payload: map[string]any{
					"aileron.action.name": "ship-update",
					"aileron.action.fqn":  "github://aileron/ship-update",
				},
			},
			want: "name=ship-update connector=github://aileron/ship-update",
		},
		{
			name: "binding-created",
			in: auditEventWire{
				EventType: "binding.created",
				Payload:   map[string]any{"aileron.connector.fqn": "github://aileron/slack"},
			},
			want: "connector=github://aileron/slack",
		},
		{
			name: "empty-payload",
			in:   auditEventWire{EventType: "unknown", Payload: map[string]any{}},
			want: "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := auditPayloadSummary(c.in); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

// TestFetchAuditList_BuildsURL: the production fetcher (not the stub)
// builds the right URL with all filter params and decodes the wire
// shape. Single test that exercises the actual HTTP path.
func TestFetchAuditList_BuildsURL(t *testing.T) {
	var sawQuery url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawQuery = r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"events":[]}`)
	}))
	defer srv.Close()
	t.Setenv("AILERON_API_URL", srv.URL+"/v1")

	_, err := fetchAuditList(auditListQuery{
		since:     "2026-05-01T00:00:00Z",
		auditID:   "audit-x",
		connector: "github://aileron/slack",
		class:     "binding_required",
		limit:     25,
	})
	if err != nil {
		t.Fatalf("fetchAuditList: %v", err)
	}
	for k, want := range map[string]string{
		"since":         "2026-05-01T00:00:00Z",
		"audit_id":      "audit-x",
		"connector_fqn": "github://aileron/slack",
		"class":         "binding_required",
		"limit":         "25",
	} {
		if got := sawQuery.Get(k); got != want {
			t.Errorf("query[%s] = %q, want %q", k, got, want)
		}
	}
}

// TestDaemonErrText covers the parser for the daemon's api.Error
// envelope. Contract:
//   - Empty body → "<no body>".
//   - Envelope with code + message → "code: message" so the operator
//     sees the underlying classification (e.g. fetch_failed) and the
//     human detail rather than a bare HTTP status.
//   - Envelope with code only → "code" alone.
//   - Anything that doesn't parse as the envelope (legacy daemons,
//     proxies, raw 500s) → the raw body verbatim so no detail is lost.
func TestDaemonErrText(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"empty", "", "<no body>"},
		{
			"envelope_with_code_and_message",
			`{"error":{"code":"fetch_failed","message":"404 (tag or release not found)"}}`,
			"fetch_failed: 404 (tag or release not found)",
		},
		{
			"envelope_with_code_only",
			`{"error":{"code":"invalid_fqn"}}`,
			"invalid_fqn",
		},
		{
			"non_envelope_passthrough",
			`<html>504 Gateway Timeout</html>`,
			`<html>504 Gateway Timeout</html>`,
		},
		{
			"envelope_missing_code_falls_through",
			`{"error":{"message":"oops"}}`,
			`{"error":{"message":"oops"}}`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := daemonErrText([]byte(tc.raw)); got != tc.want {
				t.Errorf("daemonErrText(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

// TestPreviewSuiteRefs_NonOKSurfacesDaemonError is the regression test
// for the friction documented in the daemon-error-passthrough fix: a
// 422 from /actions/preview used to surface as "daemon returned 422"
// with no reason, hiding the actual classification (e.g. fetch_failed)
// from the operator. After the fix, the error message must include
// the daemon's code + message so the suite consent screen tells the
// user what actually went wrong.
func TestPreviewSuiteRefs_NonOKSurfacesDaemonError(t *testing.T) {
	fakeBindingServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/actions/preview" {
			t.Errorf("path = %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = io.WriteString(w, `{"error":{"code":"fetch_failed","message":"404 (tag or release not found)"}}`)
	})

	ref, err := cstore.ParseRef("github://ALRubinger/aileron-connector-github/actions/list-recent-prs@9.9.9")
	if err != nil {
		t.Fatalf("ParseRef: %v", err)
	}
	previews := previewSuiteRefs([]cstore.Ref{ref}, io.Discard)
	if len(previews) != 1 {
		t.Fatalf("len(previews) = %d, want 1", len(previews))
	}
	if previews[0].err == nil {
		t.Fatalf("previews[0].err = nil, want non-nil")
	}
	got := previews[0].err.Error()
	for _, want := range []string{"422", "fetch_failed", "404 (tag or release not found)"} {
		if !strings.Contains(got, want) {
			t.Errorf("err = %q, want substring %q", got, want)
		}
	}
}

// TestRunSandboxBuildRejectsPodman is the CLI-surface regression test for
// #1051: passing --runtime=podman to `sandbox build` must surface the
// decided Docker-only message through the real build path (no stubbing),
// and must not exit 0. resolveRuntime rejects podman before any container
// shell-out, so the test is hermetic.
func TestRunSandboxBuildRejectsPodman(t *testing.T) {
	dir := t.TempDir()
	oldWd, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWd) })

	var stdout, stderr bytes.Buffer
	code := run([]string{"sandbox", "build", "--runtime=podman"}, newTestRegistry(), &stdout, &stderr)
	if code == 0 {
		t.Fatalf("expected non-zero exit for --runtime=podman; stdout=%q", stdout.String())
	}
	if want := "podman runtime is not supported yet (v4 is Docker-only); see ADR-0014"; !strings.Contains(stderr.String(), want) {
		t.Fatalf("stderr = %q, want substring %q", stderr.String(), want)
	}
}
