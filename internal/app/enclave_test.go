package app

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/ALRubinger/aileron/internal/config"
	"github.com/ALRubinger/aileron/internal/connector"
	"github.com/ALRubinger/aileron/internal/enclave"
	"github.com/ALRubinger/aileron/internal/enclave/gcs"
	"github.com/ALRubinger/aileron/internal/enclave/local"
)

func TestNewEnclaveClient_EmptyProvider(t *testing.T) {
	cfg := &config.TEEConfig{Provider: ""}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	registry := connector.NewRegistry()

	client, verifier, err := newEnclaveClient(cfg, log, registry)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if client != nil {
		t.Fatal("expected nil client for empty provider")
	}
	if verifier != nil {
		t.Fatal("expected nil verifier for empty provider")
	}
}

func TestNewEnclaveClient_Local(t *testing.T) {
	cfg := &config.TEEConfig{Provider: "local"}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	registry := connector.NewRegistry()

	client, verifier, err := newEnclaveClient(cfg, log, registry)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if client == nil {
		t.Fatal("expected non-nil client for local provider")
	}
	if verifier == nil {
		t.Fatal("expected non-nil verifier for local provider")
	}
	if _, ok := verifier.(*local.DevVerifier); !ok {
		t.Fatalf("expected *local.DevVerifier, got %T", verifier)
	}
}

func TestNewEnclaveClient_ConfidentialSpaceNoURL(t *testing.T) {
	cfg := &config.TEEConfig{Provider: "confidential-space", EnclaveURL: ""}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	registry := connector.NewRegistry()

	client, verifier, err := newEnclaveClient(cfg, log, registry)
	if err == nil {
		t.Fatal("expected error for confidential-space without EnclaveURL")
	}
	if client != nil {
		t.Fatal("expected nil client on error")
	}
	if verifier != nil {
		t.Fatal("expected nil verifier on error")
	}
}

func TestNewEnclaveClient_ConfidentialSpaceRequiresImageDigest(t *testing.T) {
	cfg := &config.TEEConfig{
		Provider:   "confidential-space",
		EnclaveURL: "https://enclave.example.com:8443",
		ProjectID:  "my-project",
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	registry := connector.NewRegistry()

	client, verifier, err := newEnclaveClient(cfg, log, registry)
	if err == nil {
		t.Fatal("expected error for confidential-space without ImageDigest")
	}
	if client != nil {
		t.Fatal("expected nil client on error")
	}
	if verifier != nil {
		t.Fatal("expected nil verifier on error")
	}
}

func TestNewEnclaveClient_ConfidentialSpaceRequiresProjectID(t *testing.T) {
	cfg := &config.TEEConfig{
		Provider:    "confidential-space",
		EnclaveURL:  "https://enclave.example.com:8443",
		ImageDigest: "sha256:abc123",
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	registry := connector.NewRegistry()

	client, verifier, err := newEnclaveClient(cfg, log, registry)
	if err == nil {
		t.Fatal("expected error for confidential-space without ProjectID")
	}
	if client != nil {
		t.Fatal("expected nil client on error")
	}
	if verifier != nil {
		t.Fatal("expected nil verifier on error")
	}
}

func TestNewEnclaveClient_ConfidentialSpaceWithURL(t *testing.T) {
	cfg := &config.TEEConfig{
		Provider:    "confidential-space",
		EnclaveURL:  "https://enclave.example.com:8443",
		ImageDigest: "sha256:abc123",
		ProjectID:   "my-project",
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	registry := connector.NewRegistry()

	client, verifier, err := newEnclaveClient(cfg, log, registry)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if client == nil {
		t.Fatal("expected non-nil client for confidential-space")
	}
	if verifier == nil {
		t.Fatal("expected non-nil verifier for confidential-space")
	}
	if _, ok := verifier.(*gcs.Verifier); !ok {
		t.Fatalf("expected *gcs.Verifier, got %T", verifier)
	}
}

func TestBuildLocalExecuteFn(t *testing.T) {
	executed := false
	stub := &stubConnectorForEnclave{executeFn: func() { executed = true }}

	registry := connector.NewRegistry()
	registry.Register(context.Background(), stub)

	fn := buildLocalExecuteFn(registry)

	// Test successful execution — action type "payment.charge" resolves
	// to "payments/enclave-stub" via resolveConnector, but our stub is
	// registered as "test/enclave-stub". Use git prefix which resolves
	// to "git/github" — but we need a matching connector. Let's register
	// the stub under "payments/stripe" instead.
	stub2 := &stubConnectorForEnclave2{executeFn: func() { executed = true }}
	registry.Register(context.Background(), stub2)

	resp, err := fn(context.Background(), enclave.ExecuteRequest{
		RequestID:  "req-1",
		ActionType: "payment.charge",
		Parameters: map[string]any{"key": "value"},
	}, []byte("credential"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Status != "succeeded" {
		t.Fatalf("expected succeeded, got %q", resp.Status)
	}
	if !executed {
		t.Fatal("connector was not called")
	}

	// Test unknown connector.
	resp2, err := fn(context.Background(), enclave.ExecuteRequest{
		ActionType: "unknown.action",
	}, []byte("cred"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp2.Status != "failed" {
		t.Fatalf("expected failed for unknown connector, got %q", resp2.Status)
	}
}

type stubConnectorForEnclave struct {
	executeFn func()
}

func (s *stubConnectorForEnclave) Type() string     { return "test" }
func (s *stubConnectorForEnclave) Provider() string { return "enclave-stub" }
func (s *stubConnectorForEnclave) Execute(_ context.Context, _ connector.ExecutionRequest) (connector.ExecutionResult, error) {
	if s.executeFn != nil {
		s.executeFn()
	}
	return connector.ExecutionResult{
		Status:     connector.ExecutionStatusSucceeded,
		Output:     map[string]any{"ok": true},
		ReceiptRef: "ref-123",
	}, nil
}

// stubConnectorForEnclave2 registers as "payments/stripe" to match resolveConnector.
type stubConnectorForEnclave2 struct {
	executeFn func()
}

func (s *stubConnectorForEnclave2) Type() string     { return "payments" }
func (s *stubConnectorForEnclave2) Provider() string { return "stripe" }
func (s *stubConnectorForEnclave2) Execute(_ context.Context, _ connector.ExecutionRequest) (connector.ExecutionResult, error) {
	if s.executeFn != nil {
		s.executeFn()
	}
	return connector.ExecutionResult{
		Status:     connector.ExecutionStatusSucceeded,
		Output:     map[string]any{"ok": true},
		ReceiptRef: "ref-123",
	}, nil
}

func TestNewEnclaveClient_UnknownProvider(t *testing.T) {
	cfg := &config.TEEConfig{Provider: "unknown-provider"}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	registry := connector.NewRegistry()

	client, verifier, err := newEnclaveClient(cfg, log, registry)
	if err == nil {
		t.Fatal("expected error for unknown provider")
	}
	if client != nil {
		t.Fatal("expected nil client on error")
	}
	if verifier != nil {
		t.Fatal("expected nil verifier on error")
	}
}
