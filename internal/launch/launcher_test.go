package launch_test

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/launch"
	sandboxcomposition "github.com/ALRubinger/aileron/internal/sandbox/composition"
	sandboxcontainer "github.com/ALRubinger/aileron/internal/sandbox/container"
)

// dropAuxiliaryDockerCalls removes the launcher's out-of-band runtime calls
// so the surrounding contract assertions count only the build/validate/run
// sequence. Three kinds are dropped:
//
//   - the baked-MCP image-label detection calls
//     (`docker image inspect --format '{{ ... ai.aileron.mcp.version }}'`) the
//     launcher issues to choose between the host-mount and baked paths (#957);
//     validateSandbox and launchSandbox each issue one.
//   - the Linux-only agent-UID resolution inspect
//     (`docker image inspect --format '{{.Config.User}}'`) the no-op-gitconfig
//     chown hook issues to decide whether the host-owned bind-mounted dir
//     needs chowning to the image's non-root agent UID (#1204, agent_uid.go).
//   - the Linux-only chown helper call itself
//     (`docker run --entrypoint chown ... -R <uid> /mnt`), delegated to a
//     short-lived root container when that UID is non-root.
//
// The latter two fire only on linux (the hook returns early on other GOOS),
// so without dropping them call-count assertions are platform-dependent —
// the cause of the #1204 CI-only regression in TestLaunch_SandboxScaffold*.
func dropAuxiliaryDockerCalls(calls []string) []string {
	out := make([]string, 0, len(calls))
	for _, c := range calls {
		if strings.Contains(c, sandboxcontainer.MCPVersionLabel) {
			continue
		}
		if strings.Contains(c, "{{.Config.User}}") {
			continue
		}
		if strings.Contains(c, "--entrypoint\nchown") {
			continue
		}
		out = append(out, c)
	}
	return out
}

