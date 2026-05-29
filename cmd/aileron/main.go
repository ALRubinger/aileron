package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ALRubinger/aileron/internal/config"
	"github.com/ALRubinger/aileron/internal/cstore"
	"github.com/ALRubinger/aileron/internal/daemon/discovery"
	"github.com/ALRubinger/aileron/internal/daemon/spawn"
	"github.com/ALRubinger/aileron/internal/launch"
	"github.com/ALRubinger/aileron/internal/launch/agents"
	"github.com/ALRubinger/aileron/internal/oauth"
	"github.com/ALRubinger/aileron/internal/sandbox/spawnhelper"
	"github.com/ALRubinger/aileron/internal/suite"
	"github.com/ALRubinger/aileron/internal/vault"
	"github.com/ALRubinger/aileron/internal/version"
	"golang.org/x/term"
)

func main() {
	// Spawn-helper dispatch (ADR-0014, Linux platform sandbox).
	// When the runtime re-execs into this binary with the hidden
	// __spawn-helper marker, this process is acting as the in-namespace
	// helper for a wrapped CLI: it applies Landlock restrictions and
	// then `syscall.Exec`s into the wrapped program. Must run BEFORE
	// any normal-CLI setup so the helper does not touch the vault,
	// launcher registry, or any other host state that would leak
	// across the namespace boundary.
	if len(os.Args) >= 3 && os.Args[1] == spawnhelper.HelperArgvMarker {
		spawnhelper.Run(os.Args[2])
		// spawnhelper.Run never returns on success; if we reach
		// here it has already exited with a structured error code.
		return
	}

	registry := launch.NewRegistry()
	registry.Register(agents.Claude{})
	registry.Register(agents.Pi{})
	registry.Register(agents.Codex{})
	registry.Register(agents.Goose{})
	registry.Register(agents.OpenCode{})
	os.Exit(run(os.Args[1:], registry, os.Stdout, os.Stderr))
}

// run executes the CLI and returns an exit code.
func run(args []string, registry *launch.Registry, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stdout, registry)
		return 1
	}

	// CLI vault state machine (#492 item 5). Runs before any vault-
	// relevant command so the dispatched handler sees a daemon with
	// an unlocked vault. The bypass allowlist short-circuits this for
	// commands that don't talk to the daemon (version/help) or don't
	// need the vault to do their job (daemon status/stop, vault init).
	if !bypassesVault(args) {
		if err := ensureVaultUnlockedFn("", stderr); err != nil {
			fmt.Fprintf(stderr, "aileron: %v\n", err)
			return 1
		}
	}

	switch args[0] {
	case "version", "--version", "-v":
		fmt.Fprintf(stdout, "aileron %s (%s)\n", version.Version, version.Commit)
		return 0
	case "launch":
		// Parse aileron-level flags before the agent name.
		launchFlags := flag.NewFlagSet("launch", flag.ContinueOnError)
		launchFlags.SetOutput(stderr)
		logLevel := launchFlags.String("log-level", "info", "Log level: trace, debug, info, warn, error")
		if err := launchFlags.Parse(args[1:]); err != nil {
			return 1
		}
		launchArgs := launchFlags.Args()

		if len(launchArgs) < 1 {
			fmt.Fprintln(stderr, "usage: aileron launch [--log-level=<level>] <agent> [args...]")
			fmt.Fprintf(stderr, "agents: %s\n", strings.Join(registry.Names(), ", "))
			return 1
		}
		agentName := launchArgs[0]
		agent, ok := registry.Get(agentName)
		if !ok {
			fmt.Fprintf(stderr, "unknown agent: %q\n", agentName)
			fmt.Fprintf(stderr, "available agents: %s\n", strings.Join(registry.Names(), ", "))
			return 1
		}

		// Capture cwd so the daemon's session record carries working_dir
		// (rendered by `aileron sessions list`). os.Getwd basically never
		// errors in practice; on the rare case it does, fall through with
		// Dir="" — the launcher and daemon already tolerate that.
		cwd, _ := os.Getwd()

		result, err := launchFn(context.Background(), launch.LaunchConfig{
			Agent:    agent,
			Args:     launchArgs[1:],
			Dir:      cwd,
			LogLevel: launch.ParseLogLevel(*logLevel),
		})
		if err != nil {
			fmt.Fprintf(stderr, "error: %v\n", err)
			return 1
		}
		return result.ExitCode
	case "secret":
		return runSecret(args[1:], stdout, stderr)
	case "binding":
		return runBinding(args[1:], os.Stdin, stdout, stderr)
	case "connector":
		return runConnector(args[1:], os.Stdin, stdout, stderr)
	case "action":
		return runAction(args[1:], os.Stdin, stdout, stderr)
	case "keyring":
		return runKeyring(args[1:], stdout, stderr)
	case "hub":
		return runHub(args[1:], stdout, stderr)
	case "approval":
		return runApproval(args[1:], stdout, stderr)
	case "status":
		return runStatus(args[1:], stdout, stderr)
	case "sync":
		return runSync(args[1:], os.Stdin, stdout, stderr)
	case "audit":
		return runAudit(args[1:], stdout, stderr)
	case "sessions":
		return runSessions(args[1:], stdout, stderr)
	case "sandbox":
		return runSandbox(args[1:], stdout, stderr)
	case "vault":
		return runVault(args[1:], stdout, stderr)
	case "daemon":
		return runDaemon(args[1:], stdout, stderr)
	case "stop":
		// Alias for `aileron daemon stop` per ADR-0012.
		return runDaemonStop(args[1:], stdout, stderr)
	case "open":
		return runOpen(args[1:], stdout, stderr)
	case "help", "--help", "-h":
		usage(stdout, registry)
		return 0
	default:
		fmt.Fprintf(stderr, "unknown command: %q\n", args[0])
		usage(stderr, registry)
		return 1
	}
}

func usage(w io.Writer, registry *launch.Registry) {
	fmt.Fprintln(w, "aileron — the execution layer for AI coding agents")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "usage:")
	fmt.Fprintln(w, "  aileron launch <agent> [args...]   Launch an AI coding agent connected to the Aileron daemon")
	fmt.Fprintln(w, "  aileron vault init                 Create the local encrypted vault with a passphrase")
	fmt.Fprintln(w, "  aileron secret set <name>          Store a secret in the encrypted vault")
	fmt.Fprintln(w, "  aileron secret list                List stored secret names")
	fmt.Fprintln(w, "  aileron binding list               List credential bindings (metadata only — no unlock)")
	fmt.Fprintln(w, "  aileron connector install <FQN>    Install a connector binary from its FQN")
	fmt.Fprintln(w, "  aileron connector check            Check installed connectors for newer versions")
	fmt.Fprintln(w, "  aileron action add <FQN>           Install an action template from its FQN")
	fmt.Fprintln(w, "  aileron action run <NAME>          Invoke an installed action directly")
	fmt.Fprintln(w, "  aileron keyring trust <auth> <key> Authorize a publisher's signing key for installs")
	fmt.Fprintln(w, "  aileron keyring list               List trusted publishers and key fingerprints")
	fmt.Fprintln(w, "  aileron keyring revoke <auth>      Remove a publisher's keys from the trust list")
	fmt.Fprintln(w, "  aileron hub list                   List connectors published to the Hub")
	fmt.Fprintln(w, "  aileron hub search <query>         Search Hub connectors by keyword")
	fmt.Fprintln(w, "  aileron hub show <FQN>             Show a single Hub entry by FQN")
	fmt.Fprintln(w, "  aileron approval list              List pending action-approval requests")
	fmt.Fprintln(w, "  aileron approval approve <id>      Approve a pending action — agent's tool call unblocks")
	fmt.Fprintln(w, "  aileron approval deny <id>         Deny a pending action — agent receives approval_denied")
	fmt.Fprintln(w, "  aileron status [section]           Show daemon runtime + config (runtime, notifications, vault)")
	fmt.Fprintln(w, "  aileron sync [--bind-all] [--yes]  Reconcile installed actions: install missing connectors; report unbound capabilities")
	fmt.Fprintln(w, "  aileron audit [list|show]          View the action-execution audit log (ADR-0010)")
	fmt.Fprintln(w, "  aileron sessions [list|get]        View `aileron launch` session records (ADR-0012)")
	fmt.Fprintln(w, "  aileron sandbox [init|plan|build]  Scaffold, inspect, or build sandbox images")
	fmt.Fprintln(w, "  aileron daemon start|stop|status   Manage the local Aileron daemon (auto-spawned on demand)")
	fmt.Fprintln(w, "  aileron stop                       Alias for 'aileron daemon stop'")
	fmt.Fprintln(w, "  aileron open [approvals|approval <id>]  Open the Aileron webapp (default: root) in your browser")
	fmt.Fprintln(w, "  aileron version                    Print version information")
	fmt.Fprintln(w, "  aileron help                       Show this help")
	fmt.Fprintln(w)
	fmt.Fprintf(w, "agents: %s\n", strings.Join(registry.Names(), ", "))
}

// runSecret handles the "aileron secret" subcommands.
func runSecret(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: aileron secret <set|list>")
		return 1
	}

	switch args[0] {
	case "set":
		return runSecretSet(args[1:], stdout, stderr)
	case "list":
		return runSecretList(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown secret command: %q\n", args[0])
		fmt.Fprintln(stderr, "usage: aileron secret <set|list>")
		return 1
	}
}

// runSecretSet stores a secret in the local vault.
func runSecretSet(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("secret set", flag.ContinueOnError)
	flags.SetOutput(stderr)
	passphraseFile := flags.String("passphrase-file", "",
		"Read the vault passphrase from the named file instead of prompting (no trailing newline)")
	if err := flags.Parse(args); err != nil {
		return 1
	}
	rest := flags.Args()
	if len(rest) < 1 {
		fmt.Fprintln(stderr, "usage: aileron secret set [--passphrase-file <path>] <name>")
		return 1
	}
	name := rest[0]

	// Check if this is a brand-new vault (no existing secrets).
	vaultPath := launch.DefaultVaultPath()
	isNewVault := true
	if fv, err := vault.NewFileVault(vaultPath); err == nil {
		isNewVault = len(fv.Names()) == 0
	}

	if isNewVault {
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

	// For a new vault, require confirmation. Confirmation only fires
	// for interactive entry — file/env sources are taken at face
	// value (re-reading the same file/env var would just confirm
	// itself).
	if isNewVault && source == passphraseSourceInteractive {
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

	fmt.Fprintf(stderr, "Value for new secret, %q: ", name)
	value, err := promptPassphrase("", nil) // already printed prompt
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	fmt.Fprintln(stderr) // newline after hidden input

	v, err := launch.OpenLocalVault(vaultPath, passphrase)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}

	if err := v.Put(context.Background(), name, []byte(value), vault.Metadata{Type: "secret"}); err != nil {
		fmt.Fprintf(stderr, "error storing secret: %v\n", err)
		return 1
	}

	fmt.Fprintf(stdout, "Stored secret %q\n", name)
	fmt.Fprintf(stdout, "Use vault:%s in aileron.yaml to reference it.\n", name)
	return 0
}

// runSecretList lists secret names in the vault.
func runSecretList(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("secret list", flag.ContinueOnError)
	flags.SetOutput(stderr)
	asJSON := flags.Bool("json", false, "Render names as NDJSON, one name per line")
	if err := flags.Parse(args); err != nil {
		return 1
	}

	vaultPath := launch.DefaultVaultPath()
	fv, err := vault.NewFileVault(vaultPath)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}

	names := fv.Names()
	if len(names) == 0 {
		if *asJSON {
			fmt.Fprintln(stdout, "[]")
			return 0
		}
		fmt.Fprintln(stdout, "No secrets stored.")
		fmt.Fprintln(stdout, "Run `aileron secret set <name>` to store one.")
		return 0
	}

	if *asJSON {
		enc := json.NewEncoder(stdout)
		for _, name := range names {
			if err := enc.Encode(name); err != nil {
				fmt.Fprintf(stderr, "encode: %v\n", err)
				return 1
			}
		}
		return 0
	}

	for _, name := range names {
		fmt.Fprintln(stdout, name)
	}
	return 0
}

// runBinding handles `aileron binding <subcommand>`. Per ADR-0006 the
// binding lifecycle is exposed as five HTTP endpoints; the CLI is a
// thin client that calls the running aileron server. The default
// server URL is http://localhost:8721/v1, overridable via the
// AILERON_API_URL environment variable.
func runBinding(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, bindingUsage)
		return 1
	}
	// Wrap stdin once so multiple promptLine calls share the same
	// buffered reader; otherwise bufio's read-ahead would drop the
	// trailing input on subsequent prompts.
	br := bufio.NewReader(stdin)
	switch args[0] {
	case "list":
		return runBindingList(args[1:], stdout, stderr)
	case "inspect":
		return runBindingInspect(args[1:], stdout, stderr)
	case "setup":
		return runBindingSetup(args[1:], br, stdout, stderr)
	case "rebind":
		return runBindingRebind(args[1:], br, stdout, stderr)
	case "reauthorize":
		return runBindingReauthorize(args[1:], stdout, stderr)
	case "revoke":
		return runBindingRevoke(args[1:], br, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown binding command: %q\n", args[0])
		fmt.Fprintln(stderr, bindingUsage)
		return 1
	}
}

// bindingBrowser is the [oauth.Opener] the CLI uses to launch the
// user's default browser during the OAuth dance. Package-level so
// tests can swap in a no-op opener without spawning real browser
// windows. Production code reads the default `oauth.SystemBrowser`.
var bindingBrowser oauth.Opener = oauth.SystemBrowser{}

const bindingUsage = `usage:
  aileron binding list        [--connector FQN] [--kind KIND] [--json]
  aileron binding inspect     <name>
  aileron binding setup       <connector-FQN>
  aileron binding rebind      <name>
  aileron binding reauthorize <name>
  aileron binding revoke      <name>`

// bindingAPIBaseURL returns the daemon's API base URL with the /v1
// suffix.
//
// AILERON_API_URL overrides everything (test/dev escape hatch); when
// set, it is returned as-is with the trailing slash trimmed. Read on
// every call so tests that set the env mid-process see the new value.
//
// Otherwise, [spawn.Resolve] auto-spawns the daemon if needed; the
// resulting URL is cached for the lifetime of the CLI process so
// repeat callers don't reprobe.
//
// Returns the spawn error verbatim on failure. Earlier versions
// returned a hardcoded `http://localhost:8721/v1` fallback so the
// caller would fail with "connection refused" — but the legacy port
// has no meaning under ADR-0012 (the daemon binds an ephemeral port
// advertised in `~/.aileron/daemon.json`), and the doomed dial
// produced a misleading second error line that obscured the real
// spawn failure. Callers now surface the spawn error directly.
//
// Declared as a `var` rather than `func` so tests can drive the
// error-propagation path through every caller without going through
// the spawn cache + daemon binary lookup.
var bindingAPIBaseURL = func() (string, error) {
	if u := os.Getenv("AILERON_API_URL"); u != "" {
		return strings.TrimRight(u, "/"), nil
	}
	return spawnResolveCached()
}

var (
	spawnURLOnce  sync.Once
	spawnURLValue string
	spawnURLErr   error
)

// spawnResolveFn is the seam that lets tests substitute spawn.Resolve
// without fork-execing a real daemon binary.
var spawnResolveFn = spawn.Resolve

// launchFn is a package-level seam so tests can assert on the
// LaunchConfig the CLI builds without depending on a running daemon.
var launchFn = launch.Launch

// ensureVaultUnlockedFn is the seam for the run() vault state-machine
// hook. Tests for the dispatched commands swap this to a no-op so they
// don't have to mock out the entire daemon-discovery + unlock chain.
var ensureVaultUnlockedFn = ensureVaultUnlocked

// spawnResolveCached calls spawnResolveOnce at most once per CLI
// process and caches the result. The cache is intentionally
// process-local — bindingAPIBaseURL re-reads AILERON_API_URL on every
// call, so tests can flip behavior without resetting any state here.
func spawnResolveCached() (string, error) {
	spawnURLOnce.Do(func() { spawnURLValue, spawnURLErr = spawnResolveOnce() })
	if spawnURLErr != nil {
		return "", spawnURLErr
	}
	return spawnURLValue, nil
}

// spawnResolveOnce performs the actual spawn.Resolve call. Split out
// from spawnResolveCached so tests can exercise the body without
// fighting the sync.Once's process-scoped state.
func spawnResolveOnce() (string, error) {
	stateDir, err := defaultStateDir()
	if err != nil {
		return "", fmt.Errorf("resolve state dir: %w", err)
	}
	binary, err := daemonBinaryPath()
	if err != nil {
		return "", fmt.Errorf("locate daemon binary: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	raw, err := spawnResolveFn(ctx, spawn.Options{
		StateDir: stateDir,
		Binary:   binary,
	})
	if err != nil {
		return "", err
	}
	return strings.TrimRight(raw, "/") + "/v1", nil
}

// defaultStateDir returns ~/.aileron, the canonical user state
// directory under which discovery files, the vault, sessions, and
// the audit log all live.
func defaultStateDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".aileron"), nil
}

// daemonBinaryPath resolves the daemon binary's path: a sibling of
// the running aileron binary named "aileron-server" — the name used
// by `task build:server` and the goreleaser/Homebrew bundle. Falls
// back to PATH lookup for layouts where os.Executable does not point
// at the same directory the daemon is installed into.
func daemonBinaryPath() (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	candidate := filepath.Join(filepath.Dir(self), "aileron-server")
	if _, err := os.Stat(candidate); err == nil {
		return filepath.Abs(candidate)
	}
	return exec.LookPath("aileron-server")
}

