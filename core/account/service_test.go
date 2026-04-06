package account_test

import (
	"context"
	"testing"

	"github.com/ALRubinger/aileron/core/account"
	"github.com/ALRubinger/aileron/core/model"
	"github.com/ALRubinger/aileron/core/store/mem"
	"github.com/ALRubinger/aileron/core/vault"
)

func TestGoogleService_Providers(t *testing.T) {
	svc := account.NewGoogleService("id", "secret", mem.NewConnectedAccountStore(), vault.NewMemVault())
	providers := svc.Providers()
	if len(providers) != 2 {
		t.Fatalf("expected 2 providers, got %d", len(providers))
	}
	if providers[0] != model.ConnectedAccountProviderGmail {
		t.Errorf("expected gmail, got %s", providers[0])
	}
	if providers[1] != model.ConnectedAccountProviderGoogleCalendar {
		t.Errorf("expected google_calendar, got %s", providers[1])
	}
}

func TestGoogleService_AuthorizationURL(t *testing.T) {
	svc := account.NewGoogleService("test-client-id", "test-secret", mem.NewConnectedAccountStore(), vault.NewMemVault())

	result, err := svc.AuthorizationURL(context.Background(), model.ConnectedAccountProviderGmail, "test-state", "http://localhost:8080/auth/connect/gmail/callback")
	if err != nil {
		t.Fatal(err)
	}
	if result.URL == "" {
		t.Fatal("expected non-empty URL")
	}
	// Should contain gmail scopes.
	if !containsSubstring(result.URL, "gmail.readonly") {
		t.Errorf("URL should contain gmail scope, got: %s", result.URL)
	}
	if !containsSubstring(result.URL, "test-client-id") {
		t.Errorf("URL should contain client ID, got: %s", result.URL)
	}
	if !containsSubstring(result.URL, "test-state") {
		t.Errorf("URL should contain state, got: %s", result.URL)
	}
}

func TestGoogleService_AuthorizationURL_CalendarScopes(t *testing.T) {
	svc := account.NewGoogleService("id", "secret", mem.NewConnectedAccountStore(), vault.NewMemVault())

	result, err := svc.AuthorizationURL(context.Background(), model.ConnectedAccountProviderGoogleCalendar, "state", "http://localhost/callback")
	if err != nil {
		t.Fatal(err)
	}
	if !containsSubstring(result.URL, "calendar") {
		t.Errorf("URL should contain calendar scope, got: %s", result.URL)
	}
}

func TestGoogleService_AuthorizationURL_UnsupportedProvider(t *testing.T) {
	svc := account.NewGoogleService("id", "secret", mem.NewConnectedAccountStore(), vault.NewMemVault())

	_, err := svc.AuthorizationURL(context.Background(), model.ConnectedAccountProviderOutlook, "state", "http://localhost/callback")
	if err == nil {
		t.Fatal("expected error for unsupported provider")
	}
}

func TestGoogleService_ListAndDisconnect(t *testing.T) {
	accountStore := mem.NewConnectedAccountStore()
	v := vault.NewMemVault()
	svc := account.NewGoogleService("id", "secret", accountStore, v)

	ctx := context.Background()

	// Manually create an account (simulating a completed OAuth flow).
	acc := model.ConnectedAccount{
		ID:       "conn_test",
		UserID:   "usr_1",
		Provider: model.ConnectedAccountProviderGmail,
		Email:    "test@example.com",
		Scopes:   []string{"gmail.readonly"},
		Status:   model.ConnectedAccountStatusActive,
	}
	accountStore.Create(ctx, acc)
	v.Put(ctx, acc.VaultPath(), []byte(`{"token":"test"}`), vault.Metadata{Type: "oauth_refresh_token"})

	// List.
	accounts, err := svc.List(ctx, "usr_1")
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 1 {
		t.Fatalf("expected 1 account, got %d", len(accounts))
	}

	// Get.
	got, err := svc.Get(ctx, "conn_test")
	if err != nil {
		t.Fatal(err)
	}
	if got.Email != "test@example.com" {
		t.Errorf("expected test@example.com, got %s", got.Email)
	}

	// Disconnect.
	if err := svc.Disconnect(ctx, "conn_test"); err != nil {
		t.Fatal(err)
	}

	// Verify deleted.
	accounts, _ = svc.List(ctx, "usr_1")
	if len(accounts) != 0 {
		t.Fatalf("expected 0 accounts after disconnect, got %d", len(accounts))
	}

	// Verify vault token removed.
	_, err = v.Get(ctx, acc.VaultPath())
	if err == nil {
		t.Fatal("expected vault token to be removed")
	}
}

func TestConnectedAccount_VaultPath(t *testing.T) {
	acc := model.ConnectedAccount{
		UserID:   "usr_abc",
		Provider: model.ConnectedAccountProviderGmail,
	}
	expected := "connected-accounts/usr_abc/gmail"
	if acc.VaultPath() != expected {
		t.Errorf("expected %s, got %s", expected, acc.VaultPath())
	}
}

func containsSubstring(s, substr string) bool {
	return len(s) >= len(substr) && findSubstring(s, substr)
}

func findSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
