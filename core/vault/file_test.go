package vault

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/ALRubinger/aileron/core/crypto"
)

func TestFileVault_RoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.json")
	fv, err := NewFileVault(path)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	err = fv.Put(ctx, "slack/bot_token", []byte("xoxb-test"), Metadata{Type: "bot_token"})
	if err != nil {
		t.Fatal(err)
	}

	got, err := fv.Get(ctx, "slack/bot_token")
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Value) != "xoxb-test" {
		t.Errorf("expected 'xoxb-test', got %q", got.Value)
	}
	if got.Metadata.Type != "bot_token" {
		t.Errorf("expected metadata type 'bot_token', got %q", got.Metadata.Type)
	}
}

func TestFileVault_Persistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.json")
	ctx := context.Background()

	// Write with first instance.
	fv1, _ := NewFileVault(path)
	fv1.Put(ctx, "my-secret", []byte("val"), Metadata{})

	// Read with second instance.
	fv2, err := NewFileVault(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := fv2.Get(ctx, "my-secret")
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Value) != "val" {
		t.Errorf("expected 'val', got %q", got.Value)
	}
}

func TestFileVault_Delete(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.json")
	ctx := context.Background()

	fv, _ := NewFileVault(path)
	fv.Put(ctx, "to-delete", []byte("val"), Metadata{})
	fv.Delete(ctx, "to-delete")

	_, err := fv.Get(ctx, "to-delete")
	if !IsNotFound(err) {
		t.Errorf("expected not found after delete, got %v", err)
	}
}

func TestFileVault_NotFound(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.json")
	fv, _ := NewFileVault(path)

	_, err := fv.Get(context.Background(), "nonexistent")
	if !IsNotFound(err) {
		t.Errorf("expected not found error, got %v", err)
	}
}

func TestFileVault_Salt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.json")
	fv, _ := NewFileVault(path)

	salt1, err := fv.Salt()
	if err != nil {
		t.Fatal(err)
	}
	if len(salt1) < crypto.SaltLen {
		t.Errorf("expected salt length >= %d, got %d", crypto.SaltLen, len(salt1))
	}

	// Second call returns same salt.
	salt2, _ := fv.Salt()
	if string(salt1) != string(salt2) {
		t.Error("salt should be stable across calls")
	}

	// Salt persists across instances.
	fv2, _ := NewFileVault(path)
	salt3, _ := fv2.Salt()
	if string(salt1) != string(salt3) {
		t.Error("salt should persist across instances")
	}
}

func TestFileVault_Names(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.json")
	fv, _ := NewFileVault(path)
	ctx := context.Background()

	fv.Put(ctx, "a", []byte("1"), Metadata{})
	fv.Put(ctx, "b", []byte("2"), Metadata{})

	names := fv.Names()
	if len(names) != 2 {
		t.Fatalf("expected 2 names, got %d", len(names))
	}
}

func TestFileVault_NewFileCreatesDir(t *testing.T) {
	path := filepath.Join(t.TempDir(), "subdir", "secrets.json")
	fv, err := NewFileVault(path)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	err = fv.Put(ctx, "test", []byte("val"), Metadata{})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(path); err != nil {
		t.Errorf("expected file to exist at %s", path)
	}
}

func TestFileVault_WithEncryptedVault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.json")
	fv, _ := NewFileVault(path)

	kek := testKEK(t)
	ev, err := NewEncryptedVault(fv, kek)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	err = ev.Put(ctx, "secret", []byte("plaintext-value"), Metadata{Type: "token"})
	if err != nil {
		t.Fatal(err)
	}

	// Read back through encrypted vault — should be plaintext.
	got, err := ev.Get(ctx, "secret")
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Value) != "plaintext-value" {
		t.Errorf("expected 'plaintext-value', got %q", got.Value)
	}

	// Read raw from file vault — should be ciphertext (not plaintext).
	raw, _ := fv.Get(ctx, "secret")
	if string(raw.Value) == "plaintext-value" {
		t.Error("raw value should be encrypted, not plaintext")
	}
}

func TestFileVault_MalformedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.json")
	os.WriteFile(path, []byte("not json"), 0o600)

	_, err := NewFileVault(path)
	if err == nil {
		t.Error("expected error for malformed file")
	}
}

func TestFileVault_FlushErrorReadOnlyDir(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "readonly", "secrets.json")
	os.MkdirAll(filepath.Join(dir, "readonly"), 0o555)
	t.Cleanup(func() { os.Chmod(filepath.Join(dir, "readonly"), 0o755) })

	fv, _ := NewFileVault(path)
	// Put should fail because the directory is read-only.
	err := fv.Put(context.Background(), "test", []byte("val"), Metadata{})
	if err == nil {
		t.Error("expected error writing to read-only directory")
	}
}

func TestErrNotFound_ErrorMessage(t *testing.T) {
	v := NewMemVault()
	_, err := v.Get(context.Background(), "my/secret/path")
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	if msg != "vault: secret not found: my/secret/path" {
		t.Errorf("unexpected error message: %q", msg)
	}
}

func TestIsNotFound(t *testing.T) {
	if IsNotFound(nil) {
		t.Error("nil should not be NotFound")
	}
	if IsNotFound(os.ErrNotExist) {
		t.Error("os.ErrNotExist should not match IsNotFound")
	}
	v := NewMemVault()
	_, err := v.Get(context.Background(), "missing")
	if !IsNotFound(err) {
		t.Error("MemVault missing key should be NotFound")
	}
}