// bindingDoRequest issues an HTTP request to the server and returns
// the parsed body. Status codes are surfaced to callers.
func bindingDoRequest(method, path string, body io.Reader) (int, []byte, error) {
	base, err := bindingAPIBaseURL()
	if err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequest(method, base+path, body)
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

func setDaemonAuthorization(req *http.Request) {
	if token := daemonAuthToken(); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
}

func daemonAuthToken() string {
	if token := os.Getenv("AILERON_TOKEN"); token != "" {
		return token
	}
	stateDir, err := defaultStateDir()
	if err != nil {
		return ""
	}
	info, err := discovery.Read(stateDir)
	if err != nil {
		return ""
	}
	return info.Token
}

// runBindingList renders the user's bindings as a fixed-width table.
// Per ADR-0011, listing does not require the vault to be unlocked;
// the server returns plaintext metadata only.
func runBindingList(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("binding list", flag.ContinueOnError)
	flags.SetOutput(stderr)
	connector := flags.String("connector", "", "filter by connector FQN")
	kind := flags.String("kind", "", "filter by credential kind")
	asJSON := flags.Bool("json", false, "Render rows as NDJSON, one binding per line")
	if err := flags.Parse(args); err != nil {
		return 1
	}
	q := url.Values{}
	if *connector != "" {
		q.Set("connector_fqn", *connector)
	}
	if *kind != "" {
		q.Set("kind", *kind)
	}
	path := "/bindings"
	if encoded := q.Encode(); encoded != "" {
		path += "?" + encoded
	}
	status, body, err := bindingDoRequest(http.MethodGet, path, nil)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	if status != http.StatusOK {
		fmt.Fprintf(stderr, "server returned %d: %s\n", status, string(body))
		return 1
	}
	var resp struct {
		Items []bindingRow `json:"items"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		fmt.Fprintf(stderr, "error parsing response: %v\n", err)
		return 1
	}
	if len(resp.Items) == 0 {
		if *asJSON {
			fmt.Fprintln(stdout, "[]")
			return 0
		}
		fmt.Fprintln(stdout, "No bindings configured.")
		fmt.Fprintln(stdout, "Run `aileron binding setup <connector-FQN>` to add one.")
		return 0
	}
	if *asJSON {
		enc := json.NewEncoder(stdout)
		for _, b := range resp.Items {
			if err := enc.Encode(b); err != nil {
				fmt.Fprintf(stderr, "encode: %v\n", err)
				return 1
			}
		}
		return 0
	}
	fmt.Fprintf(stdout, "%-40s  %-10s  %-30s  %s\n", "NAME", "KIND", "CONNECTOR", "STATUS")
	for _, b := range resp.Items {
		st := b.Status
		if st == "" {
			st = "active"
		}
		// Append the stale reason inline so a stale binding's status
		// is actionable at a glance — e.g. `stale (scope_drift)`.
		if st == "stale" && b.StaleReason != "" {
			st = st + " (" + b.StaleReason + ")"
		}
		fmt.Fprintf(stdout, "%-40s  %-10s  %-30s  %s\n", b.Name, b.Kind, b.ConnectorFQN, st)
	}
	return 0
}

// runBindingInspect prints the full metadata for a single binding.
func runBindingInspect(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "usage: aileron binding inspect <name>")
		return 1
	}
	name := args[0]
	status, body, err := bindingDoRequest(http.MethodGet, "/bindings/"+url.PathEscape(name), nil)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	if status == http.StatusNotFound {
		fmt.Fprintf(stderr, "binding not found: %s\n", name)
		return 1
	}
	if status != http.StatusOK {
		fmt.Fprintf(stderr, "server returned %d: %s\n", status, string(body))
		return 1
	}
	var b bindingRow
	if err := json.Unmarshal(body, &b); err != nil {
		fmt.Fprintf(stderr, "error parsing response: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "Name:       %s\n", b.Name)
	fmt.Fprintf(stdout, "Kind:       %s\n", b.Kind)
	fmt.Fprintf(stdout, "Service:    %s\n", b.Service)
	fmt.Fprintf(stdout, "Identity:   %s\n", b.Identity)
	fmt.Fprintf(stdout, "Connector:  %s\n", b.ConnectorFQN)
	if b.Account != "" {
		fmt.Fprintf(stdout, "Account:    %s\n", b.Account)
	}
	if b.Scope != "" {
		fmt.Fprintf(stdout, "Scope:      %s\n", b.Scope)
	}
	if !b.CreatedAt.IsZero() {
		fmt.Fprintf(stdout, "Created:    %s\n", b.CreatedAt.Format("2006-01-02 15:04:05 UTC"))
	}
	if b.Status == "stale" && b.StaleReason != "" {
		fmt.Fprintf(stdout, "Stale:      %s\n", b.StaleReason)
		if len(b.MissingScopes) > 0 {
			fmt.Fprintf(stdout, "Missing:    %s\n", strings.Join(b.MissingScopes, " "))
		}
		fmt.Fprintf(stdout, "Resolve:    aileron binding reauthorize %s\n", b.Name)
	}
	return 0
}

// runBindingSetup prompts for an identity and dispatches by the
// connector's declared credential kind:
//
//   - api_key: prompts for the key value and POSTs to
//     /v1/bindings/setup.
//   - oauth2: drives the server-side dance — POST init, spin up a
//     loopback callback listener, open the user's browser to the
//     authorize URL, await the callback, POST finish.
//
// Detection is via init-then-fall-through: the OAuth init endpoint
// returns 422 `not_oauth2` for connectors declaring api_key, and the
// CLI falls through to the api_key flow on that signal. One extra
// round trip for api_key connectors; trivial cost for the cleaner
// CLI surface.
func runBindingSetup(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "usage: aileron binding setup <connector-FQN>")
		return 1
	}
	connectorFQN := args[0]
	identity := promptLine(stdin, stdout, "Identity (e.g. work, personal): ")
	if identity == "" {
		fmt.Fprintln(stderr, "identity is required")
		return 1
	}
	// Try OAuth first.
	initBody, _ := json.Marshal(map[string]any{
		"connector_fqn": connectorFQN,
		"identity":      identity,
	})
	initStatus, initRespBody, err := bindingDoRequest(http.MethodPost,
		"/bindings/setup/oauth2/init", strings.NewReader(string(initBody)))
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	switch initStatus {
	case http.StatusOK:
		return runBindingSetupOAuth2Finish(initRespBody, stdout, stderr)
	case http.StatusUnprocessableEntity:
		// Connector isn't oauth2 — fall through to api_key flow.
	default:
		fmt.Fprintf(stderr, "server returned %d: %s\n", initStatus, string(initRespBody))
		return 1
	}
	return runBindingSetupAPIKey(connectorFQN, identity, stdin, stdout, stderr)
}

// runBindingSetupAPIKey is the legacy api_key path: prompt for the
// key bytes, POST /v1/bindings/setup with kind = "api_key".
func runBindingSetupAPIKey(connectorFQN, identity string, stdin io.Reader, stdout, stderr io.Writer) int {
	value := promptLine(stdin, stdout, "API key value: ")
	if value == "" {
		fmt.Fprintln(stderr, "value is required")
		return 1
	}
	body, _ := json.Marshal(map[string]any{
		"connector_fqn": connectorFQN,
		"bindings": []map[string]any{{
			"identity": identity,
			"source":   map[string]any{"kind": "api_key", "value": value},
		}},
	})
	status, respBody, err := bindingDoRequest(http.MethodPost, "/bindings/setup",
		strings.NewReader(string(body)))
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	if status != http.StatusCreated {
		fmt.Fprintf(stderr, "server returned %d: %s\n", status, string(respBody))
		return 1
	}
	var resp struct {
		Created []bindingRow `json:"created"`
		Skipped []string     `json:"skipped"`
	}
	if err := json.Unmarshal(respBody, &resp); err != nil {
		fmt.Fprintf(stderr, "error parsing response: %v\n", err)
		return 1
	}
	for _, b := range resp.Created {
		fmt.Fprintf(stdout, "Created: %s\n", b.Name)
	}
	for _, name := range resp.Skipped {
		fmt.Fprintf(stdout, "Skipped (already bound): %s\n", name)
	}
	return 0
}

// runBindingSetupOAuth2Finish completes a server-driven OAuth dance.
// initRespBody is the body returned from the init endpoint.
//
// Flow:
//
//  1. Parse init response for session_id + authorize_url + redirect_uri.
//  2. Bind a loopback HTTP listener at the redirect URI's port.
//  3. Open the user's browser to the authorize URL.
//  4. Await the callback (code + state).
//  5. POST /v1/bindings/setup/oauth2/finish with the captured code.
//  6. Print the resulting binding name.
func runBindingSetupOAuth2Finish(initRespBody []byte, stdout, stderr io.Writer) int {
	var initResp struct {
		SessionID    string `json:"session_id"`
		AuthorizeURL string `json:"authorize_url"`
		RedirectURI  string `json:"redirect_uri"`
	}
	if err := json.Unmarshal(initRespBody, &initResp); err != nil {
		fmt.Fprintf(stderr, "error parsing init response: %v\n", err)
		return 1
	}
	port := portFromRedirectURI(initResp.RedirectURI)
	if port == 0 {
		fmt.Fprintf(stderr, "could not parse port from redirect_uri %q\n", initResp.RedirectURI)
		return 1
	}
	listener, err := oauth.NewListener(port)
	if err != nil {
		fmt.Fprintf(stderr, "could not bind callback listener on port %d: %v\n", port, err)
		return 1
	}
	defer listener.Close()

	fmt.Fprintln(stdout, "Opening your browser to authorize. If it does not open, visit:")
	fmt.Fprintln(stdout, "  "+initResp.AuthorizeURL)
	if err := bindingBrowser.Open(initResp.AuthorizeURL); err != nil {
		// Non-fatal — user can paste the URL manually.
		fmt.Fprintf(stderr, "(browser open failed; paste the URL above): %v\n", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cb, err := listener.Await(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "OAuth callback error: %v\n", err)
		return 1
	}

	finishBody, _ := json.Marshal(map[string]any{
		"session_id": initResp.SessionID,
		"code":       cb.Code,
		"state":      cb.State,
	})
	status, respBody, err := bindingDoRequest(http.MethodPost,
		"/bindings/setup/oauth2/finish", strings.NewReader(string(finishBody)))
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	if status != http.StatusCreated {
		fmt.Fprintf(stderr, "server returned %d: %s\n", status, string(respBody))
		return 1
	}
	var got bindingRow
	if err := json.Unmarshal(respBody, &got); err != nil {
		fmt.Fprintf(stderr, "error parsing finish response: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "Bound: %s\n", got.Name)
	return 0
}

// portFromRedirectURI extracts the TCP port from a `http://host:port/...`
// URL. Returns 0 on parse failure.
func portFromRedirectURI(uri string) int {
	u, err := url.Parse(uri)
	if err != nil {
		return 0
	}
	p := u.Port()
	if p == "" {
		return 0
	}
	port := 0
	for _, r := range p {
		if r < '0' || r > '9' {
			return 0
		}
		port = port*10 + int(r-'0')
	}
	return port
}

// runBindingRebind prompts for a replacement api_key value and posts
// it to /v1/bindings/{name}/rebind, keeping the binding's identity
// and metadata intact.
func runBindingRebind(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "usage: aileron binding rebind <name>")
		return 1
	}
	name := args[0]
	value := promptLine(stdin, stdout, "New API key value: ")
	if value == "" {
		fmt.Fprintln(stderr, "value is required")
		return 1
	}
	body, _ := json.Marshal(map[string]any{
		"source": map[string]any{"kind": "api_key", "value": value},
	})
	status, respBody, err := bindingDoRequest(http.MethodPost, "/bindings/"+url.PathEscape(name)+"/rebind",
		strings.NewReader(string(body)))
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	if status == http.StatusNotFound {
		fmt.Fprintf(stderr, "binding not found: %s\n", name)
		return 1
	}
	if status != http.StatusOK {
		fmt.Fprintf(stderr, "server returned %d: %s\n", status, string(respBody))
		return 1
	}
	fmt.Fprintf(stdout, "Rebound: %s\n", name)
	return 0
}

// runBindingReauthorize refreshes the credential on an existing
// binding. For OAuth bindings, drives the same loopback dance as
// `binding setup` but with `purpose=reauthorize` so the server upserts
// (preserving CreatedAt + Account + history) and builds the authorize
// URL with `prompt=consent` — which Google requires to re-issue a
// token against the manifest's current scope set rather than silently
// reusing the user's prior consent. For api_key bindings, falls
// through to the same prompt-and-PUT path as `rebind`.
//
// Use after a connector upgrade that adds OAuth scopes and the
// webapp/CLI shows the binding as `stale`: this is the dedicated
// "fix the drift" command.
func runBindingReauthorize(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "usage: aileron binding reauthorize <name>")
		return 1
	}
	name := args[0]

	// Fetch the existing binding so we know its kind, identity, etc.
	status, body, err := bindingDoRequest(http.MethodGet, "/bindings/"+url.PathEscape(name), nil)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	if status == http.StatusNotFound {
		fmt.Fprintf(stderr, "binding not found: %s\n", name)
		return 1
	}
	if status != http.StatusOK {
		fmt.Fprintf(stderr, "server returned %d: %s\n", status, string(body))
		return 1
	}
	var existing bindingRow
	if err := json.Unmarshal(body, &existing); err != nil {
		fmt.Fprintf(stderr, "error parsing response: %v\n", err)
		return 1
	}

	if existing.Kind != "oauth2" {
		fmt.Fprintf(stderr, "binding kind %q is not OAuth; use `aileron binding rebind %s` instead\n",
			existing.Kind, name)
		return 1
	}

	initBody, _ := json.Marshal(map[string]any{
		"connector_fqn": existing.ConnectorFQN,
		"identity":      existing.Identity,
		"service":       existing.Service,
		"account":       existing.Account,
		"purpose":       "reauthorize",
	})
	initStatus, initRespBody, err := bindingDoRequest(http.MethodPost,
		"/bindings/setup/oauth2/init", strings.NewReader(string(initBody)))
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	if initStatus != http.StatusOK {
		fmt.Fprintf(stderr, "server returned %d: %s\n", initStatus, string(initRespBody))
		return 1
	}
	return runBindingReauthorizeFinish(initRespBody, name, stdout, stderr)
}

// runBindingReauthorizeFinish drives the OAuth finish leg of a
// reauthorize. Mirrors runBindingSetupOAuth2Finish but expects an
// HTTP 200 (the server returns 201 on first-time create, 200 on
// reauthorize upsert).
func runBindingReauthorizeFinish(initRespBody []byte, name string, stdout, stderr io.Writer) int {
	var initResp struct {
		SessionID    string `json:"session_id"`
		AuthorizeURL string `json:"authorize_url"`
		RedirectURI  string `json:"redirect_uri"`
	}
	if err := json.Unmarshal(initRespBody, &initResp); err != nil {
		fmt.Fprintf(stderr, "error parsing init response: %v\n", err)
		return 1
	}
	port := portFromRedirectURI(initResp.RedirectURI)
	if port == 0 {
		fmt.Fprintf(stderr, "could not parse port from redirect_uri %q\n", initResp.RedirectURI)
		return 1
	}
	listener, err := oauth.NewListener(port)
	if err != nil {
		fmt.Fprintf(stderr, "could not bind callback listener on port %d: %v\n", port, err)
		return 1
	}
	defer listener.Close()

	fmt.Fprintln(stdout, "Opening your browser to reauthorize. If it does not open, visit:")
	fmt.Fprintln(stdout, "  "+initResp.AuthorizeURL)
	if err := bindingBrowser.Open(initResp.AuthorizeURL); err != nil {
		fmt.Fprintf(stderr, "(browser open failed; paste the URL above): %v\n", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cb, err := listener.Await(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "OAuth callback error: %v\n", err)
		return 1
	}

	finishBody, _ := json.Marshal(map[string]any{
		"session_id": initResp.SessionID,
		"code":       cb.Code,
		"state":      cb.State,
	})
	status, respBody, err := bindingDoRequest(http.MethodPost,
		"/bindings/setup/oauth2/finish", strings.NewReader(string(finishBody)))
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	// Reauthorize returns 200; first-time setup returns 201. Accept
	// either so a stray race (e.g. binding revoked between the init
	// check and finish) reuses the same exit path.
	if status != http.StatusOK && status != http.StatusCreated {
		fmt.Fprintf(stderr, "server returned %d: %s\n", status, string(respBody))
		return 1
	}
	fmt.Fprintf(stdout, "Reauthorized: %s\n", name)
	return 0
}

// runBindingRevoke confirms with the user and DELETEs the binding.
func runBindingRevoke(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "usage: aileron binding revoke <name>")
		return 1
	}
	name := args[0]
	answer := promptLine(stdin, stdout, fmt.Sprintf("Revoke %s? [y/N]: ", name))
	if !strings.EqualFold(answer, "y") && !strings.EqualFold(answer, "yes") {
		fmt.Fprintln(stdout, "cancelled")
		return 0
	}
	status, respBody, err := bindingDoRequest(http.MethodDelete, "/bindings/"+url.PathEscape(name), nil)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	if status == http.StatusNotFound {
		fmt.Fprintf(stderr, "binding not found: %s\n", name)
		return 1
	}
	if status != http.StatusNoContent {
		fmt.Fprintf(stderr, "server returned %d: %s\n", status, string(respBody))
		return 1
	}
	fmt.Fprintf(stdout, "Revoked: %s\n", name)
	return 0
}

// bindingRow is the local subset of api.Binding the CLI renders.
// Defined here so the cmd/aileron module doesn't pull in the full
// internal/api/gen package as a dependency.
type bindingRow struct {
	Name          string    `json:"name"`
	Kind          string    `json:"kind"`
	Service       string    `json:"service"`
	Identity      string    `json:"identity"`
	Scope         string    `json:"scope"`
	ConnectorFQN  string    `json:"connector_fqn"`
	Account       string    `json:"account"`
	CreatedAt     time.Time `json:"created_at"`
	Status        string    `json:"status"`
	GrantedScopes []string  `json:"granted_scopes,omitempty"`
	StaleReason   string    `json:"stale_reason,omitempty"`
	MissingScopes []string  `json:"missing_scopes,omitempty"`
}

// promptLine writes prompt to stdout and reads one line from stdin
// (newline-stripped). Empty input returns an empty string.
//
// stdin is wrapped once per call only when it isn't already a
// *bufio.Reader. The runBindingX commands that prompt more than once
// must pass a *bufio.Reader so subsequent reads see the bytes left in
// the buffer after the first ReadString. (A fresh bufio.NewReader on
// each call would buffer-ahead and consume the rest of the input,
// dropping it on the floor when discarded.)
func promptLine(stdin io.Reader, stdout io.Writer, prompt string) string {
	fmt.Fprint(stdout, prompt)
	br, ok := stdin.(*bufio.Reader)
	if !ok {
		br = bufio.NewReader(stdin)
	}
	line, err := br.ReadString('\n')
	if err != nil && line == "" {
		return ""
	}
	return strings.TrimRight(line, "\r\n")
}

// promptPassphrase reads a password from the terminal without echoing.
// Replaceable in tests.
var promptPassphrase = defaultPromptPassphrase

func defaultPromptPassphrase(prompt string, w io.Writer) (string, error) {
	if w != nil && prompt != "" {
		fmt.Fprint(w, prompt)
	}
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return "", fmt.Errorf("cannot open terminal: %w", err)
	}
	defer tty.Close()

	pass, err := readPassword(int(tty.Fd()))
	if err != nil {
		return "", fmt.Errorf("reading input: %w", err)
	}
	if w != nil {
		fmt.Fprintln(w) // newline after hidden input
	}
	return string(pass), nil
}

