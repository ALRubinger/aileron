package auth

import (
	"testing"
	"time"
)

func TestTokenIssuer_IssueAndValidate(t *testing.T) {
	issuer := NewTokenIssuer([]byte("test-secret-key-32-bytes-long!!!"), "aileron-test", 15*time.Minute)

	token, err := issuer.Issue("usr_1", "ent_1", "alice@example.com", "owner")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if token == "" {
		t.Fatal("expected non-empty token")
	}

	claims, err := issuer.Validate(token)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if claims.Subject != "usr_1" {
		t.Errorf("Subject = %q, want %q", claims.Subject, "usr_1")
	}
	if claims.EnterpriseID != "ent_1" {
		t.Errorf("EnterpriseID = %q, want %q", claims.EnterpriseID, "ent_1")
	}
	if claims.Email != "alice@example.com" {
		t.Errorf("Email = %q, want %q", claims.Email, "alice@example.com")
	}
	if claims.Role != "owner" {
		t.Errorf("Role = %q, want %q", claims.Role, "owner")
	}
	if claims.Issuer != "aileron-test" {
		t.Errorf("Issuer = %q, want %q", claims.Issuer, "aileron-test")
	}
}

func TestTokenIssuer_ValidateExpired(t *testing.T) {
	issuer := NewTokenIssuer([]byte("test-secret-key-32-bytes-long!!!"), "aileron-test", -1*time.Second)

	token, err := issuer.Issue("usr_1", "ent_1", "alice@example.com", "owner")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	_, err = issuer.Validate(token)
	if err == nil {
		t.Fatal("expected error for expired token")
	}
}

func TestTokenIssuer_ValidateWrongKey(t *testing.T) {
	issuer1 := NewTokenIssuer([]byte("key-one-32-bytes-long-!!!!!!!!!!!"), "aileron", 15*time.Minute)
	issuer2 := NewTokenIssuer([]byte("key-two-32-bytes-long-!!!!!!!!!!!"), "aileron", 15*time.Minute)

	token, _ := issuer1.Issue("usr_1", "ent_1", "alice@example.com", "owner")
	_, err := issuer2.Validate(token)
	if err == nil {
		t.Fatal("expected error for token signed with different key")
	}
}

func TestTokenIssuer_ValidateGarbage(t *testing.T) {
	issuer := NewTokenIssuer([]byte("test-secret-key-32-bytes-long!!!"), "aileron-test", 15*time.Minute)
	_, err := issuer.Validate("not.a.jwt")
	if err == nil {
		t.Fatal("expected error for garbage token")
	}
}

func TestGenerateRefreshToken(t *testing.T) {
	raw, hash, err := GenerateRefreshToken()
	if err != nil {
		t.Fatalf("GenerateRefreshToken: %v", err)
	}
	if len(raw) != 64 { // 32 bytes hex-encoded
		t.Errorf("raw token length = %d, want 64", len(raw))
	}
	if hash != HashToken(raw) {
		t.Error("hash does not match HashToken(raw)")
	}

	// Two tokens should be different.
	raw2, _, _ := GenerateRefreshToken()
	if raw == raw2 {
		t.Error("expected different tokens on successive calls")
	}
}

func TestHashToken(t *testing.T) {
	hash := HashToken("test-token")
	if len(hash) != 64 { // SHA-256 hex = 64 chars
		t.Errorf("hash length = %d, want 64", len(hash))
	}
	// Deterministic.
	if HashToken("test-token") != hash {
		t.Error("expected same hash for same input")
	}
	// Different input → different hash.
	if HashToken("other-token") == hash {
		t.Error("expected different hash for different input")
	}
}
