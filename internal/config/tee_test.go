package config

import "testing"

func TestLoadTEEConfig_NoEnvVars(t *testing.T) {
	// Ensure env vars are unset.
	t.Setenv("AILERON_TEE_PROVIDER", "")
	t.Setenv("AILERON_ENCLAVE_URL", "")
	t.Setenv("AILERON_ENCLAVE_IMAGE_DIGEST", "")
	t.Setenv("AILERON_GCP_PROJECT_ID", "")

	cfg := LoadTEEConfig()
	if cfg.Provider != "" {
		t.Errorf("expected empty Provider, got %q", cfg.Provider)
	}
	if cfg.EnclaveURL != "" {
		t.Errorf("expected empty EnclaveURL, got %q", cfg.EnclaveURL)
	}
	if cfg.ImageDigest != "" {
		t.Errorf("expected empty ImageDigest, got %q", cfg.ImageDigest)
	}
	if cfg.ProjectID != "" {
		t.Errorf("expected empty ProjectID, got %q", cfg.ProjectID)
	}
}

func TestLoadTEEConfig_WithProvider(t *testing.T) {
	t.Setenv("AILERON_TEE_PROVIDER", "local")
	t.Setenv("AILERON_ENCLAVE_URL", "https://enclave.internal:8443")
	t.Setenv("AILERON_ENCLAVE_IMAGE_DIGEST", "sha256:abc123")
	t.Setenv("AILERON_GCP_PROJECT_ID", "my-project")

	cfg := LoadTEEConfig()
	if cfg.Provider != "local" {
		t.Errorf("expected Provider 'local', got %q", cfg.Provider)
	}
	if cfg.EnclaveURL != "https://enclave.internal:8443" {
		t.Errorf("expected EnclaveURL, got %q", cfg.EnclaveURL)
	}
	if cfg.ImageDigest != "sha256:abc123" {
		t.Errorf("expected ImageDigest, got %q", cfg.ImageDigest)
	}
	if cfg.ProjectID != "my-project" {
		t.Errorf("expected ProjectID, got %q", cfg.ProjectID)
	}
}

func TestTEEEnabled_EmptyProvider(t *testing.T) {
	cfg := &TEEConfig{Provider: ""}
	if cfg.TEEEnabled() {
		t.Error("expected TEEEnabled=false for empty provider")
	}
}

func TestTEEEnabled_NonEmptyProvider(t *testing.T) {
	cfg := &TEEConfig{Provider: "local"}
	if !cfg.TEEEnabled() {
		t.Error("expected TEEEnabled=true for non-empty provider")
	}
}

func TestTEEEnabled_ConfidentialSpace(t *testing.T) {
	cfg := &TEEConfig{Provider: "confidential-space"}
	if !cfg.TEEEnabled() {
		t.Error("expected TEEEnabled=true for confidential-space provider")
	}
}
