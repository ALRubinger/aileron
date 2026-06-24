package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	sandboxcontainer "github.com/ALRubinger/aileron/internal/sandbox/container"
)

// captureSandboxBuildOpts swaps sandboxBuildFn and returns a pointer to the
// BuildOptions runSandboxBuild assembles, so a test can assert how flags/env
// thread into the toolchain selection without a real build.
func captureSandboxBuildOpts(t *testing.T) *sandboxcontainer.BuildOptions {
	t.Helper()
	orig := sandboxBuildFn
	t.Cleanup(func() { sandboxBuildFn = orig })
	var got sandboxcontainer.BuildOptions
	sandboxBuildFn = func(_ context.Context, _ string, _, _ io.Writer, opts sandboxcontainer.BuildOptions) (sandboxcontainer.BuildResult, error) {
		got = opts
		return sandboxcontainer.BuildResult{Runtime: "docker", Image: opts.Plan.Image, Tier: opts.Plan.Tier}, nil
	}
	return &got
}

func TestRunSandboxBuildDefaultsToManaged(t *testing.T) {
	// #1530: the default selection (no flag, no env) now resolves to managed.
	t.Chdir(t.TempDir())
	got := captureSandboxBuildOpts(t)
	var out, errb bytes.Buffer
	if code := runSandboxBuild(nil, &out, &errb); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errb.String())
	}
	if !sandboxcontainer.IsManagedToolchain(got.ToolchainMode) {
		t.Fatalf("default ToolchainMode = %q resolved host-npx; want managed", got.ToolchainMode)
	}
	if got.NodeBinary != "" || got.DevcontainerCLIEntrypoint != "" {
		t.Fatalf("default escape hatch non-empty: node=%q cli=%q", got.NodeBinary, got.DevcontainerCLIEntrypoint)
	}
}

func TestRunSandboxBuildHostNPXFlagOptsOut(t *testing.T) {
	// The explicit host-npx flag is the opt-out from the managed default.
	t.Chdir(t.TempDir())
	got := captureSandboxBuildOpts(t)
	var out, errb bytes.Buffer
	if code := runSandboxBuild([]string{"--toolchain=host-npx"}, &out, &errb); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errb.String())
	}
	if sandboxcontainer.IsManagedToolchain(got.ToolchainMode) {
		t.Fatalf("host-npx flag resolved managed: %q", got.ToolchainMode)
	}
}

func TestRunSandboxBuildManagedFlag(t *testing.T) {
	t.Chdir(t.TempDir())
	got := captureSandboxBuildOpts(t)
	var out, errb bytes.Buffer
	if code := runSandboxBuild([]string{"--toolchain=managed"}, &out, &errb); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errb.String())
	}
	if !sandboxcontainer.IsManagedToolchain(got.ToolchainMode) {
		t.Fatalf("ToolchainMode = %q, want managed", got.ToolchainMode)
	}
}

func TestRunSandboxBuildManagedEnv(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv(sandboxcontainer.ToolchainModeEnv, "managed")
	got := captureSandboxBuildOpts(t)
	var out, errb bytes.Buffer
	if code := runSandboxBuild(nil, &out, &errb); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errb.String())
	}
	if !sandboxcontainer.IsManagedToolchain(got.ToolchainMode) {
		t.Fatalf("env-selected ToolchainMode = %q, want managed", got.ToolchainMode)
	}
}

func TestRunSandboxBuildFlagOverridesEnv(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv(sandboxcontainer.ToolchainModeEnv, "managed")
	got := captureSandboxBuildOpts(t)
	var out, errb bytes.Buffer
	if code := runSandboxBuild([]string{"--toolchain=host-npx"}, &out, &errb); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errb.String())
	}
	if sandboxcontainer.IsManagedToolchain(got.ToolchainMode) {
		t.Fatalf("flag host-npx did not override env managed: %q", got.ToolchainMode)
	}
}

func TestRunSandboxBuildEscapeHatchEnvThreaded(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv(sandboxcontainer.ToolchainModeEnv, "managed")
	t.Setenv(sandboxcontainer.NodeBinaryEnv, "/opt/node/bin/node")
	t.Setenv(sandboxcontainer.DevcontainerCLIEnv, "/opt/cli/devcontainer.js")
	got := captureSandboxBuildOpts(t)
	var out, errb bytes.Buffer
	if code := runSandboxBuild(nil, &out, &errb); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errb.String())
	}
	if got.NodeBinary != "/opt/node/bin/node" {
		t.Fatalf("NodeBinary = %q, want env value", got.NodeBinary)
	}
	if got.DevcontainerCLIEntrypoint != "/opt/cli/devcontainer.js" {
		t.Fatalf("DevcontainerCLIEntrypoint = %q, want env value", got.DevcontainerCLIEntrypoint)
	}
}

