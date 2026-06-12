package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/ALRubinger/aileron/internal/launch"
	"github.com/ALRubinger/aileron/internal/vault"
)

// splitPathArg separates the single positional path argument (the
// first non-flag token) from the flag tokens so the verbs accept the
// path either before or after their flags. Go's flag package stops at
// the first non-flag arg, so without this reorder
// `vault put agents/x/oauth --from-file f` would never parse the flag.
// Returns the path, the remaining flag args, and false if there is not
// exactly one positional argument.
func splitPathArg(args []string) (path string, flagArgs []string, ok bool) {
	flagArgs = make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			flagArgs = append(flagArgs, a)
			// A flag that takes a value in `--flag value` form keeps
			// its value token attached. `--flag=value` is self-contained.
			if !strings.Contains(a, "=") && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				// Only --from-file takes a separate value among these
				// verbs; --yes/--json are booleans. Treat the next token
				// as a value only for --from-file to avoid swallowing the
				// path arg after a boolean flag.
				if a == "--from-file" || a == "-from-file" {
					flagArgs = append(flagArgs, args[i+1])
					i++
				}
			}
			continue
		}
		if path != "" {
			return "", nil, false // more than one positional arg
		}
		path = a
	}
	if path == "" {
		return "", nil, false
	}
	return path, flagArgs, true
}

// Environment variable that supplies the vault passphrase non-
// interactively. Honored by every command that would otherwise prompt.
// Documented in #492 item 5c as the CI / scripts escape hatch.
const envVaultPassphrase = "AILERON_VAULT_PASSPHRASE"

const vaultUsage = `usage:
  aileron vault init [--passphrase-file <path>]
  aileron vault put agents/<name>/oauth --from-file <path>
  aileron vault delete agents/<name>/oauth [--yes]
  aileron vault list [--prefix agents/] [--json]`

// runVault dispatches `aileron vault <subcommand>`.
//
// init opens the local vault file directly (first-run flow); the
// put/delete/list verbs are thin daemon-backed HTTP clients scoped to
// the `agents/<name>/oauth` namespace per ADR-0025 — they never open
// the vault file themselves and reject any non-agent path client-side.
func runVault(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, vaultUsage)
		return 1
	}
	switch args[0] {
	case "init":
		return runVaultInit(args[1:], stdout, stderr)
	case "put":
		return runVaultPut(args[1:], stdout, stderr)
	case "delete":
		return runVaultDelete(args[1:], stdin, stdout, stderr)
	case "list":
		return runVaultList(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown vault command: %q\n", args[0])
		fmt.Fprintln(stderr, vaultUsage)
		return 1
	}
}

// agentOAuthPathName validates that arg matches the agents-only
// namespace `agents/<name>/oauth` and returns <name>. The vault
// put/delete verbs are namespace-locked: they refuse any other path
// client-side before issuing an HTTP call, so the operator CLI can
// never reach a non-agent vault key (ADR-0025).
func agentOAuthPathName(arg string) (string, error) {
	const prefix = "agents/"
	const suffix = "/oauth"
	if !strings.HasPrefix(arg, prefix) || !strings.HasSuffix(arg, suffix) {
		return "", fmt.Errorf("path must be agents/<name>/oauth (got %q)", arg)
	}
	name := arg[len(prefix) : len(arg)-len(suffix)]
	if name == "" || strings.Contains(name, "/") {
		return "", fmt.Errorf("path must be agents/<name>/oauth (got %q)", arg)
	}
	return name, nil
}

// agentCredentialsBody is the local minimal subset of the
// api.AgentCredentials wire shape the put verb marshals. Defined here
// so cmd/aileron does not depend on internal/api/gen (matching the
// bindingRow precedent). encoding/json base64-encodes the []byte Value
// field, matching the spec's `format: byte` declaration.
type agentCredentialsBody struct {
	Value []byte `json:"value"`
}

// agentSummary is the local subset of api.AgentCredentialSummary the
// list verb decodes. Notably it has no Value field — the daemon never
// returns credential bytes from the list endpoint (ADR-0011).
type agentSummary struct {
	Name     string `json:"name"`
	Metadata *struct {
		Type        string            `json:"type,omitempty"`
		Environment string            `json:"environment,omitempty"`
		Labels      map[string]string `json:"labels,omitempty"`
	} `json:"metadata,omitempty"`
}

// runVaultPut stores a credential envelope read verbatim from a file
// at agents/<name>/oauth via the daemon. The file bytes are stored
// as-is (whole-file read, no trailing-newline munging) since agent
// credential files (Claude's .credentials.json, Codex's auth.json) are
// exact-byte artifacts.
func runVaultPut(args []string, stdout, stderr io.Writer) int {
	pathArg, flagArgs, ok := splitPathArg(args)
	if !ok {
		fmt.Fprintln(stderr, "usage: aileron vault put agents/<name>/oauth --from-file <path>")
		return 1
	}
	flags := flag.NewFlagSet("vault put", flag.ContinueOnError)
	flags.SetOutput(stderr)
	fromFile := flags.String("from-file", "", "Read the credential bytes verbatim from the named file")
	flags.Bool("yes", false, "Accepted for symmetry with delete; put never prompts")
	if err := flags.Parse(flagArgs); err != nil {
		return 1
	}
	name, err := agentOAuthPathName(pathArg)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	if *fromFile == "" {
		fmt.Fprintln(stderr, "error: --from-file <path> is required")
		return 1
	}
	data, err := os.ReadFile(*fromFile)
	if err != nil {
		fmt.Fprintf(stderr, "error reading %q: %v\n", *fromFile, err)
		return 1
	}
	if len(data) == 0 {
		fmt.Fprintf(stderr, "error: %q is empty\n", *fromFile)
		return 1
	}

	body, err := json.Marshal(agentCredentialsBody{Value: data})
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	status, respBody, err := vaultDoRequest(http.MethodPut,
		"/vault/agents/"+name+"/credentials", body)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	switch status {
	case http.StatusNoContent:
		fmt.Fprintf(stdout, "Stored agents/%s/oauth\n", name)
		return 0
	case http.StatusLocked:
		fmt.Fprintln(stderr, "error: vault is locked; unlock it first")
		return 1
	case http.StatusServiceUnavailable:
		fmt.Fprintln(stderr, "error: daemon is not configured with a vault")
		return 1
	default:
		fmt.Fprintf(stderr, "server returned %d: %s\n", status, string(respBody))
		return 1
	}
}

