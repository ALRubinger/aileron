package agents_test

import (
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/launch/agents"
	"github.com/ALRubinger/aileron/internal/vault"
)

// Goose AuthSpec contract (#982):
//
//   - AuthSpec ships one EnvBinding (provider API key) and no
//     FileBindings or StaticFiles. Goose authenticates with a static
//     provider API key, so there is nothing to Capture or refresh.
//   - VaultPath is the canonical agents/goose/oauth.
//   - Render maps a non-empty key to the provider env var, trimming
//     surrounding whitespace.
//   - Render rejects an empty/whitespace-only Value.

func TestGoose_AuthSpec_Shape(t *testing.T) {
	spec := agents.Goose{}.AuthSpec()
	if len(spec.EnvBindings) != 1 {
		t.Fatalf("EnvBindings = %d, want 1", len(spec.EnvBindings))
	}
	if len(spec.FileBindings) != 0 {
		t.Errorf("FileBindings = %d, want 0 (env-key agent has no file)", len(spec.FileBindings))
	}
	if len(spec.StaticFiles) != 0 {
		t.Errorf("StaticFiles = %d, want 0", len(spec.StaticFiles))
	}

	eb := spec.EnvBindings[0]
	if eb.VaultPath != "agents/goose/oauth" {
		t.Errorf("VaultPath = %q, want agents/goose/oauth", eb.VaultPath)
	}
	if eb.Required {
		t.Errorf("Required = true; empty vault must fall through to Goose's own key resolution")
	}
	if eb.Render == nil {
		t.Fatal("Render must be set")
	}
}

func TestGoose_Render_MapsKeyToProviderEnv(t *testing.T) {
	spec := agents.Goose{}.AuthSpec()
	got, err := spec.EnvBindings[0].Render(vault.Secret{Value: []byte("sk-ant-test")})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got["ANTHROPIC_API_KEY"] != "sk-ant-test" {
		t.Errorf("Render = %v, want ANTHROPIC_API_KEY=sk-ant-test", got)
	}
	if len(got) != 1 {
		t.Errorf("Render produced %d env vars, want 1", len(got))
	}
}

func TestGoose_Render_TrimsWhitespace(t *testing.T) {
	// A key copied from another machine can carry a trailing newline;
	// Goose would reject the key with the newline embedded.
	spec := agents.Goose{}.AuthSpec()
	got, err := spec.EnvBindings[0].Render(vault.Secret{Value: []byte("  sk-ant-test\n")})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got["ANTHROPIC_API_KEY"] != "sk-ant-test" {
		t.Errorf("Render = %q, want trimmed sk-ant-test", got["ANTHROPIC_API_KEY"])
	}
}

func TestGoose_Render_RejectsEmptyValue(t *testing.T) {
	spec := agents.Goose{}.AuthSpec()
	_, err := spec.EnvBindings[0].Render(vault.Secret{Value: nil})
	if err == nil {
		t.Fatal("expected error for empty Value")
	}
	if !strings.Contains(err.Error(), "empty API key") {
		t.Errorf("err = %v, want mention of empty API key", err)
	}
}

func TestGoose_Render_RejectsWhitespaceOnlyValue(t *testing.T) {
	spec := agents.Goose{}.AuthSpec()
	_, err := spec.EnvBindings[0].Render(vault.Secret{Value: []byte("   \n\t")})
	if err == nil {
		t.Fatal("expected error for whitespace-only Value")
	}
}
