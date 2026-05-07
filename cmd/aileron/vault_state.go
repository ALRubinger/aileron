package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ALRubinger/aileron/internal/daemon/discovery"
	"github.com/ALRubinger/aileron/internal/launch"
	"github.com/ALRubinger/aileron/internal/vault"
)

// ensureVaultUnlocked is the four-state CLI vault flow per #492 item 5.
// Called by run() before any vault-relevant command, it makes sure the
// daemon is running with an unlocked vault by the time the dispatched
// command issues its first daemon request. The state matrix:
//
//	Daemon  | Vault file | Vault unlocked | CLI behavior
//	--------+------------+----------------+--------------
//	running | exists     | yes            | pass-through, no prompt
//	running | exists     | no             | prompt → POST /v1/vault/unlock
//	stopped | exists     | n/a            | prompt → spawn → POST /v1/vault/unlock
//	stopped | missing    | n/a            | prompt+confirm → vault.Init → spawn → unlock
//
// Honors AILERON_VAULT_PASSPHRASE / --passphrase-file at every prompt
// point via [readVaultPassphrase] (PR #500). Bypass for vault-irrelevant
// commands is enforced upstream by [bypassesVault]; this helper assumes
// it would not have been called for one of those.
//
// The "passphraseFile" parameter is empty in the top-level integration
// because flag parsing belongs to the dispatched command. The env var
// path remains active either way.
func ensureVaultUnlocked(passphraseFile string, stderr io.Writer) error {
	stateDir, err := defaultStateDir()
	if err != nil {
		return err
	}

	info, derr := discoveryReadFn(stateDir)
	daemonRunning := derr == nil

	if daemonRunning {
		locked, ok := probeLocalVaultLocked(info.URL)
		if !ok {
			// Daemon doesn't expose the local-vault surface (cloud-shape
			// or older binary). Nothing to do — let the dispatched
			// command try and surface whatever error it would naturally.
			return nil
		}
		if !locked {
			return nil
		}
		return promptAndUnlockRunning(info.URL, passphraseFile, stderr)
	}

	// Daemon not running: classify by vault state.
	vaultPath := launch.DefaultVaultPath()
	state, err := vaultCheckStateFn(vaultPath)
	if err != nil {
		return fmt.Errorf("checking vault: %w", err)
	}

	if state == vault.StateMissing {
		return promptCreateAndUnlock(vaultPath, passphraseFile, stderr)
	}
	return promptAndSpawnUnlock(passphraseFile, stderr)
}

// promptAndUnlockRunning is the "daemon running, vault locked" branch.
// Prompts (or reads non-interactive source) and POSTs /v1/vault/unlock.
// Retries once on a 401 for interactive entry — file/env sources don't
// retry because the source isn't going to change between attempts.
func promptAndUnlockRunning(daemonURL, passphraseFile string, stderr io.Writer) error {
	for attempt := 0; attempt < 2; attempt++ {
		passphrase, source, err := readVaultPassphrase(passphraseFile, "Vault passphrase: ", stderr)
		if err != nil {
			return err
		}
		if passphrase == "" {
			return errors.New("vault passphrase cannot be empty")
		}
		err = postVaultUnlockFn(daemonURL, passphrase)
		if err == nil {
			return nil
		}
		if !errors.Is(err, errVaultUnlockRejected) || source != passphraseSourceInteractive {
			return err
		}
		fmt.Fprintln(stderr, "aileron: wrong passphrase — try again.")
	}
	return errVaultUnlockRejected
}

// promptCreateAndUnlock is the first-run branch: no daemon, no vault
// file. Prompts for a new passphrase + confirmation, writes the vault,
// then spawns the daemon and unlocks it. Equivalent in effect to
// `aileron vault init && aileron daemon start && /v1/vault/unlock`,
// but folded into one prompt so a fresh user doesn't have to learn
// three commands before their first action.
func promptCreateAndUnlock(vaultPath, passphraseFile string, stderr io.Writer) error {
	passphrase, source, err := readVaultPassphrase(passphraseFile, "Vault passphrase: ", stderr)
	if err != nil {
		return err
	}
	if passphrase == "" {
		return errors.New("vault passphrase cannot be empty")
	}
	if source == passphraseSourceInteractive {
		printNewVaultBanner(stderr, vaultPath)
		confirm, _, err := readVaultPassphrase("", "Confirm passphrase: ", stderr)
		if err != nil {
			return err
		}
		if passphrase != confirm {
			return errors.New("passphrases do not match")
		}
	}
	if _, err := vaultInitFn(vaultPath, passphrase); err != nil {
		return fmt.Errorf("creating vault: %w", err)
	}
	url, err := spawnResolveCached()
	if err != nil {
		return fmt.Errorf("spawning daemon: %w", err)
	}
	return postVaultUnlockFn(strings.TrimSuffix(url, "/v1"), passphrase)
}

