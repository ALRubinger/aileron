package account_test

import (
	"context"
	"testing"

	"github.com/ALRubinger/aileron/core/account"
	"github.com/ALRubinger/aileron/core/model"
	"github.com/ALRubinger/aileron/core/store/mem"
	"github.com/ALRubinger/aileron/core/vault"
)

func newTestRegistry(t *testing.T) (*account.Registry, *account.GoogleService) {
	t.Helper()
	r := account.NewRegistry()
	svc := account.NewGoogleService("test-id", "test-secret", mem.NewConnectedAccountStore(), vault.NewMemVault())
	r.Register(svc)
	return r, svc
}

func TestRegistry_Providers(t *testing.T) {
	r, _ := newTestRegistry(t)
	providers := r.Providers()
	if len(providers) != 2 {
		t.Fatalf("expected 2 providers (gmail, google_calendar), got %d", len(providers))
	}
	got := make(map[model.ConnectedAccountProvider]bool)
	for _, p := range providers {
		got[p] = true
	}
	if !got[model.ConnectedAccountProviderGmail] {
		t.Error("expected gmail provider")
	}
	if !got[model.ConnectedAccountProviderGoogleCalendar] {
		t.Error("expected google_calendar provider")
	}
}

func TestRegistry_ProviderFor(t *testing.T) {
	r, _ := newTestRegistry(t)

	svc, err := r.ProviderFor(model.ConnectedAccountProviderGmail)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if svc.ClientID() != "test-id" {
		t.Errorf("expected client ID 'test-id', got %q", svc.ClientID())
	}
}

func TestRegistry_ProviderFor_Unsupported(t *testing.T) {
	r := account.NewRegistry()
	_, err := r.ProviderFor("nonexistent")
	if err == nil {
		t.Fatal("expected error for unsupported provider")
	}
}

func TestRegistry_AuthorizationURL(t *testing.T) {
	r, _ := newTestRegistry(t)
	result, err := r.AuthorizationURL(context.Background(), model.ConnectedAccountProviderGmail, "state123", "http://localhost/callback")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.URL == "" {
		t.Error("expected non-empty authorization URL")
	}
}

func TestRegistry_AuthorizationURL_Unsupported(t *testing.T) {
	r := account.NewRegistry()
	_, err := r.AuthorizationURL(context.Background(), "nonexistent", "state", "http://localhost/callback")
	if err == nil {
		t.Fatal("expected error for unsupported provider")
	}
}

func TestRegistry_Empty_List(t *testing.T) {
	r := account.NewRegistry()
	accounts, err := r.List(context.Background(), "user1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if accounts != nil {
		t.Errorf("expected nil accounts from empty registry, got %v", accounts)
	}
}

func TestRegistry_Empty_Get(t *testing.T) {
	r := account.NewRegistry()
	_, err := r.Get(context.Background(), "conn_123")
	if err == nil {
		t.Fatal("expected error from empty registry")
	}
}

func TestRegistry_Empty_Disconnect(t *testing.T) {
	r := account.NewRegistry()
	err := r.Disconnect(context.Background(), "conn_123")
	if err == nil {
		t.Fatal("expected error from empty registry")
	}
}