// readPassword reads a password from a file descriptor. Extracted for testing.
var readPassword = defaultReadPassword

func defaultReadPassword(fd int) ([]byte, error) {
	return term.ReadPassword(fd)
}

// runStatus shows the current configuration state.
func runStatus(args []string, stdout, stderr io.Writer) int {
	dir, _ := os.Getwd()
	section := ""
	if len(args) > 0 {
		section = args[0]
	}

	switch section {
	case "":
		showStatusRuntime(stdout)
		fmt.Fprintln(stdout)
		showStatusNotifications(dir, stdout)
		fmt.Fprintln(stdout)
		showStatusVault(dir, stdout)
		return 0
	case "runtime":
		showStatusRuntime(stdout)
		return 0
	case "notifications":
		showStatusNotifications(dir, stdout)
		return 0
	case "vault":
		showStatusVault(dir, stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "unknown status section: %q\n", section)
		fmt.Fprintln(stderr, "usage: aileron status [runtime|notifications|vault]")
		return 1
	}
}

// runtimeStatusFetcher fetches the daemon's GET /v1/status snapshot.
// Replaceable in tests so they don't depend on a running server.
var runtimeStatusFetcher = fetchRuntimeStatus

// runtimeStatus mirrors the JSON shape of api.StatusResponse. We don't
// import the generated types here to keep the CLI binary's dependency
// graph slim — the wire shape is what's stable per ADR-0004.
type runtimeStatus struct {
	Version        string  `json:"version"`
	Commit         *string `json:"commit,omitempty"`
	ListenAddr     *string `json:"listen_addr,omitempty"`
	ActionCount    int     `json:"action_count"`
	ConnectorCount int     `json:"connector_count"`
	BindingCount   int     `json:"binding_count"`
	VaultState     string  `json:"vault_state"`
	GatewayUrl     *string `json:"gateway_url,omitempty"`
	SessionId      *string `json:"session_id,omitempty"`
}

// fetchRuntimeStatus calls GET /v1/status against the local daemon.
// A short timeout keeps `aileron status` snappy when the daemon isn't
// running — the CLI prints a "(daemon not reachable)" hint and moves
// on rather than blocking the operator.
func fetchRuntimeStatus() (*runtimeStatus, error) {
	base, err := bindingAPIBaseURL()
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 2 * time.Second}
	req, err := http.NewRequest(http.MethodGet, base+"/status", nil)
	if err != nil {
		return nil, err
	}
	setDaemonAuthorization(req)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("daemon returned %d", resp.StatusCode)
	}
	var rs runtimeStatus
	if err := json.NewDecoder(resp.Body).Decode(&rs); err != nil {
		return nil, fmt.Errorf("decoding status: %w", err)
	}
	return &rs, nil
}

func showStatusRuntime(w io.Writer) {
	fmt.Fprintln(w, "\033[1mRuntime\033[0m")

	rs, err := runtimeStatusFetcher()
	if err != nil {
		fmt.Fprintf(w, "  Daemon:            (not reachable: %v)\n", err)
		fmt.Fprintln(w, "  Hint:              start the daemon with 'aileron daemon start' or run any 'aileron' command (auto-spawns).")
		return
	}

	fmt.Fprintf(w, "  Version:           %s", rs.Version)
	if rs.Commit != nil && *rs.Commit != "" {
		fmt.Fprintf(w, " (%s)", *rs.Commit)
	}
	fmt.Fprintln(w)
	if rs.ListenAddr != nil && *rs.ListenAddr != "" {
		fmt.Fprintf(w, "  Listen:            %s\n", *rs.ListenAddr)
	}
	if rs.GatewayUrl != nil && *rs.GatewayUrl != "" {
		fmt.Fprintf(w, "  Gateway:           %s\n", *rs.GatewayUrl)
	}
	if rs.SessionId != nil && *rs.SessionId != "" {
		fmt.Fprintf(w, "  Session:           %s\n", *rs.SessionId)
	}
	fmt.Fprintf(w, "  Vault state:       %s\n", rs.VaultState)
	fmt.Fprintf(w, "  Actions:           %d installed\n", rs.ActionCount)
	fmt.Fprintf(w, "  Connectors:        %d installed\n", rs.ConnectorCount)
	fmt.Fprintf(w, "  Bindings:          %d active\n", rs.BindingCount)
}

// showStatusNotifications surfaces the user-scoped notification config
// (Slack / Discord / quiet hours) the daemon reads at startup. Lives in
// `~/.aileron/config.yaml` per ADR-0012 step 9B-2 — moved out of the
// per-project `aileron.yaml` along with listener ownership.
//
// The `dir` argument is unused now (kept for the call-site signature
// alignment with the other showStatus* helpers); future enhancements
// might re-introduce per-project overrides.
func showStatusNotifications(_ string, w io.Writer) {
	fmt.Fprintln(w, "\033[1mNotifications\033[0m")

	configPath := config.DefaultAileronConfigPath()
	cfg, err := config.LoadAileronConfig(configPath)
	if err != nil {
		fmt.Fprintf(w, "  error: %v\n", err)
		return
	}

	fmt.Fprintf(w, "  config file: %s\n", configPath)
	if cfg.Notifications == nil {
		fmt.Fprintln(w, "  No notifications configured.")
		return
	}

	if qh := cfg.Notifications.QuietHours; qh != nil {
		tz := qh.Timezone
		if tz == "" {
			tz = "(local)"
		}
		fmt.Fprintf(w, "  Quiet hours: %s–%s %s\n", qh.Start, qh.End, tz)
	}
}

func showStatusVault(dir string, w io.Writer) {
	fmt.Fprintln(w, "\033[1mVault\033[0m")

	vaultPath := launch.DefaultVaultPath()
	if _, err := os.Stat(vaultPath); err != nil {
		fmt.Fprintf(w, "  Vault file: %s (not created)\n", vaultPath)
		fmt.Fprintln(w, "  Run 'aileron secret set <name>' to create it.")
		return
	}

	fv, err := vault.NewFileVault(vaultPath)
	if err != nil {
		fmt.Fprintf(w, "  Vault file: %s (error: %v)\n", vaultPath, err)
		return
	}

	names := fv.Names()
	fmt.Fprintf(w, "  Vault file: %s\n", vaultPath)
	fmt.Fprintf(w, "  Secrets:    %d stored\n", len(names))
	for _, name := range names {
		fmt.Fprintf(w, "    - %s\n", name)
	}
}

// --- Connector / action install (ADR-0004 / ADR-0003 + #366) ---

const connectorUsage = `usage:
  aileron connector install <FQN> [--version=<v>] [--hash=<sha256:...>] [--yes]
  aileron connector check [--include-prerelease] [--json]`

const actionUsage = `usage:
  aileron action list                  [--json]
  aileron action run <NAME>            [--arg k=v ...] [--args <json>] [--json] [--audit-id-out <path>]
  aileron action enable <NAME>
  aileron action disable <NAME>
  aileron action add <FQN>             [--version=<v>] [--force] [--yes] [--no-bind]
  aileron action add-suite <SOURCE>    [--force] [--yes] [--no-bind]

The list/enable/disable subcommands inspect and control which installed
actions are surfaced to the LLM. Disabling does NOT uninstall the action;
the manifest file is left in place and the change is recorded in
~/.aileron/action-state.json. MCP servers that cached the tool list at
boot need to be restarted before a toggle takes effect with the LLM.

Single-action form: install one action by FQN. Trust + connector
install are absorbed into this command (issue #563): on a fresh
machine the CLI prompts to trust the publisher's signing key before
preview, then prompts for the install consent.

Suite form (issue #564): <SOURCE> is either:
  - <scheme>://<owner>/<repo>/<file-path>@<ref>   (remote)
  - <local-filesystem-path>                       (no scheme)

Remote refs:
  @latest     latest non-draft, non-prerelease release
  @<tag>      a specific release tag (e.g. v0.0.6)
  @<sha>      a specific commit SHA (40 hex chars)

The CLI installs each action in declaration order, sharing trust +
connector state across the set so a publisher's prompt fires at most
once per run. Per-action failures are surfaced in a final summary;
the remaining actions still install (failure-soft).

Pass --yes to auto-accept all trust + consent prompts in
non-interactive use.`

// runConnector dispatches `aileron connector <subcommand>`.
func runConnector(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, connectorUsage)
		return 1
	}
	switch args[0] {
	case "install":
		return runConnectorInstall(args[1:], stdin, stdout, stderr)
	case "check":
		return runConnectorCheck(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown connector command: %q\n", args[0])
		fmt.Fprintln(stderr, connectorUsage)
		return 1
	}
}

// connectorCheckResult mirrors api.ConnectorCheckResult on the wire.
// Defined locally so the CLI binary doesn't pull the full generated
// types graph just to render this surface.
type connectorCheckResult struct {
	Fqn               string   `json:"fqn"`
	CurrentVersion    string   `json:"current_version"`
	UpdateAvailable   bool     `json:"update_available"`
	LatestVersion     *string  `json:"latest_version,omitempty"`
	AvailableVersions []string `json:"available_versions,omitempty"`
	Error             *string  `json:"error,omitempty"`
}

type connectorsCheckResponse struct {
	Results []connectorCheckResult `json:"results"`
}

// connectorCheckFetcher fetches the daemon's GET /v1/connectors/check
// snapshot. Replaceable in tests so they don't depend on a running
// server. Same pattern as runtimeStatusFetcher.
var connectorCheckFetcher = fetchConnectorCheck

func fetchConnectorCheck(includePrerelease bool) (*connectorsCheckResponse, error) {
	path := "/connectors/check"
	if includePrerelease {
		path += "?include_prerelease=true"
	}
	base, err := bindingAPIBaseURL()
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 30 * time.Second}
	req, err := http.NewRequest(http.MethodGet, base+path, nil)
	if err != nil {
		return nil, err
	}
	setDaemonAuthorization(req)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("daemon returned %d: %s", resp.StatusCode, string(body))
	}
	var out connectorsCheckResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}
	return &out, nil
}

// runConnectorCheck implements `aileron connector check`. Walks the
// daemon's installed connectors and renders one row per connector with
// the current version, the latest available version (per ADR-0004 §
// "connector check"), and a per-row error when the source couldn't be
// reached.
func runConnectorCheck(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("connector check", flag.ContinueOnError)
	flags.SetOutput(stderr)
	includePrerelease := flags.Bool("include-prerelease", false, "include pre-release versions when computing the latest version")
	asJSON := flags.Bool("json", false, "Render results as NDJSON, one connector per line")
	if err := flags.Parse(args); err != nil {
		return 1
	}

	resp, err := connectorCheckFetcher(*includePrerelease)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}

	if len(resp.Results) == 0 {
		if *asJSON {
			fmt.Fprintln(stdout, "[]")
			return 0
		}
		fmt.Fprintln(stdout, "No connectors installed.")
		fmt.Fprintln(stdout, "Run `aileron connector install <FQN>` to add one.")
		return 0
	}

	if *asJSON {
		enc := json.NewEncoder(stdout)
		for _, r := range resp.Results {
			if err := enc.Encode(r); err != nil {
				fmt.Fprintf(stderr, "encode: %v\n", err)
				return 1
			}
		}
		return 0
	}

	updates := 0
	errored := 0
	for _, r := range resp.Results {
		switch {
		case r.Error != nil:
			fmt.Fprintf(stdout, "  %s@%s\n", r.Fqn, r.CurrentVersion)
			fmt.Fprintf(stdout, "    \033[31mcheck failed:\033[0m %s\n", *r.Error)
			errored++
		case r.UpdateAvailable && r.LatestVersion != nil:
			fmt.Fprintf(stdout, "  %s@%s → \033[32m%s\033[0m (update available)\n",
				r.Fqn, r.CurrentVersion, *r.LatestVersion)
			updates++
		case r.LatestVersion != nil:
			fmt.Fprintf(stdout, "  %s@%s (up to date)\n", r.Fqn, r.CurrentVersion)
		default:
			fmt.Fprintf(stdout, "  %s@%s (no released versions found)\n", r.Fqn, r.CurrentVersion)
		}
	}
	fmt.Fprintln(stdout)
	switch {
	case updates > 0:
		fmt.Fprintf(stdout, "%d update(s) available; %d connector(s) checked.\n", updates, len(resp.Results))
	case errored > 0:
		fmt.Fprintf(stdout, "%d connector(s) up to date; %d check failure(s).\n", len(resp.Results)-errored, errored)
	default:
		fmt.Fprintf(stdout, "All %d connector(s) are up to date.\n", len(resp.Results))
	}
	return 0
}

// runAction dispatches `aileron action <subcommand>`.
func runAction(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, actionUsage)
		return 1
	}
	br := bufio.NewReader(stdin)
	switch args[0] {
	case "add":
		return runActionAdd(args[1:], br, stdout, stderr)
	case "add-suite":
		return runActionAddSuite(args[1:], br, stdout, stderr)
	case "list":
		return runActionList(args[1:], stdout, stderr)
	case "run":
		return runActionRun(args[1:], stdout, stderr)
	case "enable":
		return runActionToggle(args[1:], true, stdout, stderr)
	case "disable":
		return runActionToggle(args[1:], false, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown action command: %q\n", args[0])
		fmt.Fprintln(stderr, actionUsage)
		return 1
	}
}

// syncResult mirrors the wire shape of api.SyncResponse. Defined
// locally so the CLI binary doesn't pull the full generated types
// graph just to render this surface.
type syncResult struct {
	ActionsSeen      int                      `json:"actions_seen"`
	Required         []connectorRefWire       `json:"required"`
	Installed        []installedConnectorWire `json:"installed"`
	AlreadyInstalled []connectorRefWire       `json:"already_installed"`
	InstallFailures  []connectorFailureWire   `json:"install_failures"`
	Unbound          []unboundCapabilityWire  `json:"unbound"`
}

type connectorRefWire struct {
	Fqn     string `json:"fqn"`
	Version string `json:"version"`
}

type installedConnectorWire struct {
	Fqn              string `json:"fqn"`
	Version          string `json:"version"`
	Hash             string `json:"hash"`
	EntryDir         string `json:"entry_dir"`
	AlreadyInstalled *bool  `json:"already_installed,omitempty"`
}

type connectorFailureWire struct {
	Fqn     string `json:"fqn"`
	Version string `json:"version"`
	Error   string `json:"error"`
}

type unboundCapabilityWire struct {
	ConnectorFqn string  `json:"connector_fqn"`
	Kind         string  `json:"kind"`
	Scope        *string `json:"scope,omitempty"`
}

// syncFetcher posts to /v1/sync and decodes the response. Replaceable
// in tests so they don't depend on a running daemon. Same pattern as
// runtimeStatusFetcher and connectorCheckFetcher.
var syncFetcher = postSyncRequest

func postSyncRequest(autoInstall bool) (*syncResult, error) {
	base, err := bindingAPIBaseURL()
	if err != nil {
		return nil, err
	}
	body, _ := json.Marshal(map[string]any{"auto_install": autoInstall})
	client := &http.Client{Timeout: 5 * time.Minute}
	req, err := http.NewRequest(http.MethodPost, base+"/sync",
		strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	setDaemonAuthorization(req)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("daemon returned %d: %s", resp.StatusCode, string(raw))
	}
	var out syncResult
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}
	return &out, nil
}

// runSync implements `aileron sync [--bind-all] [--yes]`. Posts to
// `/v1/sync`, prints a per-section report, and surfaces unbound
// capabilities so the operator knows what to bind next.
//
// `--bind-all` walks the unbound list and prompts interactively to
// create a binding per entry — api_key kinds prompt for the value
// inline, oauth2 kinds drive the server-side OAuth dance through the
// same code path as `aileron binding setup`. Failures don't abort
// the loop; a final summary reports bound vs failed.
//
// `--yes` is accepted but has no effect today: the install pipeline
// is unconditional in v1 (no consent prompt). The flag is honored on
// the wire so adding consent later doesn't break callers.
func runSync(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("sync", flag.ContinueOnError)
	flags.SetOutput(stderr)
	bindAll := flags.Bool("bind-all", false, "after sync, prompt to create a binding for each unbound capability")
	yes := flags.Bool("yes", false, "auto-approve install consent for new connectors (no-op in v1; install is unconditional)")
	if err := flags.Parse(args); err != nil {
		return 1
	}

	resp, err := syncFetcher(*yes)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}

	fmt.Fprintf(stdout, "Walked %d action(s).\n", resp.ActionsSeen)

	if len(resp.Required) == 0 {
		fmt.Fprintln(stdout, "No connector dependencies declared by any installed action.")
	} else {
		fmt.Fprintf(stdout, "%d connector dependency(s) collected:\n", len(resp.Required))
		for _, r := range resp.Required {
			fmt.Fprintf(stdout, "  - %s@%s\n", r.Fqn, r.Version)
		}
	}

	if len(resp.Installed) > 0 {
		fmt.Fprintln(stdout)
		fmt.Fprintf(stdout, "Installed %d connector(s):\n", len(resp.Installed))
		for _, c := range resp.Installed {
			fmt.Fprintf(stdout, "  \033[32m✓\033[0m %s@%s (%s)\n", c.Fqn, c.Version, c.Hash)
		}
	}
	if len(resp.AlreadyInstalled) > 0 {
		fmt.Fprintln(stdout)
		fmt.Fprintf(stdout, "%d connector(s) already installed:\n", len(resp.AlreadyInstalled))
		for _, r := range resp.AlreadyInstalled {
			fmt.Fprintf(stdout, "  - %s@%s\n", r.Fqn, r.Version)
		}
	}
	if len(resp.InstallFailures) > 0 {
		fmt.Fprintln(stdout)
		fmt.Fprintf(stdout, "\033[31m%d install failure(s):\033[0m\n", len(resp.InstallFailures))
		for _, f := range resp.InstallFailures {
			fmt.Fprintf(stdout, "  ✗ %s@%s — %s\n", f.Fqn, f.Version, f.Error)
		}
	}
	if len(resp.Unbound) > 0 {
		fmt.Fprintln(stdout)
		fmt.Fprintf(stdout, "%d unbound capability(s):\n", len(resp.Unbound))
		for _, u := range resp.Unbound {
			scope := ""
			if u.Scope != nil && *u.Scope != "" {
				scope = " — " + *u.Scope
			}
			fmt.Fprintf(stdout, "  - %s [%s]%s\n", u.ConnectorFqn, u.Kind, scope)
		}
		fmt.Fprintln(stdout, "  Bind each with: aileron binding setup <FQN>")
	}

	if *bindAll && len(resp.Unbound) > 0 {
		fmt.Fprintln(stdout)
		fmt.Fprintf(stdout, "Binding %d capability(s)...\n", len(resp.Unbound))
		bound, failed := bindAllUnbound(resp.Unbound, stdin, stdout, stderr)
		fmt.Fprintln(stdout)
		fmt.Fprintf(stdout, "Bound %d of %d capability(s)", bound, len(resp.Unbound))
		if failed > 0 {
			fmt.Fprintf(stdout, " (%d failed)", failed)
		}
		fmt.Fprintln(stdout, ".")
		if failed > 0 {
			return 1
		}
	}

	// Exit non-zero on install failures so operators wiring sync
	// into shell scripts can branch on it.
	if len(resp.InstallFailures) > 0 {
		return 1
	}
	return 0
}

