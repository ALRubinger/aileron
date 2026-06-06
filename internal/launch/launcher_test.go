package launch_test

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/launch"
	sandboxcomposition "github.com/ALRubinger/aileron/internal/sandbox/composition"
	sandboxcontainer "github.com/ALRubinger/aileron/internal/sandbox/container"
)

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
func (a scriptAgent) ConfigureMCP(string, map[string]string, string) ([]string, error) {
	return a.mcpArgs, nil
}

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
		"--env\nAILERON_TOOLS_FILE=/etc/aileron/tools.txt\n",
		"--env\nAILERON_SHIMS_DIR=/usr/local/bin\n",
		"--env\nAILERON_URL=http://host.docker.internal:",
		"ghcr.io/acme/agent:latest\ncodex\n--ask-for-approval\nnever\n",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("expected %q in docker args:\n%s", want, args)
		}
	}
	if !regexp.MustCompile(`--env\nAILERON_API_URL=http://host\.docker\.internal:[0-9]+/v1\n`).MatchString(args) {
		t.Errorf("expected container AILERON_API_URL /v1 env in docker args:\n%s", args)
	}
	for _, unwanted := range []string{"HTTPS_PROXY=", "HTTP_PROXY=", "AILERON_SANDBOX_PROXY_MODE="} {
		if strings.Contains(args, unwanted) {
			t.Errorf("did not expect proxy bootstrap env %q by default:\n%s", unwanted, args)
		}
	}
}

func TestLaunch_SandboxProxyBootstrapEnvAndCAMount(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("AILERON_TOKEN", "daemon-token")
	t.Setenv("AILERON_SANDBOX_PROXY_BOOTSTRAP", "1")

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
		"host.containers.internal",
		"aileron-run-with-proxy-ca\ncodex\n",
	} {
		if !strings.Contains(args, want) {
			t.Fatalf("run call missing %q:\n%s", want, args)
		}
	}
}

func TestLaunch_SandboxShellMediationOptInValidatesAndPassesEnv(t *testing.T) {
	t.Setenv("AILERON_SANDBOX_SHELL_MEDIATION", "1")

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
	calls := strings.Split(strings.TrimPrefix(string(data), "---\n"), "\n---\n")
	if len(calls) != 2 {
		t.Fatalf("docker calls = %d, want validate and run:\n%s", len(calls), data)
	}
	validateCall, runCall := calls[0], calls[1]
	validateArgs := strings.Split(strings.TrimSpace(validateCall), "\n")
	if len(validateArgs) < 5 {
		t.Fatalf("validation call too short:\n%s", validateCall)
	}
	validateArgs = validateArgs[len(validateArgs)-5:]
	if validateArgs[0] != "aileron-validate" || validateArgs[1] != "codex" || validateArgs[4] != "1" {
		t.Fatalf("validation call did not require shell mediation helper:\n%s", validateCall)
	}
	for _, want := range []string{
		"--env\nAILERON_SANDBOX_SHELL_MEDIATION=1\n",
		"--env\nAILERON_SHELL_RCFILE=/etc/aileron/shell/aileron-bashrc\n",
		"--env\nAILERON_REAL_SHELL=/bin/bash\n",
		// Routing is activated for the live session: the trap installer for
		// non-interactive children and the wrapper for $SHELL-consulting agents.
		"--env\nBASH_ENV=/etc/aileron/shell/aileron-bashrc\n",
		"--env\nSHELL=/usr/local/bin/bash\n",
		"ghcr.io/acme/agent:latest\ncodex\n",
	} {
		if !strings.Contains(runCall, want) {
			t.Fatalf("run call missing %q:\n%s", want, runCall)
		}
	}
	// R8: routing is shell-layer, so the agent command is still run directly,
	// not wrapped in the mediator.
	if strings.Contains(runCall, "aileron-shell-mediator\ncodex\n") {
		t.Fatalf("launch should not wrap the agent command:\n%s", runCall)
	}
}

