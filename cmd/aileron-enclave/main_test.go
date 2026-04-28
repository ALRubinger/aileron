package main

import (
	"encoding/base64"
	"testing"
)

func TestEscrowKeyConfigFromEnvConfidentialSpaceRequiresExternalKey(t *testing.T) {
	t.Setenv("AILERON_ENCLAVE_ESCROW_KEY_B64", "")
	t.Setenv("AILERON_ENCLAVE_ALLOW_RAW_ESCROW_KEY", "")

	cfg, err := escrowKeyConfigFromEnv("confidential-space")
	if err != nil {
		t.Fatalf("escrowKeyConfigFromEnv: %v", err)
	}
	if cfg.allowRawFile {
		t.Fatal("confidential-space should not allow raw escrow key files by default")
	}
	if len(cfg.key) != 0 {
		t.Fatal("expected no external key")
	}
}

func TestEscrowKeyConfigFromEnvLocalAllowsRawKeyFile(t *testing.T) {
	t.Setenv("AILERON_ENCLAVE_ESCROW_KEY_B64", "")
	t.Setenv("AILERON_ENCLAVE_ALLOW_RAW_ESCROW_KEY", "")

	cfg, err := escrowKeyConfigFromEnv("local")
	if err != nil {
		t.Fatalf("escrowKeyConfigFromEnv: %v", err)
	}
	if !cfg.allowRawFile {
		t.Fatal("local provider should allow raw escrow key files by default")
	}
}

func TestEscrowKeyConfigFromEnvExternalKey(t *testing.T) {
	key := []byte("01234567890123456789012345678901")
	t.Setenv("AILERON_ENCLAVE_ESCROW_KEY_B64", base64.StdEncoding.EncodeToString(key))
	t.Setenv("AILERON_ENCLAVE_ALLOW_RAW_ESCROW_KEY", "true")

	cfg, err := escrowKeyConfigFromEnv("confidential-space")
	if err != nil {
		t.Fatalf("escrowKeyConfigFromEnv: %v", err)
	}
	if cfg.allowRawFile {
		t.Fatal("external key should disable raw escrow key files")
	}
	if string(cfg.key) != string(key) {
		t.Fatal("decoded external key mismatch")
	}
}

func TestEscrowKeyConfigFromEnvAllowsRawOverride(t *testing.T) {
	t.Setenv("AILERON_ENCLAVE_ESCROW_KEY_B64", "")
	t.Setenv("AILERON_ENCLAVE_ALLOW_RAW_ESCROW_KEY", "true")

	cfg, err := escrowKeyConfigFromEnv("confidential-space")
	if err != nil {
		t.Fatalf("escrowKeyConfigFromEnv: %v", err)
	}
	if !cfg.allowRawFile {
		t.Fatal("raw escrow key override should be allowed")
	}
}

func TestEscrowKeyConfigFromEnvRejectsBadBase64(t *testing.T) {
	t.Setenv("AILERON_ENCLAVE_ESCROW_KEY_B64", "not base64")

	if _, err := escrowKeyConfigFromEnv("confidential-space"); err == nil {
		t.Fatal("expected invalid base64 to fail")
	}
}

func TestEscrowKeyConfigFromEnvRejectsWrongKeySize(t *testing.T) {
	t.Setenv("AILERON_ENCLAVE_ESCROW_KEY_B64", base64.StdEncoding.EncodeToString([]byte("short")))

	if _, err := escrowKeyConfigFromEnv("confidential-space"); err == nil {
		t.Fatal("expected wrong key size to fail")
	}
}