// bindAllUnbound walks the unbound list returned by /v1/sync and
// dispatches by credential kind. It returns (bound, failed) counts.
// Failures don't abort the loop — partial binding is more useful
// than nothing.
func bindAllUnbound(unbound []unboundCapabilityWire, stdin io.Reader, stdout, stderr io.Writer) (bound, failed int) {
	br := bufio.NewReader(stdin)
	for _, u := range unbound {
		fmt.Fprintln(stdout)
		scope := ""
		if u.Scope != nil && *u.Scope != "" {
			scope = " — " + *u.Scope
		}
		fmt.Fprintf(stdout, "→ %s [%s]%s\n", u.ConnectorFqn, u.Kind, scope)

		identity := promptLine(br, stdout, "  Identity (e.g. work, personal): ")
		if identity == "" {
			fmt.Fprintln(stderr, "  identity is required; skipping")
			failed++
			continue
		}

		var rc int
		switch u.Kind {
		case "oauth2":
			rc = bindOneOAuth2(u.ConnectorFqn, identity, stdout, stderr)
		case "api_key":
			rc = runBindingSetupAPIKey(u.ConnectorFqn, identity, br, stdout, stderr)
		default:
			fmt.Fprintf(stderr, "  unsupported credential kind %q for %s; skipping\n", u.Kind, u.ConnectorFqn)
			failed++
			continue
		}
		if rc == 0 {
			bound++
		} else {
			failed++
		}
	}
	return bound, failed
}

// bindOneOAuth2 drives the server-side OAuth dance for a single
// (connector, identity). It is the bind-all counterpart to the OAuth
// branch of `runBindingSetup`: the kind is known up front (from the
// sync response), so we POST init unconditionally and treat anything
// other than 200 as a hard error rather than falling through to
// api_key. The post-init flow shares `runBindingSetupOAuth2Finish`.
func bindOneOAuth2(connectorFQN, identity string, stdout, stderr io.Writer) int {
	initBody, _ := json.Marshal(map[string]any{
		"connector_fqn": connectorFQN,
		"identity":      identity,
	})
	initStatus, initRespBody, err := bindingDoRequest(http.MethodPost,
		"/bindings/setup/oauth2/init", strings.NewReader(string(initBody)))
	if err != nil {
		fmt.Fprintf(stderr, "  error: %v\n", err)
		return 1
	}
	if initStatus != http.StatusOK {
		fmt.Fprintf(stderr, "  server returned %d: %s\n", initStatus, string(initRespBody))
		return 1
	}
	return runBindingSetupOAuth2Finish(initRespBody, stdout, stderr)
}

// connectorPreviewWire mirrors the JSON shape of api.ConnectorPreview
// on the wire. Locally defined so the CLI binary doesn't pull in the
// full generated types graph for this single endpoint.
type connectorPreviewWire struct {
	Fqn              string `json:"fqn"`
	Version          string `json:"version"`
	Hash             string `json:"hash"`
	Publisher        string `json:"publisher"`
	SignatureStatus  string `json:"signature_status"`
	AlreadyInstalled bool   `json:"already_installed"`
	Capabilities     struct {
		NetworkHosts []string `json:"network_hosts,omitempty"`
		Credential   *struct {
			Kind  string  `json:"kind"`
			Scope *string `json:"scope,omitempty"`
		} `json:"credential,omitempty"`
	} `json:"capabilities"`
}

// installedConnectorWireForInstall mirrors api.InstalledConnector for
// the install response. Same locality argument as the preview wire.
type installedConnectorWireForInstall struct {
	Fqn              string `json:"fqn"`
	Version          string `json:"version"`
	Hash             string `json:"hash"`
	EntryDir         string `json:"entry_dir"`
	AlreadyInstalled bool   `json:"already_installed"`
}

// hubInstallDecisionWire mirrors api.HubInstallDecision on the wire.
// Declared locally for the same reason as the other CLI wire types:
// keep the binary off the generated-types dependency graph.
type hubInstallDecisionWire struct {
	Fqn                string   `json:"fqn"`
	Description        string   `json:"description"`
	PublisherGithub    string   `json:"publisher_github"`
	Fingerprint        string   `json:"fingerprint"`
	TrustState         string   `json:"trust_state"`
	PublisherFootprint []string `json:"publisher_footprint"`
	RiskIndicators     []string `json:"risk_indicators"`
}

// hubInstallAuthorityWire mirrors api.HubInstallAuthority — the
// per-connector trust-panel element inside a composite install-decision.
type hubInstallAuthorityWire struct {
	Fqn                string   `json:"fqn"`
	PublisherGithub    string   `json:"publisher_github"`
	Fingerprint        string   `json:"fingerprint"`
	TrustState         string   `json:"trust_state"`
	PublisherFootprint []string `json:"publisher_footprint"`
	RiskIndicators     []string `json:"risk_indicators"`
}

// hubActionInstallDecisionWire mirrors api.HubActionInstallDecision —
// the composite payload returned by /v1/hub/action-install-decision.
type hubActionInstallDecisionWire struct {
	Kind            string                    `json:"kind"`
	Fqn             string                    `json:"fqn"`
	Description     string                    `json:"description"`
	PublisherGithub string                    `json:"publisher_github"`
	ConnectorFqn    string                    `json:"connector_fqn"`
	Authorities     []hubInstallAuthorityWire `json:"authorities"`
}

// hubSuiteInstallDecisionWire mirrors api.HubSuiteInstallDecision —
// the composite payload returned by /v1/hub/suite-install-decision.
type hubSuiteInstallDecisionWire struct {
	Kind            string                    `json:"kind"`
	Fqn             string                    `json:"fqn"`
	Description     string                    `json:"description"`
	PublisherGithub string                    `json:"publisher_github"`
	MemberActions   []string                  `json:"member_actions"`
	Authorities     []hubInstallAuthorityWire `json:"authorities"`
}

// runConnectorInstall implements the consent flow per ADR-0007 with
// the Hub install-decision layer added on top per ADR-0013 / #487:
//
//  1. GET /v1/hub/install-decision?fqn=<fqn>. If the FQN is in the Hub
//     (200), render the publisher / fingerprint / trust-state / risks
//     and prompt y/n/d=details. On confirm, call install with
//     `confirmed_fingerprint`; the daemon writes per-FQN trust to the
//     keyring before running the install pipeline (#487 Q4).
//
//  2. If the FQN is not in the Hub (404), fall back to the legacy
//     preview-then-install flow. Trust must be pre-established via
//     `aileron keyring trust` for that path to succeed.
//
//  3. Other failure modes on the Hub call (503 hub_disabled,
//     hub_unreachable) also fall back to the legacy flow with a one-
//     line stderr note so users with daemon-side Hub config issues can
//     still install connectors they've pre-trusted.
//
// The legacy preview flow remains unchanged:
//
//  1. POST /v1/connectors/preview to fetch + verify + parse without
//     committing. Signature failure or hash mismatch aborts here.
//  2. Render the preview (FQN, version, hash, publisher, signature
//     status, capabilities) for the operator.
//  3. Prompt y/N for confirmation. `--yes` skips the prompt.
//  4. POST /v1/connectors/install on confirmation. Server runs the
//     same pipeline a second time — the small re-fetch cost buys a
//     much cleaner two-phase API surface (no server-side staging).
//
// Already-installed-at-this-hash short-circuits: if the preview
// reports AlreadyInstalled=true, the CLI skips the prompt and prints
// "already installed" without re-running the install endpoint.
//
// `--yes` skips the prompt but does NOT bypass signature verification
// (that happens server-side, before the prompt fires).
func runConnectorInstall(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fqnArg, rest, ok := extractPositional(args)
	if !ok {
		fmt.Fprintln(stderr, connectorUsage)
		return 1
	}
	flags := flag.NewFlagSet("connector install", flag.ContinueOnError)
	flags.SetOutput(stderr)
	version := flags.String("version", "", "strict SemVer to install (required if FQN omits @<version>)")
	hash := flags.String("hash", "", "expected sha256:<hex>; install aborts if computed hash does not match")
	yes := flags.Bool("yes", false, "skip the consent prompt and proceed without confirmation")
	force := flags.Bool("force", false, "(reserved for future use)")
	if err := flags.Parse(rest); err != nil {
		return 1
	}
	_ = force
	resolvedFQN, resolvedVersion, perr := splitFQNVersion(fqnArg, *version)
	if perr != nil {
		fmt.Fprintf(stderr, "%v\n", perr)
		return 1
	}

	// Step 0 (ADR-0013 / #487): try the Hub install-decision flow. If
	// the FQN is in the Hub we render the publisher / risk surface and
	// pass the confirmed fingerprint to the install endpoint, which
	// writes per-FQN trust before running the install pipeline.
	confirmedFP, hubCancelled, hubPath := tryHubInstallDecisionFlow(
		resolvedFQN, *yes, stdin, stdout, stderr)
	if hubCancelled {
		return 0
	}
	if hubPath {
		return doConfirmedInstall(resolvedFQN, resolvedVersion, *hash, confirmedFP, stdout, stderr)
	}

	// Step 1: preview.
	previewBody := map[string]any{"fqn": resolvedFQN, "version": resolvedVersion}
	if *hash != "" {
		previewBody["expected_hash"] = *hash
	}
	previewJSON, _ := json.Marshal(previewBody)
	previewStatus, previewResp, err := bindingDoRequest(http.MethodPost, "/connectors/preview",
		strings.NewReader(string(previewJSON)))
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	if previewStatus != http.StatusOK {
		fmt.Fprintf(stderr, "server returned %d: %s\n", previewStatus, string(previewResp))
		return 1
	}
	var preview connectorPreviewWire
	if err := json.Unmarshal(previewResp, &preview); err != nil {
		fmt.Fprintf(stderr, "error parsing preview: %v\n", err)
		return 1
	}

	// Already-installed short-circuit: skip prompt, skip install.
	if preview.AlreadyInstalled {
		fmt.Fprintf(stdout, "Already installed: %s@%s\n  hash: %s\n",
			preview.Fqn, preview.Version, preview.Hash)
		return 0
	}

	// Step 2: render the preview.
	renderConnectorPreview(stdout, &preview)

	// Step 3: prompt unless --yes.
	if !*yes {
		answer := strings.ToLower(strings.TrimSpace(promptLine(stdin, stdout, "Install? [y/n]: ")))
		if answer != "y" && answer != "yes" {
			fmt.Fprintln(stdout, "Cancelled.")
			return 0
		}
	}

	// Step 4: real install.
	installJSON, _ := json.Marshal(previewBody)
	installStatus, installResp, err := bindingDoRequest(http.MethodPost, "/connectors/install",
		strings.NewReader(string(installJSON)))
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	if installStatus != http.StatusCreated && installStatus != http.StatusOK {
		fmt.Fprintf(stderr, "server returned %d: %s\n", installStatus, string(installResp))
		return 1
	}
	var resp installedConnectorWireForInstall
	if err := json.Unmarshal(installResp, &resp); err != nil {
		fmt.Fprintf(stderr, "error parsing response: %v\n", err)
		return 1
	}
	verb := "Installed"
	if resp.AlreadyInstalled {
		verb = "Already installed"
	}
	fmt.Fprintf(stdout, "%s: %s@%s\n  hash: %s\n  path: %s\n",
		verb, resp.Fqn, resp.Version, resp.Hash, resp.EntryDir)
	return 0
}

// tryHubInstallDecisionFlow asks the daemon for the Hub install-decision
// payload for the FQN. Returns:
//
//   - (fingerprint, false, true)  → operator confirmed. Caller should
//     POST /v1/connectors/install with confirmed_fingerprint.
//   - ("", true, true)            → operator declined. Caller stops.
//   - ("", false, false)          → no Hub entry (404) or Hub error.
//     Caller falls back to the legacy preview flow.
//
// `--yes` short-circuits the prompt and auto-confirms.
//
// Hub failures other than 404 (503 hub_disabled, hub_unreachable) emit
// a one-line stderr note and return as fall-through. Users with broken
// daemon-side Hub config can still install connectors they've pre-
// trusted via `aileron keyring trust`.
func tryHubInstallDecisionFlow(fqn string, autoYes bool, stdin io.Reader, stdout, stderr io.Writer) (string, bool, bool) {
	q := url.Values{"fqn": []string{fqn}}.Encode()
	status, body, err := bindingDoRequest(http.MethodGet, "/hub/install-decision?"+q, nil)
	if err != nil {
		fmt.Fprintf(stderr, "note: hub install-decision unavailable (%v); using local trust\n", err)
		return "", false, false
	}
	if status == http.StatusNotFound {
		return "", false, false
	}
	if status != http.StatusOK {
		fmt.Fprintf(stderr, "note: hub install-decision returned %d; using local trust\n", status)
		return "", false, false
	}
	var d hubInstallDecisionWire
	if err := json.Unmarshal(body, &d); err != nil {
		fmt.Fprintf(stderr, "note: could not parse hub install-decision (%v); using local trust\n", err)
		return "", false, false
	}

	if autoYes {
		return d.Fingerprint, false, true
	}

	// Wrap stdin once so the `d`-then-`y` two-line flow doesn't lose
	// buffered bytes between iterations. promptLine builds its own
	// bufio.Reader when handed a raw reader, and the buffered bytes
	// inside that local reader are discarded when the call returns.
	br, ok := stdin.(*bufio.Reader)
	if !ok {
		br = bufio.NewReader(stdin)
	}

	showDetails := false
	for {
		renderHubInstallDecision(stdout, &d, showDetails)
		var ans string
		if showDetails {
			ans = strings.ToLower(strings.TrimSpace(promptLine(br, stdout, "Install? [y/n]: ")))
		} else {
			ans = strings.ToLower(strings.TrimSpace(promptLine(br, stdout, "Install? [y/n/d=details]: ")))
		}
		switch ans {
		case "y", "yes":
			return d.Fingerprint, false, true
		case "d", "details":
			if showDetails {
				// Already showing details; treat as 'no'.
				fmt.Fprintln(stdout, "Cancelled.")
				return "", true, true
			}
			showDetails = true
			continue
		default:
			fmt.Fprintln(stdout, "Cancelled.")
			return "", true, true
		}
	}
}

// renderHubInstallDecision prints the install-time trust summary for a
// Hub-listed connector. Trust state is colored — green when the
// publisher key is already trusted at this FQN, yellow when it's the
// first time, red when a sibling repo by the same publisher carries a
// different key (likely rotation, possibly impersonation). Risk
// indicators inherit the same red/yellow distinction.
//
// `verbose` adds the full publisher_footprint (every other Hub-listed
// connector by this publisher) and any further risk lines. Compact
// mode prints the first risk indicator only.
func renderHubInstallDecision(w io.Writer, d *hubInstallDecisionWire, verbose bool) {
	fmt.Fprintln(w)
	fmt.Fprintln(w, "\033[1mHub install-decision\033[0m")
	fmt.Fprintln(w)
	fmt.Fprintf(w, "  FQN:         %s\n", d.Fqn)
	if d.Description != "" {
		fmt.Fprintf(w, "  Description: %s\n", d.Description)
	}
	fmt.Fprintf(w, "  Publisher:   %s\n", d.PublisherGithub)
	fmt.Fprintf(w, "  Fingerprint: %s\n", d.Fingerprint)
	fmt.Fprintf(w, "  Trust:       %s\n", colorizeTrustState(d.TrustState))
	if len(d.RiskIndicators) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "  \033[1mRisk indicators:\033[0m")
		risks := d.RiskIndicators
		if !verbose && len(risks) > 1 {
			risks = risks[:1]
		}
		for _, r := range risks {
			fmt.Fprintf(w, "    %s\n", colorizeRisk(d.TrustState, r))
		}
	}
	if verbose && len(d.PublisherFootprint) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "  \033[1mOther connectors by this publisher:\033[0m")
		for _, fqn := range d.PublisherFootprint {
			fmt.Fprintf(w, "    - %s\n", fqn)
		}
	}
	fmt.Fprintln(w)
}

