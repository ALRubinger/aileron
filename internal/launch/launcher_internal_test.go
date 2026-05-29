package launch

import "testing"

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
