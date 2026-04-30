package launch

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/ALRubinger/aileron/internal/vault"
	"golang.org/x/term"
)

// PassphrasePrompter reads a passphrase from the user. The default
// implementation hides input on the controlling tty; tests inject a
// scripted prompter.
type PassphrasePrompter func(prompt string, w io.Writer) (string, error)

// EnsureVault returns an unlocked [vault.Vault] for the local vault
// at path, prompting the user for a passphrase. On first launch
// (file missing), the user is prompted twice — passphrase and
// confirmation — before the vault is created. On subsequent launches
// the user is prompted once, with retry on wrong passphrase up to
// `maxRetries` times. A tampered vault never retries: the function
// returns immediately.
//
// The KEK derived from the passphrase is held inside the returned
// vault for the lifetime of the process; nothing is logged or written
// to disk beyond what the existing FileVault already persists.
//
// Per ADR-0011, this is the v1 entry point that `aileron launch`
// calls before any agent runs. The runtime will not accept chat
// completion requests until [vault.Vault] is unlocked (the gate is
// enforced inside the gateway server when constructed via
// [app.NewHandlerWithConfig]).
func EnsureVault(path string, prompter PassphrasePrompter, w io.Writer, maxRetries int) (vault.Vault, error) {
	if prompter == nil {
		prompter = defaultPassphrasePrompter
	}
	if maxRetries < 1 {
		maxRetries = 3
	}

	state, err := vault.CheckState(path)
	if err != nil {
		return nil, fmt.Errorf("checking vault: %w", err)
	}

	if state == vault.StateMissing {
		return createNewVault(path, prompter, w)
	}
	return unlockExistingVault(path, prompter, w, maxRetries)
}

// createNewVault runs the first-launch flow: prompt for a passphrase,
// confirm, and call vault.Init. Mismatched passphrases retry the
// prompt; an empty passphrase aborts.
func createNewVault(path string, prompter PassphrasePrompter, w io.Writer) (vault.Vault, error) {
	fmt.Fprintln(w, "aileron: no vault found at", path)
	fmt.Fprintln(w, "aileron: creating one — choose a passphrase you'll remember.")
	for attempt := 0; attempt < 3; attempt++ {
		p1, err := prompter("vault passphrase: ", w)
		if err != nil {
			return nil, fmt.Errorf("reading passphrase: %w", err)
		}
		if p1 == "" {
			return nil, fmt.Errorf("passphrase cannot be empty")
		}
		p2, err := prompter("confirm passphrase: ", w)
		if err != nil {
			return nil, fmt.Errorf("reading passphrase: %w", err)
		}
		if p1 != p2 {
			fmt.Fprintln(w, "aileron: passphrases do not match — try again.")
			continue
		}
		v, err := vault.Init(path, p1)
		if err != nil {
			return nil, fmt.Errorf("creating vault: %w", err)
		}
		fmt.Fprintln(w, "aileron: vault created.")
		return v, nil
	}
	return nil, fmt.Errorf("passphrase confirmation failed after 3 attempts")
}

// unlockExistingVault prompts for the passphrase, calling vault.Unlock
// with retry on wrong passphrase up to maxRetries times. Tampered
// vaults exit immediately without retry — the file is corrupt and
// re-prompting cannot recover.
func unlockExistingVault(path string, prompter PassphrasePrompter, w io.Writer, maxRetries int) (vault.Vault, error) {
	for attempt := 1; attempt <= maxRetries; attempt++ {
		p, err := prompter("vault passphrase: ", w)
		if err != nil {
			return nil, fmt.Errorf("reading passphrase: %w", err)
		}
		v, err := vault.Unlock(path, p)
		if err == nil {
			return v, nil
		}
		if errors.Is(err, vault.ErrVaultTampered) {
			return nil, err
		}
		if !errors.Is(err, vault.ErrPassphraseRejected) {
			return nil, err
		}
		remaining := maxRetries - attempt
		if remaining > 0 {
			fmt.Fprintf(w, "aileron: wrong passphrase (%d %s left).\n", remaining, plural("attempt", remaining))
			continue
		}
		return nil, fmt.Errorf("vault unlock failed after %d attempts", maxRetries)
	}
	// Unreachable; the loop returns explicitly above.
	return nil, fmt.Errorf("vault unlock loop exited without resolution")
}

// defaultPassphrasePrompter reads from /dev/tty without echoing.
// Used as the fallback when the caller doesn't inject a prompter.
func defaultPassphrasePrompter(prompt string, w io.Writer) (string, error) {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return "", fmt.Errorf("cannot open /dev/tty: %w", err)
	}
	defer tty.Close()
	fmt.Fprint(w, "aileron: ", prompt)
	pw, err := term.ReadPassword(int(tty.Fd()))
	fmt.Fprintln(w)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(pw), "\r\n"), nil
}

func plural(word string, n int) string {
	if n == 1 {
		return word
	}
	return word + "s"
}
