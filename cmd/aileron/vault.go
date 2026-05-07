package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/ALRubinger/aileron/internal/launch"
	"github.com/ALRubinger/aileron/internal/vault"
)

// Environment variable that supplies the vault passphrase non-
// interactively. Honored by every command that would otherwise prompt.
// Documented in #492 item 5c as the CI / scripts escape hatch.
const envVaultPassphrase = "AILERON_VAULT_PASSPHRASE"

const vaultUsage = `usage:
  aileron vault init [--passphrase-file <path>]`

// runVault dispatches `aileron vault <subcommand>`.
func runVault(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, vaultUsage)
		return 1
	}
	switch args[0] {
	case "init":
		return runVaultInit(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown vault command: %q\n", args[0])
		fmt.Fprintln(stderr, vaultUsage)
		return 1
	}
}

// runVaultInit creates a new local file vault at the canonical path.
// Errors out (without overwriting) if a vault already exists. This is
// the deliberate first-run flow per #492 item 5d — the alternative to
// letting `secret set` or `binding setup` create the vault implicitly.
//
// Passphrase source order, per #492 item 5c:
//  1. --passphrase-file <path>  (read once, no confirmation needed)
//  2. AILERON_VAULT_PASSPHRASE  (read once, no confirmation needed)
//  3. interactive prompt + confirmation
func runVaultInit(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("vault init", flag.ContinueOnError)
	flags.SetOutput(stderr)
	passphraseFile := flags.String("passphrase-file", "",
		"Read the passphrase from the named file instead of prompting (no trailing newline)")
	if err := flags.Parse(args); err != nil {
		return 1
	}

	vaultPath := launch.DefaultVaultPath()

	state, err := vault.CheckState(vaultPath)
	if err != nil {
		fmt.Fprintf(stderr, "error checking vault: %v\n", err)
		return 1
	}
	if state == vault.StateReady {
		fmt.Fprintf(stderr, "vault already exists at %s\n", vaultPath)
		fmt.Fprintln(stderr, "delete the file first if you intend to start over (this destroys all stored secrets).")
		return 1
	}

	// Print the new-vault banner BEFORE the first prompt so the user
	// reads the irretrievable-passphrase warning before choosing one.
	// File/env sources are non-interactive (CI, scripts) and don't need
	// the warning — gate on willPromptInteractively, not on the source
	// returned by readVaultPassphrase (which only resolves after the
	// prompt fires).
	if willPromptInteractively(*passphraseFile) {
		printNewVaultBanner(stderr, vaultPath)
	}
	passphrase, source, err := readVaultPassphrase(*passphraseFile, "Vault passphrase: ", stderr)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	if passphrase == "" {
		fmt.Fprintln(stderr, "error: passphrase cannot be empty")
		return 1
	}

	// Confirmation is only meaningful for interactive entry: when the
	// passphrase comes from a file or env, re-reading the same source
	// would just confirm itself, so we skip the second prompt.
	if source == passphraseSourceInteractive {
		confirm, _, err := readVaultPassphrase("", "Confirm passphrase: ", stderr)
		if err != nil {
			fmt.Fprintf(stderr, "error: %v\n", err)
			return 1
		}
		if passphrase != confirm {
			fmt.Fprintln(stderr, "error: passphrases do not match")
			return 1
		}
	}

	if _, err := vault.Init(vaultPath, passphrase); err != nil {
		if errors.Is(err, vault.ErrVaultExists) {
			fmt.Fprintf(stderr, "vault already exists at %s\n", vaultPath)
			return 1
		}
		fmt.Fprintf(stderr, "error creating vault: %v\n", err)
		return 1
	}

	fmt.Fprintf(stdout, "Vault created at %s\n", vaultPath)
	return 0
}

// passphraseSource indicates which source produced a passphrase; used by
// callers to decide whether to require confirmation.
type passphraseSource int

const (
	passphraseSourceInteractive passphraseSource = iota
	passphraseSourceFile
	passphraseSourceEnv
)

// readVaultPassphrase resolves a passphrase from (in order):
//  1. The supplied file path, if non-empty.
//  2. The AILERON_VAULT_PASSPHRASE env var.
//  3. An interactive prompt on /dev/tty.
//
// Returns the passphrase and the source it came from. Trailing CR/LF is
// stripped from file/env values so common shell idioms (heredocs,
// `echo > file`) Just Work.
func readVaultPassphrase(passphraseFile, prompt string, w io.Writer) (string, passphraseSource, error) {
	if passphraseFile != "" {
		data, err := os.ReadFile(passphraseFile)
		if err != nil {
			return "", passphraseSourceFile, fmt.Errorf("reading passphrase file %q: %w", passphraseFile, err)
		}
		return strings.TrimRight(string(data), "\r\n"), passphraseSourceFile, nil
	}
	if env := os.Getenv(envVaultPassphrase); env != "" {
		return env, passphraseSourceEnv, nil
	}
	pass, err := promptPassphrase(prompt, w)
	if err != nil {
		return "", passphraseSourceInteractive, err
	}
	return pass, passphraseSourceInteractive, nil
}

// willPromptInteractively reports whether the next readVaultPassphrase
// call would fall through to /dev/tty. Mirrors readVaultPassphrase's
// dispatch order — file > env > interactive — so callers can decide
// whether to print user-facing context (e.g. the new-vault banner)
// BEFORE the prompt fires, instead of inferring interactivity from
// the post-hoc passphraseSource return value.
func willPromptInteractively(passphraseFile string) bool {
	return passphraseFile == "" && os.Getenv(envVaultPassphrase) == ""
}

// printNewVaultBanner prints the irretrievable-passphrase warning. Same
// language as runSecretSet's inline first-run banner — kept in sync so
// users see the same warning regardless of which path created the vault.
func printNewVaultBanner(w io.Writer, vaultPath string) {
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "  Creating a new Aileron vault.")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "  The passphrase you choose protects all secrets in this vault.")
	fmt.Fprintln(w, "  It is never stored, transmitted, or recoverable. No one can")
	fmt.Fprintln(w, "  read it, tell you what it is, or help you retrieve it.")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "  If you lose this passphrase, you must delete the vault file")
	fmt.Fprintf(w, "  (%s) and re-add all secrets.\n", vaultPath)
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "  Store this passphrase securely. Do not share it.")
	fmt.Fprintln(w, "")
}