func TestRunSandboxBuildOfflineFlagThreads(t *testing.T) {
	// --offline must surface on BuildOptions.Offline so the CLI layer constructs
	// an offline provisioner for a managed build.
	t.Chdir(t.TempDir())
	got := captureSandboxBuildOpts(t)
	var out, errb bytes.Buffer
	if code := runSandboxBuild([]string{"--offline"}, &out, &errb); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errb.String())
	}
	if !got.Offline {
		t.Fatal("BuildOptions.Offline = false, want true with --offline")
	}
	if !sandboxcontainer.IsManagedToolchain(got.ToolchainMode) {
		t.Fatalf("default ToolchainMode = %q, want managed", got.ToolchainMode)
	}
}

func TestRunSandboxBuildOfflineDefaultsFalse(t *testing.T) {
	t.Chdir(t.TempDir())
	got := captureSandboxBuildOpts(t)
	var out, errb bytes.Buffer
	if code := runSandboxBuild(nil, &out, &errb); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errb.String())
	}
	if got.Offline {
		t.Fatal("BuildOptions.Offline = true without --offline, want false")
	}
}

// captureSandboxWarm swaps sandboxWarmFn so a test exercises the warm command
// surface without network, returning a pointer to a call counter.
func captureSandboxWarm(t *testing.T, managed sandboxcontainer.ManagedToolchain, err error) *int {
	t.Helper()
	orig := sandboxWarmFn
	t.Cleanup(func() { sandboxWarmFn = orig })
	calls := 0
	sandboxWarmFn = func(_ context.Context) (sandboxcontainer.ManagedToolchain, error) {
		calls++
		return managed, err
	}
	return &calls
}

func TestRunSandboxWarmDispatchesAndPrints(t *testing.T) {
	calls := captureSandboxWarm(t, sandboxcontainer.ManagedToolchain{
		NodeBinary:    "/cache/node/sha256/abc/bin/node",
		CLIEntrypoint: "/cache/devcontainer-cli/1.0.0/devcontainer.js",
	}, nil)
	var out, errb bytes.Buffer
	if code := runSandboxWarm(nil, &out, &errb); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errb.String())
	}
	if *calls != 1 {
		t.Fatalf("warm provision calls = %d, want 1", *calls)
	}
	got := out.String()
	for _, want := range []string{"node: /cache/node/sha256/abc/bin/node", "devcontainer-cli: /cache/devcontainer-cli/1.0.0/devcontainer.js", "warmed: ok"} {
		if !strings.Contains(got, want) {
			t.Fatalf("warm output missing %q:\n%s", want, got)
		}
	}
}

func TestRunSandboxWarmRejectsHostNPX(t *testing.T) {
	calls := captureSandboxWarm(t, sandboxcontainer.ManagedToolchain{}, nil)
	var out, errb bytes.Buffer
	if code := runSandboxWarm([]string{"--toolchain=host-npx"}, &out, &errb); code != 1 {
		t.Fatalf("exit = %d, want 1 for host-npx warm", code)
	}
	if *calls != 0 {
		t.Fatalf("warm provision ran (%d calls) for host-npx; want 0", *calls)
	}
	if !strings.Contains(errb.String(), "host-npx") {
		t.Fatalf("stderr = %q, want a host-npx rejection message", errb.String())
	}
}

func TestRunSandboxWarmRejectsUnknownArgs(t *testing.T) {
	calls := captureSandboxWarm(t, sandboxcontainer.ManagedToolchain{}, nil)
	var out, errb bytes.Buffer
	if code := runSandboxWarm([]string{"extra"}, &out, &errb); code != 1 {
		t.Fatalf("exit = %d, want 1 for unexpected args", code)
	}
	if *calls != 0 {
		t.Fatalf("warm provision ran (%d calls) with bad args; want 0", *calls)
	}
	if !strings.Contains(errb.String(), "usage: aileron sandbox warm") {
		t.Fatalf("stderr = %q, want usage", errb.String())
	}
}

func TestRunSandboxWarmSurfacesProvisionError(t *testing.T) {
	captureSandboxWarm(t, sandboxcontainer.ManagedToolchain{}, errors.New("warm boom"))
	var out, errb bytes.Buffer
	if code := runSandboxWarm(nil, &out, &errb); code != 1 {
		t.Fatalf("exit = %d, want 1 on provision error", code)
	}
	if !strings.Contains(errb.String(), "warm boom") {
		t.Fatalf("stderr = %q, want the provision error surfaced", errb.String())
	}
}

func TestRunSandboxBuildEscapeHatchFlagOverridesEnv(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv(sandboxcontainer.NodeBinaryEnv, "/env/node")
	got := captureSandboxBuildOpts(t)
	var out, errb bytes.Buffer
	if code := runSandboxBuild([]string{"--toolchain=managed", "--node=/flag/node", "--devcontainer-cli=/flag/cli.js"}, &out, &errb); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errb.String())
	}
	if got.NodeBinary != "/flag/node" {
		t.Fatalf("NodeBinary = %q, want flag override /flag/node", got.NodeBinary)
	}
	if got.DevcontainerCLIEntrypoint != "/flag/cli.js" {
		t.Fatalf("DevcontainerCLIEntrypoint = %q, want /flag/cli.js", got.DevcontainerCLIEntrypoint)
	}
}
