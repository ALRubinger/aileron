package launch

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/ALRubinger/aileron/core/crypto"
	"github.com/ALRubinger/aileron/core/vault"
	"golang.org/x/term"
)

const vaultPrefix = "vault:"

// DefaultVaultPath returns the default vault file path (~/.aileron/secrets.json).
func DefaultVaultPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".aileron", "secrets.json")
	}
	return filepath.Join(home, ".aileron", "secrets.json")
}

// OpenLocalVault opens the local encrypted vault, deriving a KEK from the
// passphrase and the salt stored in the vault file. If the vault file
// doesn't exist yet, it is created with a fresh salt.
func OpenLocalVault(vaultPath, passphrase string) (vault.Vault, error) {
	fv, err := vault.NewFileVault(vaultPath)
	if err != nil {
		return nil, fmt.Errorf("opening vault: %w", err)
	}

	salt, err := fv.Salt()
	if err != nil {
		return nil, fmt.Errorf("vault salt: %w", err)
	}

	kek, err := crypto.DeriveKEK([]byte(passphrase), salt)
	if err != nil {
		return nil, fmt.Errorf("deriving key: %w", err)
	}

	ev, err := vault.NewEncryptedVault(fv, kek)
	if err != nil {
		return nil, fmt.Errorf("creating encrypted vault: %w", err)
	}
	return ev, nil
}

// IsVaultRef returns true if the value is a vault reference (starts with "vault:").
func IsVaultRef(value string) bool {
	return strings.HasPrefix(value, vaultPrefix)
}

// promptAndOpenVault prompts the user for a vault passphrase on /dev/tty
// and opens the local encrypted vault.
func promptAndOpenVault(w io.Writer) (vault.Vault, error) {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("cannot open /dev/tty for passphrase prompt: %w", err)
	}
	defer tty.Close()

	fmt.Fprint(w, "aileron: vault passphrase: ")
	passphrase, err := term.ReadPassword(int(tty.Fd()))
	fmt.Fprintln(w) // newline after hidden input
	if err != nil {
		return nil, fmt.Errorf("reading passphrase: %w", err)
	}
	if len(passphrase) == 0 {
		return nil, fmt.Errorf("passphrase cannot be empty")
	}

	return OpenLocalVault(DefaultVaultPath(), string(passphrase))
}

// ResolveVaultRef resolves a value that may be a vault reference. If it
// starts with "vault:", the remainder is looked up in the vault. Otherwise
// the value is returned as-is.
func ResolveVaultRef(value string, v vault.Vault) (string, error) {
	if !IsVaultRef(value) {
		return value, nil
	}
	name := strings.TrimPrefix(value, vaultPrefix)
	secret, err := v.Get(context.Background(), name)
	if err != nil {
		return "", fmt.Errorf("resolving vault:%s: %w", name, err)
	}
	return string(secret.Value), nil
}
