package agents_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/launch/agents"
	"github.com/ALRubinger/aileron/internal/vault"
)

// OpenCode AuthSpec contract (#982):
//
//   - AuthSpec ships one FileBinding (auth.json) and no EnvBindings or
//     StaticFiles.
//   - VaultPath is the canonical agents/opencode/oauth; ContainerPath
//     is the XDG data-dir auth.json; Mode 0600; not Required;
//     MountAsFile false.
//   - Render is byte-identity over a valid envelope and rejects an
//     empty Value.
//   - Capture is byte-identity, stamps Metadata.Type, and rejects a
//     malformed envelope.

func TestOpenCode_AuthSpec_Shape(t *testing.T) {
	spec := agents.OpenCode{}.AuthSpec()
	if len(spec.FileBindings) != 1 {
		t.Fatalf("FileBindings = %d, want 1", len(spec.FileBindings))
	}
	if len(spec.EnvBindings) != 0 {
		t.Errorf("EnvBindings = %d, want 0", len(spec.EnvBindings))
	}
	if len(spec.StaticFiles) != 0 {
		t.Errorf("StaticFiles = %d, want 0", len(spec.StaticFiles))
	}

	fb := spec.FileBindings[0]
	if fb.VaultPath != "agents/opencode/oauth" {
		t.Errorf("VaultPath = %q, want agents/opencode/oauth", fb.VaultPath)
	}
	if fb.ContainerPath != "/home/agent/.local/share/opencode/auth.json" {
		t.Errorf("ContainerPath = %q, want /home/agent/.local/share/opencode/auth.json", fb.ContainerPath)
	}
	if fb.Mode != 0o600 {
		t.Errorf("Mode = %v, want 0600", fb.Mode)
	}
	if fb.Required {
		t.Errorf("Required = true; empty vault must trigger in-container login fallthrough")
	}
	if fb.MountAsFile {
		t.Errorf("MountAsFile = true; OpenCode has no sibling mount to coexist with")
	}
	if fb.Render == nil {
		t.Error("Render must be set")
	}
	if fb.Capture == nil {
		t.Error("Capture must be set")
	}
	if fb.PreLaunchRefresh != nil {
		t.Error("PreLaunchRefresh must be nil; OpenCode has no refresh hook")
	}
}

func TestOpenCode_Render_ByteIdentityOnValidEnvelope(t *testing.T) {
	envelope := []byte(`{"anthropic":{"type":"api","key":"sk-ant-x"}}`)
	spec := agents.OpenCode{}.AuthSpec()
	got, err := spec.FileBindings[0].Render(vault.Secret{Value: envelope})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !bytes.Equal(got, envelope) {
		t.Errorf("Render bytes = %q, want byte-identity %q", got, envelope)
	}
}

func TestOpenCode_Render_RejectsEmptyValue(t *testing.T) {
	spec := agents.OpenCode{}.AuthSpec()
	_, err := spec.FileBindings[0].Render(vault.Secret{Value: nil})
	if err == nil {
		t.Fatal("expected error for empty Value")
	}
	if !strings.Contains(err.Error(), "empty Value") {
		t.Errorf("err = %v, want mention of empty Value", err)
	}
}

func TestOpenCode_Render_RejectsMalformedEnvelope(t *testing.T) {
	spec := agents.OpenCode{}.AuthSpec()
	_, err := spec.FileBindings[0].Render(vault.Secret{Value: []byte("not-json")})
	if err == nil {
		t.Fatal("expected error for malformed envelope")
	}
}

func TestOpenCode_Render_RejectsEmptyObject(t *testing.T) {
	// An empty object parses as JSON but carries no provider credential.
	spec := agents.OpenCode{}.AuthSpec()
	_, err := spec.FileBindings[0].Render(vault.Secret{Value: []byte("{}")})
	if err == nil {
		t.Fatal("expected error for empty object (no provider entries)")
	}
	if !strings.Contains(err.Error(), "no provider entries") {
		t.Errorf("err = %v, want mention of no provider entries", err)
	}
}

func TestOpenCode_Capture_RoundTripStampsOAuthRefreshToken(t *testing.T) {
	envelope := []byte(`{"openai":{"type":"api","key":"sk-x"}}`)
	spec := agents.OpenCode{}.AuthSpec()
	got, err := spec.FileBindings[0].Capture(envelope)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if !bytes.Equal(got.Value, envelope) {
		t.Errorf("Capture Value = %q, want byte-identity %q", got.Value, envelope)
	}
	if got.Metadata.Type != "oauth_refresh_token" {
		t.Errorf("Metadata.Type = %q, want oauth_refresh_token", got.Metadata.Type)
	}
}

func TestOpenCode_Capture_RejectsMalformedEnvelope(t *testing.T) {
	spec := agents.OpenCode{}.AuthSpec()
	_, err := spec.FileBindings[0].Capture([]byte("garbage bytes"))
	if err == nil {
		t.Fatal("expected error for non-JSON envelope")
	}
	if !strings.Contains(err.Error(), "parse") {
		t.Errorf("err = %v, want mention of parse failure", err)
	}
}

func TestOpenCode_AuthSpec_RoundTrip(t *testing.T) {
	envelope := []byte(`{"anthropic":{"type":"oauth","refresh":"r","access":"a"}}`)
	spec := agents.OpenCode{}.AuthSpec()
	rendered, err := spec.FileBindings[0].Render(vault.Secret{Value: envelope})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	captured, err := spec.FileBindings[0].Capture(rendered)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if !bytes.Equal(captured.Value, envelope) {
		t.Errorf("round-trip drift: got %q want %q", captured.Value, envelope)
	}
}