// promptAndSpawnUnlock is the "daemon stopped, vault file present"
// branch. Prompts, spawns the daemon (which starts vault-locked under
// selectVault's no-TTY path), then POSTs /v1/vault/unlock.
func promptAndSpawnUnlock(passphraseFile string, stderr io.Writer) error {
	for attempt := 0; attempt < 2; attempt++ {
		passphrase, source, err := readVaultPassphrase(passphraseFile, "Vault passphrase: ", stderr)
		if err != nil {
			return err
		}
		if passphrase == "" {
			return errors.New("vault passphrase cannot be empty")
		}
		url, err := spawnResolveCached()
		if err != nil {
			return fmt.Errorf("spawning daemon: %w", err)
		}
		err = postVaultUnlockFn(strings.TrimSuffix(url, "/v1"), passphrase)
		if err == nil {
			return nil
		}
		if !errors.Is(err, errVaultUnlockRejected) || source != passphraseSourceInteractive {
			return err
		}
		fmt.Fprintln(stderr, "aileron: wrong passphrase — try again.")
	}
	return errVaultUnlockRejected
}

// errVaultUnlockRejected is the sentinel for an HTTP 401 from
// /v1/vault/unlock — the supplied passphrase didn't derive the KEK.
var errVaultUnlockRejected = errors.New("vault unlock rejected: wrong passphrase")

// postVaultUnlock POSTs the passphrase to the daemon's
// /v1/vault/unlock endpoint. Returns nil on 200 or 409 (already
// unlocked is benign for our purposes), errVaultUnlockRejected on 401,
// and a wrapped error otherwise.
func postVaultUnlock(daemonURL, passphrase string) error {
	body, err := json.Marshal(map[string]string{"passphrase": passphrase})
	if err != nil {
		return fmt.Errorf("marshal unlock body: %w", err)
	}
	req, err := http.NewRequest(http.MethodPost, daemonURL+"/v1/vault/unlock", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build unlock request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("posting unlock: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK, http.StatusConflict:
		return nil
	case http.StatusUnauthorized:
		return errVaultUnlockRejected
	default:
		out, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("daemon returned %d: %s", resp.StatusCode, string(out))
	}
}

// commandsBypassingVault is the bypass allowlist for #492 item 5a.
// The state machine fires for commands that talk to the daemon (and
// therefore need an unlocked vault by the time their first request
// lands). Commands that operate on local files only — vault file,
// audit log, aileron.yaml — short-circuit the flow. The issue spec
// guessed "4–5 commands"; the actual list is larger because the CLI
// has more local-file affordances than anticipated.
var commandsBypassingVault = map[string]bool{
	"version":   true,
	"--version": true,
	"-v":        true,
	"help":      true,
	"--help":    true,
	"-h":        true,
	"init":      true, // scaffolds aileron.yaml — local file
	"log":       true, // reads local shell-audit JSONL log
	"stop":      true, // alias for `daemon stop` — signals existing daemon
}

// commandsBypassingVaultPair handles two-token bypass paths. Includes
// commands that manipulate the vault file *directly* (vault init,
// secret set/list) — they don't go through the daemon, so the state
// machine would either be redundant or would race the file write.
// `daemon start` bypasses because selectVault now errors if the vault
// file is missing — the operator should `aileron vault init` first
// rather than have the state machine create one mid-`daemon start`.
var commandsBypassingVaultPair = map[string]bool{
	"vault init":    true,
	"secret set":    true,
	"secret list":   true,
	"daemon status": true,
	"daemon stop":   true,
	"daemon start":  true,
	"policy test":   true,
	"policy save":   true,
}

// bypassesVault reports whether the given args correspond to a command
// that should skip the four-state vault flow. Caller is responsible for
// running the state machine when this returns false.
func bypassesVault(args []string) bool {
	if len(args) == 0 {
		// No command → usage; never prompt.
		return true
	}
	if commandsBypassingVault[args[0]] {
		return true
	}
	if len(args) >= 2 && commandsBypassingVaultPair[args[0]+" "+args[1]] {
		return true
	}
	return false
}

// Test seams. ensureVaultUnlocked is exercised end-to-end with these
// swapped to fakes so tests don't fork-exec a daemon, hit a real HTTP
// endpoint, or write to ~/.aileron/secrets.json.
var (
	discoveryReadFn   = discovery.Read
	vaultCheckStateFn = vault.CheckState
	vaultInitFn       = vault.Init
	postVaultUnlockFn = postVaultUnlock
)