// tryHubActionInstallDecisionFlow asks the daemon for the composite
// install-decision payload for an action FQN. Returns:
//
//   - (true,  false, true)  → operator confirmed. Caller programmatically
//     trusts each composite authority (`trust.ensure` with autoYes=true)
//     so the existing per-authority prompt loop later in installOneAction
//     becomes a no-op.
//   - (false, true,  true)  → operator declined. Caller stops.
//   - (false, false, false) → no Hub entry for the action (404), composite
//     can't be precomputed (422 missing connector), or any other Hub
//     error. Caller falls back to the existing per-authority trust flow.
//
// 404 and 422 fall through silently (these are legitimate non-Hub cases).
// Other failure modes (5xx, parse errors) emit a one-line stderr note so
// daemon-side Hub config issues are visible but don't block users with
// pre-established trust from installing.
//
// `--yes` short-circuits the prompt and auto-confirms.
//
// The returned payload is non-nil whenever hubPath is true (whether
// accepted or declined). Callers should walk payload.Authorities to
// pre-trust each before running the install.
func tryHubActionInstallDecisionFlow(actionFQN string, autoYes bool, stdin io.Reader, stdout, stderr io.Writer) (accepted, declined, hubPath bool, payload *hubActionInstallDecisionWire) {
	q := url.Values{"fqn": []string{actionFQN}}.Encode()
	status, body, err := bindingDoRequest(http.MethodGet, "/hub/action-install-decision?"+q, nil)
	if err != nil {
		fmt.Fprintf(stderr, "note: hub install-decision unavailable (%v); using local trust\n", err)
		return false, false, false, nil
	}
	// 404: action not in the Hub catalog; legitimate non-Hub install.
	// 422: action's declared connector_fqn isn't Hub-listed; install can
	// still proceed via direct FQN resolution, but the Hub can't
	// precompute the trust panel.
	if status == http.StatusNotFound || status == http.StatusUnprocessableEntity {
		return false, false, false, nil
	}
	if status != http.StatusOK {
		fmt.Fprintf(stderr, "note: hub install-decision returned %d; using local trust\n", status)
		return false, false, false, nil
	}
	var d hubActionInstallDecisionWire
	if err := json.Unmarshal(body, &d); err != nil {
		fmt.Fprintf(stderr, "note: could not parse hub install-decision (%v); using local trust\n", err)
		return false, false, false, nil
	}
	// Empty payload (no FQN, no authorities) is a no-op. Treat it as a
	// non-Hub case rather than rendering an empty trust panel — the
	// composite endpoint should never legitimately return this, but
	// downstream tests and mock daemons sometimes echo `{}` for catch-
	// all 200 responses, and we don't want a blank panel to follow.
	if d.Fqn == "" || len(d.Authorities) == 0 {
		return false, false, false, nil
	}

	if autoYes {
		return true, false, true, &d
	}

	// Wrap stdin once so the `d`-then-`y` two-line flow doesn't lose
	// buffered bytes between iterations. promptLine builds its own
	// bufio.Reader when handed a raw reader.
	br, ok := stdin.(*bufio.Reader)
	if !ok {
		br = bufio.NewReader(stdin)
	}

	showDetails := false
	for {
		renderHubActionCompositeDecision(stdout, &d, showDetails)
		var ans string
		if showDetails {
			ans = strings.ToLower(strings.TrimSpace(promptLine(br, stdout, "Trust these publishers and install? [y/n]: ")))
		} else {
			ans = strings.ToLower(strings.TrimSpace(promptLine(br, stdout, "Trust these publishers and install? [y/n/d=details]: ")))
		}
		switch ans {
		case "y", "yes":
			return true, false, true, &d
		case "d", "details":
			if showDetails {
				fmt.Fprintln(stdout, "Cancelled.")
				return false, true, true, &d
			}
			showDetails = true
			continue
		default:
			fmt.Fprintln(stdout, "Cancelled.")
			return false, true, true, &d
		}
	}
}

// renderHubActionCompositeDecision prints the composite trust panel for
// an action install. Action-level metadata at the top, one panel per
// connector authority below. Per-authority trust state and risks use
// the same colorization as the connector-level install-decision render.
//
// `verbose` expands risk indicators (one per line; compact mode shows
// the first only) and adds the per-authority publisher footprint.
func renderHubActionCompositeDecision(w io.Writer, d *hubActionInstallDecisionWire, verbose bool) {
	fmt.Fprintln(w)
	fmt.Fprintln(w, "\033[1mHub install-decision (action)\033[0m")
	fmt.Fprintln(w)
	fmt.Fprintf(w, "  Action:    %s\n", d.Fqn)
	if d.Description != "" {
		fmt.Fprintf(w, "  Summary:   %s\n", d.Description)
	}
	fmt.Fprintf(w, "  Publisher: %s\n", d.PublisherGithub)
	fmt.Fprintf(w, "  Connector: %s\n", d.ConnectorFqn)
	for _, auth := range d.Authorities {
		fmt.Fprintln(w)
		fmt.Fprintf(w, "  ● %s\n", auth.Fqn)
		fmt.Fprintf(w, "      Publisher:   %s\n", auth.PublisherGithub)
		fmt.Fprintf(w, "      Fingerprint: %s\n", auth.Fingerprint)
		fmt.Fprintf(w, "      Trust:       %s\n", colorizeTrustState(auth.TrustState))
		if len(auth.RiskIndicators) > 0 {
			risks := auth.RiskIndicators
			if !verbose && len(risks) > 1 {
				risks = risks[:1]
			}
			for _, r := range risks {
				fmt.Fprintf(w, "      Risk:        %s\n", colorizeRisk(auth.TrustState, r))
			}
		}
		if verbose && len(auth.PublisherFootprint) > 0 {
			fmt.Fprintln(w, "      Other connectors by this publisher:")
			for _, fqn := range auth.PublisherFootprint {
				fmt.Fprintf(w, "        - %s\n", fqn)
			}
		}
	}
	fmt.Fprintln(w)
}

// tryHubSuiteInstallDecisionFlow is the suite-level counterpart to
// tryHubActionInstallDecisionFlow. Asks the daemon for
// /v1/hub/suite-install-decision and, when the suite is Hub-listed,
// renders the composite trust panel (suite metadata + member actions
// + per-authority trust gates) and collapses every per-authority
// consent into a single y/n/d=details prompt.
//
// Fall-through, error-note, and empty-payload semantics match the
// action variant.
func tryHubSuiteInstallDecisionFlow(suiteFQN string, autoYes bool, stdin io.Reader, stdout, stderr io.Writer) (accepted, declined, hubPath bool, payload *hubSuiteInstallDecisionWire) {
	q := url.Values{"fqn": []string{suiteFQN}}.Encode()
	status, body, err := bindingDoRequest(http.MethodGet, "/hub/suite-install-decision?"+q, nil)
	if err != nil {
		fmt.Fprintf(stderr, "note: hub install-decision unavailable (%v); using local trust\n", err)
		return false, false, false, nil
	}
	if status == http.StatusNotFound || status == http.StatusUnprocessableEntity {
		return false, false, false, nil
	}
	if status != http.StatusOK {
		fmt.Fprintf(stderr, "note: hub install-decision returned %d; using local trust\n", status)
		return false, false, false, nil
	}
	var d hubSuiteInstallDecisionWire
	if err := json.Unmarshal(body, &d); err != nil {
		fmt.Fprintf(stderr, "note: could not parse hub install-decision (%v); using local trust\n", err)
		return false, false, false, nil
	}
	if d.Fqn == "" || len(d.Authorities) == 0 {
		return false, false, false, nil
	}

	if autoYes {
		return true, false, true, &d
	}

	br, ok := stdin.(*bufio.Reader)
	if !ok {
		br = bufio.NewReader(stdin)
	}

	showDetails := false
	for {
		renderHubSuiteCompositeDecision(stdout, &d, showDetails)
		var ans string
		if showDetails {
			ans = strings.ToLower(strings.TrimSpace(promptLine(br, stdout, "Trust these publishers and continue? [y/n]: ")))
		} else {
			ans = strings.ToLower(strings.TrimSpace(promptLine(br, stdout, "Trust these publishers and continue? [y/n/d=details]: ")))
		}
		switch ans {
		case "y", "yes":
			return true, false, true, &d
		case "d", "details":
			if showDetails {
				fmt.Fprintln(stdout, "Cancelled.")
				return false, true, true, &d
			}
			showDetails = true
			continue
		default:
			fmt.Fprintln(stdout, "Cancelled.")
			return false, true, true, &d
		}
	}
}

// renderHubSuiteCompositeDecision prints the composite trust panel for
// a suite install. Suite-level metadata at the top, member actions in
// the middle, one panel per connector authority below. Per-authority
// trust state and risks use the same colorization as the connector and
// action variants.
//
// `verbose` expands risk indicators (one per line; compact mode shows
// the first only) and adds the per-authority publisher footprint.
func renderHubSuiteCompositeDecision(w io.Writer, d *hubSuiteInstallDecisionWire, verbose bool) {
	fmt.Fprintln(w)
	fmt.Fprintln(w, "\033[1mHub install-decision (suite)\033[0m")
	fmt.Fprintln(w)
	fmt.Fprintf(w, "  Suite:     %s\n", d.Fqn)
	if d.Description != "" {
		fmt.Fprintf(w, "  Summary:   %s\n", d.Description)
	}
	fmt.Fprintf(w, "  Publisher: %s\n", d.PublisherGithub)
	if len(d.MemberActions) > 0 {
		fmt.Fprintf(w, "  Actions:   %d\n", len(d.MemberActions))
		for _, a := range d.MemberActions {
			fmt.Fprintf(w, "    - %s\n", a)
		}
	}
	for _, auth := range d.Authorities {
		fmt.Fprintln(w)
		fmt.Fprintf(w, "  ● %s\n", auth.Fqn)
		fmt.Fprintf(w, "      Publisher:   %s\n", auth.PublisherGithub)
		fmt.Fprintf(w, "      Fingerprint: %s\n", auth.Fingerprint)
		fmt.Fprintf(w, "      Trust:       %s\n", colorizeTrustState(auth.TrustState))
		if len(auth.RiskIndicators) > 0 {
			risks := auth.RiskIndicators
			if !verbose && len(risks) > 1 {
				risks = risks[:1]
			}
			for _, r := range risks {
				fmt.Fprintf(w, "      Risk:        %s\n", colorizeRisk(auth.TrustState, r))
			}
		}
		if verbose && len(auth.PublisherFootprint) > 0 {
			fmt.Fprintln(w, "      Other connectors by this publisher:")
			for _, fqn := range auth.PublisherFootprint {
				fmt.Fprintf(w, "        - %s\n", fqn)
			}
		}
	}
	fmt.Fprintln(w)
}

// deriveHubSuiteFQN translates a remote suite source's parsed FQN into
// the canonical Hub suite FQN. The Hub catalogs suites by
// `<scheme>://<owner>/<repo>/<subpath-without-.toml>` while CLI users
// type `<scheme>://<owner>/<repo>/<subpath>.toml@<ref>`.
//
// Returns an empty string when the source isn't shaped like a TOML
// path under a git-host scheme — local sources and non-`.toml`
// subpaths skip the Hub composite flow.
func deriveHubSuiteFQN(parsed cstore.FQN) string {
	if parsed.Scheme != "github" && parsed.Scheme != "gitlab" {
		return ""
	}
	if parsed.Owner == "" || parsed.Repo == "" || parsed.Subpath == "" {
		return ""
	}
	subpath := parsed.Subpath
	const tomlExt = ".toml"
	if !strings.HasSuffix(subpath, tomlExt) {
		return ""
	}
	subpath = strings.TrimSuffix(subpath, tomlExt)
	if subpath == "" {
		return ""
	}
	return parsed.Scheme + "://" + parsed.Owner + "/" + parsed.Repo + "/" + subpath
}

// colorizeTrustState renders a trust-state label with ANSI color:
// green for already_trusted, yellow for unknown, red for conflict.
// Unknown labels are returned unstyled so future enum additions don't
// surface as terminal escape codes the operator can't decipher.
func colorizeTrustState(state string) string {
	switch state {
	case "already_trusted":
		return "\033[32malready trusted\033[0m"
	case "unknown":
		return "\033[33munknown (first install)\033[0m"
	case "conflict":
		return "\033[31mconflict — key differs from a trusted sibling repo\033[0m"
	default:
		return state
	}
}

// colorizeRisk maps a single risk indicator string to a colored
// rendering: red when the umbrella trust state is `conflict`, yellow
// otherwise (the indicator is informational, not blocking).
func colorizeRisk(state, risk string) string {
	if state == "conflict" {
		return "\033[31m✘ " + risk + "\033[0m"
	}
	return "\033[33m• " + risk + "\033[0m"
}

// doConfirmedInstall calls /v1/connectors/install with confirmed_
// fingerprint set. The daemon writes per-FQN trust to the keyring
// before running the install pipeline, so a missing publisher key in
// the local keyring isn't a blocker — that's the point of the Hub
// install-decision flow. The legacy preview-flow path of
// runConnectorInstall is bypassed entirely.
func doConfirmedInstall(fqn, version, expectedHash, fingerprint string, stdout, stderr io.Writer) int {
	body := map[string]any{
		"fqn":                   fqn,
		"version":               version,
		"confirmed_fingerprint": fingerprint,
	}
	if expectedHash != "" {
		body["expected_hash"] = expectedHash
	}
	payload, _ := json.Marshal(body)
	status, resp, err := bindingDoRequest(http.MethodPost, "/connectors/install",
		strings.NewReader(string(payload)))
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	if status != http.StatusCreated && status != http.StatusOK {
		fmt.Fprintf(stderr, "server returned %d: %s\n", status, string(resp))
		return 1
	}
	var installed installedConnectorWireForInstall
	if err := json.Unmarshal(resp, &installed); err != nil {
		fmt.Fprintf(stderr, "error parsing response: %v\n", err)
		return 1
	}
	verb := "Installed"
	if installed.AlreadyInstalled {
		verb = "Already installed"
	}
	fmt.Fprintf(stdout, "%s: %s@%s\n  hash: %s\n  path: %s\n",
		verb, installed.Fqn, installed.Version, installed.Hash, installed.EntryDir)
	return 0
}

// renderConnectorPreview prints the consent-prompt summary the
// operator reads before deciding whether to install. Sections shown:
// FQN + version, short hash, publisher, signature status,
// capabilities (network hosts, credential kind + scope). Absent
// capability sub-tables are skipped — the prompt renders only what
// the connector actually declares.
func renderConnectorPreview(w io.Writer, p *connectorPreviewWire) {
	fmt.Fprintln(w)
	fmt.Fprintln(w, "\033[1mConnector install preview\033[0m")
	fmt.Fprintln(w)
	fmt.Fprintf(w, "  FQN:        %s\n", p.Fqn)
	fmt.Fprintf(w, "  Version:    %s\n", p.Version)
	fmt.Fprintf(w, "  Hash:       %s\n", shortHash(p.Hash))
	fmt.Fprintf(w, "  Publisher:  %s\n", p.Publisher)
	fmt.Fprintf(w, "  Signature:  \033[32m%s\033[0m\n", p.SignatureStatus)
	hasAny := false
	if len(p.Capabilities.NetworkHosts) > 0 {
		hasAny = true
		fmt.Fprintln(w)
		fmt.Fprintln(w, "  \033[1mNetwork access:\033[0m")
		for _, h := range p.Capabilities.NetworkHosts {
			fmt.Fprintf(w, "    - %s\n", h)
		}
	}
	if p.Capabilities.Credential != nil {
		hasAny = true
		fmt.Fprintln(w)
		fmt.Fprintln(w, "  \033[1mCredential required:\033[0m")
		fmt.Fprintf(w, "    kind:  %s\n", p.Capabilities.Credential.Kind)
		if p.Capabilities.Credential.Scope != nil && *p.Capabilities.Credential.Scope != "" {
			fmt.Fprintf(w, "    scope: %s\n", *p.Capabilities.Credential.Scope)
		}
	}
	if !hasAny {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "  Capabilities: \033[2m(none declared)\033[0m")
	}
	fmt.Fprintln(w)
}

// actionPreviewWire mirrors api.ActionPreview on the wire. Defined
// locally so the CLI binary doesn't pull the full generated types
// graph just to render this surface.
type actionPreviewWire struct {
	Fqn              string                  `json:"fqn"`
	Version          string                  `json:"version"`
	Hash             string                  `json:"hash"`
	Name             string                  `json:"name"`
	Intent           string                  `json:"intent,omitempty"`
	SignatureStatus  string                  `json:"signature_status,omitempty"`
	AlreadyInstalled bool                    `json:"already_installed,omitempty"`
	Existing         *installedActionRefWire `json:"existing,omitempty"`
	ConnectorDeps    []struct {
		Fqn              string   `json:"fqn"`
		Version          string   `json:"version"`
		Hash             string   `json:"hash"`
		Capabilities     []string `json:"capabilities,omitempty"`
		AlreadyInstalled bool     `json:"already_installed"`
	} `json:"connector_deps"`
}

// installedActionRefWire mirrors api.InstalledActionRef. Set by the
// preview when an action with the same name is already installed but
// its bytes differ — the CLI uses this to render the upgrade prompt.
type installedActionRefWire struct {
	Version string `json:"version"`
	Hash    string `json:"hash"`
	Source  string `json:"source"`
	Path    string `json:"path"`
}

// staleBindingWire mirrors api.StaleBinding. Populated on the
// install response when this install caused one or more existing
// bindings to transition to `stale` — either because the new manifest
// demands a scope the recorded grant lacks (`scope_drift`) or because
// the binding predates scope tracking and needs a one-time
// reauthorize (`no_grant_record`). The CLI prompts the operator to
// drop into `aileron binding reauthorize <name>` inline (#741).
type staleBindingWire struct {
	Name          string   `json:"name"`
	ConnectorFQN  string   `json:"connector_fqn"`
	StaleReason   string   `json:"stale_reason"`
	MissingScopes []string `json:"missing_scopes,omitempty"`
	Service       string   `json:"service,omitempty"`
}

// renderActionPreview prints the consent-prompt summary for an
// action add. Sections: action metadata (name, version, hash,
// intent, signature status), then the connector deps split into
// "already installed" (informational) vs. "will be installed"
// (the operator's actual consent decision).
func renderActionPreview(w io.Writer, p *actionPreviewWire) {
	fmt.Fprintln(w)
	fmt.Fprintln(w, "\033[1mAction install preview\033[0m")
	fmt.Fprintln(w)
	fmt.Fprintf(w, "  Name:       %s\n", p.Name)
	fmt.Fprintf(w, "  Version:    %s\n", p.Version)
	fmt.Fprintf(w, "  Source:     %s@%s\n", p.Fqn, p.Version)
	fmt.Fprintf(w, "  Hash:       %s\n", shortHash(p.Hash))
	if p.Intent != "" {
		fmt.Fprintf(w, "  Intent:     %s\n", p.Intent)
	}
	switch p.SignatureStatus {
	case "verified":
		fmt.Fprintf(w, "  Signature:  \033[32mverified\033[0m\n")
	case "unsigned":
		fmt.Fprintf(w, "  Signature:  \033[33munsigned\033[0m (v1 makes signing optional)\n")
	}

	var newDeps, existingDeps int
	for _, d := range p.ConnectorDeps {
		if d.AlreadyInstalled {
			existingDeps++
		} else {
			newDeps++
		}
	}

	if newDeps > 0 {
		fmt.Fprintln(w)
		fmt.Fprintf(w, "  \033[1mConnectors that will be installed (%d):\033[0m\n", newDeps)
		for _, d := range p.ConnectorDeps {
			if d.AlreadyInstalled {
				continue
			}
			fmt.Fprintf(w, "    + %s@%s\n", d.Fqn, d.Version)
			if d.Hash != "" {
				fmt.Fprintf(w, "      hash: %s\n", shortHash(d.Hash))
			}
			if len(d.Capabilities) > 0 {
				fmt.Fprintf(w, "      capabilities: %s\n", strings.Join(d.Capabilities, ", "))
			}
		}
	}
	if existingDeps > 0 {
		fmt.Fprintln(w)
		fmt.Fprintf(w, "  \033[2mConnectors already installed (%d):\033[0m\n", existingDeps)
		for _, d := range p.ConnectorDeps {
			if !d.AlreadyInstalled {
				continue
			}
			fmt.Fprintf(w, "    \033[2m✓ %s@%s\033[0m\n", d.Fqn, d.Version)
		}
	}
	if newDeps == 0 && existingDeps == 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "  \033[2mNo connector dependencies declared.\033[0m")
	}
	fmt.Fprintln(w)
}

