package launch

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/ALRubinger/aileron/internal/crypto"
	"github.com/ALRubinger/aileron/internal/vault"
	"golang.org/x/term"
)

const vaultPrefix = "vault:"

// DefaultVaultPath returns the default vault file path (~/.aileron/secrets.json).
// Replaceable in tests.
var DefaultVaultPath = func() string {
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

	// Validate the passphrase by trying to decrypt an existing secret.
	// This prevents adding secrets with a wrong passphrase, which would
	// leave the vault in a state where no single passphrase can decrypt
	// all secrets.
	if names := fv.Names(); len(names) > 0 {
		if _, err := ev.Get(context.Background(), names[0]); err != nil {
			return nil, fmt.Errorf("vault: decrypting secret at %q: %w", names[0], err)
		}
	}

	return ev, nil
}

// IsVaultRef returns true if the value is a vault reference (starts with "vault:").
func IsVaultRef(value string) bool {
	return strings.HasPrefix(value, vaultPrefix)
}

// ValidateTokenRef checks that a token value is either empty or a vault
// reference. Plaintext tokens are rejected to prevent secrets from being
// committed to version control alongside source.
func ValidateTokenRef(field, value string) error {
	if value == "" || IsVaultRef(value) {
		return nil
	}
	return fmt.Errorf("%s contains a plaintext token — use 'aileron secret set <name>' and reference it as 'vault:secret/<name>' instead", field)
}

// OpenVaultFunc is the function used to open the vault when vault
// references are found in token fields. Defaults to prompting for a
// passphrase on the controlling terminal. Replaceable in tests.
var OpenVaultFunc = promptAndOpenVault

func promptAndOpenVault(w io.Writer) (vault.Vault, error) {
	tty, err := openControllingTerminal()
	if err != nil {
		return nil, fmt.Errorf("cannot open terminal for passphrase prompt: %w", err)
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


// ResolveTokens resolves a slice of token values that may contain vault
// references. Returns the resolved values in the same order. If any token
// is a vault reference, the provided vault is used for lookups. If v is
// nil and vault references exist, an error is returned.
func ResolveTokens(tokens []string, v vault.Vault) ([]string, error) {
	resolved := make([]string, len(tokens))
	for i, tok := range tokens {
		if !IsVaultRef(tok) {
			resolved[i] = tok
			continue
		}
		if v == nil {
			return nil, fmt.Errorf("vault reference %q requires a vault", tok)
		}
		val, err := ResolveVaultRef(tok, v)
		if err != nil {
			return nil, err
		}
		resolved[i] = val
	}
	return resolved, nil
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
