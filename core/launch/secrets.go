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

// ValidateTokenRef checks that a token value is either empty or a vault
// reference. Plaintext tokens are rejected to prevent secrets from being
// committed to version control in aileron.yaml.
func ValidateTokenRef(field, value string) error {
	if value == "" || IsVaultRef(value) {
		return nil
	}
	return fmt.Errorf("%s contains a plaintext token — use 'aileron secret set <name>' and reference it as 'vault:<name>' instead", field)
}

// OpenVaultFunc is the function used to open the vault when vault
// references are found in token fields. Defaults to prompting for a
// passphrase on /dev/tty. Replaceable in tests.
var OpenVaultFunc = promptAndOpenVault

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

// PromptVaultWithPanel shows a Panel-based passphrase prompt with retry.
// The user can enter their passphrase, see errors on wrong input, and
// retry or press Esc to skip. Returns nil if the user skips.
func PromptVaultWithPanel(copier *OutputCopier, router *KeyRouter, bar *StatusBar) vault.Vault {
	inputCh := router.StealInput()
	defer router.ReleaseInput()

	copier.SetPaused(true)
	defer copier.SetPaused(false)

	termRows, termCols := 24, 80
	if bar != nil {
		termRows, termCols = bar.Dims()
	}

	var passBuf []byte
	var errMsg string

	for {
		panel := NewPanel(PanelConfig{
			Title:      "✈️ Aileron ─ Vault",
			InnerWidth: 74,
			Centered:   true,
		}, termRows, termCols)

		var content []string
		content = append(content, panel.BlankLine())
		content = append(content, panel.PadLine("  Enter your vault passphrase to unlock notifications."))
		content = append(content, panel.BlankLine())
		masked := strings.Repeat("*", len(passBuf))
		content = append(content, panel.PadLine("  Passphrase: "+masked+"\033[7m \033[0m"))
		content = append(content, panel.BlankLine())
		if errMsg != "" {
			content = append(content, panel.PadLine("  \033[31m"+errMsg+"\033[0m"))
			content = append(content, panel.BlankLine())
		}
		content = append(content, panel.PadLine("  \033[2mEnter to unlock  Esc to skip\033[0m"))
		content = append(content, panel.BlankLine())

		screen := panel.RenderToAltScreen(content)
		copier.WriteExclusive([]byte(screen))

		// Read one byte at a time.
		b := <-inputCh
		switch {
		case b == 0x1B: // Esc — skip vault
			copier.WriteExclusive([]byte("\033[?1049l"))
			return nil
		case b == '\r' || b == '\n': // Enter — try unlock
			if len(passBuf) == 0 {
				errMsg = "Passphrase cannot be empty."
				continue
			}
			v, err := OpenLocalVault(DefaultVaultPath(), string(passBuf))
			if err != nil {
				passBuf = passBuf[:0]
				if strings.Contains(err.Error(), "decryption failed") {
					errMsg = "Wrong passphrase. Try again or press Esc to skip."
				} else {
					errMsg = err.Error()
				}
				continue
			}
			// Success — restore screen and return.
			copier.WriteExclusive([]byte("\033[?1049l"))
			return v
		case b == 0x7F || b == 0x08: // Backspace
			if len(passBuf) > 0 {
				passBuf = passBuf[:len(passBuf)-1]
			}
		default:
			if b >= 0x20 && b < 0x7F { // printable ASCII
				passBuf = append(passBuf, b)
			}
		}
	}
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
