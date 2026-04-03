package auth

import (
	"context"
	"testing"

	"github.com/ALRubinger/aileron/core/model"
)

func TestStoreEnforcer_IsProviderAllowed(t *testing.T) {
	tests := []struct {
		name     string
		allowed  []string
		provider string
		want     bool
	}{
		{"no restrictions", nil, "google", true},
		{"empty restrictions", []string{}, "google", true},
		{"allowed", []string{"google", "okta"}, "google", true},
		{"not allowed", []string{"okta"}, "google", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newStubStore(model.Enterprise{ID: "ent_1", AllowedAuthProviders: tt.allowed})
			e := NewStoreEnforcer(s)
			got, err := e.IsProviderAllowed(context.Background(), "ent_1", tt.provider)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("IsProviderAllowed = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestStoreEnforcer_IsSSORequired(t *testing.T) {
	s := newStubStore(
		model.Enterprise{ID: "ent_1", SSORequired: false},
		model.Enterprise{ID: "ent_2", SSORequired: true},
	)
	e := NewStoreEnforcer(s)

	got, _ := e.IsSSORequired(context.Background(), "ent_1")
	if got {
		t.Error("expected false for ent_1")
	}
	got, _ = e.IsSSORequired(context.Background(), "ent_2")
	if !got {
		t.Error("expected true for ent_2")
	}
}

func TestStoreEnforcer_IsEmailDomainAllowed(t *testing.T) {
	tests := []struct {
		name    string
		domains []string
		email   string
		want    bool
	}{
		{"no restrictions", nil, "alice@example.com", true},
		{"allowed domain", []string{"acme.com"}, "alice@acme.com", true},
		{"not allowed domain", []string{"acme.com"}, "alice@other.com", false},
		{"case insensitive", []string{"Acme.COM"}, "alice@acme.com", true},
		{"invalid email", []string{"acme.com"}, "invalid", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newStubStore(model.Enterprise{ID: "ent_1", AllowedEmailDomains: tt.domains})
			e := NewStoreEnforcer(s)
			got, err := e.IsEmailDomainAllowed(context.Background(), "ent_1", tt.email)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("IsEmailDomainAllowed = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestStoreEnforcer_EnterpriseNotFound(t *testing.T) {
	s := newStubStore()
	e := NewStoreEnforcer(s)

	_, err := e.IsProviderAllowed(context.Background(), "nonexistent", "google")
	if err == nil {
		t.Fatal("expected error for nonexistent enterprise")
	}
}