// renderActionUpgradeBanner prints the side-by-side "version X is
// installed; version Y is requested" panel above the upgrade prompt.
// Only called when preview.Existing is non-nil — the caller has
// already rendered the regular preview, so this banner only needs to
// surface the existing install the operator may not know about.
func renderActionUpgradeBanner(w io.Writer, p *actionPreviewWire) {
	if p.Existing == nil {
		return
	}
	fmt.Fprintln(w, "\033[1;33mUpgrade required\033[0m — an action with this name is already installed.")
	fmt.Fprintln(w)
	fmt.Fprintf(w, "  Installed:  v%s  hash: %s\n",
		p.Existing.Version, shortHash(p.Existing.Hash))
	if p.Existing.Source != "" {
		fmt.Fprintf(w, "              source: %s\n", p.Existing.Source)
	}
	fmt.Fprintf(w, "  Requested:  v%s  hash: %s\n",
		p.Version, shortHash(p.Hash))
	fmt.Fprintf(w, "              source: %s@%s\n", p.Fqn, p.Version)
	fmt.Fprintln(w)
	fmt.Fprintf(w, "  Confirming will \033[1moverwrite\033[0m %s.\n", p.Existing.Path)
	fmt.Fprintln(w)
}

// shortHash renders only the first 12 hex chars after the algorithm
// prefix — enough to disambiguate but readable in the consent prompt.
// Operators who want the full hash can read it from `aileron status`.
func shortHash(h string) string {
	const prefix = "sha256:"
	if !strings.HasPrefix(h, prefix) {
		return h
	}
	rest := h[len(prefix):]
	if len(rest) > 12 {
		return prefix + rest[:12] + "…"
	}
	return h
}

// actionAddOptions carries the per-install knobs that drive
// installOneAction. Both the single-action and suite paths build
// one of these per action; the only difference is the source
// (CLI argv vs. suite manifest entry).
type actionAddOptions struct {
	fqn     string // raw FQN; may include @<version>
	version string // optional separate version flag
	force   bool
	yes     bool
	noBind  bool
	// suiteConsented is set by the suite-install path after the user
	// has approved the suite-level consent screen. installOneAction
	// then skips its own per-action Y/N prompt and the per-action
	// preview render — both have already been surfaced at the suite
	// level. Distinct from `yes` (which means "the operator passed
	// --yes on the CLI; skip every interactive prompt including
	// publisher trust") so each path can degrade independently.
	suiteConsented bool
	// outcome, when non-nil, is populated with a richer status code
	// so the suite-install path can render a per-entry summary that
	// distinguishes added / upgraded / already-installed / skipped /
	// cancelled / failed. The single-action caller leaves it nil and
	// relies on the int return alone.
	outcome *actionInstallOutcome
}

// actionInstallOutcome is the rich status the suite path collects for
// each entry. The integer return code from installOneAction tells the
// caller success vs. failure; this struct adds the texture needed to
// render "Upgraded" vs. "Added" vs. "Skipped" in the summary.
//
// staleBindings collects bindings the install transitioned to `stale`
// so the suite-install path can present a single batch reauthorize
// prompt at the end of the loop (#741) — per-action prompts would
// fragment the OAuth dances across each install, which is the very
// disruption ADR-0006 / #726 was avoiding when it chose against a
// mid-install reauthorize prompt.
type actionInstallOutcome struct {
	status        actionInstallStatus
	staleBindings []staleBindingWire
}

type actionInstallStatus string

const (
	actionInstallStatusAdded            actionInstallStatus = "added"
	actionInstallStatusUpgraded         actionInstallStatus = "upgraded"
	actionInstallStatusAlreadyInstalled actionInstallStatus = "already_installed"
	actionInstallStatusSkipped          actionInstallStatus = "skipped"
	actionInstallStatusCancelled        actionInstallStatus = "cancelled"
	actionInstallStatusFailed           actionInstallStatus = "failed"
)

// runActionAdd installs a single action by FQN (issue #563
// auto-trust + connector install). Suite installs go through the
// peer subcommand `aileron action add-suite` (issue #564).
func runActionAdd(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fqnArg, rest, ok := extractPositional(args)
	if !ok {
		fmt.Fprintln(stderr, actionUsage)
		return 1
	}
	flags := flag.NewFlagSet("action add", flag.ContinueOnError)
	flags.SetOutput(stderr)
	version := flags.String("version", "", "strict SemVer (required if FQN omits @<version>)")
	force := flags.Bool("force", false, "overwrite an existing action with the same name")
	noBind := flags.Bool("no-bind", false, "skip the auto-prompt for unbound credentials (use in scripts)")
	yes := flags.Bool("yes", false, "skip the consent prompt and proceed without confirmation")
	if err := flags.Parse(rest); err != nil {
		return 1
	}
	opts := actionAddOptions{
		fqn:     fqnArg,
		version: *version,
		force:   *force,
		yes:     *yes,
		noBind:  *noBind,
	}
	return installOneAction(opts, stdin, stdout, stderr, newTrustState())
}

// installOneAction runs the full trust → preview → consent →
// install → auto-bind flow for a single action. The trust state is
// passed in so suite installs can share it across actions; a
// single-action invocation passes a fresh state.
//
// Per ADR-0006 v1 UX, on success it prompts the user to drop into
// the binding setup flow for any unbound credential capabilities
// the action's connectors declare. The user stays in the CLI
// surface throughout, which avoids the
// hit-binding_required-at-agent-invocation friction.
//
// When the server returns `connectors_missing` (issue #413), the
// CLI shows the resolved FQN, version, and hash for each missing
// connector and retries the install with `auto_install_connectors=true`
// so the server runs the connector pipeline transparently.
func installOneAction(opts actionAddOptions, stdin io.Reader, stdout, stderr io.Writer, trust *trustState) int {
	setOutcome := func(s actionInstallStatus) {
		if opts.outcome != nil {
			opts.outcome.status = s
		}
	}
	resolvedFQN, resolvedVersion, perr := splitFQNVersion(opts.fqn, opts.version)
	if perr != nil {
		fmt.Fprintf(stderr, "%v\n", perr)
		setOutcome(actionInstallStatusFailed)
		return 1
	}

	// Step 0 (issue #563): ensure the action's authority is trusted.
	// Preview verifies the action signature against this authority's
	// keys; without one, the daemon would fail with `signature_failure`
	// before we ever rendered the consent prompt.
	actionFQN, perr := cstore.ParseFQN(resolvedFQN)
	if perr != nil {
		fmt.Fprintf(stderr, "error: %v\n", perr)
		setOutcome(actionInstallStatusFailed)
		return 1
	}

	// Step 0a (ADR-0013 action-first amendment / #709): try the Hub
	// composite install-decision flow. If the action is Hub-listed (200),
	// render one trust panel grouping the action plus every connector
	// authority it transitively depends on, and collapse trust consent
	// into a single y/N. Suite-consented installs skip this — the suite-
	// level composite (or the suite preflight) already covered consent.
	if !opts.suiteConsented {
		accepted, hubDeclined, hubPath, payload := tryHubActionInstallDecisionFlow(
			resolvedFQN, opts.yes, stdin, stdout, stderr)
		if hubDeclined {
			setOutcome(actionInstallStatusCancelled)
			return 0
		}
		if hubPath && accepted && payload != nil {
			// Operator consented to the composite. Programmatically
			// trust the action's own authority plus each connector
			// authority surfaced by the composite. fetchPublisherKey
			// re-fetches and re-fingerprints; a publisher who rotates
			// between the composite call and this fetch is caught by
			// the existing fingerprint check in keyring add — same code
			// path as `aileron keyring trust`. Marking trustState
			// suppresses the per-authority prompts that fire later in
			// installOneAction.
			if err := trust.ensure(actionFQN.Authority(), true, stdin, stdout, stderr); err != nil {
				fmt.Fprintf(stderr, "error: %v\n", err)
				setOutcome(actionInstallStatusFailed)
				return 1
			}
			for _, auth := range payload.Authorities {
				authFQN, parseErr := cstore.ParseFQN(auth.Fqn)
				if parseErr != nil {
					// Defensive: the daemon validated this already.
					continue
				}
				if err := trust.ensure(authFQN.Authority(), true, stdin, stdout, stderr); err != nil {
					fmt.Fprintf(stderr, "error: %v\n", err)
					setOutcome(actionInstallStatusFailed)
					return 1
				}
			}
		}
	}

	if err := trust.ensure(actionFQN.Authority(), opts.yes, stdin, stdout, stderr); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		setOutcome(actionInstallStatusFailed)
		return 1
	}

	// Step 1: preview. Fetches + parses the action manifest plus
	// enumerates connector deps with their already-installed status.
	// Signature failure or parse error aborts here, before the
	// consent prompt — `--yes` does not bypass.
	previewBody := map[string]any{"fqn": resolvedFQN, "version": resolvedVersion}
	previewJSON, _ := json.Marshal(previewBody)
	previewStatus, previewRaw, err := bindingDoRequest(http.MethodPost, "/actions/preview",
		strings.NewReader(string(previewJSON)))
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		setOutcome(actionInstallStatusFailed)
		return 1
	}
	if previewStatus != http.StatusOK {
		fmt.Fprintf(stderr, "server returned %d: %s\n", previewStatus, string(previewRaw))
		setOutcome(actionInstallStatusFailed)
		return 1
	}
	var preview actionPreviewWire
	if err := json.Unmarshal(previewRaw, &preview); err != nil {
		fmt.Fprintf(stderr, "error parsing preview: %v\n", err)
		setOutcome(actionInstallStatusFailed)
		return 1
	}

	// Already-installed short-circuit: same name + same hash → no
	// prompt, no install. Mirrors the connector-install pattern.
	if preview.AlreadyInstalled {
		fmt.Fprintf(stdout, "Already installed: %s\n  hash: %s\n", preview.Name, preview.Hash)
		setOutcome(actionInstallStatusAlreadyInstalled)
		return 0
	}

	// Upgrade detection: same name on disk but different bytes.
	// We render the regular preview so the operator sees the
	// incoming manifest, plus an upgrade banner showing the
	// installed version side-by-side, then prompt for confirmation
	// to overwrite. On accept, the install retries with force=true;
	// on decline, exit 0 with a "skipped" outcome so the suite
	// summary doesn't count it as a failure.
	upgrade := preview.Existing != nil
	forceForInstall := opts.force

	// Step 2: render the consent prompt. Shows action metadata plus
	// the connector deps split into "already installed" vs. "will
	// be installed alongside this action".
	//
	// Suite-consented installs skip this render: the suite preview
	// already showed the operator what's coming. Per-action verbosity
	// during the install loop would just repeat that for every entry.
	if !opts.suiteConsented {
		renderActionPreview(stdout, &preview)
	}

	// Step 2b (issue #563): for any connector dep that will be newly
	// installed and whose authority differs from the action's,
	// prompt to trust before the install consent. Already-installed
	// deps are skipped — their authorities were trusted on the
	// install that put them in the cstore.
	//
	// Suite-consented installs pre-resolve every dep authority in the
	// preflight phase, so this loop sees trustState.ensure as a fast
	// no-op (already-trusted short-circuit) and never prompts.
	for _, dep := range preview.ConnectorDeps {
		if dep.AlreadyInstalled {
			continue
		}
		depFQN, perr := cstore.ParseFQN(dep.Fqn)
		if perr != nil {
			// The daemon already validated this — defensive skip.
			continue
		}
		if err := trust.ensure(depFQN.Authority(), opts.yes, stdin, stdout, stderr); err != nil {
			fmt.Fprintf(stderr, "error: %v\n", err)
			setOutcome(actionInstallStatusFailed)
			return 1
		}
	}

	// Step 3: prompt unless --yes or suite-consented. Upgrades get a
	// different prompt that names both versions so the operator can't
	// miss that they're overwriting an existing install.
	switch {
	case opts.yes, opts.suiteConsented:
		// --yes implies consent to upgrade just like fresh install.
		// suite-consented inherits the user's "install all" decision
		// covering both shapes, so the same force-on-upgrade rule
		// applies.
		if upgrade {
			forceForInstall = true
		}
	default:
		var prompt string
		if upgrade {
			renderActionUpgradeBanner(stdout, &preview)
			prompt = fmt.Sprintf("Replace v%s with v%s? [y/n]: ",
				preview.Existing.Version, preview.Version)
		} else {
			prompt = "Install? [y/n]: "
		}
		answer := strings.ToLower(strings.TrimSpace(promptLine(stdin, stdout, prompt)))
		if answer != "y" && answer != "yes" {
			if upgrade {
				fmt.Fprintf(stdout, "Skipped: %s (kept v%s)\n",
					preview.Name, preview.Existing.Version)
				setOutcome(actionInstallStatusSkipped)
			} else {
				fmt.Fprintln(stdout, "Cancelled.")
				setOutcome(actionInstallStatusCancelled)
			}
			return 0
		}
		if upgrade {
			forceForInstall = true
		}
	}

	// Step 4: install. The server walks the connector deps server-
	// side via auto_install_connectors=true. Failures abort the
	// chain atomically — nothing commits unless every step succeeds.
	body := map[string]any{
		"fqn":                     resolvedFQN,
		"version":                 resolvedVersion,
		"auto_install_connectors": true,
	}
	if forceForInstall {
		body["force"] = true
	}
	bodyJSON, _ := json.Marshal(body)
	status, respBody, err := bindingDoRequest(http.MethodPost, "/actions/install",
		strings.NewReader(string(bodyJSON)))
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		setOutcome(actionInstallStatusFailed)
		return 1
	}
	if status != http.StatusCreated && status != http.StatusOK {
		fmt.Fprintf(stderr, "server returned %d: %s\n", status, string(respBody))
		setOutcome(actionInstallStatusFailed)
		return 1
	}
	var resp struct {
		Name                string `json:"name"`
		Fqn                 string `json:"fqn"`
		Version             string `json:"version"`
		Source              string `json:"source"`
		Path                string `json:"path"`
		AlreadyInstalled    bool   `json:"already_installed"`
		UnboundCapabilities []struct {
			ConnectorFQN string  `json:"connector_fqn"`
			Kind         string  `json:"kind"`
			Scope        *string `json:"scope,omitempty"`
		} `json:"unbound_capabilities,omitempty"`
		NewlyStaleBindings []staleBindingWire `json:"newly_stale_bindings,omitempty"`
	}
	if err := json.Unmarshal(respBody, &resp); err != nil {
		fmt.Fprintf(stderr, "error parsing response: %v\n", err)
		setOutcome(actionInstallStatusFailed)
		return 1
	}
	verb := "Added"
	switch {
	case resp.AlreadyInstalled:
		verb = "Already installed"
		setOutcome(actionInstallStatusAlreadyInstalled)
	case upgrade:
		verb = "Upgraded"
		setOutcome(actionInstallStatusUpgraded)
	default:
		setOutcome(actionInstallStatusAdded)
	}
	fmt.Fprintf(stdout, "%s: %s\n  source: %s\n  path: %s\n",
		verb, resp.Name, resp.Source, resp.Path)

	// Hand stale-binding transitions back to the suite path through
	// the per-entry outcome so the loop can offer one batch prompt at
	// the end — installOneAction itself only prompts in the
	// non-suite-consented path so a single-action install still gets
	// the inline prompt.
	if opts.outcome != nil {
		opts.outcome.staleBindings = resp.NewlyStaleBindings
	}

	if opts.noBind || len(resp.UnboundCapabilities) == 0 {
		if len(resp.UnboundCapabilities) > 0 {
			fmt.Fprintln(stdout, "")
			fmt.Fprintln(stdout, "Action has unbound credential capabilities:")
			for _, u := range resp.UnboundCapabilities {
				fmt.Fprintf(stdout, "  - %s (%s)\n", u.ConnectorFQN, u.Kind)
			}
			fmt.Fprintln(stdout, "Run `aileron binding setup <connector-FQN>` for each before invoking the action.")
		}
		// Even when --no-bind suppresses the prompt, surface the
		// stale-binding list so a scripted install path doesn't
		// silently drop the signal. Skipped under suite-consented
		// installs because the suite loop owns the batch prompt.
		if !opts.suiteConsented {
			promptReauthorizeStaleBindings(resp.NewlyStaleBindings, opts.yes, opts.noBind, stdin, stdout, stderr)
		}
		return 0
	}

	// Prompt the user to bind each unbound capability now.
	for _, u := range resp.UnboundCapabilities {
		fmt.Fprintln(stdout, "")
		desc := u.Kind
		if u.Scope != nil && *u.Scope != "" {
			desc = u.Kind + " — " + *u.Scope
		}
		answer := promptLine(stdin, stdout,
			fmt.Sprintf("This action needs %s access for %s. Set up now? [Y/n]: ", desc, u.ConnectorFQN))
		if strings.EqualFold(answer, "n") || strings.EqualFold(answer, "no") {
			fmt.Fprintf(stdout, "  Skipped. Run `aileron binding setup %s` later.\n", u.ConnectorFQN)
			continue
		}
		if rc := runBindingSetup([]string{u.ConnectorFQN}, stdin, stdout, stderr); rc != 0 {
			fmt.Fprintf(stderr, "  binding setup for %s exited %d\n", u.ConnectorFQN, rc)
			// Continue to the next capability — partial bind is
			// better than aborting; the user can retry the failing
			// one later.
		}
	}

	// Suite-consented installs defer the stale-binding prompt to the
	// suite loop, which dedupes across all entries and presents one
	// batch prompt at the tail; a per-action prompt here would
	// fragment the OAuth dances and undo that batching.
	if !opts.suiteConsented {
		promptReauthorizeStaleBindings(resp.NewlyStaleBindings, opts.yes, opts.noBind, stdin, stdout, stderr)
	}
	return 0
}

