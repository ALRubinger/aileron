package launch

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sandboxcontainer "github.com/ALRubinger/aileron/internal/sandbox/container"
)

type emptyBinaryAgent struct{}

func (emptyBinaryAgent) Name() string           { return "empty" }
func (emptyBinaryAgent) BinaryNames() []string  { return nil }
func (emptyBinaryAgent) Args() []string         { return nil }
func (emptyBinaryAgent) Env() map[string]string { return nil }
func (emptyBinaryAgent) LLMEndpointEnv() string { return "" }
func (emptyBinaryAgent) ConfigureMCP(string, map[string]string, string) ([]string, error) {
	return nil, nil
}

type namedBinaryAgent struct{ name string }

func (a namedBinaryAgent) Name() string           { return "named" }
func (a namedBinaryAgent) BinaryNames() []string  { return []string{a.name} }
func (a namedBinaryAgent) Args() []string         { return nil }
func (a namedBinaryAgent) Env() map[string]string { return nil }
func (a namedBinaryAgent) LLMEndpointEnv() string { return "" }
func (a namedBinaryAgent) ConfigureMCP(string, map[string]string, string) ([]string, error) {
	return nil, nil
}

func TestFirstAgentBinaryHandlesEmptyAgent(t *testing.T) {
	if got := firstAgentBinary(emptyBinaryAgent{}); got != "" {
		t.Fatalf("firstAgentBinary = %q, want empty", got)
	}
	if got := firstAgentBinary(namedBinaryAgent{name: "codex"}); got != "codex" {
		t.Fatalf("firstAgentBinary = %q, want codex", got)
	}
}

func TestContainerURLForRuntimeRewritesLoopbackAliases(t *testing.T) {
	tests := []struct {
		name    string
		rawURL  string
		runtime string
		want    string
	}{
		{
			name:    "docker localhost",
			rawURL:  "http://localhost:48123",
			runtime: "docker",
			want:    "http://host.docker.internal:48123",
		},
		{
			name:    "podman localhost",
			rawURL:  "http://127.0.0.1:48123",
			runtime: "podman",
			want:    "http://host.containers.internal:48123",
		},
		{
			name:    "non-loopback unchanged",
			rawURL:  "http://10.0.0.5:48123",
			runtime: "docker",
			want:    "http://10.0.0.5:48123",
		},
		{
			name:    "localhost without port",
			rawURL:  "http://localhost",
			runtime: "docker",
			want:    "http://host.docker.internal",
		},
		{
			name:    "invalid unchanged",
			rawURL:  "://not-a-url",
			runtime: "docker",
			want:    "://not-a-url",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := containerURLForRuntime(tt.rawURL, tt.runtime); got != tt.want {
				t.Fatalf("containerURLForRuntime(%q, %q) = %q, want %q", tt.rawURL, tt.runtime, got, tt.want)
			}
		})
	}
}

func TestLaunchSandboxRejectsAgentWithoutBinary(t *testing.T) {
	_, err := launchSandbox(nil, SandboxLaunchPlan{Runtime: "docker", Image: "image:test"}, LaunchConfig{
		Agent: emptyBinaryAgent{},
	}, nil)
	if err == nil {
		t.Fatal("expected missing container command error")
	}
}

func TestValidateSandboxRejectsAgentWithoutBinary(t *testing.T) {
	err := validateSandbox(nil, SandboxLaunchPlan{Runtime: "docker", Image: "image:test"}, LaunchConfig{
		Agent: emptyBinaryAgent{},
	})
	if err == nil {
		t.Fatal("expected missing container command error")
	}
}

func TestValidateSandboxPassesRuntimeMounts(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	actionsDir := filepath.Join(home, ".aileron", "actions")
	if err := os.MkdirAll(actionsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(actionsDir, "send-email.md"), []byte(validActionManifest), 0o644); err != nil {
		t.Fatal(err)
	}

	orig := validateSandboxImageForLaunch
	t.Cleanup(func() { validateSandboxImageForLaunch = orig })
	var got []sandboxcontainer.Volume
	validateSandboxImageForLaunch = func(_ context.Context, _ SandboxLaunchPlan, _ LaunchConfig, mounts []sandboxcontainer.Volume, commandName string) error {
		got = append(got, mounts...)
		if commandName != "codex" {
			t.Fatalf("commandName = %q, want codex", commandName)
		}
		return nil
	}

	err := validateSandbox(context.Background(), SandboxLaunchPlan{Runtime: "docker", Image: "image:test"}, LaunchConfig{
		Agent: namedBinaryAgent{name: "codex"},
		Dir:   t.TempDir(),
	})
	if err != nil {
		t.Fatalf("validateSandbox: %v", err)
	}
	if !hasMountTarget(got, "/opt/aileron/manifests/actions") || !hasMountTarget(got, "/etc/aileron/tools.txt") {
		t.Fatalf("validateSandbox mounts = %#v, want actions and tools.txt mounts", got)
	}
}

