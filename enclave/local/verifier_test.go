package local

import (
	"context"
	"testing"
	"time"
)

func TestDevVerifier(t *testing.T) {
	before := time.Now()
	v := &DevVerifier{}
	claims, err := v.Verify(context.Background(), "any-token", []byte("any-nonce"))
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if claims.ImageDigest != "dev" {
		t.Fatalf("expected ImageDigest dev, got %q", claims.ImageDigest)
	}
	if claims.ProjectID != "local" {
		t.Fatalf("expected ProjectID local, got %q", claims.ProjectID)
	}
	if claims.Issuer != "local-dev" {
		t.Fatalf("expected Issuer local-dev, got %q", claims.Issuer)
	}
	if claims.HWModel != "none" {
		t.Fatalf("expected HWModel none, got %q", claims.HWModel)
	}
	if claims.IssuedAt.Before(before) {
		t.Fatal("expected IssuedAt to be recent")
	}
	if claims.ExpiresAt.Before(claims.IssuedAt.Add(23 * time.Hour)) {
		t.Fatal("expected ExpiresAt to be ~24h after IssuedAt")
	}
}