func TestLaunch_SandboxShellMediationOffOmitsRoutingEnv(t *testing.T) {
	// Opt-in unset: non-mediated launches are byte-for-byte unchanged (R7). No
	// BASH_ENV, no SHELL override, no mediation env, no shell-mediation
	// requirement on the validation call.
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
	runCall := string(data)
	for _, unwanted := range []string{
		"BASH_ENV=",
		"SHELL=/usr/local/bin/bash",
		"AILERON_SANDBOX_SHELL_MEDIATION=1",
	} {
		if strings.Contains(runCall, unwanted) {
			t.Errorf("non-mediated launch leaked %q:\n%s", unwanted, runCall)
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
	calls := strings.Split(strings.TrimPrefix(string(data), "---\n"), "\n---\n")
	if len(calls) != 2 {
		t.Fatalf("docker calls = %d, want validate and run:\n%s", len(calls), data)
	}
	validateCall, runCall := calls[0], calls[1]
	for _, call := range []struct {
		name string
		args string
	}{
		{name: "validation", args: validateCall},
		{name: "run", args: runCall},
	} {
		for _, want := range []string{
			":/etc/aileron/tools.txt:ro\n",
			":/usr/local/bin/google:ro\n",
		} {
			if !strings.Contains(call.args, want) {
				t.Fatalf("%s call missing %q:\n%s", call.name, want, call.args)
			}
		}
	}
	if !strings.Contains(validateCall, "/bin/sh\n-c\n") {
		t.Fatalf("validation call did not run contract probe:\n%s", validateCall)
	}
	if !strings.HasSuffix(strings.TrimSpace(validateCall), "\ncodex\n1\n0\n0") {
		t.Fatalf("validation call did not enable shim HTTP-client validation:\n%s", validateCall)
	}
	if !regexp.MustCompile(`--env\nAILERON_API_URL=http://host\.docker\.internal:[0-9]+/v1\n`).MatchString(runCall) {
		t.Fatalf("run call missing container AILERON_API_URL:\n%s", runCall)
	}
	for _, want := range []string{
		"--env\nAILERON_TOOLS_FILE=/etc/aileron/tools.txt\n",
		"--env\nAILERON_SHIMS_DIR=/usr/local/bin\n",
	} {
		if !strings.Contains(runCall, want) {
			t.Fatalf("run call missing discovery hint env %q:\n%s", want, runCall)
		}
	}
	if !strings.Contains(runCall, "ghcr.io/acme/agent:latest\ncodex\n") {
		t.Fatalf("run call did not execute agent image command:\n%s", runCall)
	}
}

func TestLaunch_SandboxDiscoveryMountsConnectorSpecShims(t *testing.T) {
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
	calls := strings.Split(strings.TrimPrefix(string(data), "---\n"), "\n---\n")
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
		for _, want := range []string{
			":/etc/aileron/tools.txt:ro\n",
			":/usr/local/bin/google:ro\n",
		} {
			if !strings.Contains(call.args, want) {
				t.Fatalf("%s call missing %q:\n%s", call.name, want, call.args)
			}
		}
	}
}

func TestLaunch_SandboxScaffoldBuildsAndRunsWithDiscoveryMounts(t *testing.T) {
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
	docker := filepath.Join(binDir, "docker")
	script := "#!/bin/sh\nprintf '%s\\n' '---' >> " + argsFile + "\nprintf '%s\\n' \"$@\" >> " + argsFile + "\n"
	if err := os.WriteFile(docker, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
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
	data, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("read docker args: %v", err)
	}
	calls := strings.Split(strings.TrimPrefix(string(data), "---\n"), "\n---\n")
	if len(calls) != 3 {
		t.Fatalf("docker calls = %d, want build, validate, and run:\n%s", len(calls), data)
	}
	buildCall, validateCall, runCall := calls[0], calls[1], calls[2]
	image := sandboxcontainer.ProjectImageTag(dir)
	dockerfile := filepath.Join(dir, ".devcontainer", "Dockerfile")
	for _, want := range []string{
		"build\n-t\n" + image + "\n",
		"-f\n" + dockerfile + "\n",
		"\n" + dir,
	} {
		if !strings.Contains(buildCall, want) {
			t.Fatalf("build call missing %q:\n%s", want, buildCall)
		}
	}
	for _, call := range []struct {
		name string
		args string
	}{
		{name: "validation", args: validateCall},
		{name: "run", args: runCall},
	} {
		for _, want := range []string{
			":/etc/aileron/tools.txt:ro\n",
			":/usr/local/bin/google:ro\n",
			image + "\n",
		} {
			if !strings.Contains(call.args, want) {
				t.Fatalf("%s call missing %q:\n%s", call.name, want, call.args)
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
		"--env\nAILERON_TOOLS_FILE=/etc/aileron/tools.txt\n",
		"--env\nAILERON_SHIMS_DIR=/usr/local/bin\n",
		"codex\n--ask-for-approval\nnever\n",
	} {
		if !strings.Contains(runCall, want) {
			t.Fatalf("run call missing %q:\n%s", want, runCall)
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
		"build\n-t\naileron/sandbox-base:latest\n",
		"run\n--rm\n-i\n",
		"--env\nAILERON_SANDBOX_IMAGE=aileron/sandbox-base:latest\n",
		"--env\nAILERON_SANDBOX_TIER=base\n",
		"--env\nAILERON_SANDBOX_RUNTIME=docker\n",
		"aileron/sandbox-base:latest\nclaude\n--dangerously-skip-permissions\n",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("expected %q in docker args:\n%s", want, args)
		}
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
