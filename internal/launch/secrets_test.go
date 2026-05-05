package launch_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/launch"
	"github.com/ALRubinger/aileron/internal/vault"
)

func TestIsVaultRef(t *testing.T) {
	if !launch.IsVaultRef("vault:slack_bot_token") {
		t.Error("expected true for vault: prefix")
	}
	if launch.IsVaultRef("xoxb-plain-token") {
		t.Error("expected false for plain value")
	}
	if launch.IsVaultRef("") {
		t.Error("expected false for empty string")
	}
}

func TestResolveVaultRef_PlainValue(t *testing.T) {
	v := vault.NewMemVault()
	got, err := launch.ResolveVaultRef("plain-token", v)
	if err != nil {
		t.Fatal(err)
	}
	if got != "plain-token" {
		t.Errorf("expected 'plain-token', got %q", got)
	}
}

func TestResolveVaultRef_VaultLookup(t *testing.T) {
	v := vault.NewMemVault()
	v.Put(context.Background(), "slack_bot_token", []byte("xoxb-secret"), vault.Metadata{})

	got, err := launch.ResolveVaultRef("vault:slack_bot_token", v)
	if err != nil {
		t.Fatal(err)
	}
	if got != "xoxb-secret" {
		t.Errorf("expected 'xoxb-secret', got %q", got)
	}
}

func TestResolveVaultRef_NotFound(t *testing.T) {
	v := vault.NewMemVault()
	_, err := launch.ResolveVaultRef("vault:missing", v)
	if err == nil {
		t.Error("expected error for missing vault secret")
	}
}

func TestOpenLocalVault_RoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.json")
	passphrase := "test-passphrase"

	// Store a secret.
	v1, err := launch.OpenLocalVault(path, passphrase)
	if err != nil {
		t.Fatal(err)
	}
	err = v1.Put(context.Background(), "my-token", []byte("secret-value"), vault.Metadata{})
	if err != nil {
		t.Fatal(err)
	}

	// Retrieve with same passphrase.
	v2, err := launch.OpenLocalVault(path, passphrase)
	if err != nil {
		t.Fatal(err)
	}
	got, err := v2.Get(context.Background(), "my-token")
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Value) != "secret-value" {
		t.Errorf("expected 'secret-value', got %q", got.Value)
	}
}

func TestOpenLocalVault_WrongPassphrase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.json")

	v1, err := launch.OpenLocalVault(path, "correct")
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	v1.Put(context.Background(), "token", []byte("value"), vault.Metadata{})

	// Opening with the wrong passphrase should fail immediately now that
	// the vault validates the passphrase against existing secrets.
	_, err = launch.OpenLocalVault(path, "wrong")
	if err == nil {
		t.Error("expected error with wrong passphrase")
	}
}

func TestValidateTokenRef_VaultRef(t *testing.T) {
	err := launch.ValidateTokenRef("slack.bot_token", "vault:my_token")
	if err != nil {
		t.Errorf("vault ref should be valid, got %v", err)
	}
}

func TestValidateTokenRef_Empty(t *testing.T) {
	err := launch.ValidateTokenRef("slack.bot_token", "")
	if err != nil {
		t.Errorf("empty should be valid, got %v", err)
	}
}

func TestValidateTokenRef_Plaintext(t *testing.T) {
	err := launch.ValidateTokenRef("slack.bot_token", "xoxb-real-token-here")
	if err == nil {
		t.Fatal("expected error for plaintext token")
	}
	if !strings.Contains(err.Error(), "plaintext token") {
		t.Errorf("expected 'plaintext token' in error, got %q", err.Error())
	}
	if !strings.Contains(err.Error(), "slack.bot_token") {
		t.Errorf("expected field name in error, got %q", err.Error())
	}
}

func TestResolveTokens_PlainValues(t *testing.T) {
	resolved, err := launch.ResolveTokens([]string{"token-a", "token-b"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resolved[0] != "token-a" || resolved[1] != "token-b" {
		t.Errorf("expected plain values unchanged, got %v", resolved)
	}
}

func TestResolveTokens_VaultRefs(t *testing.T) {
	v := vault.NewMemVault()
	v.Put(context.Background(), "slack_app", []byte("xapp-resolved"), vault.Metadata{})
	v.Put(context.Background(), "slack_bot", []byte("xoxb-resolved"), vault.Metadata{})

	resolved, err := launch.ResolveTokens([]string{"vault:slack_app", "vault:slack_bot"}, v)
	if err != nil {
		t.Fatal(err)
	}
	if resolved[0] != "xapp-resolved" || resolved[1] != "xoxb-resolved" {
		t.Errorf("expected resolved values, got %v", resolved)
	}
}

func TestResolveTokens_MixedRefs(t *testing.T) {
	v := vault.NewMemVault()
	v.Put(context.Background(), "my_token", []byte("secret"), vault.Metadata{})

	resolved, err := launch.ResolveTokens([]string{"", "vault:my_token"}, v)
	if err != nil {
		t.Fatal(err)
	}
	if resolved[0] != "" || resolved[1] != "secret" {
		t.Errorf("expected ['', 'secret'], got %v", resolved)
	}
}

func TestResolveTokens_VaultRefWithNilVault(t *testing.T) {
	_, err := launch.ResolveTokens([]string{"vault:missing"}, nil)
	if err == nil {
		t.Error("expected error when vault is nil but ref exists")
	}
}

func TestResolveTokens_VaultRefNotFound(t *testing.T) {
	v := vault.NewMemVault()
	_, err := launch.ResolveTokens([]string{"vault:nonexistent"}, v)
	if err == nil {
		t.Error("expected error for missing vault secret")
	}
}

func TestResolveTokens_Empty(t *testing.T) {
	resolved, err := launch.ResolveTokens(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(resolved) != 0 {
		t.Errorf("expected empty, got %v", resolved)
	}
}

func TestOpenLocalVault_EmptyPassphrase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.json")
	_, err := launch.OpenLocalVault(path, "")
	if err == nil {
		t.Error("expected error for empty passphrase")
	}
}

func TestOpenLocalVault_BadPath(t *testing.T) {
	_, err := launch.OpenLocalVault("/dev/null/impossible/secrets.json", "pass")
	if err == nil {
		t.Error("expected error for bad path")
	}
}

func TestDefaultVaultPath(t *testing.T) {
	path := launch.DefaultVaultPath()
	if path == "" {
		t.Error("expected non-empty path")
	}
	if !filepath.IsAbs(path) {
		// May not be absolute if HOME is unset, but should at least contain the filename.
		if filepath.Base(path) != "secrets.json" {
			t.Errorf("expected secrets.json, got %q", filepath.Base(path))
		}
	}
}

