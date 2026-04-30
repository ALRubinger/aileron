package launch

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/ALRubinger/aileron/internal/crypto"
	"github.com/ALRubinger/aileron/internal/vault"
)

// tamperFileVerification rewrites the verification blob inside the
// vault file at path with one that decrypts (under the same KEK as
// the supplied passphrase) to a string OTHER than VerificationPlaintext.
// Used to simulate a tampered vault for ErrVaultTampered tests.
func tamperFileVerification(t *testing.T, path, passphrase string) {
	t.Helper()
	fv, err := vault.NewFileVault(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	salt, _ := fv.Salt()
	kek, err := crypto.DeriveKEK([]byte(passphrase), salt)
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	tampered, err := crypto.Encrypt([]byte("not-aileron-vault"), kek)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatalf("decode: %v", err)
	}
	data["verification"] = tampered
	out, _ := json.Marshal(data)
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
}