// TestDropAuxiliaryDockerCalls pins the filter that makes call-count
// assertions platform-independent: the launcher's out-of-band image-inspect
// and (linux-only) no-op-gitconfig chown helper calls must be dropped while
// the build/validate/run sequence is preserved. Regression for #1204: the
// always-mount gitconfig path added a `docker run --entrypoint chown` call on
// linux that broke TestLaunch_SandboxScaffoldBuildsAndRuns' "exactly two
// calls" assertion (the call is a no-op on darwin, so it only failed in CI).
func TestDropAuxiliaryDockerCalls(t *testing.T) {
	mcpInspect := "image\ninspect\n--format\n{{ index .Config.Labels \"" + sandboxcontainer.MCPVersionLabel + "\" }}\nimg:test"
	uidInspect := "image\ninspect\n--format\n{{.Config.User}}\nimg:test"
	chownCall := "run\n--rm\n--user\n0\n--entrypoint\nchown\n--volume\n/host/transient:/mnt\nimg:test\n-R\n1000\n/mnt"
	validateCall := "run\n--rm\nimg:test\n/bin/sh\n-c\nprobe"
	runCall := "run\n--rm\n-it\nimg:test"

	got := dropAuxiliaryDockerCalls([]string{mcpInspect, validateCall, uidInspect, chownCall, runCall})
	want := []string{validateCall, runCall}
	if len(got) != len(want) {
		t.Fatalf("dropAuxiliaryDockerCalls kept %d calls, want %d:\n%v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("call %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestResolveBinary_Found(t *testing.T) {
	path, err := launch.ResolveBinary([]string{"echo"})
	if err != nil {
		t.Fatalf("expected to find 'echo': %v", err)
	}
	if !filepath.IsAbs(path) {
		t.Errorf("expected absolute path, got %q", path)
	}
}

func TestResolveBinary_FallsBackToSecondCandidate(t *testing.T) {
	path, err := launch.ResolveBinary([]string{"nonexistent-xyz-1234", "echo"})
	if err != nil {
		t.Fatalf("expected to find 'echo' as fallback: %v", err)
	}
	if !strings.HasSuffix(path, "echo") {
		t.Errorf("expected path ending in 'echo', got %q", path)
	}
}

func TestResolveBinary_NotFound(t *testing.T) {
	_, err := launch.ResolveBinary([]string{"nonexistent-binary-xyz-9999"})
	if err == nil {
		t.Fatal("expected error for missing binary")
	}
}

// scriptAgent launches a specific script/binary directly. Used to
// exercise Launch's environment + arg flow against a real subprocess
// without requiring a real coding-agent binary.
type scriptAgent struct {
	script   string
	extraEnv map[string]string
	mcpArgs  []string
}

func (a scriptAgent) Name() string           { return "test-script" }
func (a scriptAgent) BinaryNames() []string  { return []string{a.script} }
func (a scriptAgent) Args() []string         { return nil }
func (a scriptAgent) Env() map[string]string { return a.extraEnv }
func (a scriptAgent) LLMEndpointEnv() string { return "" }
func (a scriptAgent) ConfigureMCP(string, map[string]string, string, launch.Mode) ([]string, []launch.MCPMount, error) {
	return a.mcpArgs, nil, nil
}
func (a scriptAgent) AuthSpec() launch.AuthSpec { return launch.AuthSpec{} }

func TestLaunch_AgentEnvVarsFlowThrough(t *testing.T) {
	dir := t.TempDir()
	outFile := filepath.Join(dir, "env.txt")
	script := filepath.Join(dir, "capture.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nenv > "+outFile+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	agent := scriptAgent{script: script, extraEnv: map[string]string{
		"CUSTOM_VAR": "hello",
	}}

	_, err := launch.Launch(context.Background(), launch.LaunchConfig{Agent: agent})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("read env: %v", err)
	}
	if !strings.Contains(string(data), "CUSTOM_VAR=hello") {
		t.Errorf("CUSTOM_VAR not propagated to child env")
	}
}

func TestLaunch_ShimEnvVarsNotInjected(t *testing.T) {
	// ADR-0015: launch no longer sets SHELL=<shim>, AILERON_REAL_SHELL,
	// AILERON_AGENT, or AILERON_AUDIT_DIR. The child inherits the
	// parent's $SHELL untouched.
	dir := t.TempDir()
	outFile := filepath.Join(dir, "env.txt")
	script := filepath.Join(dir, "capture.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nenv > "+outFile+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SHELL", "/bin/zsh")

	_, err := launch.Launch(context.Background(), launch.LaunchConfig{
		Agent: scriptAgent{script: script},
	})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	data, _ := os.ReadFile(outFile)
	env := string(data)
	if !strings.Contains(env, "SHELL=/bin/zsh") {
		t.Errorf("expected SHELL=/bin/zsh to flow through; got:\n%s", env)
	}
	for _, key := range []string{"AILERON_REAL_SHELL=", "AILERON_AGENT=", "AILERON_AUDIT_DIR="} {
		if strings.Contains(env, key) {
			t.Errorf("expected %q to not be set in child env (per ADR-0015):\n%s", key, env)
		}
	}
}

func TestLaunch_ExitCodePropagation(t *testing.T) {
	agent := scriptAgent{script: "/bin/sh"}
	result, err := launch.Launch(context.Background(), launch.LaunchConfig{
		Agent: agent,
		Args:  []string{"-c", "exit 42"},
	})
	if err != nil {
		t.Fatalf("launch failed: %v", err)
	}
	if result.ExitCode != 42 {
		t.Errorf("expected exit code 42, got %d", result.ExitCode)
	}
}

func TestLaunch_BinaryNotFound(t *testing.T) {
	agent := scriptAgent{script: "nonexistent-binary-xyz-9999"}
	_, err := launch.Launch(context.Background(), launch.LaunchConfig{
		Agent: agent,
	})
	if err == nil {
		t.Fatal("expected error for missing binary")
	}
}

func TestLaunch_WorkingDirectory(t *testing.T) {
	dir := t.TempDir()
	workDir := filepath.Join(dir, "subdir")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatal(err)
	}
	outFile := filepath.Join(dir, "pwd.txt")
	script := filepath.Join(dir, "pwd.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\npwd > "+outFile+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	_, err := launch.Launch(context.Background(), launch.LaunchConfig{
		Agent: scriptAgent{script: script},
		Dir:   workDir,
	})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	got, _ := os.ReadFile(outFile)
	if !strings.Contains(string(got), workDir) {
		t.Errorf("child pwd = %q, want it under %q", string(got), workDir)
	}
}

func TestLaunch_AgentMCPArgs_Appended(t *testing.T) {
	// aileron-mcp resolution is handled package-wide by TestMain
	// (a fake binary is on PATH for every test).
	dir := t.TempDir()
	outFile := filepath.Join(dir, "args.txt")
	// Capture argv as one line per argument so we can assert presence.
	script := filepath.Join(dir, "args.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > "+outFile+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	_, err := launch.Launch(context.Background(), launch.LaunchConfig{
		Agent: scriptAgent{
			script:  script,
			mcpArgs: []string{"--mcp-flag", "mcp-value"},
		},
		Args: []string{"user-arg"},
	})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	data, _ := os.ReadFile(outFile)
	args := strings.Split(strings.TrimSpace(string(data)), "\n")
	want := []string{"user-arg", "--mcp-flag", "mcp-value"}
	for _, w := range want {
		found := false
		for _, a := range args {
			if a == w {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected arg %q in child argv, got %v", w, args)
		}
	}
}

func TestLaunch_SandboxBYOImageRunsContainer(t *testing.T) {
	// Proxy bootstrap is default-on for --sandbox=docker per the
	// U3 plan. This test exercises the opt-out path via
	// --sandbox-proxy=off so it can assert the rest of the container
	// shape without coupling to the proxy bootstrap env. The default-on
	// behavior is covered separately by
	// TestLaunch_SandboxProxyDefaultOnForDocker.
	dir := t.TempDir()
	devcontainerDir := filepath.Join(dir, ".devcontainer")
	if err := os.MkdirAll(devcontainerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(devcontainerDir, "devcontainer.json"), []byte(`{"customizations":{"aileron":{"image":"ghcr.io/acme/agent:latest"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	binDir := t.TempDir()
	argsFile := filepath.Join(dir, "docker-args.txt")
	docker := filepath.Join(binDir, "docker")
	if err := os.WriteFile(docker, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > "+argsFile+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	_, err := launch.Launch(context.Background(), launch.LaunchConfig{
		Agent:          scriptAgent{script: "codex"},
		Dir:            dir,
		Args:           []string{"--ask-for-approval", "never"},
		SandboxRuntime: "auto",
		SandboxProxy:   "off",
	})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	data, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("read docker args: %v", err)
	}
	args := string(data)
	for _, want := range []string{
		"run\n",
		"--workdir\n/home/agent/workspace\n",
		"--volume\n" + dir + ":/home/agent/workspace\n",
		"--env\nAILERON_SANDBOX_IMAGE=ghcr.io/acme/agent:latest\n",
		"--env\nAILERON_SANDBOX_TIER=byo_image\n",
		"--env\nAILERON_SANDBOX_RUNTIME=docker\n",
		"--env\nAILERON_URL=http://host.docker.internal:",
		// Sandbox launch disables the in-container auto-updater: the
		// image's npm-global prefix is root-owned and the image is the
		// versioning unit, so a self-update would fail and be pointless.
		"--env\nDISABLE_AUTOUPDATER=1\n",
		"ghcr.io/acme/agent:latest\ncodex\n--ask-for-approval\nnever\n",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("expected %q in docker args:\n%s", want, args)
		}
	}
	if !regexp.MustCompile(`--env\nAILERON_API_URL=http://host\.docker\.internal:[0-9]+/v1\n`).MatchString(args) {
		t.Errorf("expected container AILERON_API_URL /v1 env in docker args:\n%s", args)
	}
	// The shim surface is retired (#959): no tools.txt/shim env vars and no
	// shim mounts are emitted; aileron-mcp is the sole tool surface.
	for _, unwanted := range []string{
		"HTTPS_PROXY=", "HTTP_PROXY=", "AILERON_SANDBOX_PROXY_MODE=",
		"AILERON_TOOLS_FILE=", "AILERON_SHIMS_DIR=",
		"/etc/aileron/tools.txt", "/usr/local/bin/google",
	} {
		if strings.Contains(args, unwanted) {
			t.Errorf("did not expect retired/proxy artifact %q under --sandbox-proxy=off:\n%s", unwanted, args)
		}
	}
}

// TestLaunch_SandboxProxyDefaultOnForDocker covers the default-on flip
// (#896 U3): `aileron launch --sandbox=docker` enables HTTPS proxy
// bootstrap without any flag or env var. The container starts through
// aileron-run-with-proxy-ca, the session CA lands at
// /etc/aileron/proxy/ca.pem, and proxy env (HTTPS_PROXY, HTTP_PROXY,
// AILERON_SANDBOX_PROXY_*) reach the agent. This was previously gated
// by AILERON_SANDBOX_PROXY_BOOTSTRAP=1; that env var no longer
// participates.
func TestLaunch_SandboxProxyDefaultOnForDocker(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("AILERON_TOKEN", "daemon-token")
	// Make sure neither the new nor the legacy env vars are set so the
	// default-on path is what's being exercised.
	t.Setenv("AILERON_SANDBOX_PROXY", "")
	t.Setenv("AILERON_SANDBOX_PROXY_BOOTSTRAP", "")

	dir := t.TempDir()
	devcontainerDir := filepath.Join(dir, ".devcontainer")
	if err := os.MkdirAll(devcontainerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(devcontainerDir, "devcontainer.json"), []byte(`{"customizations":{"aileron":{"image":"ghcr.io/acme/agent:latest"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	binDir := t.TempDir()
	argsFile := filepath.Join(dir, "docker-args.txt")
	docker := filepath.Join(binDir, "docker")
	if err := os.WriteFile(docker, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > "+argsFile+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	_, err := launch.Launch(context.Background(), launch.LaunchConfig{
		Agent:          scriptAgent{script: "codex"},
		Dir:            dir,
		SandboxRuntime: "docker",
		// No SandboxProxy set — exercises default-on.
	})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	data, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("read docker args: %v", err)
	}
	args := string(data)
	caPath := filepath.Join(home, ".aileron", "sessions", "01HK0000000000000000000FAK", "sandbox-proxy", "ca.pem")
	if _, err := os.Stat(caPath); err != nil {
		t.Fatalf("stat generated CA: %v", err)
	}
	for _, want := range []string{
		"--user\nroot\n",
		"--volume\n" + caPath + ":/etc/aileron/proxy/ca.pem:ro\n",
		"--env\nAILERON_TOKEN=daemon-token\n",
		"--env\nAILERON_SANDBOX_PROXY_CA_FILE=/etc/aileron/proxy/ca.pem\n",
		"--env\nAILERON_SANDBOX_PROXY_MODE=bootstrap\n",
		"--env\nAILERON_SANDBOX_PROXY_URL=http://01HK0000000000000000000FAK:daemon-token@host.docker.internal:",
		"--env\nHTTPS_PROXY=http://01HK0000000000000000000FAK:daemon-token@host.docker.internal:",
		"--env\nHTTP_PROXY=http://01HK0000000000000000000FAK:daemon-token@host.docker.internal:",
		"host.docker.internal",
		"aileron-run-with-proxy-ca\ncodex\n",
	} {
		if !strings.Contains(args, want) {
			t.Fatalf("run call missing %q:\n%s", want, args)
		}
	}
}

// TestLaunch_SandboxProxyLegacyEnvVarIgnored guards the rename from
// AILERON_SANDBOX_PROXY_BOOTSTRAP to AILERON_SANDBOX_PROXY (#896 U3):
// setting only the legacy env var has no effect on the default-on
// behavior — proxy bootstrap is still active because we're on
// --sandbox=docker. Opt-out requires the new env var or the flag.
func TestLaunch_SandboxProxyLegacyEnvVarIgnored(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	// Only set the legacy env var. The new one stays unset, so the
	// resolver falls through to the default-on policy for docker.
	t.Setenv("AILERON_SANDBOX_PROXY", "")
	t.Setenv("AILERON_SANDBOX_PROXY_BOOTSTRAP", "off")

	dir := t.TempDir()
	devcontainerDir := filepath.Join(dir, ".devcontainer")
	if err := os.MkdirAll(devcontainerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(devcontainerDir, "devcontainer.json"), []byte(`{"customizations":{"aileron":{"image":"ghcr.io/acme/agent:latest"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	binDir := t.TempDir()
	argsFile := filepath.Join(dir, "docker-args.txt")
	docker := filepath.Join(binDir, "docker")
	if err := os.WriteFile(docker, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > "+argsFile+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	_, err := launch.Launch(context.Background(), launch.LaunchConfig{
		Agent:          scriptAgent{script: "codex"},
		Dir:            dir,
		SandboxRuntime: "docker",
	})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	data, _ := os.ReadFile(argsFile)
	args := string(data)
	// The legacy env var was set to "off" but should not be honored;
	// proxy bootstrap stays default-on.
	if !strings.Contains(args, "--env\nAILERON_SANDBOX_PROXY_MODE=bootstrap\n") {
		t.Fatalf("legacy AILERON_SANDBOX_PROXY_BOOTSTRAP=off should not disable bootstrap:\n%s", args)
	}
}

// TestLaunch_SandboxProxyOffViaFlag covers the --sandbox-proxy=off
// opt-out path: docker sandbox launches without proxy bootstrap, no
// AILERON_SANDBOX_PROXY_MODE env, no --user root, no
// aileron-run-with-proxy-ca prefix.
func TestLaunch_SandboxProxyOffViaFlag(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("AILERON_SANDBOX_PROXY", "")
	t.Setenv("AILERON_SANDBOX_PROXY_BOOTSTRAP", "")

	dir := t.TempDir()
	devcontainerDir := filepath.Join(dir, ".devcontainer")
	if err := os.MkdirAll(devcontainerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(devcontainerDir, "devcontainer.json"), []byte(`{"customizations":{"aileron":{"image":"ghcr.io/acme/agent:latest"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	binDir := t.TempDir()
	argsFile := filepath.Join(dir, "docker-args.txt")
	docker := filepath.Join(binDir, "docker")
	if err := os.WriteFile(docker, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > "+argsFile+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	_, err := launch.Launch(context.Background(), launch.LaunchConfig{
		Agent:          scriptAgent{script: "codex"},
		Dir:            dir,
		SandboxRuntime: "docker",
		SandboxProxy:   "off",
	})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	data, _ := os.ReadFile(argsFile)
	args := string(data)
	for _, unwanted := range []string{
		"AILERON_SANDBOX_PROXY_MODE=",
		"HTTPS_PROXY=",
		"aileron-run-with-proxy-ca",
		"--user\nroot\n",
	} {
		if strings.Contains(args, unwanted) {
			t.Fatalf("expected no %q under --sandbox-proxy=off:\n%s", unwanted, args)
		}
	}
}

// TestLaunch_SandboxProxyEnvOverridesDefault verifies
// AILERON_SANDBOX_PROXY=off disables proxy bootstrap with no flag set,
// honoring the env-var rename.
func TestLaunch_SandboxProxyEnvOverridesDefault(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("AILERON_SANDBOX_PROXY", "off")
	t.Setenv("AILERON_SANDBOX_PROXY_BOOTSTRAP", "")

	dir := t.TempDir()
	devcontainerDir := filepath.Join(dir, ".devcontainer")
	if err := os.MkdirAll(devcontainerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(devcontainerDir, "devcontainer.json"), []byte(`{"customizations":{"aileron":{"image":"ghcr.io/acme/agent:latest"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	binDir := t.TempDir()
	argsFile := filepath.Join(dir, "docker-args.txt")
	docker := filepath.Join(binDir, "docker")
	if err := os.WriteFile(docker, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > "+argsFile+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	_, err := launch.Launch(context.Background(), launch.LaunchConfig{
		Agent:          scriptAgent{script: "codex"},
		Dir:            dir,
		SandboxRuntime: "docker",
	})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	data, _ := os.ReadFile(argsFile)
	args := string(data)
	if strings.Contains(args, "AILERON_SANDBOX_PROXY_MODE=") {
		t.Fatalf("AILERON_SANDBOX_PROXY=off did not disable bootstrap:\n%s", args)
	}
}

// TestLaunch_SandboxProxyFlagWinsOverEnv verifies flag > env: when
// the flag forces "on" but env says "off", bootstrap activates.
func TestLaunch_SandboxProxyFlagWinsOverEnv(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("AILERON_SANDBOX_PROXY", "off")
	t.Setenv("AILERON_SANDBOX_PROXY_BOOTSTRAP", "")

	dir := t.TempDir()
	devcontainerDir := filepath.Join(dir, ".devcontainer")
	if err := os.MkdirAll(devcontainerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(devcontainerDir, "devcontainer.json"), []byte(`{"customizations":{"aileron":{"image":"ghcr.io/acme/agent:latest"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	binDir := t.TempDir()
	argsFile := filepath.Join(dir, "docker-args.txt")
	docker := filepath.Join(binDir, "docker")
	if err := os.WriteFile(docker, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > "+argsFile+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	_, err := launch.Launch(context.Background(), launch.LaunchConfig{
		Agent:          scriptAgent{script: "codex"},
		Dir:            dir,
		SandboxRuntime: "docker",
		SandboxProxy:   "on",
	})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	data, _ := os.ReadFile(argsFile)
	args := string(data)
	if !strings.Contains(args, "AILERON_SANDBOX_PROXY_MODE=bootstrap") {
		t.Fatalf("flag=on env=off should keep bootstrap on:\n%s", args)
	}
}

// TestLaunch_SandboxProxyPreflightFailureRefusesAndCitesDocs
// covers the BYO-non-compliant path: default-on proxy bootstrap
// requested, but the validate-time contract probe exits 127 with a
// message matching the contract markers. Launch must surface an
// actionable error citing the contract docs URL and the opt-out flag,
// and never invoke the agent run command.
func TestLaunch_SandboxProxyPreflightFailureRefusesAndCitesDocs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("AILERON_SANDBOX_PROXY", "")
	t.Setenv("AILERON_SANDBOX_PROXY_BOOTSTRAP", "")

	dir := t.TempDir()
	devcontainerDir := filepath.Join(dir, ".devcontainer")
	if err := os.MkdirAll(devcontainerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(devcontainerDir, "devcontainer.json"), []byte(`{"customizations":{"aileron":{"image":"ghcr.io/acme/agent:latest"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	// Fake docker fails with the runtime contract probe's missing-helper
	// message to mimic a BYO image lacking aileron-install-proxy-ca.
	binDir := t.TempDir()
	argsFile := filepath.Join(dir, "docker-args.txt")
	docker := filepath.Join(binDir, "docker")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" >> " + argsFile + "\necho 'sandbox proxy bootstrap requires aileron-install-proxy-ca in the sandbox image' >&2\nexit 127\n"
	if err := os.WriteFile(docker, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	_, err := launch.Launch(context.Background(), launch.LaunchConfig{
		Agent:          scriptAgent{script: "codex"},
		Dir:            dir,
		SandboxRuntime: "docker",
	})
	if err == nil {
		t.Fatal("expected sandbox proxy preflight failure")
	}
	msg := err.Error()
	for _, want := range []string{
		"sandbox proxy bootstrap preflight failed",
		"ghcr.io/acme/agent:latest",
		"https://docs.withaileron.ai/development/sandbox-agent-images/#byo-image-proxy-contract",
		"--sandbox-proxy=off",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q missing %q", msg, want)
		}
	}
	// The agent command must not have been invoked — the run call would
	// include "codex" after the image name in the captured argv.
	data, _ := os.ReadFile(argsFile)
	if strings.Contains(string(data), "ghcr.io/acme/agent:latest\naileron-run-with-proxy-ca\ncodex\n") {
		t.Fatalf("agent run happened despite preflight failure:\n%s", data)
	}
}

// TestLaunch_SandboxProxyRefusesUnsupportedMode verifies that
// --sandbox-proxy=on against --sandbox=off (a mode that cannot
// support bootstrap) refuses to launch with an actionable error.
func TestLaunch_SandboxProxyRefusesUnsupportedMode(t *testing.T) {
	t.Setenv("AILERON_SANDBOX_PROXY", "")
	t.Setenv("AILERON_SANDBOX_PROXY_BOOTSTRAP", "")

	_, err := launch.Launch(context.Background(), launch.LaunchConfig{
		Agent:          scriptAgent{script: "/bin/sh"},
		SandboxRuntime: "off",
		SandboxProxy:   "on",
	})
	if err == nil {
		t.Fatal("expected refuse-launch error for --sandbox-proxy=on --sandbox=off")
	}
	msg := err.Error()
	for _, want := range []string{"sandbox proxy bootstrap", "--sandbox=docker", "--sandbox-proxy=off"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q missing %q", msg, want)
		}
	}
}

func TestLaunch_SandboxMountsAileronManifestStores(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	actionsDir := filepath.Join(home, ".aileron", "actions")
	connectorsDir := filepath.Join(home, ".aileron", "store", "connectors")
	for _, dir := range []string{actionsDir, connectorsDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	dir := t.TempDir()
	devcontainerDir := filepath.Join(dir, ".devcontainer")
	if err := os.MkdirAll(devcontainerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(devcontainerDir, "devcontainer.json"), []byte(`{"customizations":{"aileron":{"image":"ghcr.io/acme/agent:latest"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	binDir := t.TempDir()
	argsFile := filepath.Join(dir, "docker-args.txt")
	docker := filepath.Join(binDir, "docker")
	if err := os.WriteFile(docker, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > "+argsFile+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	_, err := launch.Launch(context.Background(), launch.LaunchConfig{
		Agent:          scriptAgent{script: "codex"},
		Dir:            dir,
		SandboxRuntime: "docker",
	})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	data, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("read docker args: %v", err)
	}
	args := string(data)
	for _, want := range []string{
		"--volume\n" + actionsDir + ":/opt/aileron/manifests/actions:ro\n",
		"--volume\n" + connectorsDir + ":/opt/aileron/manifests/connectors:ro\n",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("expected %q in docker args:\n%s", want, args)
		}
	}
}

func TestLaunch_SandboxDiscoverySmokeMountsShimsForValidateAndRun(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	actionsDir := filepath.Join(home, ".aileron", "actions")
	if err := os.MkdirAll(actionsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(actionsDir, "send-email.md"), []byte(sandboxDiscoveryActionManifest), 0o644); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	devcontainerDir := filepath.Join(dir, ".devcontainer")
	if err := os.MkdirAll(devcontainerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(devcontainerDir, "devcontainer.json"), []byte(`{"customizations":{"aileron":{"image":"ghcr.io/acme/agent:latest"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	binDir := t.TempDir()
	argsFile := filepath.Join(dir, "docker-args.txt")
	docker := filepath.Join(binDir, "docker")
	script := "#!/bin/sh\nprintf '%s\\n' '---' >> " + argsFile + "\nprintf '%s\\n' \"$@\" >> " + argsFile + "\n"
	if err := os.WriteFile(docker, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	_, err := launch.Launch(context.Background(), launch.LaunchConfig{
		Agent:          scriptAgent{script: "codex"},
		Dir:            dir,
		SandboxRuntime: "docker",
	})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	data, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("read docker args: %v", err)
	}
	calls := dropAuxiliaryDockerCalls(strings.Split(strings.TrimPrefix(string(data), "---\n"), "\n---\n"))
	if len(calls) != 2 {
		t.Fatalf("docker calls = %d, want validate and run:\n%s", len(calls), data)
	}
	validateCall, runCall := calls[0], calls[1]
	// The shim surface is retired (#959): neither call mounts a tools.txt
	// manifest or a /usr/local/bin shim. aileron-mcp is the sole tool path.
	for _, call := range []struct {
		name string
		args string
	}{
		{name: "validation", args: validateCall},
		{name: "run", args: runCall},
	} {
		for _, unwanted := range []string{
			":/etc/aileron/tools.txt:ro\n",
			":/usr/local/bin/google:ro\n",
		} {
			if strings.Contains(call.args, unwanted) {
				t.Fatalf("%s call should not mount retired shim surface %q:\n%s", call.name, unwanted, call.args)
			}
		}
	}
	if !strings.Contains(validateCall, "/bin/sh\n-c\n") {
		t.Fatalf("validation call did not run contract probe:\n%s", validateCall)
	}
	// After the shim slot was retired (#959), under default-on proxy
	// bootstrap for --sandbox=docker the contract probe expects
	// proxy-trust=1, MCP-binary=1 (positions 2/3 in the trailing argv).
	if !strings.HasSuffix(strings.TrimSpace(validateCall), "\ncodex\n1\n1") {
		t.Fatalf("validation call did not enable proxy-trust + MCP-binary validation:\n%s", validateCall)
	}
	if !regexp.MustCompile(`--env\nAILERON_API_URL=http://host\.docker\.internal:[0-9]+/v1\n`).MatchString(runCall) {
		t.Fatalf("run call missing container AILERON_API_URL:\n%s", runCall)
	}
	for _, unwanted := range []string{"AILERON_TOOLS_FILE=", "AILERON_SHIMS_DIR="} {
		if strings.Contains(runCall, unwanted) {
			t.Fatalf("run call should not set retired shim env %q:\n%s", unwanted, runCall)
		}
	}
	if !strings.Contains(runCall, "ghcr.io/acme/agent:latest\naileron-run-with-proxy-ca\ncodex\n") {
		t.Fatalf("run call did not execute agent image command through proxy-ca wrapper:\n%s", runCall)
	}
}

// TestLaunch_SandboxConnectorSpecPresentEmitsNoShim is the end-to-end
// guard for #959: an installed connector spec no longer produces a
// /usr/local/bin shim or a tools.txt mount. The connectors manifest
// directory is still mounted read-only for the data plane, and
// aileron-mcp remains the sole tool surface.
func TestLaunch_SandboxConnectorSpecPresentEmitsNoShim(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	specDir := filepath.Join(home, ".aileron", "store", "connectors", "sha256", "abc123")
	if err := os.MkdirAll(specDir, 0o755); err != nil {
		t.Fatal(err)
	}
	spec := `{
	  "schema_version": "aileron.connector.v1",
	  "connector": {"fqn": "github://acme/aileron-connector-google", "version": "1.2.3"},
	  "tools": [{
	    "name": "google",
	    "description": "Google APIs",
	    "operations": [{"name": "gmail.messages.search", "summary": "Search Gmail messages"}]
	  }]
	}`
	if err := os.WriteFile(filepath.Join(specDir, "aileron.connector.v1.json"), []byte(spec), 0o644); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	devcontainerDir := filepath.Join(dir, ".devcontainer")
	if err := os.MkdirAll(devcontainerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(devcontainerDir, "devcontainer.json"), []byte(`{"customizations":{"aileron":{"image":"ghcr.io/acme/agent:latest"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	binDir := t.TempDir()
	argsFile := filepath.Join(dir, "docker-args.txt")
	docker := filepath.Join(binDir, "docker")
	script := "#!/bin/sh\nprintf '%s\\n' '---' >> " + argsFile + "\nprintf '%s\\n' \"$@\" >> " + argsFile + "\n"
	if err := os.WriteFile(docker, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	_, err := launch.Launch(context.Background(), launch.LaunchConfig{
		Agent:          scriptAgent{script: "codex"},
		Dir:            dir,
		SandboxRuntime: "docker",
	})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	data, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("read docker args: %v", err)
	}
	calls := dropAuxiliaryDockerCalls(strings.Split(strings.TrimPrefix(string(data), "---\n"), "\n---\n"))
	if len(calls) != 2 {
		t.Fatalf("docker calls = %d, want validate and run:\n%s", len(calls), data)
	}
	for _, call := range []struct {
		name string
		args string
	}{
		{name: "validation", args: calls[0]},
		{name: "run", args: calls[1]},
	} {
		for _, unwanted := range []string{
			":/etc/aileron/tools.txt:ro\n",
			":/usr/local/bin/google:ro\n",
		} {
			if strings.Contains(call.args, unwanted) {
				t.Fatalf("%s call should not mount retired shim surface %q:\n%s", call.name, unwanted, call.args)
			}
		}
		if !regexp.MustCompile(`--volume\n[^\n]*/aileron-mcp:/usr/local/bin/aileron-mcp:ro\n`).MatchString(call.args) {
			t.Fatalf("%s call missing aileron-mcp mount:\n%s", call.name, call.args)
		}
	}
}

func TestLaunch_SandboxScaffoldBuildsAndRuns(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	actionsDir := filepath.Join(home, ".aileron", "actions")
	if err := os.MkdirAll(actionsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(actionsDir, "send-email.md"), []byte(sandboxDiscoveryActionManifest), 0o644); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	if _, err := sandboxcomposition.Init(sandboxcomposition.InitOptions{WorkDir: dir, Version: "test"}); err != nil {
		t.Fatalf("init scaffold: %v", err)
	}

	binDir := t.TempDir()
	argsFile := filepath.Join(dir, "docker-args.txt")
	npxArgsFile := filepath.Join(dir, "npx-args.txt")
	// The Feature-composing scaffold (no Dockerfile) builds through
	// @devcontainers/cli, which the build path execs as `npx`. Validation and
	// run still go through `docker`. Stub both so the launch wiring is asserted
	// hermetically; a real Feature build is proven in the dedicated
	// integration-sandbox-features job.
	writeStub := func(name, dest string) {
		script := "#!/bin/sh\nprintf '%s\\n' '---' >> " + dest + "\nprintf '%s\\n' \"$@\" >> " + dest + "\n"
		if err := os.WriteFile(filepath.Join(binDir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeStub("docker", argsFile)
	writeStub("npx", npxArgsFile)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	_, err := launch.Launch(context.Background(), launch.LaunchConfig{
		Agent:              scriptAgent{script: "codex"},
		Dir:                dir,
		Args:               []string{"--ask-for-approval", "never"},
		SandboxRuntime:     "docker",
		SandboxBuildPolicy: "always",
	})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	image := sandboxcontainer.ProjectImageTag(dir)
	// The Feature scaffold builds via @devcontainers/cli (npx), not docker build.
	npxData, err := os.ReadFile(npxArgsFile)
	if err != nil {
		t.Fatalf("read npx args: %v", err)
	}
	for _, want := range []string{
		"@devcontainers/cli@",
		"build\n",
		"--workspace-folder\n" + dir + "\n",
		"--image-name\n" + image + "\n",
	} {
		if !strings.Contains(string(npxData), want) {
			t.Fatalf("devcontainers/cli build call missing %q:\n%s", want, npxData)
		}
	}
	data, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("read docker args: %v", err)
	}
	calls := dropAuxiliaryDockerCalls(strings.Split(strings.TrimPrefix(string(data), "---\n"), "\n---\n"))
	if len(calls) != 2 {
		t.Fatalf("docker calls = %d, want validate and run (build runs via @devcontainers/cli):\n%s", len(calls), data)
	}
	validateCall, runCall := calls[0], calls[1]
	for _, call := range []struct {
		name string
		args string
	}{
		{name: "validation", args: validateCall},
		{name: "run", args: runCall},
	} {
		if !strings.Contains(call.args, image+"\n") {
			t.Fatalf("%s call missing image %q:\n%s", call.name, image, call.args)
		}
		for _, unwanted := range []string{
			":/etc/aileron/tools.txt:ro\n",
			":/usr/local/bin/google:ro\n",
		} {
			if strings.Contains(call.args, unwanted) {
				t.Fatalf("%s call should not mount retired shim surface %q:\n%s", call.name, unwanted, call.args)
			}
		}
	}
	if !strings.Contains(validateCall, "/bin/sh\n-c\n") {
		t.Fatalf("validation call did not run contract probe:\n%s", validateCall)
	}
	for _, want := range []string{
		"--env\nAILERON_SANDBOX_IMAGE=" + image + "\n",
		"--env\nAILERON_SANDBOX_TIER=devcontainer\n",
		"--env\nAILERON_SANDBOX_RUNTIME=docker\n",
		"codex\n--ask-for-approval\nnever\n",
	} {
		if !strings.Contains(runCall, want) {
			t.Fatalf("run call missing %q:\n%s", want, runCall)
		}
	}
	for _, unwanted := range []string{"AILERON_TOOLS_FILE=", "AILERON_SHIMS_DIR="} {
		if strings.Contains(runCall, unwanted) {
			t.Fatalf("run call should not set retired shim env %q:\n%s", unwanted, runCall)
		}
	}
}

func TestLaunch_SandboxBuildRunsPreparedImage(t *testing.T) {
	dir := t.TempDir()
	baseDir := filepath.Join(dir, "images", "sandbox-base")
	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(baseDir, "Containerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	argsFile := filepath.Join(dir, "docker-args.txt")
	docker := filepath.Join(binDir, "docker")
	if err := os.WriteFile(docker, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" >> "+argsFile+"\nprintf '%s\\n' --- >> "+argsFile+"\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	_, err := launch.Launch(context.Background(), launch.LaunchConfig{
		Agent:              scriptAgent{script: "claude"},
		Dir:                dir,
		Args:               []string{"--dangerously-skip-permissions"},
		SandboxRuntime:     "docker",
		SandboxBuildPolicy: "always",
	})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	data, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("read docker args: %v", err)
	}
	args := string(data)
	for _, want := range []string{
		"build\n-t\nghcr.io/alrubinger/aileron-sandbox-base:edge\n",
		"run\n--rm\n-i\n",
		"--env\nAILERON_SANDBOX_IMAGE=ghcr.io/alrubinger/aileron-sandbox-base:edge\n",
		"--env\nAILERON_SANDBOX_TIER=base\n",
		"--env\nAILERON_SANDBOX_RUNTIME=docker\n",
		// Default-on proxy bootstrap routes the agent through
		// aileron-run-with-proxy-ca; the image name still precedes the
		// agent argv but the bootstrap wrapper sits between them.
		"ghcr.io/alrubinger/aileron-sandbox-base:edge\naileron-run-with-proxy-ca\nclaude\n--dangerously-skip-permissions\n",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("expected %q in docker args:\n%s", want, args)
		}
	}
}

// publishedAgent reports a Name() that has a published per-agent sandbox image
// (claude/codex), so the no-.devcontainer launch path resolves the build-free
// TierPublished default and pulls the per-agent image instead of building the
// local base.
type publishedAgent struct {
	scriptAgent
	name string
}

func (a publishedAgent) Name() string { return a.name }

func TestLaunch_SandboxNoDevcontainerPublishedAgentPullsPerAgentImage(t *testing.T) {
	dir := t.TempDir()
	// No images/sandbox-base context and no .devcontainer: the only build-free
	// route is the published per-agent image. If the launcher fell back to the
	// base build it would fail to find the Containerfile context.
	binDir := t.TempDir()
	argsFile := filepath.Join(dir, "docker-args.txt")
	docker := filepath.Join(binDir, "docker")
	if err := os.WriteFile(docker, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" >> "+argsFile+"\nprintf '%s\\n' --- >> "+argsFile+"\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	_, err := launch.Launch(context.Background(), launch.LaunchConfig{
		Agent:              publishedAgent{scriptAgent: scriptAgent{script: "claude"}, name: "claude"},
		Dir:                dir,
		Args:               []string{"--dangerously-skip-permissions"},
		SandboxRuntime:     "docker",
		SandboxBuildPolicy: "always",
	})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	data, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("read docker args: %v", err)
	}
	args := string(data)
	const image = "ghcr.io/alrubinger/aileron-sandbox-claude:edge"
	for _, want := range []string{
		"pull\n" + image + "\n",
		"run\n--rm\n-i\n",
		"--env\nAILERON_SANDBOX_IMAGE=" + image + "\n",
		"--env\nAILERON_SANDBOX_TIER=published\n",
		image + "\naileron-run-with-proxy-ca\nclaude\n--dangerously-skip-permissions\n",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("expected %q in docker args:\n%s", want, args)
		}
	}
	// The published path must not build the local base image.
	if strings.Contains(args, "build\n-t\nghcr.io/alrubinger/aileron-sandbox-base") {
		t.Errorf("published launch unexpectedly built the local base image:\n%s", args)
	}
}

func TestLaunch_SandboxBuildFailureFailsBeforeAgentStart(t *testing.T) {
	dir := t.TempDir()
	baseDir := filepath.Join(dir, "images", "sandbox-base")
	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(baseDir, "Containerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	docker := filepath.Join(binDir, "docker")
	if err := os.WriteFile(docker, []byte("#!/bin/sh\nexit 7\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	outFile := filepath.Join(dir, "agent-started")
	script := filepath.Join(dir, "capture.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ntouch "+outFile+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	_, err := launch.Launch(context.Background(), launch.LaunchConfig{
		Agent:              scriptAgent{script: script},
		Dir:                dir,
		SandboxRuntime:     "docker",
		SandboxBuildPolicy: "always",
	})
	if err == nil {
		t.Fatal("expected sandbox build failure")
	}
	if !strings.Contains(err.Error(), "prepare sandbox") {
		t.Fatalf("error = %v", err)
	}
	if _, statErr := os.Stat(outFile); !os.IsNotExist(statErr) {
		t.Fatalf("agent started despite build failure; stat err=%v", statErr)
	}
}

func TestLaunch_SandboxBuildNeverFailsBeforeAgentStart(t *testing.T) {
	dir := t.TempDir()
	baseDir := filepath.Join(dir, "images", "sandbox-base")
	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(baseDir, "Containerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	docker := filepath.Join(binDir, "docker")
	if err := os.WriteFile(docker, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	_, err := launch.Launch(context.Background(), launch.LaunchConfig{
		Agent:              scriptAgent{script: "claude"},
		Dir:                dir,
		SandboxRuntime:     "docker",
		SandboxBuildPolicy: "never",
	})
	if err == nil {
		t.Fatal("expected missing image failure")
	}
	if !strings.Contains(err.Error(), "sandbox build policy is never") {
		t.Fatalf("error = %v", err)
	}
}

func TestLaunch_SandboxValidationFailureFailsBeforeAgentRun(t *testing.T) {
	dir := t.TempDir()
	devcontainerDir := filepath.Join(dir, ".devcontainer")
	if err := os.MkdirAll(devcontainerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(devcontainerDir, "devcontainer.json"), []byte(`{"customizations":{"aileron":{"image":"ghcr.io/acme/agent:latest"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	binDir := t.TempDir()
	argsFile := filepath.Join(dir, "docker-args.txt")
	docker := filepath.Join(binDir, "docker")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + argsFile + "\necho 'agent command not found in sandbox image: codex' >&2\nexit 127\n"
	if err := os.WriteFile(docker, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	_, err := launch.Launch(context.Background(), launch.LaunchConfig{
		Agent:          scriptAgent{script: "codex"},
		Dir:            dir,
		SandboxRuntime: "docker",
	})
	if err == nil {
		t.Fatal("expected sandbox validation failure")
	}
	msg := err.Error()
	for _, want := range []string{
		"sandbox image ghcr.io/acme/agent:latest is not launchable for test-script",
		"agent command not found in sandbox image: codex",
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error %q does not contain %q", msg, want)
		}
	}
	data, readErr := os.ReadFile(argsFile)
	if readErr != nil {
		t.Fatalf("read docker args: %v", readErr)
	}
	if strings.Contains(string(data), "ghcr.io/acme/agent:latest\ncodex\n") {
		t.Fatalf("agent run happened despite validation failure:\n%s", data)
	}
}

func TestLaunch_SandboxInvalidRuntimeFails(t *testing.T) {
	_, err := launch.Launch(context.Background(), launch.LaunchConfig{
		Agent:          scriptAgent{script: "/bin/sh"},
		SandboxRuntime: "containerd",
	})
	if err == nil {
		t.Fatal("expected invalid sandbox runtime error")
	}
	if !strings.Contains(err.Error(), "unsupported sandbox runtime") {
		t.Fatalf("error = %v", err)
	}
}

func TestLaunch_AileronMCPMissing_FailsLoud(t *testing.T) {
	// Strip the package-wide fake aileron-mcp from PATH and point HOME
	// at a tempdir so resolveSibling has no chance of finding one.
	// Launch should fail with a structured error before session
	// registration happens.
	t.Setenv("PATH", "")
	t.Setenv("HOME", t.TempDir())

	_, err := launch.Launch(context.Background(), launch.LaunchConfig{
		Agent: scriptAgent{script: "/bin/sh"},
	})
	if err == nil {
		t.Fatal("expected error when aileron-mcp is unresolvable, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "aileron-mcp") {
		t.Errorf("error message does not mention aileron-mcp: %q", msg)
	}
	// The message should point the user at the lookup paths it tried so
	// they can debug installation issues without diving into source.
	if !strings.Contains(msg, "PATH") {
		t.Errorf("error message does not mention PATH: %q", msg)
	}
}

func TestSessionLogPath_UnderCWD(t *testing.T) {
	dir := t.TempDir()
	got := launch.SessionLogPath(dir)
	want := filepath.Join(dir, ".aileron", "session.log")
	if got != want {
		t.Errorf("SessionLogPath = %q, want %q", got, want)
	}
}

func TestSessionLogPath_EmptyDirFallsBackToCWD(t *testing.T) {
	got := launch.SessionLogPath("")
	cwd, _ := os.Getwd()
	want := filepath.Join(cwd, ".aileron", "session.log")
	if got != want {
		t.Errorf("SessionLogPath(\"\") = %q, want %q", got, want)
	}
}

// --- Sandbox MCP wiring (U2 / #953) ---

// setupSandboxDockerCaptureLaunch is the shared scaffolding for the
// sandbox-MCP wiring tests: writes a BYO-image devcontainer.json,
// installs a fake docker that captures argv, and launches the supplied
// agent under SandboxRuntime=docker. Returns the resulting docker
// argv as a single string for substring assertions.
func setupSandboxDockerCaptureLaunch(t *testing.T, dir string, agent launch.Agent) string {
	t.Helper()
	devcontainerDir := filepath.Join(dir, ".devcontainer")
	if err := os.MkdirAll(devcontainerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(devcontainerDir, "devcontainer.json"), []byte(`{"customizations":{"aileron":{"image":"ghcr.io/acme/agent:latest"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	argsFile := filepath.Join(dir, "docker-args.txt")
	docker := filepath.Join(binDir, "docker")
	if err := os.WriteFile(docker, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > "+argsFile+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Prepend the fake docker dir to PATH while keeping the rest (TestMain
	// stamped the fake aileron-mcp dir into PATH already; we keep both).
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	_, err := launch.Launch(context.Background(), launch.LaunchConfig{
		Agent:          agent,
		Dir:            dir,
		SandboxRuntime: "docker",
	})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	data, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("read docker args: %v", err)
	}
	return string(data)
}

// TestLaunch_Sandbox_MountsAileronMCPBinary verifies the host-built
// aileron-mcp binary is bind-mounted into the container at
// /usr/local/bin/aileron-mcp:ro. Covers R1.
func TestLaunch_Sandbox_MountsAileronMCPBinary(t *testing.T) {
	dir := t.TempDir()
	args := setupSandboxDockerCaptureLaunch(t, dir, claudeTestAgent{})

	// The fake aileron-mcp is planted on PATH by TestMain at <tmp>/aileron-mcp;
	// docker volume looks like SOURCE:/usr/local/bin/aileron-mcp:ro.
	if !regexp.MustCompile(`--volume\n[^\n]*/aileron-mcp:/usr/local/bin/aileron-mcp:ro\n`).MatchString(args) {
		t.Errorf("expected aileron-mcp read-only bind mount; got:\n%s", args)
	}
}

// TestLaunch_Sandbox_Claude_ConfigureMCPArgs covers R2 / R4 for Claude
// Code: --mcp-config arrives in the agent argv with the container-side
// binary path and the rewritten daemon URL.
func TestLaunch_Sandbox_Claude_ConfigureMCPArgs(t *testing.T) {
	dir := t.TempDir()
	args := setupSandboxDockerCaptureLaunch(t, dir, claudeTestAgent{})

	wants := []string{
		"--mcp-config\n",
		`"command":"/usr/local/bin/aileron-mcp"`,
		`"AILERON_URL":"http://host.docker.internal:`,
		`"AILERON_SESSION_ID":"01HK0000000000000000000FAK"`,
	}
	for _, want := range wants {
		if !strings.Contains(args, want) {
			t.Errorf("missing %q in docker args:\n%s", want, args)
		}
	}
}

// TestLaunch_Sandbox_Pi_ConfigureMCPArgs is the same shape as Claude
// (Pi shares the --mcp-config mechanism). Covers R4 for Pi.
func TestLaunch_Sandbox_Pi_ConfigureMCPArgs(t *testing.T) {
	dir := t.TempDir()
	args := setupSandboxDockerCaptureLaunch(t, dir, piTestAgent{})

	for _, want := range []string{
		"--mcp-config\n",
		`"command":"/usr/local/bin/aileron-mcp"`,
		`"AILERON_URL":"http://host.docker.internal:`,
	} {
		if !strings.Contains(args, want) {
			t.Errorf("missing %q in docker args:\n%s", want, args)
		}
	}
}

// TestLaunch_Sandbox_Goose_ConfigureMCPArgs covers R4 for Goose:
// --with-extension carries env + the container-side aileron-mcp path.
func TestLaunch_Sandbox_Goose_ConfigureMCPArgs(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // isolate goose config writes
	dir := t.TempDir()
	args := setupSandboxDockerCaptureLaunch(t, dir, gooseTestAgent{})

	for _, want := range []string{
		"--with-extension\n",
		"AILERON_URL=http://host.docker.internal:",
		"/usr/local/bin/aileron-mcp",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("missing %q in docker args:\n%s", want, args)
		}
	}
}

// TestLaunch_Sandbox_OpenCode_WritesWorkspaceConfig covers R4 for
// OpenCode: opencode.json lands in the launch dir (= workspace bind
// mount) with the aileron MCP server entry pointing at the
// container-side binary path.
func TestLaunch_Sandbox_OpenCode_WritesWorkspaceConfig(t *testing.T) {
	dir := t.TempDir()
	_ = setupSandboxDockerCaptureLaunch(t, dir, openCodeTestAgent{})

	data, err := os.ReadFile(filepath.Join(dir, "opencode.json"))
	if err != nil {
		t.Fatalf("read opencode.json: %v", err)
	}
	content := string(data)
	for _, want := range []string{
		`"aileron"`,
		`"/usr/local/bin/aileron-mcp"`,
		"host.docker.internal",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("missing %q in opencode.json:\n%s", want, content)
		}
	}
}

// TestLaunch_Sandbox_AileronMCPMissing_FailsLoud covers R1's hard-error
// path: a sandbox launch with docker available but no aileron-mcp on
// the host fails with the same shape the host-launch path produces.
// HOME is repointed away from the test runner's real home so a
// developer-installed aileron-mcp doesn't accidentally satisfy
// resolveMCPBinary's sibling lookup.
func TestLaunch_Sandbox_AileronMCPMissing_FailsLoud(t *testing.T) {
	// Install a fake docker so the runtime resolution succeeds, but no
	// aileron-mcp in the same dir — and the empty PATH prevents the
	// fallback PATH lookup from finding TestMain's planted fake.
	binDir := t.TempDir()
	docker := filepath.Join(binDir, "docker")
	if err := os.WriteFile(docker, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)
	t.Setenv("HOME", t.TempDir())

	dir := t.TempDir()
	devcontainerDir := filepath.Join(dir, ".devcontainer")
	if err := os.MkdirAll(devcontainerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(devcontainerDir, "devcontainer.json"), []byte(`{"customizations":{"aileron":{"image":"ghcr.io/acme/agent:latest"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := launch.Launch(context.Background(), launch.LaunchConfig{
		Agent:          claudeTestAgent{},
		Dir:            dir,
		SandboxRuntime: "docker",
	})
	if err == nil {
		t.Fatal("expected error when aileron-mcp is unresolvable under sandbox, got nil")
	}
	if !strings.Contains(err.Error(), "aileron-mcp") {
		t.Errorf("error message does not mention aileron-mcp: %q", err.Error())
	}
}

// --- Real-agent stubs (avoid pulling internal/launch/agents into a
// _test package by reimplementing the surface; the actual ConfigureMCP
// behavior is covered by tests in internal/launch/agents/.) ---

type claudeTestAgent struct{}

func (claudeTestAgent) Name() string           { return "claude" }
func (claudeTestAgent) BinaryNames() []string  { return []string{"claude"} }
func (claudeTestAgent) Args() []string         { return nil }
func (claudeTestAgent) Env() map[string]string { return nil }
func (claudeTestAgent) LLMEndpointEnv() string { return "" }
func (claudeTestAgent) ConfigureMCP(mcpBin string, mcpEnv map[string]string, _ string, _ launch.Mode) ([]string, []launch.MCPMount, error) {
	envJSON := encodeJSON(t1, mcpEnv)
	return []string{"--mcp-config", `{"mcpServers":{"aileron":{"command":"` + mcpBin + `","env":` + envJSON + `}}}`}, nil, nil
}
func (claudeTestAgent) AuthSpec() launch.AuthSpec { return launch.AuthSpec{} }

type piTestAgent struct{}

func (piTestAgent) Name() string           { return "pi" }
func (piTestAgent) BinaryNames() []string  { return []string{"pi"} }
func (piTestAgent) Args() []string         { return nil }
func (piTestAgent) Env() map[string]string { return nil }
func (piTestAgent) LLMEndpointEnv() string { return "" }
func (piTestAgent) ConfigureMCP(mcpBin string, mcpEnv map[string]string, _ string, _ launch.Mode) ([]string, []launch.MCPMount, error) {
	envJSON := encodeJSON(t1, mcpEnv)
	return []string{"--mcp-config", `{"mcpServers":{"aileron":{"command":"` + mcpBin + `","env":` + envJSON + `}}}`}, nil, nil
}
func (piTestAgent) AuthSpec() launch.AuthSpec { return launch.AuthSpec{} }

type gooseTestAgent struct{}

func (gooseTestAgent) Name() string           { return "goose" }
func (gooseTestAgent) BinaryNames() []string  { return []string{"goose"} }
func (gooseTestAgent) Args() []string         { return []string{"session"} }
func (gooseTestAgent) Env() map[string]string { return nil }
func (gooseTestAgent) LLMEndpointEnv() string { return "" }
func (gooseTestAgent) ConfigureMCP(mcpBin string, mcpEnv map[string]string, _ string, _ launch.Mode) ([]string, []launch.MCPMount, error) {
	// Mirror agents/goose.go's "ENV=val ENV=val cmd" shape, deterministic order.
	keys := []string{}
	for k := range mcpEnv {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := []string{}
	for _, k := range keys {
		parts = append(parts, k+"="+mcpEnv[k])
	}
	parts = append(parts, mcpBin)
	return []string{"--with-extension", strings.Join(parts, " ")}, nil, nil
}
func (gooseTestAgent) AuthSpec() launch.AuthSpec { return launch.AuthSpec{} }

type openCodeTestAgent struct{}

func (openCodeTestAgent) Name() string           { return "opencode" }
func (openCodeTestAgent) BinaryNames() []string  { return []string{"opencode"} }
func (openCodeTestAgent) Args() []string         { return nil }
func (openCodeTestAgent) Env() map[string]string { return nil }
func (openCodeTestAgent) LLMEndpointEnv() string { return "" }
func (openCodeTestAgent) ConfigureMCP(mcpBin string, mcpEnv map[string]string, dir string, _ launch.Mode) ([]string, []launch.MCPMount, error) {
	if dir == "" {
		cwd, _ := os.Getwd()
		dir = cwd
	}
	envJSON := encodeJSON(t1, mcpEnv)
	body := `{"mcp":{"aileron":{"type":"local","command":["` + mcpBin + `"],"enabled":true,"environment":` + envJSON + `}}}` + "\n"
	return nil, nil, os.WriteFile(filepath.Join(dir, "opencode.json"), []byte(body), 0o644)
}
func (openCodeTestAgent) AuthSpec() launch.AuthSpec { return launch.AuthSpec{} }

// t1 is a tiny *testing.T-shaped stand-in used by the test stubs above
// to forward fatal failures. The real agent definitions use t.Fatal
// for marshal errors, which are unreachable for our env maps; the
// indirection here just keeps the stub body terse.
var t1 = &struct{ Fatal func(...any) }{Fatal: func(args ...any) { panic(args) }}

// encodeJSON renders a stable JSON object literal for {string: string}
// maps with sorted keys, so the test stubs produce deterministic argv
// content without depending on encoding/json's map ordering quirks.
func encodeJSON(_ any, m map[string]string) string {
	keys := []string{}
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := []string{}
	for _, k := range keys {
		parts = append(parts, `"`+k+`":"`+m[k]+`"`)
	}
	return "{" + strings.Join(parts, ",") + "}"
}

const sandboxDiscoveryActionManifest = `+++
name = "send-email"
version = "1.0.0"
source = "hub://aileron/send-email@1.0.0"

[[requires.connectors]]
name = "github://ALRubinger/aileron-connector-google"
version = "1.0.0"
hash = "sha256:abc123"
capabilities = ["send"]

[match]
intent = "send email"

[[inputs]]
name = "to"
type = "string"
description = "recipient"

[[execute]]
id = "send"
connector = "github://ALRubinger/aileron-connector-google"
op = "send_email"
+++

# Send Email
`
