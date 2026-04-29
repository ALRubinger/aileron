package main

import (
	"encoding/base64"
	"testing"
)

func TestEscrowKeyConfigFromEnvConfidentialSpaceRequiresExternalKey(t *testing.T) {
	t.Setenv("AILERON_ENCLAVE_ESCROW_KEY_B64", "")
	t.Setenv("AILERON_ENCLAVE_ALLOW_RAW_ESCROW_KEY", "")
	t.Setenv("AILERON_ENCLAVE_ESCROW_KEY_RELEASE_URL", "")

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
	t.Setenv("AILERON_ENCLAVE_ESCROW_KEY_RELEASE_URL", "")

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
	t.Setenv("AILERON_ENCLAVE_ESCROW_KEY_RELEASE_URL", "")

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

func TestEscrowKeyConfigFromEnvAttestedKeyRelease(t *testing.T) {
	t.Setenv("AILERON_ENCLAVE_ESCROW_KEY_B64", "")
	t.Setenv("AILERON_ENCLAVE_ALLOW_RAW_ESCROW_KEY", "true")
	t.Setenv("AILERON_ENCLAVE_ESCROW_KEY_RELEASE_URL", "https://key-release.example.com/escrow")
	t.Setenv("AILERON_ENCLAVE_ESCROW_KEY_RELEASE_AUDIENCE", "key-release-audience")

	cfg, err := escrowKeyConfigFromEnv("confidential-space")
	if err != nil {
		t.Fatalf("escrowKeyConfigFromEnv: %v", err)
	}
	if cfg.allowRawFile {
		t.Fatal("attested key release should disable raw escrow key files")
	}
	if len(cfg.key) != 0 {
		t.Fatal("expected no static key")
	}
	provider, ok := cfg.keyProvider.(attestedEscrowKeyProvider)
	if !ok {
		t.Fatalf("key provider = %T, want attestedEscrowKeyProvider", cfg.keyProvider)
	}
	if provider.releaseURL != "https://key-release.example.com/escrow" {
		t.Fatalf("releaseURL = %q", provider.releaseURL)
	}
	if provider.audience != "key-release-audience" {
		t.Fatalf("audience = %q", provider.audience)
	}
}

func TestEscrowKeyConfigFromEnvAttestedKeyReleaseDefaultsAudienceToURL(t *testing.T) {
	t.Setenv("AILERON_ENCLAVE_ESCROW_KEY_RELEASE_URL", "https://key-release.example.com/escrow")
	t.Setenv("AILERON_ENCLAVE_ESCROW_KEY_RELEASE_AUDIENCE", "")

	cfg, err := escrowKeyConfigFromEnv("confidential-space")
	if err != nil {
		t.Fatalf("escrowKeyConfigFromEnv: %v", err)
	}
	provider, ok := cfg.keyProvider.(attestedEscrowKeyProvider)
	if !ok {
		t.Fatalf("key provider = %T, want attestedEscrowKeyProvider", cfg.keyProvider)
	}
	if provider.audience != provider.releaseURL {
		t.Fatalf("audience = %q, want release URL %q", provider.audience, provider.releaseURL)
	}
}

func TestEscrowKeyConfigFromEnvRejectsAmbiguousKeySources(t *testing.T) {
	key := []byte("01234567890123456789012345678901")
	t.Setenv("AILERON_ENCLAVE_ESCROW_KEY_B64", base64.StdEncoding.EncodeToString(key))
	t.Setenv("AILERON_ENCLAVE_ESCROW_KEY_RELEASE_URL", "https://key-release.example.com/escrow")

	if _, err := escrowKeyConfigFromEnv("confidential-space"); err == nil {
		t.Fatal("expected multiple key sources to fail")
	}
}

func TestEscrowKeyConfigFromEnvAllowsRawOverride(t *testing.T) {
	t.Setenv("AILERON_ENCLAVE_ESCROW_KEY_B64", "")
	t.Setenv("AILERON_ENCLAVE_ALLOW_RAW_ESCROW_KEY", "true")
	t.Setenv("AILERON_ENCLAVE_ESCROW_KEY_RELEASE_URL", "")

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
	t.Setenv("AILERON_ENCLAVE_ESCROW_KEY_RELEASE_URL", "")

	if _, err := escrowKeyConfigFromEnv("confidential-space"); err == nil {
		t.Fatal("expected invalid base64 to fail")
	}
}

func TestEscrowKeyConfigFromEnvRejectsWrongKeySize(t *testing.T) {
	t.Setenv("AILERON_ENCLAVE_ESCROW_KEY_B64", base64.StdEncoding.EncodeToString([]byte("short")))
	t.Setenv("AILERON_ENCLAVE_ESCROW_KEY_RELEASE_URL", "")

	if _, err := escrowKeyConfigFromEnv("confidential-space"); err == nil {
		t.Fatal("expected wrong key size to fail")
	}
}
