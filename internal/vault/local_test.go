package vault_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/ALRubinger/aileron/internal/crypto"
	"github.com/ALRubinger/aileron/internal/vault"
)

// vault.Init / vault.Unlock contract:
//
//   - Init on a missing path: creates the vault, writes salt +
//     verification blob + version=1, returns an EncryptedVault.
//   - Init on an existing vault: ErrVaultExists.
//   - Unlock with the right passphrase: returns the vault.
//   - Unlock with wrong passphrase: ErrPassphraseRejected.
//   - Unlock with tampered verification blob: ErrVaultTampered.
//   - Unlock on missing file: ErrVaultMissing.
//   - OpenOrCreate dispatches to Init when missing, Unlock when ready.
//   - Legacy files (no verification blob) still unlock via the
//     heuristic fallback so existing on-disk vaults aren't broken.

func tmpVaultPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "secrets.json")
}

func TestInit_CreatesVaultWithVerificationBlob(t *testing.T) {
	path := tmpVaultPath(t)
	v, err := vault.Init(path, "correct horse battery staple")
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if v == nil {
		t.Fatal("Init returned nil vault")
	}
	// File must exist with version=1, non-empty salt + verification.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var data struct {
		Version      int    `json:"version"`
		Salt         []byte `json:"salt"`
		Verification []byte `json:"verification"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if data.Version != vault.CurrentFileFormatVersion {
		t.Errorf("version = %d, want %d", data.Version, vault.CurrentFileFormatVersion)
	}
	if len(data.Salt) == 0 {
		t.Error("salt is empty")
	}
	if len(data.Verification) == 0 {
		t.Error("verification blob is empty")
	}
}

func TestInit_RejectsExistingVault(t *testing.T) {
	path := tmpVaultPath(t)
	if _, err := vault.Init(path, "first"); err != nil {
		t.Fatalf("first init: %v", err)
	}
	_, err := vault.Init(path, "second")
	if !errors.Is(err, vault.ErrVaultExists) {
		t.Errorf("err = %v, want ErrVaultExists", err)
	}
}

func TestInit_RejectsEmptyPassphrase(t *testing.T) {
	if _, err := vault.Init(tmpVaultPath(t), ""); err == nil {
		t.Error("expected error on empty passphrase")
	}
}

func TestUnlock_AcceptsCorrectPassphrase(t *testing.T) {
	path := tmpVaultPath(t)
	if _, err := vault.Init(path, "p@ss"); err != nil {
		t.Fatalf("Init: %v", err)
	}
	v, err := vault.Unlock(path, "p@ss")
	if err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	if v == nil {
		t.Fatal("Unlock returned nil vault")
	}
}

func TestUnlock_RejectsWrongPassphrase(t *testing.T) {
	path := tmpVaultPath(t)
	if _, err := vault.Init(path, "right"); err != nil {
		t.Fatalf("Init: %v", err)
	}
	_, err := vault.Unlock(path, "wrong")
	if !errors.Is(err, vault.ErrPassphraseRejected) {
		t.Errorf("err = %v, want ErrPassphraseRejected", err)
	}
}

func TestUnlock_RejectsTamperedVerification(t *testing.T) {
	path := tmpVaultPath(t)
	if _, err := vault.Init(path, "p"); err != nil {
		t.Fatalf("Init: %v", err)
	}
	// Replace verification with a blob that decrypts to something
	// other than VerificationPlaintext under the same KEK.
	fv, err := vault.NewFileVault(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	salt, _ := fv.Salt()
	kek, _ := crypto.DeriveKEK([]byte("p"), salt)
	tampered, _ := crypto.Encrypt([]byte("not-aileron-vault"), kek)
	// Overwrite the file directly with an alternate verification blob.
	raw, _ := os.ReadFile(path)
	var data map[string]any
	json.Unmarshal(raw, &data)
	data["verification"] = tampered
	out, _ := json.Marshal(data)
	os.WriteFile(path, out, 0o600)

	_, err = vault.Unlock(path, "p")
	if !errors.Is(err, vault.ErrVaultTampered) {
		t.Errorf("err = %v, want ErrVaultTampered", err)
	}
}

func TestUnlock_MissingFile(t *testing.T) {
	_, err := vault.Unlock(filepath.Join(t.TempDir(), "nope.json"), "p")
	if !errors.Is(err, vault.ErrVaultMissing) {
		t.Errorf("err = %v, want ErrVaultMissing", err)
	}
}

func TestUnlock_AcceptsLegacyVaultWithMatchingHeuristic(t *testing.T) {
	// Build a v0 file by hand: salt + one secret encrypted under a
	// known passphrase, but no verification blob. Simulates a vault
	// created before ADR-0011 landed.
	path := tmpVaultPath(t)
	fv, err := vault.NewFileVault(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	salt, _ := fv.Salt()
	kek, _ := crypto.DeriveKEK([]byte("legacy"), salt)
	ev, _ := vault.NewEncryptedVault(fv, kek)
	if err := ev.Put(context.Background(), "github/token", []byte("ghp_xxx"), vault.Metadata{}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// Right passphrase: the heuristic decrypts the existing secret.
	if _, err := vault.Unlock(path, "legacy"); err != nil {
		t.Errorf("legacy Unlock with correct passphrase: %v", err)
	}
	// Wrong passphrase: rejected.
	if _, err := vault.Unlock(path, "wrong"); !errors.Is(err, vault.ErrPassphraseRejected) {
		t.Errorf("legacy Unlock with wrong passphrase: %v, want ErrPassphraseRejected", err)
	}
}

func TestUnlock_AcceptsEmptyLegacyVault(t *testing.T) {
	// A legacy vault file with a salt but no secrets and no
	// verification: any passphrase is accepted. The next write will
	// install a verification blob.
	path := tmpVaultPath(t)
	fv, err := vault.NewFileVault(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := fv.Salt(); err != nil {
		t.Fatalf("salt: %v", err)
	}
	if _, err := vault.Unlock(path, "anything"); err != nil {
		t.Errorf("empty legacy vault should accept any passphrase: %v", err)
	}
}

func TestOpenOrCreate_DispatchesByState(t *testing.T) {
	path := tmpVaultPath(t)

	// First call: creates.
	v, created, err := vault.OpenOrCreate(path, "p")
	if err != nil {
		t.Fatalf("first OpenOrCreate: %v", err)
	}
	if !created {
		t.Error("first call should report created=true")
	}
	if v == nil {
		t.Error("vault is nil")
	}

	// Second call: unlocks.
	v2, created, err := vault.OpenOrCreate(path, "p")
	if err != nil {
		t.Fatalf("second OpenOrCreate: %v", err)
	}
	if created {
		t.Error("second call should report created=false")
	}
	if v2 == nil {
		t.Error("vault is nil")
	}

	// Third call with wrong passphrase: returns the unlock error.
	if _, _, err := vault.OpenOrCreate(path, "different"); !errors.Is(err, vault.ErrPassphraseRejected) {
		t.Errorf("OpenOrCreate with wrong passphrase: %v, want ErrPassphraseRejected", err)
	}
}

func TestCheckState(t *testing.T) {
	path := tmpVaultPath(t)
	if got, _ := vault.CheckState(path); got != vault.StateMissing {
		t.Errorf("missing path state = %v, want StateMissing", got)
	}
	if _, err := vault.Init(path, "p"); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if got, _ := vault.CheckState(path); got != vault.StateReady {
		t.Errorf("after Init state = %v, want StateReady", got)
	}
}

func TestCheckState_LegacyVaultReportsLegacy(t *testing.T) {
	// A vault file with a salt + secrets but no verification blob
	// is the pre-ADR-0011 shape. CheckState must report StateLegacy
	// so callers know to migrate (or accept the heuristic fallback).
	path := tmpVaultPath(t)
	fv, err := vault.NewFileVault(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	salt, _ := fv.Salt()
	kek, _ := crypto.DeriveKEK([]byte("legacy"), salt)
	ev, _ := vault.NewEncryptedVault(fv, kek)
	if err := ev.Put(context.Background(), "x", []byte("v"), vault.Metadata{}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := vault.CheckState(path)
	if err != nil {
		t.Fatalf("CheckState: %v", err)
	}
	if got != vault.StateLegacy {
		t.Errorf("state = %v, want StateLegacy", got)
	}
}

func TestInit_RejectsEmptyPassphrase_BeforeStateCheck(t *testing.T) {
	// Empty passphrase fails fast — never touches disk.
	if _, err := vault.Init(tmpVaultPath(t), ""); err == nil {
		t.Error("expected error on empty passphrase")
	}
}

func TestUnlock_RejectsEmptyPassphrase(t *testing.T) {
	path := tmpVaultPath(t)
	if _, err := vault.Init(path, "p"); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if _, err := vault.Unlock(path, ""); err == nil {
		t.Error("expected error on empty passphrase")
	}
}

func TestFileVault_HasContents(t *testing.T) {
	// HasContents reports whether the file has any data on disk.
	// New file: false. After salt is generated: true. Used by
	// future migration helpers to detect "is this a fresh path or
	// an existing vault?"
	path := tmpVaultPath(t)
	fv, err := vault.NewFileVault(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if fv.HasContents() {
		t.Error("fresh vault HasContents=true, want false")
	}
	if _, err := fv.Salt(); err != nil {
		t.Fatalf("Salt: %v", err)
	}
	if !fv.HasContents() {
		t.Error("after Salt, HasContents=false")
	}
}

func TestFileVault_SetVerification_RefusesOverwrite(t *testing.T) {
	// Once a verification blob is set, the FileVault refuses to
	// overwrite it — overwriting would silently invalidate every
	// existing encrypted secret.
	path := tmpVaultPath(t)
	if _, err := vault.Init(path, "p"); err != nil {
		t.Fatalf("Init: %v", err)
	}
	fv, _ := vault.NewFileVault(path)
	if err := fv.SetVerification([]byte("anything")); err == nil {
		t.Error("SetVerification should refuse to overwrite an existing blob")
	}
}

func TestInit_OnLegacyFile_UpgradesIt(t *testing.T) {
	// Calling Init on a legacy file (salt + secrets, no verification)
	// is allowed — it stamps the verification blob in place. The
	// existing secrets become readable only with the same passphrase.
	path := tmpVaultPath(t)
	fv, err := vault.NewFileVault(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := fv.Salt(); err != nil {
		t.Fatalf("Salt: %v", err)
	}
	// State is now Legacy (salt set, no verification, no secrets).
	if got, _ := vault.CheckState(path); got != vault.StateLegacy {
		t.Fatalf("preconditon: state = %v, want StateLegacy", got)
	}
	v, err := vault.Init(path, "p")
	if err != nil {
		t.Fatalf("Init on legacy: %v", err)
	}
	if v == nil {
		t.Fatal("Init returned nil")
	}
	// After Init, the file is StateReady.
	if got, _ := vault.CheckState(path); got != vault.StateReady {
		t.Errorf("after Init state = %v, want StateReady", got)
	}
}