// runVaultDelete removes the credential envelope at agents/<name>/oauth
// via the daemon. Unless --yes is passed it confirms interactively;
// this CLI prompt is the only human gate (the daemon applies no
// approval block for operator vault management per the plan).
func runVaultDelete(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	pathArg, flagArgs, ok := splitPathArg(args)
	if !ok {
		fmt.Fprintln(stderr, "usage: aileron vault delete agents/<name>/oauth [--yes]")
		return 1
	}
	flags := flag.NewFlagSet("vault delete", flag.ContinueOnError)
	flags.SetOutput(stderr)
	yes := flags.Bool("yes", false, "Skip the confirmation prompt")
	if err := flags.Parse(flagArgs); err != nil {
		return 1
	}
	name, err := agentOAuthPathName(pathArg)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}

	if !*yes {
		answer := promptLine(stdin, stdout, fmt.Sprintf("Delete agents/%s/oauth? [y/N]: ", name))
		if !strings.EqualFold(answer, "y") && !strings.EqualFold(answer, "yes") {
			fmt.Fprintln(stdout, "cancelled")
			return 0
		}
	}

	status, respBody, err := vaultDoRequest(http.MethodDelete,
		"/vault/agents/"+name+"/credentials", nil)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	switch status {
	case http.StatusNoContent:
		fmt.Fprintf(stdout, "Deleted agents/%s/oauth\n", name)
		return 0
	case http.StatusNotFound:
		fmt.Fprintf(stderr, "error: no credential entry for agent %s\n", name)
		return 1
	case http.StatusLocked:
		fmt.Fprintln(stderr, "error: vault is locked; unlock it first")
		return 1
	case http.StatusServiceUnavailable:
		fmt.Fprintln(stderr, "error: daemon is not configured with a vault")
		return 1
	default:
		fmt.Fprintf(stderr, "server returned %d: %s\n", status, string(respBody))
		return 1
	}
}

// runVaultList prints the agent credential entries the daemon holds.
// Output mirrors `aileron secret list`: one name per line by default,
// or NDJSON (one entry per line) with --json. Only the agents/ prefix
// is supported; any other --prefix is rejected with a clear error.
func runVaultList(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("vault list", flag.ContinueOnError)
	flags.SetOutput(stderr)
	prefix := flags.String("prefix", "", "Restrict to a path prefix (only agents/ is supported)")
	asJSON := flags.Bool("json", false, "Render entries as NDJSON, one entry per line")
	if err := flags.Parse(args); err != nil {
		return 1
	}
	if *prefix != "" && *prefix != "agents/" {
		fmt.Fprintf(stderr, "error: only the agents/ prefix is supported (got %q)\n", *prefix)
		return 1
	}

	status, respBody, err := vaultDoRequest(http.MethodGet, "/vault/agents", nil)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	switch status {
	case http.StatusOK:
		// handled below
	case http.StatusServiceUnavailable:
		fmt.Fprintln(stderr, "error: daemon is not configured with a vault")
		return 1
	default:
		fmt.Fprintf(stderr, "server returned %d: %s\n", status, string(respBody))
		return 1
	}

	var out struct {
		Agents []agentSummary `json:"agents"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		fmt.Fprintf(stderr, "error: decode response: %v\n", err)
		return 1
	}

	if len(out.Agents) == 0 {
		if *asJSON {
			return 0
		}
		fmt.Fprintln(stdout, "No agent credentials stored.")
		return 0
	}

	if *asJSON {
		enc := json.NewEncoder(stdout)
		for _, a := range out.Agents {
			if err := enc.Encode(a); err != nil {
				fmt.Fprintf(stderr, "encode: %v\n", err)
				return 1
			}
		}
		return 0
	}

	for _, a := range out.Agents {
		fmt.Fprintln(stdout, a.Name)
	}
	return 0
}

// vaultDoRequest is a thin daemon-backed HTTP-client helper mirroring
// approvalDoRequest: it resolves the base URL via bindingAPIBaseURL
// (honoring AILERON_API_URL) and attaches the daemon authorization
// header. Returns the status and full body so callers map codes.
func vaultDoRequest(method, path string, body []byte) (int, []byte, error) {
	base, err := bindingAPIBaseURL()
	if err != nil {
		return 0, nil, err
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, base+path, reader)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	setDaemonAuthorization(req)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, out, nil
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

// printAileronBanner prints the Aileron ASCII-art welcome shown at the
// top of any interactive vault create/unlock prompt. Callers gate this
// on [willPromptInteractively] so non-interactive callers (env var,
// --passphrase-file) don't get a banner dumped into their logs.
func printAileronBanner(w io.Writer) {
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, `░█▀█░▀█▀░█░░░█▀▀░█▀▄░█▀█░█▀█`)
	fmt.Fprintln(w, `░█▀█░░█░░█░░░█▀▀░█▀▄░█░█░█░█`)
	fmt.Fprintln(w, `░▀░▀░▀▀▀░▀▀▀░▀▀▀░▀░▀░▀▀▀░▀░▀`)
	fmt.Fprintln(w, "")
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
