package main

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/ALRubinger/aileron/internal/credential"
	"github.com/ALRubinger/aileron/internal/cstore"
)

func TestDevResolver_NoCredentialDeclaredReturnsNoBindingResolver(t *testing.T) {
	r := newDevResolver(&cstore.Manifest{}, "tok")
	_, err := r.Resolve(context.Background())
	if !errors.Is(err, credential.ErrNoBindingResolver) {
		t.Fatalf("got %v, want ErrNoBindingResolver", err)
	}
}

func TestDevResolver_NoTokenReturnsBindingMissing(t *testing.T) {
	m := &cstore.Manifest{}
	m.Capabilities.Credential = &cstore.ManifestCredential{Kind: "oauth2"}
	r := newDevResolver(m, "")
	_, err := r.Resolve(context.Background())
	if !errors.Is(err, credential.ErrBindingMissing) {
		t.Fatalf("got %v, want ErrBindingMissing", err)
	}
}

func TestDevResolver_HappyPathReturnsCredential(t *testing.T) {
	m := &cstore.Manifest{}
	m.Capabilities.Credential = &cstore.ManifestCredential{Kind: "oauth2"}
	r := newDevResolver(m, "ya29.test-token")

	cred, err := r.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if cred.Kind != "oauth2" {
		t.Errorf("Kind = %q, want oauth2", cred.Kind)
	}
	if string(cred.Value) != "ya29.test-token" {
		t.Errorf("Value = %q, want ya29.test-token", string(cred.Value))
	}
}

func TestEnvelope_OutputShape(t *testing.T) {
	got, err := json.Marshal(envelope{Output: map[string]any{"k": "v"}})
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"output":{"k":"v"}}` {
		t.Errorf("got %s", got)
	}
}

func TestEnvelope_ErrorShape(t *testing.T) {
	got, err := json.Marshal(envelope{Error: "boom"})
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"error":"boom"}` {
		t.Errorf("got %s", got)
	}
}
