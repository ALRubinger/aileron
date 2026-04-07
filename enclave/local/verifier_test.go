package local

import (
	"context"
	"testing"
)

func TestDevVerifier(t *testing.T) {
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
}
