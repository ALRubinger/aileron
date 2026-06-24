package main

import (
	"bytes"
	"context"
	"io"
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