// promptReauthorizeStaleBindings walks a stale-binding list emitted
// by an install endpoint and, for each entry, offers to drop into
// `aileron binding reauthorize <name>` inline. Mirrors the existing
// unbound-capability prompt loop at the tail of installOneAction —
// the operator stays in the CLI surface, doesn't have to navigate to
// `aileron binding list` or the webapp to discover the stale state,
// and the install does not silently complete with a credential the
// next action invocation will fail against.
//
// Behavior:
//
//   - noBind suppresses the prompt entirely but still prints the list
//     so a scripted install retains the signal in stdout (parity with
//     the existing unbound-capability `--no-bind` branch above).
//   - autoYes prints the list but does NOT auto-reauthorize. The
//     reauthorize flow opens a browser tab; doing that without an
//     explicit Y is a foot-gun. Operators scripting installs see the
//     list and decide; "skip the prompt" defaults to "skip the
//     credential operation," not "do it without confirmation."
//   - Otherwise, each binding gets its own Y/n. "n" prints a hint
//     pointing at the standalone `aileron binding reauthorize` verb
//     and the loop continues — partial reauthorize is better than
//     aborting the rest of the list on one decline.
func promptReauthorizeStaleBindings(stale []staleBindingWire, autoYes, noBind bool, stdin io.Reader, stdout, stderr io.Writer) {
	if len(stale) == 0 {
		return
	}
	if noBind {
		fmt.Fprintln(stdout, "")
		fmt.Fprintln(stdout, "This install requires reauthorization on existing bindings:")
		for _, b := range stale {
			fmt.Fprintf(stdout, "  - %s\n", staleBindingHeader(b))
		}
		fmt.Fprintln(stdout, "Run `aileron binding reauthorize <name>` for each before invoking the action.")
		return
	}
	if autoYes {
		// --yes signals "no interactive prompts" but the credential
		// operation requires its own browser hand-off and explicit
		// confirmation; we surface the work to do instead of doing it.
		fmt.Fprintln(stdout, "")
		fmt.Fprintln(stdout, "This install requires reauthorization on existing bindings:")
		for _, b := range stale {
			renderStaleBindingDetail(stdout, b)
			fmt.Fprintf(stdout, "    Run `aileron binding reauthorize %s` when ready.\n", b.Name)
		}
		return
	}
	for _, b := range stale {
		fmt.Fprintln(stdout, "")
		renderStaleBindingDetail(stdout, b)
		answer := promptLine(stdin, stdout, "    Reauthorize now? [Y/n]: ")
		if strings.EqualFold(answer, "n") || strings.EqualFold(answer, "no") {
			fmt.Fprintf(stdout, "    Skipped. Run `aileron binding reauthorize %s` when ready.\n", b.Name)
			continue
		}
		if rc := runBindingReauthorize([]string{b.Name}, stdout, stderr); rc != 0 {
			fmt.Fprintf(stderr, "    binding reauthorize for %s exited %d\n", b.Name, rc)
		}
	}
}

// staleBindingHeader formats the "name (label)" line used in
// non-interactive paths (--no-bind list). Service is preferred for
// human readability; FQN is the fallback for older daemons that
// don't populate the optional Service field.
func staleBindingHeader(b staleBindingWire) string {
	label := b.Service
	if label == "" {
		label = b.ConnectorFQN
	}
	return fmt.Sprintf("%s (%s)", b.Name, label)
}

// renderStaleBindingDetail prints the multi-line prompt header for
// one stale binding: name, service/connector, reason-specific
// phrasing (migration vs. drift), and — for drift — the missing
// scope list. The two reason phrasings stay distinct so the operator
// understands a no_grant_record migration is a one-time event rather
// than a sign their manifest is churning; collapsing them into a
// generic "needs reauthorize" line hides that distinction.
func renderStaleBindingDetail(w io.Writer, b staleBindingWire) {
	fmt.Fprintf(w, "  - %s\n", staleBindingHeader(b))
	switch b.StaleReason {
	case "scope_drift":
		fmt.Fprintln(w, "    This upgrade requires additional OAuth scopes the existing binding doesn't grant.")
		if len(b.MissingScopes) > 0 {
			fmt.Fprintf(w, "    Missing: %s\n", strings.Join(b.MissingScopes, ", "))
		}
	case "no_grant_record":
		fmt.Fprintln(w, "    This connector now tracks granted OAuth scopes; the existing binding predates")
		fmt.Fprintln(w, "    that and needs a one-time reauthorize to record what was granted.")
	default:
		fmt.Fprintln(w, "    Binding marked stale and needs reauthorize.")
	}
}

// runActionAddSuite implements `aileron action add-suite <SOURCE>`
// (issue #564). Source is one of:
//
//   - Remote: `<scheme>://<owner>/<repo>/<file-path>@<ref>` — the
//     CLI resolves the ref (latest / tag / sha), fetches the suite
//     manifest from raw.githubusercontent.com, and uses the resolved
//     ref to set the install version of path-form entries.
//   - Local: any path without a `<scheme>://` prefix — the CLI
//     reads the file from disk and installs entries that all carry
//     explicit `<FQN>@<version>` (path-form requires a remote ref
//     to inherit).
//
// Each action installs in declaration order via installOneAction,
// sharing a single trustState so a publisher's prompt fires once
// per run regardless of how many entries reference it. Failure
// semantics are failure-soft (per-action failures captured in the
// summary; loop continues; nonzero exit if any failed).
func runActionAddSuite(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("action add-suite", flag.ContinueOnError)
	flags.SetOutput(stderr)
	force := flags.Bool("force", false, "overwrite existing actions with the same name")
	noBind := flags.Bool("no-bind", false, "skip the auto-prompt for unbound credentials (use in scripts)")
	yes := flags.Bool("yes", false, "skip the consent prompts and proceed without confirmation")
	if err := flags.Parse(args); err != nil {
		return 1
	}
	if len(flags.Args()) != 1 {
		fmt.Fprintln(stderr, "error: add-suite takes exactly one source argument")
		fmt.Fprintln(stderr, actionUsage)
		return 1
	}
	source := flags.Args()[0]

	opts := actionAddOptions{force: *force, yes: *yes, noBind: *noBind}

	// Buffered stdin so each prompt picks up where the previous
	// left off — same reason runAction wraps stdin for the single-
	// action path.
	br, ok := stdin.(*bufio.Reader)
	if !ok {
		br = bufio.NewReader(stdin)
	}

	if strings.Contains(source, "://") {
		return runActionAddSuiteRemote(source, opts, br, stdout, stderr)
	}
	return runActionAddSuiteLocal(source, opts, br, stdout, stderr)
}

// runActionAddSuiteLocal handles a filesystem-path source: read,
// parse, resolve with no inheritance context (path-form entries
// in a local manifest are an error), then drive the shared loop.
// Local sources never carry a Hub suite FQN, so the composite flow
// is skipped — the install runs through the legacy per-authority
// preflight unchanged.
func runActionAddSuiteLocal(path string, opts actionAddOptions, stdin *bufio.Reader, stdout, stderr io.Writer) int {
	data, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(stderr, "error: read suite manifest: %v\n", err)
		return 1
	}
	manifest, err := suite.Parse(path, data)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	refs, err := suite.Resolve(manifest, cstore.FQN{}, "")
	if err != nil {
		fmt.Fprintf(stderr, "error: %s: %v\n", path, err)
		return 1
	}
	return installSuiteEntries(manifest, refs, "", opts, stdin, stdout, stderr)
}

// runActionAddSuiteRemote handles an FQN-style source: parse the
// `<URL>@<ref>` shape, resolve `latest` to a concrete tag if needed,
// fetch the suite manifest from raw.githubusercontent.com, and
// drive the shared loop with the resolved ref's bare SemVer (when
// applicable) as the inheritance version for path-form entries.
func runActionAddSuiteRemote(source string, opts actionAddOptions, stdin *bufio.Reader, stdout, stderr io.Writer) int {
	suiteFQN, ref, err := parseSuiteSource(source)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	ctx := context.Background()
	resolvedRef := ref
	if ref == "latest" {
		fmt.Fprintf(stdout, "Resolving @latest for %s/%s …\n", suiteFQN.Owner, suiteFQN.Repo)
		tag, rerr := resolveLatestRef(ctx, suiteFQN)
		if rerr != nil {
			fmt.Fprintf(stderr, "error: %v\n", rerr)
			return 1
		}
		resolvedRef = tag
		fmt.Fprintf(stdout, "  → %s\n", tag)
	}

	data, err := fetchSuiteTOML(ctx, suiteFQN, resolvedRef)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	display := source
	manifest, err := suite.Parse(display, data)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}

	// Path-form entries inherit the suite's version. Strip the `v`
	// prefix from a release tag to get bare SemVer; SHAs and other
	// non-SemVer refs leave inheritVersion empty, which surfaces as
	// a clear error from suite.Resolve when path-form entries are
	// present.
	inheritVersion, _ := suiteVersionFromRef(resolvedRef)

	refs, err := suite.Resolve(manifest, suiteFQN, inheritVersion)
	if err != nil {
		fmt.Fprintf(stderr, "error: %s: %v\n", display, err)
		return 1
	}
	// Heuristic: a remote source ending in `.toml` maps to a Hub suite
	// FQN by stripping the extension. The composite flow no-ops when
	// the Hub doesn't list it (404 fall-through).
	hubFQN := deriveHubSuiteFQN(suiteFQN)
	return installSuiteEntries(manifest, refs, hubFQN, opts, stdin, stdout, stderr)
}

// installSuiteEntries drives a suite install in three phases (#640):
//
//  1. Preflight: ensure publisher trust for every action authority,
//     preview every ref against the daemon, ensure trust for every
//     new connector-dep authority. Every prompt the run will issue
//     happens here, before the user commits.
//  2. Consent: render the collapsed suite preview (one line per
//     action) and prompt once for the whole suite. --yes skips the
//     prompt. All-already-installed skips the prompt. Decline marks
//     every action as cancelled and exits 0.
//  3. Install: call installOneAction per ref with suiteConsented=true.
//     trustState is fully populated; per-action previews are silent
//     (already shown in phase 2); no Y/N prompts fire. Per-action
//     failures stay failure-soft; the summary at the end reports
//     counts and a nonzero exit if anything failed.
func installSuiteEntries(manifest *suite.Manifest, refs []cstore.Ref, hubSuiteFQN string, opts actionAddOptions, stdin *bufio.Reader, stdout, stderr io.Writer) int {
	fmt.Fprintf(stdout, "Suite: %s\n", manifest.Name)
	if manifest.Description != "" {
		fmt.Fprintf(stdout, "  %s\n", manifest.Description)
	}
	fmt.Fprintf(stdout, "  %d action(s) to install\n", len(refs))

	trust := newTrustState()

	// Phase 0 (#709): when the suite is Hub-listed, render the composite
	// trust panel covering every connector authority in the dependency
	// closure and collapse per-authority consent into a single y/N.
	// On accept, seed trustState so the legacy preflight prompts in
	// phases 1a and 1c short-circuit; the operator sees one trust
	// gate, not N.
	if hubSuiteFQN != "" {
		accepted, hubDeclined, hubPath, payload := tryHubSuiteInstallDecisionFlow(
			hubSuiteFQN, opts.yes, stdin, stdout, stderr)
		if hubDeclined {
			return 0
		}
		if hubPath && accepted && payload != nil {
			for _, auth := range payload.Authorities {
				authFQN, parseErr := cstore.ParseFQN(auth.Fqn)
				if parseErr != nil {
					continue
				}
				if err := trust.ensure(authFQN.Authority(), true, stdin, stdout, stderr); err != nil {
					fmt.Fprintf(stderr, "error: %v\n", err)
					return 1
				}
			}
			// Each member action's signing authority is what `aileron
			// connector install` would sign against; phase 1a re-checks
			// these so we pre-seed them here too. A typical same-
			// publisher suite collapses to one trust.ensure call.
			for _, a := range payload.MemberActions {
				memberFQN, parseErr := cstore.ParseFQN(a)
				if parseErr != nil {
					continue
				}
				if err := trust.ensure(memberFQN.Authority(), true, stdin, stdout, stderr); err != nil {
					fmt.Fprintf(stderr, "error: %v\n", err)
					return 1
				}
			}
		}
	}

	// Phase 1a: trust action authorities up front so preview can
	// verify signatures. Any decline here aborts the suite before
	// the consent screen — the operator hasn't committed yet.
	if rc := trustAuthoritiesForRefs(refs, opts.yes, trust, stdin, stdout, stderr); rc != 0 {
		return rc
	}

	// Phase 1b: preview every ref. Per-ref preview failures land in
	// the preflight result with a clear status so the consent screen
	// surfaces them; the user can still install the healthy actions.
	previews := previewSuiteRefs(refs, stdout)

	// Phase 1c: trust new connector-dep authorities. Same abort-on-
	// decline rule as 1a; the consent screen wouldn't render honest
	// data with mixed trust state otherwise.
	if rc := trustConnectorDepAuthorities(previews, opts.yes, trust, stdin, stdout, stderr); rc != 0 {
		return rc
	}

	// Phase 2: collapsed consent screen + single Y/N.
	plan := buildSuitePlan(refs, previews)
	if plan.toInstall == 0 && plan.previewFailed == 0 {
		fmt.Fprintln(stdout)
		fmt.Fprintf(stdout, "All %d action(s) already installed; nothing to do.\n", len(refs))
		return 0
	}
	cancelled := false
	if !opts.yes {
		renderSuitePreview(stdout, manifest, plan)
		ans := strings.ToLower(strings.TrimSpace(promptLine(stdin, stdout, "Install? [y/n]: ")))
		if ans != "y" && ans != "yes" {
			cancelled = true
		}
	}

	// Phase 3: install loop. Cancellation collapses every still-
	// pending ref into actionInstallStatusCancelled; preview-failed
	// refs stay failed regardless. The trustState carries the
	// authorities resolved in phase 1.
	results, staleBindings := runSuiteInstallLoop(refs, previews, opts, trust, cancelled, stdin, stdout, stderr)
	// Phase 4 (#741): one batch reauthorize prompt for the
	// deduplicated set of bindings the install loop transitioned to
	// `stale`. Cancellation skips this — no install ran, so no
	// transitions to surface. opts.yes / opts.noBind are respected
	// the same way as in the single-action path.
	if !cancelled {
		promptReauthorizeStaleBindings(staleBindings, opts.yes, opts.noBind, stdin, stdout, stderr)
	}
	return renderSuiteSummary(stdout, results)
}

// suiteRefPreview pairs a ref with its preview attempt. err is non-
// nil when the daemon refused to preview the action; the consent
// screen surfaces that explicitly and the install loop short-circuits
// the ref as failed.
type suiteRefPreview struct {
	ref     cstore.Ref
	preview *actionPreviewWire
	err     error
}

// suitePlan is the per-ref classification the consent screen renders
// and the install loop consults. Built once at the boundary between
// preflight and consent so both paths see the same view.
type suitePlan struct {
	entries         []suitePlanEntry
	toInstall       int
	alreadyExisting int
	previewFailed   int
	hasUpgrade      bool
	connectors      []suitePlanConnector
}

type suitePlanEntry struct {
	ref             string
	name            string
	version         string
	status          suitePlanStatus
	existingVersion string // populated for statusUpgrade
	previewErr      string // populated for statusPreviewFailed
}

type suitePlanStatus int

const (
	statusFresh suitePlanStatus = iota
	statusUpgrade
	statusAlreadyInstalled
	statusPreviewFailed
)

type suitePlanConnector struct {
	fqn              string
	version          string
	alreadyInstalled bool
}

func buildSuitePlan(refs []cstore.Ref, previews []suiteRefPreview) suitePlan {
	plan := suitePlan{entries: make([]suitePlanEntry, 0, len(previews))}
	seenConnectors := map[string]suitePlanConnector{}
	for i, p := range previews {
		entry := suitePlanEntry{ref: refs[i].String()}
		switch {
		case p.err != nil:
			entry.status = statusPreviewFailed
			entry.previewErr = p.err.Error()
			plan.previewFailed++
		case p.preview != nil && p.preview.AlreadyInstalled:
			entry.name = p.preview.Name
			entry.version = p.preview.Version
			entry.status = statusAlreadyInstalled
			plan.alreadyExisting++
		case p.preview != nil && p.preview.Existing != nil:
			entry.name = p.preview.Name
			entry.version = p.preview.Version
			entry.status = statusUpgrade
			entry.existingVersion = p.preview.Existing.Version
			plan.toInstall++
			plan.hasUpgrade = true
		case p.preview != nil:
			entry.name = p.preview.Name
			entry.version = p.preview.Version
			entry.status = statusFresh
			plan.toInstall++
		}
		plan.entries = append(plan.entries, entry)

		// Aggregate connectors deduped by FQN+version. New (not yet
		// installed) trumps already-installed when the same FQN
		// shows up under multiple actions in either state.
		if p.preview == nil {
			continue
		}
		for _, dep := range p.preview.ConnectorDeps {
			key := dep.Fqn + "@" + dep.Version
			cur, exists := seenConnectors[key]
			if !exists {
				seenConnectors[key] = suitePlanConnector{
					fqn: dep.Fqn, version: dep.Version, alreadyInstalled: dep.AlreadyInstalled,
				}
				continue
			}
			if cur.alreadyInstalled && !dep.AlreadyInstalled {
				cur.alreadyInstalled = false
				seenConnectors[key] = cur
			}
		}
	}
	plan.connectors = make([]suitePlanConnector, 0, len(seenConnectors))
	for _, c := range seenConnectors {
		plan.connectors = append(plan.connectors, c)
	}
	return plan
}

func renderSuitePreview(w io.Writer, manifest *suite.Manifest, plan suitePlan) {
	fmt.Fprintln(w)
	fmt.Fprintln(w, "\033[1mSuite install preview\033[0m")
	fmt.Fprintln(w)
	header := fmt.Sprintf("Install %d action(s) from this suite", plan.toInstall)
	if plan.alreadyExisting > 0 {
		header += fmt.Sprintf(" (%d already installed will be skipped)", plan.alreadyExisting)
	}
	if plan.previewFailed > 0 {
		header += fmt.Sprintf(" (%d preview-failed will be marked failed)", plan.previewFailed)
	}
	fmt.Fprintln(w, header+":")
	for _, e := range plan.entries {
		switch e.status {
		case statusFresh:
			fmt.Fprintf(w, "  \033[32m+\033[0m %-30s v%s   (new)\n", e.name, e.version)
		case statusUpgrade:
			fmt.Fprintf(w, "  \033[33m↺\033[0m %-30s v%s   (upgrade from v%s)\n",
				e.name, e.version, e.existingVersion)
		case statusAlreadyInstalled:
			fmt.Fprintf(w, "  \033[2m✓\033[0m \033[2m%-30s v%s   (already installed)\033[0m\n",
				e.name, e.version)
		case statusPreviewFailed:
			fmt.Fprintf(w, "  \033[31m✗\033[0m %-30s        (preview failed: %s)\n",
				e.ref, e.previewErr)
		}
	}
	if len(plan.connectors) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "\033[1mConnectors:\033[0m")
		for _, c := range plan.connectors {
			if c.alreadyInstalled {
				fmt.Fprintf(w, "  \033[2m✓ %s@%s   (already installed)\033[0m\n", c.fqn, c.version)
			} else {
				fmt.Fprintf(w, "  \033[32m+\033[0m %s@%s   (new)\n", c.fqn, c.version)
			}
		}
	}
	fmt.Fprintln(w)
}

