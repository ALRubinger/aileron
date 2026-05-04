package launch

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/ALRubinger/aileron/internal/crypto"
	"github.com/ALRubinger/aileron/internal/vault"
)

func TestComposeAgentEnv_NoOverrideReturnsInputUnchanged(t *testing.T) {
	in := map[string]string{"FOO": "bar"}

	// Empty env-var name: agent doesn't support override; pass-through.
	out := composeAgentEnv(in, "", "http://127.0.0.1:8080")
	if got, want := len(out), 1; got != want {
		t.Fatalf("expected len %d, got %d", want, got)
	}
	if out["FOO"] != "bar" {
		t.Fatalf("FOO mismatch: got %q", out["FOO"])
	}

	// Empty URL: no gateway running; pass-through.
	out = composeAgentEnv(in, "ANTHROPIC_BASE_URL", "")
	if got, want := len(out), 1; got != want {
		t.Fatalf("expected len %d, got %d", want, got)
	}
}

func TestComposeAgentEnv_AddsOverrideAndPreservesExisting(t *testing.T) {
	in := map[string]string{
		"AILERON_REAL_SHELL": "/bin/bash",
		"CLAUDE_CODE_SHELL":  "/home/x/.aileron/bash",
	}
	out := composeAgentEnv(in, "ANTHROPIC_BASE_URL", "http://127.0.0.1:54321")

	if out["ANTHROPIC_BASE_URL"] != "http://127.0.0.1:54321" {
		t.Fatalf("ANTHROPIC_BASE_URL not set: %q", out["ANTHROPIC_BASE_URL"])
	}
	if out["AILERON_REAL_SHELL"] != "/bin/bash" {
		t.Fatalf("existing env not preserved")
	}
	if out["CLAUDE_CODE_SHELL"] != "/home/x/.aileron/bash" {
		t.Fatalf("existing env not preserved")
	}
}

func TestComposeAgentEnv_DoesNotMutateInput(t *testing.T) {
	in := map[string]string{"FOO": "bar"}
	_ = composeAgentEnv(in, "ANTHROPIC_BASE_URL", "http://127.0.0.1:1234")
	if _, ok := in["ANTHROPIC_BASE_URL"]; ok {
		t.Fatal("composeAgentEnv mutated input map")
	}
	if got, want := len(in), 1; got != want {
		t.Fatalf("input map size changed: got %d, want %d", got, want)
	}
}

func TestComposeAgentEnv_NilInputProducesNilWhenNoOverride(t *testing.T) {
	if out := composeAgentEnv(nil, "", ""); out != nil {
		t.Fatalf("expected nil pass-through, got %v", out)
	}
}

func TestComposeAgentEnv_NilInputProducesMapWhenOverrideApplies(t *testing.T) {
	out := composeAgentEnv(nil, "OPENAI_BASE_URL", "http://127.0.0.1:9999")
	if got, want := len(out), 1; got != want {
		t.Fatalf("expected len %d, got %d", want, got)
	}
	if out["OPENAI_BASE_URL"] != "http://127.0.0.1:9999" {
		t.Fatalf("override not applied: %q", out["OPENAI_BASE_URL"])
	}
}

func TestGatewayURL_NilReturnsEmpty(t *testing.T) {
	if got := gatewayURL(nil); got != "" {
		t.Fatalf("expected empty string for nil gateway, got %q", got)
	}
}

func TestGatewayURL_RunningReturnsURL(t *testing.T) {
	g := &Gateway{URL: "http://127.0.0.1:1234"}
	if got := gatewayURL(g); got != "http://127.0.0.1:1234" {
		t.Fatalf("URL mismatch: got %q", got)
	}
}

func TestGatewayClose_NilReceiverNoop(t *testing.T) {
	var g *Gateway
	if err := g.Close(context.Background()); err != nil {
		t.Fatalf("Close on nil receiver should be a no-op, got %v", err)
	}
}

func TestStartGateway_RequiresVault(t *testing.T) {
	_, err := StartGateway(context.Background(), nil, nil)
	if err == nil {
		t.Fatal("expected error when vault is nil")
	}
}

// TestStartGateway_BindsAndServes verifies the embedded gateway binds
// to an ephemeral localhost port and the URL it returns is reachable.
// It hits /v1/health (a minimal endpoint that doesn't depend on vault
// state) to confirm the handler is wired and the server is serving.
func TestStartGateway_BindsAndServes(t *testing.T) {
	kek, err := crypto.GenerateRandomKEK()
	if err != nil {
		t.Fatalf("GenerateRandomKEK: %v", err)
	}
	v, err := vault.NewEncryptedVault(vault.NewMemVault(), kek)
	if err != nil {
		t.Fatalf("NewEncryptedVault: %v", err)
	}

	g, err := StartGateway(context.Background(), v, nil)
	if err != nil {
		t.Fatalf("StartGateway: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = g.Close(ctx)
	})

	if g.URL == "" || g.URL[:len("http://127.0.0.1:")] != "http://127.0.0.1:" {
		t.Fatalf("URL = %q, want http://127.0.0.1:<port>", g.URL)
	}

	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(g.URL + "/v1/health")
	if err != nil {
		t.Fatalf("GET /v1/health: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

// TestGatewayClose_RunningServerShutsDown verifies Close on a real
// running gateway returns without error. After Close, subsequent
// requests to the URL should fail.
func TestGatewayClose_RunningServerShutsDown(t *testing.T) {
	kek, err := crypto.GenerateRandomKEK()
	if err != nil {
		t.Fatalf("GenerateRandomKEK: %v", err)
	}
	v, err := vault.NewEncryptedVault(vault.NewMemVault(), kek)
	if err != nil {
		t.Fatalf("NewEncryptedVault: %v", err)
	}

	g, err := StartGateway(context.Background(), v, nil)
	if err != nil {
		t.Fatalf("StartGateway: %v", err)
	}
	if err := g.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	client := &http.Client{Timeout: 1 * time.Second}
	if _, err := client.Get(g.URL + "/v1/health"); err == nil {
		t.Fatal("expected request to fail after Close")
	}
}
