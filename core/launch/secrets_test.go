package launch_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/core/launch"
	"github.com/ALRubinger/aileron/core/vault"
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

	v1, _ := launch.OpenLocalVault(path, "correct")
	v1.Put(context.Background(), "token", []byte("value"), vault.Metadata{})

	v2, _ := launch.OpenLocalVault(path, "wrong")
	_, err := v2.Get(context.Background(), "token")
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