// trustAuthoritiesForRefs runs trust.ensure for every unique action
// authority across the refs. Returns nonzero when any authority is
// declined or the keyring write fails — the suite cannot proceed past
// preview without trust, and per-issue #640 the suite is atomic at
// the consent layer, so a partial set of trusted authorities does not
// produce a partial install. Authorities the user already trusts in
// this run short-circuit at trustState.ensure with no further prompt.
func trustAuthoritiesForRefs(refs []cstore.Ref, autoYes bool, trust *trustState, stdin io.Reader, stdout, stderr io.Writer) int {
	for _, ref := range refs {
		// ref.FQN.Authority() is the canonical accessor; the FQN was
		// already validated by suite.Resolve, so no parse error is
		// expected here.
		if err := trust.ensure(ref.FQN.Authority(), autoYes, stdin, stdout, stderr); err != nil {
			fmt.Fprintf(stderr, "error: %v\n", err)
			return 1
		}
	}
	return 0
}

// daemonErrText extracts a human-readable summary from a daemon error
// response body. The daemon returns errors as the api.Error envelope
// (`{"error":{"code":"...","message":"..."}}`); when that parses, we
// render `code: message` so the operator sees the underlying cause
// (e.g. `fetch_failed: 404 (tag or release not found)`) instead of a
// bare HTTP status. Falls back to the raw body for anything that
// doesn't match the envelope, and to `<no body>` for empty responses.
func daemonErrText(raw []byte) string {
	if len(raw) == 0 {
		return "<no body>"
	}
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err == nil && env.Error.Code != "" {
		if env.Error.Message != "" {
			return env.Error.Code + ": " + env.Error.Message
		}
		return env.Error.Code
	}
	return string(raw)
}

// previewSuiteRefs calls /actions/preview for every ref in declaration
// order. Failures are captured per-entry rather than aborting the
// loop; the consent screen surfaces them with their error so the
// operator sees what went wrong.
func previewSuiteRefs(refs []cstore.Ref, _ io.Writer) []suiteRefPreview {
	out := make([]suiteRefPreview, 0, len(refs))
	for _, ref := range refs {
		fqnStr, version, _ := splitFQNVersion(ref.String(), "")
		body, _ := json.Marshal(map[string]any{"fqn": fqnStr, "version": version})
		status, raw, err := bindingDoRequest(http.MethodPost, "/actions/preview",
			strings.NewReader(string(body)))
		if err != nil {
			out = append(out, suiteRefPreview{ref: ref, err: err})
			continue
		}
		if status != http.StatusOK {
			out = append(out, suiteRefPreview{ref: ref,
				err: fmt.Errorf("daemon returned %d: %s", status, daemonErrText(raw))})
			continue
		}
		var preview actionPreviewWire
		if err := json.Unmarshal(raw, &preview); err != nil {
			out = append(out, suiteRefPreview{ref: ref, err: err})
			continue
		}
		out = append(out, suiteRefPreview{ref: ref, preview: &preview})
	}
	return out
}

// trustConnectorDepAuthorities walks the preflight previews, collects
// the unique authorities of every new (not-already-installed)
// connector dep, and runs trust.ensure for each. Same atomic semantics
// as trustAuthoritiesForRefs — any decline aborts before consent.
func trustConnectorDepAuthorities(previews []suiteRefPreview, autoYes bool, trust *trustState, stdin io.Reader, stdout, stderr io.Writer) int {
	for _, p := range previews {
		if p.preview == nil {
			continue
		}
		for _, dep := range p.preview.ConnectorDeps {
			if dep.AlreadyInstalled {
				continue
			}
			depFQN, perr := cstore.ParseFQN(dep.Fqn)
			if perr != nil {
				continue
			}
			if err := trust.ensure(depFQN.Authority(), autoYes, stdin, stdout, stderr); err != nil {
				fmt.Fprintf(stderr, "error: %v\n", err)
				return 1
			}
		}
	}
	return 0
}

// suiteInstallResult mirrors the per-ref slot of the pre-#640 anonymous
// struct in installSuiteEntries; named here so the helper can return
// a clean slice.
type suiteInstallResult struct {
	ref     string
	rc      int
	outcome actionInstallStatus
}

// runSuiteInstallLoop runs installOneAction per ref, threading the
// already-populated trustState through. cancelled=true short-circuits
// every ref into actionInstallStatusCancelled and skips the install
// call entirely (this is what fires after the user says "n" at the
// suite consent prompt).
//
// Preview-failed refs (from phase 1b) are reported as failed without
// re-running installOneAction — the underlying preview call would
// just fail again.
//
// Returns the per-ref result list plus the deduplicated set of
// bindings the loop flipped to `stale` across every install (#741).
// Suite-consented entries defer their per-install reauthorize prompt
// to the caller, which presents a single batch prompt at the tail
// instead of N fragmented browser hand-offs — the disruption #726
// flagged when it chose against a mid-install reauthorize prompt.
func runSuiteInstallLoop(refs []cstore.Ref, previews []suiteRefPreview, opts actionAddOptions, trust *trustState, cancelled bool, stdin *bufio.Reader, stdout, stderr io.Writer) ([]suiteInstallResult, []staleBindingWire) {
	results := make([]suiteInstallResult, 0, len(refs))
	var aggregate []staleBindingWire
	seen := make(map[string]struct{})
	for i, ref := range refs {
		fmt.Fprintln(stdout)
		fmt.Fprintf(stdout, "── [%d/%d] %s\n", i+1, len(refs), ref.String())
		if previews[i].err != nil {
			fmt.Fprintf(stderr, "  preview failed earlier: %v\n", previews[i].err)
			results = append(results, suiteInstallResult{ref: ref.String(), rc: 1, outcome: actionInstallStatusFailed})
			continue
		}
		if cancelled {
			results = append(results, suiteInstallResult{ref: ref.String(), rc: 0, outcome: actionInstallStatusCancelled})
			continue
		}
		entryOpts := opts
		entryOpts.fqn = ref.String()
		entryOpts.suiteConsented = true
		var outcome actionInstallOutcome
		entryOpts.outcome = &outcome
		rc := installOneAction(entryOpts, stdin, stdout, stderr, trust)
		results = append(results, suiteInstallResult{ref: ref.String(), rc: rc, outcome: outcome.status})
		for _, sb := range outcome.staleBindings {
			// Dedupe by binding name: two actions that share the same
			// connector both fire the hook against the same binding,
			// and we want to prompt for it exactly once at the tail.
			if _, ok := seen[sb.Name]; ok {
				continue
			}
			seen[sb.Name] = struct{}{}
			aggregate = append(aggregate, sb)
		}
	}
	return results, aggregate
}

// renderSuiteSummary prints the per-ref outcome list + the rolled-up
// count line. Mirrors the pre-#640 trailing summary; returns the exit
// code (nonzero if any ref failed). Cancellation counts toward the
// summary; the user explicitly chose to cancel, so the run is "all
// cancelled" rather than "0 added, 4 cancelled".
func renderSuiteSummary(w io.Writer, results []suiteInstallResult) int {
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Suite install summary:")
	var counts struct {
		added, upgraded, alreadyInstalled, skipped, cancelled, failed int
	}
	for _, r := range results {
		marker, label := summaryLine(r.outcome, r.rc)
		fmt.Fprintf(w, "  %s %s%s\n", marker, r.ref, label)
		switch r.outcome {
		case actionInstallStatusAdded:
			counts.added++
		case actionInstallStatusUpgraded:
			counts.upgraded++
		case actionInstallStatusAlreadyInstalled:
			counts.alreadyInstalled++
		case actionInstallStatusSkipped:
			counts.skipped++
		case actionInstallStatusCancelled:
			counts.cancelled++
		case actionInstallStatusFailed:
			counts.failed++
		default:
			if r.rc == 0 {
				counts.added++
			} else {
				counts.failed++
			}
		}
	}
	fmt.Fprintln(w)
	parts := make([]string, 0, 6)
	if counts.added > 0 {
		parts = append(parts, fmt.Sprintf("%d added", counts.added))
	}
	if counts.upgraded > 0 {
		parts = append(parts, fmt.Sprintf("%d upgraded", counts.upgraded))
	}
	if counts.alreadyInstalled > 0 {
		parts = append(parts, fmt.Sprintf("%d already installed", counts.alreadyInstalled))
	}
	if counts.skipped > 0 {
		parts = append(parts, fmt.Sprintf("%d skipped", counts.skipped))
	}
	if counts.cancelled > 0 {
		parts = append(parts, fmt.Sprintf("%d cancelled", counts.cancelled))
	}
	if counts.failed > 0 {
		parts = append(parts, fmt.Sprintf("%d failed", counts.failed))
	}
	fmt.Fprintf(w, "%d action(s): %s.\n", len(results), strings.Join(parts, ", "))
	if counts.failed > 0 {
		return 1
	}
	return 0
}

// summaryLine returns the per-entry marker glyph and trailing label
// the suite summary uses for one result. Failures still surface as a
// red ✗; non-failure no-ops (already installed, declined upgrade, fresh
// cancel) get neutral markers so the operator doesn't read them as
// errors.
func summaryLine(s actionInstallStatus, rc int) (string, string) {
	switch s {
	case actionInstallStatusAdded:
		return "\033[32m✓\033[0m", " (added)"
	case actionInstallStatusUpgraded:
		return "\033[32m✓\033[0m", " (upgraded)"
	case actionInstallStatusAlreadyInstalled:
		return "\033[2m✓\033[0m", " (already installed)"
	case actionInstallStatusSkipped:
		return "\033[2m·\033[0m", " (skipped — kept existing version)"
	case actionInstallStatusCancelled:
		return "\033[2m·\033[0m", " (cancelled)"
	case actionInstallStatusFailed:
		return "\033[31m✗\033[0m", fmt.Sprintf(" (exit %d)", rc)
	default:
		if rc == 0 {
			return "\033[32m✓\033[0m", ""
		}
		return "\033[31m✗\033[0m", fmt.Sprintf(" (exit %d)", rc)
	}
}

// extractPositional pulls the first non-flag argument out of args and
// returns it plus the remaining slice. Used by `aileron connector
// install <FQN> [--flags]` and `aileron action add <FQN> [--flags]`
// because Go's stdlib flag.Parse stops at the first non-flag, so a
// positional FQN before flags would prevent flag parsing.
//
// Returns ok=false when args is empty or has only flag-shaped entries
// (i.e. no FQN). The "first positional" heuristic is the simplest one
// that matches `<command> <FQN> --flag=value` ergonomics.
func extractPositional(args []string) (string, []string, bool) {
	for i, a := range args {
		if strings.HasPrefix(a, "-") {
			continue
		}
		rest := append([]string(nil), args[:i]...)
		rest = append(rest, args[i+1:]...)
		return a, rest, true
	}
	return "", nil, false
}

// splitFQNVersion accepts either "<fqn>" + a separate version flag, or
// "<fqn>@<version>" with no flag. Returns the FQN and version; errors
// when both forms supply a version (ambiguous) or neither does
// (missing version).
func splitFQNVersion(fqnArg, versionFlag string) (string, string, error) {
	if at := strings.LastIndex(fqnArg, "@"); at >= 0 {
		fqn := fqnArg[:at]
		v := fqnArg[at+1:]
		if versionFlag != "" && versionFlag != v {
			return "", "", fmt.Errorf("FQN already includes @%s; --version=%s conflicts", v, versionFlag)
		}
		if v == "" {
			return "", "", fmt.Errorf("FQN ends with @ but no version follows")
		}
		return fqn, v, nil
	}
	if versionFlag == "" {
		return "", "", fmt.Errorf("version is required (use --version=<v> or <FQN>@<version>)")
	}
	return fqnArg, versionFlag, nil
}

// auditEventWire mirrors api.AuditEvent. Local copy so the CLI binary
// doesn't pull in the full generated types graph for one read.
type auditEventWire struct {
	AuditID   string         `json:"audit_id"`
	EventType string         `json:"event_type"`
	Timestamp time.Time      `json:"timestamp"`
	Actor     auditActorWire `json:"actor"`
	Payload   map[string]any `json:"payload"`
}

type auditActorWire struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

type auditListWire struct {
	Events []auditEventWire `json:"events"`
}

const auditUsage = `usage:
  aileron audit list  [--since RFC3339] [--audit-id ID] [--connector FQN] [--class CLASS] [--limit N] [--json]
  aileron audit show  <audit-id>`

// auditListFetcher and auditGetFetcher are the HTTP clients for the
// two audit endpoints. Replaceable in tests so they don't depend on a
// running daemon (same pattern as runtimeStatusFetcher / connectorCheckFetcher).
var (
	auditListFetcher = fetchAuditList
	auditGetFetcher  = fetchAuditGet
)

type auditListQuery struct {
	since     string
	auditID   string
	connector string
	class     string
	limit     int
}

func fetchAuditList(q auditListQuery) (*auditListWire, error) {
	base, err := bindingAPIBaseURL()
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(base + "/audit")
	if err != nil {
		return nil, err
	}
	qs := u.Query()
	if q.since != "" {
		qs.Set("since", q.since)
	}
	if q.auditID != "" {
		qs.Set("audit_id", q.auditID)
	}
	if q.connector != "" {
		qs.Set("connector_fqn", q.connector)
	}
	if q.class != "" {
		qs.Set("class", q.class)
	}
	if q.limit > 0 {
		qs.Set("limit", strconv.Itoa(q.limit))
	}
	u.RawQuery = qs.Encode()
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(u.String())
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("daemon returned %d: %s", resp.StatusCode, string(raw))
	}
	var out auditListWire
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}
	return &out, nil
}

func fetchAuditGet(auditID string) (*auditEventWire, int, error) {
	base, err := bindingAPIBaseURL()
	if err != nil {
		return nil, 0, err
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(base + "/audit/" + url.PathEscape(auditID))
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, http.StatusNotFound, nil
	}
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		return nil, resp.StatusCode, fmt.Errorf("daemon returned %d: %s", resp.StatusCode, string(raw))
	}
	var out auditEventWire
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("decoding response: %w", err)
	}
	return &out, resp.StatusCode, nil
}

// runAudit dispatches `aileron audit <subcommand>`. With no subcommand
// it lists; that mirrors `aileron log`'s "no-flag → list" shape.
func runAudit(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return runAuditList(nil, stdout, stderr)
	}
	switch args[0] {
	case "list":
		return runAuditList(args[1:], stdout, stderr)
	case "show":
		return runAuditShow(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown audit command: %q\n", args[0])
		fmt.Fprintln(stderr, auditUsage)
		return 1
	}
}

func runAuditList(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("audit list", flag.ContinueOnError)
	flags.SetOutput(stderr)
	since := flags.String("since", "", "Lower-bound on event timestamp (RFC 3339, e.g. 2026-05-01T00:00:00Z)")
	auditID := flags.String("audit-id", "", "Match events with this exact audit_id")
	connector := flags.String("connector", "", "Match events that reference this connector FQN")
	class := flags.String("class", "", "Match failure events with this class (e.g. binding_required)")
	limit := flags.Int("limit", 0, "Maximum events to return (default: server default of 100)")
	asJSON := flags.Bool("json", false, "Render full event records as JSON, one per line")
	if err := flags.Parse(args); err != nil {
		return 1
	}

	resp, err := auditListFetcher(auditListQuery{
		since:     *since,
		auditID:   *auditID,
		connector: *connector,
		class:     *class,
		limit:     *limit,
	})
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	if len(resp.Events) == 0 {
		if *asJSON {
			fmt.Fprintln(stdout, "[]")
			return 0
		}
		fmt.Fprintln(stdout, "No audit events.")
		return 0
	}
	if *asJSON {
		enc := json.NewEncoder(stdout)
		for _, e := range resp.Events {
			if err := enc.Encode(e); err != nil {
				fmt.Fprintf(stderr, "encode: %v\n", err)
				return 1
			}
		}
		return 0
	}
	for _, e := range resp.Events {
		ts := e.Timestamp.Local().Format("2006-01-02 15:04:05")
		fmt.Fprintf(stdout, "%s  %-20s  %-22s  %s\n", ts, e.AuditID, e.EventType, auditPayloadSummary(e))
	}
	return 0
}

func runAuditShow(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("audit show", flag.ContinueOnError)
	flags.SetOutput(stderr)
	if err := flags.Parse(args); err != nil {
		return 1
	}
	rest := flags.Args()
	if len(rest) != 1 {
		fmt.Fprintln(stderr, "usage: aileron audit show <audit-id>")
		return 1
	}
	ev, status, err := auditGetFetcher(rest[0])
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	if status == http.StatusNotFound {
		fmt.Fprintf(stderr, "audit event %q not found\n", rest[0])
		return 1
	}
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(ev); err != nil {
		fmt.Fprintf(stderr, "encode: %v\n", err)
		return 1
	}
	return 0
}

// auditPayloadSummary renders a one-line, human-readable hint about
// the event. Pulls the most identifying fields from each event shape
// the recorder emits today. Payload field names follow the
// OTel-namespaced audit schema (issue #390 Phase 6.5).
func auditPayloadSummary(e auditEventWire) string {
	if e.EventType == "execution.failed" {
		class, _ := e.Payload["aileron.failure.class"].(string)
		conn := payloadConnector(e.Payload)
		if conn != "" {
			return fmt.Sprintf("class=%s connector=%s", class, conn)
		}
		return "class=" + class
	}
	name := payloadName(e.Payload)
	if name != "" {
		conn := payloadConnector(e.Payload)
		if conn != "" {
			return fmt.Sprintf("name=%s connector=%s", name, conn)
		}
		return "name=" + name
	}
	if conn := payloadConnector(e.Payload); conn != "" {
		return "connector=" + conn
	}
	return ""
}

// payloadName pulls a human-identifying name from the namespaced
// audit payload. Action and binding events surface different keys;
// either resolves to the same single-line summary slot.
func payloadName(p map[string]any) string {
	if s, _ := p["aileron.action.name"].(string); s != "" {
		return s
	}
	if s, _ := p["aileron.binding.name"].(string); s != "" {
		return s
	}
	return ""
}

func payloadConnector(p map[string]any) string {
	if s, _ := p["aileron.connector.fqn"].(string); s != "" {
		return s
	}
	if s, _ := p["aileron.action.fqn"].(string); s != "" {
		return s
	}
	if d, ok := p["aileron.failure.details"].(map[string]any); ok {
		if s, _ := d["connector"].(string); s != "" {
			return s
		}
	}
	return ""
}