func TestNormalizeLaunchBuildPolicy(t *testing.T) {
	tests := map[string]string{
		"":       "auto",
		" auto ": "auto",
		"always": "always",
		"never":  "never",
	}
	for input, want := range tests {
		got, err := normalizeLaunchBuildPolicy(input)
		if err != nil {
			t.Fatalf("normalizeLaunchBuildPolicy(%q): %v", input, err)
		}
		if got != want {
			t.Fatalf("normalizeLaunchBuildPolicy(%q) = %q, want %q", input, got, want)
		}
	}
	if _, err := normalizeLaunchBuildPolicy("sometimes"); err == nil {
		t.Fatal("expected unsupported build policy error")
	}
}

func TestSandboxRuntimeMountsReportsToolsTempDirError(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	actionsDir := filepath.Join(home, ".aileron", "actions")
	if err := os.MkdirAll(actionsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(actionsDir, "send-email.md"), []byte(validActionManifest), 0o644); err != nil {
		t.Fatal(err)
	}
	tmpFile := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(tmpFile, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", tmpFile)

	_, cleanup, err := sandboxRuntimeMounts()
	defer cleanup()
	if err == nil {
		t.Fatal("expected tempdir error")
	}
	if !strings.Contains(err.Error(), "create sandbox discovery tempdir") {
		t.Fatalf("error = %v, want discovery tempdir context", err)
	}
}

func TestSandboxRuntimeMountsSkipsMissingStores(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	got, cleanup, err := sandboxRuntimeMounts()
	defer cleanup()
	if err != nil {
		t.Fatalf("sandboxRuntimeMounts: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("sandboxRuntimeMounts = %#v, want no mounts", got)
	}
}

func TestSandboxRuntimeMountsIncludesExistingStores(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	actionsDir := filepath.Join(home, ".aileron", "actions")
	connectorsDir := filepath.Join(home, ".aileron", "store", "connectors")
	for _, dir := range []string{actionsDir, connectorsDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	mounts, cleanup, err := sandboxRuntimeMounts()
	defer cleanup()
	if err != nil {
		t.Fatalf("sandboxRuntimeMounts: %v", err)
	}
	if len(mounts) != 2 {
		t.Fatalf("mounts = %#v, want two mounts", mounts)
	}
	if mounts[0].Source != actionsDir || mounts[0].Target != "/opt/aileron/manifests/actions" || !mounts[0].ReadOnly {
		t.Fatalf("actions mount = %#v", mounts[0])
	}
	if mounts[1].Source != connectorsDir || mounts[1].Target != "/opt/aileron/manifests/connectors" || !mounts[1].ReadOnly {
		t.Fatalf("connectors mount = %#v", mounts[1])
	}
}

func TestSandboxRuntimeMountsIncludesGeneratedToolsText(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	actionsDir := filepath.Join(home, ".aileron", "actions")
	if err := os.MkdirAll(actionsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	actionPath := filepath.Join(actionsDir, "send-email.md")
	if err := os.WriteFile(actionPath, []byte(validActionManifest), 0o644); err != nil {
		t.Fatal(err)
	}

	mounts, cleanup, err := sandboxRuntimeMounts()
	defer cleanup()
	if err != nil {
		t.Fatalf("sandboxRuntimeMounts: %v", err)
	}
	var toolsMount *sandboxcontainer.Volume
	for i := range mounts {
		if mounts[i].Target == "/etc/aileron/tools.txt" {
			toolsMount = &mounts[i]
			break
		}
	}
	if toolsMount == nil {
		t.Fatalf("tools.txt mount missing from %#v", mounts)
	}
	if !toolsMount.ReadOnly {
		t.Fatalf("tools.txt mount should be read-only: %#v", toolsMount)
	}
	data, err := os.ReadFile(toolsMount.Source)
	if err != nil {
		t.Fatalf("read tools.txt: %v", err)
	}
	if !strings.Contains(string(data), "google\tgithub://ALRubinger/aileron-connector-google") {
		t.Fatalf("tools.txt did not include google connector:\n%s", data)
	}
}

func hasMountTarget(mounts []sandboxcontainer.Volume, target string) bool {
	for _, mount := range mounts {
		if mount.Target == target {
			return true
		}
	}
	return false
}

const validActionManifest = `+++
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
