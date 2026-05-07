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

	"github.com/ALRubinger/aileron/internal/audit"
	"github.com/ALRubinger/aileron/internal/comms"
	"github.com/ALRubinger/aileron/internal/config"
	"github.com/ALRubinger/aileron/internal/daemon/spawn"
	"github.com/ALRubinger/aileron/internal/launch"
	"github.com/ALRubinger/aileron/internal/launch/agents"
	"github.com/ALRubinger/aileron/internal/model"
	"github.com/ALRubinger/aileron/internal/oauth"
	launchpolicy "github.com/ALRubinger/aileron/internal/policy/launch"
	"github.com/ALRubinger/aileron/internal/vault"
	"github.com/ALRubinger/aileron/internal/version"
	"golang.org/x/term"
)

func main() {
	registry := launch.NewRegistry()
	registry.Register(agents.Claude{})
	registry.Register(agents.Pi{})
	os.Exit(run(os.Args[1:], registry, os.Stdout, os.Stderr))
}

// run executes the CLI and returns an exit code.
func run(args []string, registry *launch.Registry, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stdout, registry)
		return 1
	}

	switch args[0] {
	case "version", "--version", "-v":
		fmt.Fprintf(stdout, "aileron %s (%s)\n", version.Version, version.Commit)
		return 0
	case "launch":
		// Parse aileron-level flags before the agent name.
		launchFlags := flag.NewFlagSet("launch", flag.ContinueOnError)
		launchFlags.SetOutput(stderr)
		logLevel := launchFlags.String("log-level", "warn", "Log level: trace, debug, info, warn, error")
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

		shimPath, err := resolveShim()
		if err != nil {
			fmt.Fprintf(stderr, "error: %v\n", err)
			return 1
		}

		result, err := launch.Launch(context.Background(), launch.LaunchConfig{
			Agent:     agent,
			ShellShim: shimPath,
			Args:      launchArgs[1:],
			LogLevel:  launch.ParseLogLevel(*logLevel),
		})
		if err != nil {
			fmt.Fprintf(stderr, "error: %v\n", err)
			return 1
		}
		return result.ExitCode
	case "init":
		return runInit(stdout, stderr)
	case "policy":
		if len(args) >= 2 {
			switch args[1] {
			case "test":
				return runPolicyTest(args[2:], stdout, stderr)
			case "save":
				return runPolicySave(args[2:], stdout, stderr)
			}
		}
		fmt.Fprintln(stderr, "usage: aileron policy <test|save>")
		return 1
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
	case "approval":
		return runApproval(args[1:], stdout, stderr)
	case "status":
		return runStatus(args[1:], stdout, stderr)
	case "sync":
		return runSync(args[1:], os.Stdin, stdout, stderr)
	case "log":
		return runLog(args[1:], stdout, stderr)
	case "audit":
		return runAudit(args[1:], stdout, stderr)
	case "sessions":
		return runSessions(args[1:], stdout, stderr)
	case "daemon":
		return runDaemon(args[1:], stdout, stderr)
	case "stop":
		// Alias for `aileron daemon stop` per ADR-0012.
		return runDaemonStop(args[1:], stdout, stderr)
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
	fmt.Fprintln(w, "  aileron init                       Scaffold aileron.yaml for this project")
	fmt.Fprintln(w, "  aileron launch [--verbose] <agent>  Launch an agent with policy-enforced shell")
	fmt.Fprintln(w, "  aileron policy test <cmd> [cmd..]  Dry-run commands against loaded policy")
	fmt.Fprintln(w, "  aileron policy save [flags]        Save user-approved commands as policy rules")
	fmt.Fprintln(w, "  aileron secret set <name>          Store a secret in the encrypted vault")
	fmt.Fprintln(w, "  aileron secret list                List stored secret names")
	fmt.Fprintln(w, "  aileron binding list               List credential bindings (metadata only — no unlock)")
	fmt.Fprintln(w, "  aileron connector install <FQN>    Install a connector binary from its FQN")
	fmt.Fprintln(w, "  aileron connector check            Check installed connectors for newer versions")
	fmt.Fprintln(w, "  aileron action add <FQN>           Install an action template from its FQN")
	fmt.Fprintln(w, "  aileron keyring trust <auth> <key> Authorize a publisher's signing key for installs")
	fmt.Fprintln(w, "  aileron keyring list               List trusted publishers and key fingerprints")
	fmt.Fprintln(w, "  aileron keyring revoke <auth>      Remove a publisher's keys from the trust list")
	fmt.Fprintln(w, "  aileron approval list              List pending action-approval requests")
	fmt.Fprintln(w, "  aileron approval approve <id>      Approve a pending action — agent's tool call unblocks")
	fmt.Fprintln(w, "  aileron approval deny <id>         Deny a pending action — agent receives approval_denied")
	fmt.Fprintln(w, "  aileron status [section]           Show daemon runtime + merged config (runtime, policy, env, notifications, vault)")
	fmt.Fprintln(w, "  aileron sync [--bind-all] [--yes]  Reconcile installed actions: install missing connectors; report unbound capabilities")
	fmt.Fprintln(w, "  aileron log [flags]                View the shell-policy log")
	fmt.Fprintln(w, "  aileron audit [list|show]          View the action-execution audit log (ADR-0010)")
	fmt.Fprintln(w, "  aileron sessions [list|get]        View `aileron launch` session records (ADR-0012)")
	fmt.Fprintln(w, "  aileron daemon start|stop|status   Manage the local Aileron daemon (auto-spawned on demand)")
	fmt.Fprintln(w, "  aileron stop                       Alias for 'aileron daemon stop'")
	fmt.Fprintln(w, "  aileron version                    Print version information")
	fmt.Fprintln(w, "  aileron help                       Show this help")
	fmt.Fprintln(w)
	fmt.Fprintf(w, "agents: %s\n", strings.Join(registry.Names(), ", "))
}

// runInit scaffolds an aileron.yaml in the current directory.
func runInit(stdout, stderr io.Writer) int {
	dir, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}

	path, err := launch.InitPolicy(dir)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}

	fmt.Fprintf(stdout, "Created %s\n", filepath.Base(path))
	fmt.Fprintln(stdout, "Language toolchain and OS rules are built in — no configuration needed.")
	return 0
}

// runPolicyTest evaluates commands against the loaded policy without executing them.
func runPolicyTest(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: aileron policy test <command> [command...]")
		return 1
	}

	dir, _ := os.Getwd()
	policyPath := launch.FindPolicyFile(dir)

	if policyPath == "" {
		fmt.Fprintln(stderr, "no aileron.yaml found (run 'aileron init' to create one)")
		return 1
	}

	fmt.Fprintf(stdout, "Policy: %s\n\n", policyPath)

	exitCode := 0
	for _, cmd := range args {
		result := launch.EvaluateCommand(policyPath, cmd, dir)

		var icon, label string
		switch result.Disposition {
		case model.DispositionAllow:
			icon, label = "\033[32m✓\033[0m", "allow"
		case model.DispositionDeny:
			icon, label = "\033[31m✗\033[0m", "deny"
			exitCode = 1
		default:
			icon, label = "\033[33m?\033[0m", "ask"
		}

		fmt.Fprintf(stdout, "  %s %-5s  %s", icon, label, cmd)
		if result.Reason != "" {
			fmt.Fprintf(stdout, "  (%s)", result.Reason)
		}
		if result.RuleID != "" {
			if result.Layer != "" {
				fmt.Fprintf(stdout, "  [%s] (%s)", result.RuleID, result.Layer)
			} else {
				fmt.Fprintf(stdout, "  [%s]", result.RuleID)
			}
		}
		fmt.Fprintln(stdout)
	}
	return exitCode
}

// runPolicySave reads the audit log for user-approved commands and offers to
// save them as persistent policy rules. Commands already present in the
// allow list are skipped.
func runPolicySave(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("policy save", flag.ContinueOnError)
	fs.SetOutput(stderr)
	session := fs.String("session", "", "Filter by session ID (default: all sessions)")
	path := fs.String("path", "", "Audit log file path (default: auto-detect)")
	scope := fs.String("scope", "", "Save scope: project or user (default: prompt)")
	dryRun := fs.Bool("dry-run", false, "Show what would be saved without writing")

	if err := fs.Parse(args); err != nil {
		return 1
	}

	logPath := *path
	if logPath == "" {
		logPath = launch.ResolveAuditLogFromCwd()
	}

	entries, err := audit.ReadShellEntriesFiltered(logPath, audit.ShellFilter{
		SessionID:   *session,
		Disposition: "ask_approved",
	})
	if err != nil {
		fmt.Fprintf(stderr, "error reading audit log %s: %v\n", logPath, err)
		return 1
	}

	if len(entries) == 0 {
		fmt.Fprintln(stdout, "No user-approved commands found in the audit log.")
		return 0
	}

	// Deduplicate commands.
	seen := make(map[string]bool)
	var commands []string
	for _, e := range entries {
		if !seen[e.Command] {
			seen[e.Command] = true
			commands = append(commands, e.Command)
		}
	}

	// Resolve policy paths.
	dir, _ := os.Getwd()
	policyPath := launch.FindPolicyFile(dir)
	home, _ := os.UserHomeDir()
	userPath := ""
	if home != "" {
		userPath = filepath.Join(home, ".aileron", "settings.yaml")
	}

	// Filter out commands already in the allow list.
	commands = filterAlreadyAllowed(commands, policyPath, userPath)
	if len(commands) == 0 {
		fmt.Fprintln(stdout, "All approved commands are already in the policy. Nothing to save.")
		return 0
	}

	fmt.Fprintf(stdout, "Found %d approved command(s) not yet in policy:\n\n", len(commands))
	for i, cmd := range commands {
		fmt.Fprintf(stdout, "  %d. %s\n", i+1, cmd)
	}
	fmt.Fprintln(stdout)

	if *dryRun {
		fmt.Fprintln(stdout, "(dry run — no changes written)")
		return 0
	}

	// Determine target scope.
	saveScope := *scope
	if saveScope == "" {
		saveScope = "project"
	}

	var targetPath, label string
	switch saveScope {
	case "project":
		if policyPath == "" {
			fmt.Fprintln(stderr, "no aileron.yaml found (run 'aileron init' to create one)")
			return 1
		}
		targetPath = policyPath
		label = "project policy"
	case "user":
		if userPath == "" {
			fmt.Fprintln(stderr, "cannot determine home directory for user settings")
			return 1
		}
		targetPath = userPath
		label = "user settings"
	default:
		fmt.Fprintf(stderr, "invalid scope %q (must be 'project' or 'user')\n", saveScope)
		return 1
	}

	saved := 0
	for _, cmd := range commands {
		if err := launchpolicy.AppendAllowRule(targetPath, cmd); err != nil {
			fmt.Fprintf(stderr, "error saving rule %q: %v\n", cmd, err)
			continue
		}
		saved++
	}

	fmt.Fprintf(stdout, "Saved %d rule(s) to %s (%s)\n", saved, targetPath, label)
	return 0
}

// filterAlreadyAllowed removes commands that are already in the allow lists
// of the project or user policy files.
func filterAlreadyAllowed(commands []string, projectPath, userPath string) []string {
	allowed := make(map[string]bool)

	loadAllowed := func(path string) {
		if path == "" {
			return
		}
		pf, err := launchpolicy.Load(path)
		if err != nil {
			return
		}
		for _, rule := range pf.Allow {
			allowed[rule.Command] = true
		}
	}

	loadAllowed(projectPath)
	loadAllowed(userPath)

	var filtered []string
	for _, cmd := range commands {
		if !allowed[cmd] {
			filtered = append(filtered, cmd)
		}
	}
	return filtered
}

// runLog reads and displays the audit trail.
func runLog(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("log", flag.ContinueOnError)
	fs.SetOutput(stderr)
	session := fs.String("session", "", "Filter by session ID")
	disposition := fs.String("disposition", "", "Filter by disposition (allow/deny/ask_approved/ask_denied)")
	command := fs.String("command", "", "Filter by command substring")
	path := fs.String("path", "", "Audit log file path (default: auto-detect)")

	if err := fs.Parse(args); err != nil {
		return 1
	}

	logPath := *path
	if logPath == "" {
		logPath = launch.ResolveAuditLogFromCwd()
	}

	entries, err := audit.ReadShellEntriesFiltered(logPath, audit.ShellFilter{
		SessionID:      *session,
		Disposition:    *disposition,
		CommandPattern: *command,
	})
	if err != nil {
		fmt.Fprintf(stderr, "error reading audit log %s: %v\n", logPath, err)
		return 1
	}

	if len(entries) == 0 {
		fmt.Fprintln(stdout, "No audit entries found.")
		return 0
	}

	for _, e := range entries {
		ts := e.Timestamp.Format("15:04:05")
		fmt.Fprintf(stdout, "%s  %-13s  %-12s  %s\n", ts, e.Disposition, e.RuleID, e.Command)
	}
	return 0
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
		return runSecretList(stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown secret command: %q\n", args[0])
		fmt.Fprintln(stderr, "usage: aileron secret <set|list>")
		return 1
	}
}

// runSecretSet stores a secret in the local vault.
func runSecretSet(args []string, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		fmt.Fprintln(stderr, "usage: aileron secret set <name>")
		return 1
	}
	name := args[0]

	// Check if this is a brand-new vault (no existing secrets).
	vaultPath := launch.DefaultVaultPath()
	isNewVault := true
	if fv, err := vault.NewFileVault(vaultPath); err == nil {
		isNewVault = len(fv.Names()) == 0
	}

	if isNewVault {
		fmt.Fprintln(stderr, "")
		fmt.Fprintln(stderr, "  Creating a new Aileron vault.")
		fmt.Fprintln(stderr, "")
		fmt.Fprintln(stderr, "  The passphrase you choose protects all secrets in this vault.")
		fmt.Fprintln(stderr, "  It is never stored, transmitted, or recoverable. No one can")
		fmt.Fprintln(stderr, "  read it, tell you what it is, or help you retrieve it.")
		fmt.Fprintln(stderr, "")
		fmt.Fprintln(stderr, "  If you lose this passphrase, you must delete the vault file")
		fmt.Fprintf(stderr, "  (%s) and re-add all secrets.\n", vaultPath)
		fmt.Fprintln(stderr, "")
		fmt.Fprintln(stderr, "  Store this passphrase securely. Do not share it.")
		fmt.Fprintln(stderr, "")
	}

	passphrase, err := promptPassphrase("Vault passphrase: ", stderr)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}

	// For a new vault, require confirmation.
	if isNewVault {
		confirm, err := promptPassphrase("Confirm passphrase: ", stderr)
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
func runSecretList(stdout, stderr io.Writer) int {
	vaultPath := launch.DefaultVaultPath()
	fv, err := vault.NewFileVault(vaultPath)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}

	names := fv.Names()
	if len(names) == 0 {
		fmt.Fprintln(stdout, "No secrets stored.")
		fmt.Fprintln(stdout, "Run `aileron secret set <name>` to store one.")
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
  aileron binding list   [--connector FQN] [--kind KIND]
  aileron binding inspect <name>
  aileron binding setup  <connector-FQN>
  aileron binding rebind <name>
  aileron binding revoke <name>`

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
// the running aileron binary named "server" (current build artifact
// per task build:server). Falls back to PATH lookup so users running
// `aileron` from PATH can still spawn the daemon if `server` is also
// on PATH.
func daemonBinaryPath() (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	candidate := filepath.Join(filepath.Dir(self), "server")
	if _, err := os.Stat(candidate); err == nil {
		return filepath.Abs(candidate)
	}
	return exec.LookPath("server")
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

// runBindingList renders the user's bindings as a fixed-width table.
// Per ADR-0011, listing does not require the vault to be unlocked;
// the server returns plaintext metadata only.
func runBindingList(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("binding list", flag.ContinueOnError)
	flags.SetOutput(stderr)
	connector := flags.String("connector", "", "filter by connector FQN")
	kind := flags.String("kind", "", "filter by credential kind")
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
		fmt.Fprintln(stdout, "No bindings configured.")
		fmt.Fprintln(stdout, "Run `aileron binding setup <connector-FQN>` to add one.")
		return 0
	}
	fmt.Fprintf(stdout, "%-40s  %-10s  %-30s  %s\n", "NAME", "KIND", "CONNECTOR", "STATUS")
	for _, b := range resp.Items {
		st := b.Status
		if st == "" {
			st = "active"
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
	status, body, err := bindingDoRequest(http.MethodGet, "/bindings/"+name, nil)
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
	status, respBody, err := bindingDoRequest(http.MethodPost, "/bindings/"+name+"/rebind",
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
	status, respBody, err := bindingDoRequest(http.MethodDelete, "/bindings/"+name, nil)
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
	Name         string    `json:"name"`
	Kind         string    `json:"kind"`
	Service      string    `json:"service"`
	Identity     string    `json:"identity"`
	Scope        string    `json:"scope"`
	ConnectorFQN string    `json:"connector_fqn"`
	Account      string    `json:"account"`
	CreatedAt    time.Time `json:"created_at"`
	Status       string    `json:"status"`
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
		showStatusPolicy(dir, stdout)
		fmt.Fprintln(stdout)
		showStatusEnv(dir, stdout)
		fmt.Fprintln(stdout)
		showStatusNotifications(dir, stdout)
		fmt.Fprintln(stdout)
		showStatusVault(dir, stdout)
		return 0
	case "runtime":
		showStatusRuntime(stdout)
		return 0
	case "policy":
		showStatusPolicy(dir, stdout)
		return 0
	case "env":
		showStatusEnv(dir, stdout)
		return 0
	case "notifications":
		showStatusNotifications(dir, stdout)
		return 0
	case "vault":
		showStatusVault(dir, stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "unknown status section: %q\n", section)
		fmt.Fprintln(stderr, "usage: aileron status [runtime|policy|env|notifications|vault]")
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

func showStatusPolicy(dir string, w io.Writer) {
	fmt.Fprintln(w, "\033[1mPolicy\033[0m")

	defaults := launchpolicy.DefaultPolicy()
	fmt.Fprintf(w, "  Built-in defaults: %d allow, %d deny\n",
		len(defaults.Allow), len(defaults.Deny))

	userSettings, err := launchpolicy.LoadUserSettings()
	if err != nil {
		fmt.Fprintf(w, "  User settings:     error: %v\n", err)
	} else {
		total := len(userSettings.Allow) + len(userSettings.Deny) + len(userSettings.Ask)
		if total == 0 {
			fmt.Fprintln(w, "  User settings:     (none)")
		} else {
			fmt.Fprintf(w, "  User settings:     %d allow, %d deny, %d ask\n",
				len(userSettings.Allow), len(userSettings.Deny), len(userSettings.Ask))
		}
	}

	policyPath := launch.FindPolicyFile(dir)
	if policyPath == "" {
		fmt.Fprintln(w, "  Project policy:    (no aileron.yaml found)")
	} else {
		project, err := launchpolicy.Load(policyPath)
		if err != nil {
			fmt.Fprintf(w, "  Project policy:    error: %v\n", err)
		} else {
			fmt.Fprintf(w, "  Project policy:    %s\n", policyPath)
			total := len(project.Allow) + len(project.Deny) + len(project.Ask)
			if total > 0 {
				fmt.Fprintf(w, "                     %d allow, %d deny, %d ask\n",
					len(project.Allow), len(project.Deny), len(project.Ask))
			}
			if project.Default != "" {
				fmt.Fprintf(w, "  Default:           %s\n", project.Default)
			}
		}
	}
}

func showStatusEnv(dir string, w io.Writer) {
	fmt.Fprintln(w, "\033[1mEnvironment\033[0m")

	policyPath := launch.FindPolicyFile(dir)
	var merged *launchpolicy.PolicyFile
	if policyPath != "" {
		var err error
		merged, err = launchpolicy.LoadWithProfiles(policyPath)
		if err != nil {
			fmt.Fprintf(w, "  error: %v\n", err)
			return
		}
	} else {
		merged = launchpolicy.DefaultPolicy()
	}

	if merged.Env == nil {
		fmt.Fprintln(w, "  No env scrubbing configured.")
		return
	}
	if len(merged.Env.Scrub) > 0 {
		fmt.Fprintln(w, "  Scrub:")
		for _, p := range merged.Env.Scrub {
			fmt.Fprintf(w, "    - %s\n", p)
		}
	}
	if len(merged.Env.Passthrough) > 0 {
		fmt.Fprintln(w, "  Passthrough:")
		for _, p := range merged.Env.Passthrough {
			fmt.Fprintf(w, "    - %s\n", p)
		}
	}
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

	if slack := cfg.Notifications.Slack; slack != nil {
		fmt.Fprintln(w, "  Slack:")
		fmt.Fprintf(w, "    app_token: %s\n", tokenStatus(slack.AppToken))
		fmt.Fprintf(w, "    bot_token: %s\n", tokenStatus(slack.BotToken))
		if slack.UserToken != "" {
			fmt.Fprintf(w, "    user_token: %s\n", tokenStatus(slack.UserToken))
		}
		for _, ch := range slack.Channels {
			draft := ""
			if ch.AutoDraft {
				draft = " (auto-draft)"
			}
			fmt.Fprintf(w, "    channel: %s [show=%s]%s\n", ch.Name, ch.Show, draft)
		}
	}

	if discord := cfg.Notifications.Discord; discord != nil {
		fmt.Fprintln(w, "  Discord:")
		fmt.Fprintf(w, "    bot_token: %s\n", tokenStatus(discord.BotToken))
		for _, ch := range discord.Channels {
			fmt.Fprintf(w, "    channel: %s [show=%s]\n", ch.Name, ch.Show)
		}
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

func tokenStatus(value string) string {
	if value == "" {
		return "(not set)"
	}
	if comms.IsVaultRef(value) {
		return value
	}
	return "(plaintext — use vault: reference)"
}

// resolveShim finds the aileron-sh binary next to this executable, or on PATH.
func resolveShim() (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolving self path: %w", err)
	}
	self, err = filepath.EvalSymlinks(self)
	if err != nil {
		return "", fmt.Errorf("resolving self symlinks: %w", err)
	}
	return launch.ResolveShim(self)
}

// --- Connector / action install (ADR-0004 / ADR-0003 + #366) ---

const connectorUsage = `usage:
  aileron connector install <FQN> [--version=<v>] [--hash=<sha256:...>] [--yes]
  aileron connector check [--include-prerelease]`

const actionUsage = `usage:
  aileron action add <FQN> [--version=<v>] [--force] [--yes] [--no-bind]`

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
	if err := flags.Parse(args); err != nil {
		return 1
	}

	resp, err := connectorCheckFetcher(*includePrerelease)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}

	if len(resp.Results) == 0 {
		fmt.Fprintln(stdout, "No connectors installed.")
		fmt.Fprintln(stdout, "Run `aileron connector install <FQN>` to add one.")
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
	ActionsSeen      int                       `json:"actions_seen"`
	Required         []connectorRefWire        `json:"required"`
	Installed        []installedConnectorWire  `json:"installed"`
	AlreadyInstalled []connectorRefWire        `json:"already_installed"`
	InstallFailures  []connectorFailureWire    `json:"install_failures"`
	Unbound          []unboundCapabilityWire   `json:"unbound"`
}

type connectorRefWire struct {
	Fqn     string `json:"fqn"`
	Version string `json:"version"`
}

type installedConnectorWire struct {
	Fqn              string  `json:"fqn"`
	Version          string  `json:"version"`
	Hash             string  `json:"hash"`
	EntryDir         string  `json:"entry_dir"`
	AlreadyInstalled *bool   `json:"already_installed,omitempty"`
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

// runConnectorInstall implements the consent flow per ADR-0007:
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
		answer := strings.ToLower(strings.TrimSpace(promptLine(stdin, stdout, "Install? [y/N]: ")))
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
	Fqn              string `json:"fqn"`
	Version          string `json:"version"`
	Hash             string `json:"hash"`
	Name             string `json:"name"`
	Intent           string `json:"intent,omitempty"`
	SignatureStatus  string `json:"signature_status,omitempty"`
	AlreadyInstalled bool   `json:"already_installed,omitempty"`
	ConnectorDeps    []struct {
		Fqn              string   `json:"fqn"`
		Version          string   `json:"version"`
		Hash             string   `json:"hash"`
		Capabilities     []string `json:"capabilities,omitempty"`
		AlreadyInstalled bool     `json:"already_installed"`
	} `json:"connector_deps"`
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

// runActionAdd posts to /v1/actions/install, renders the envelope,
// and (per ADR-0006 v1 UX) prompts the user to drop into the binding
// setup flow for any unbound credential capabilities the action's
// connectors declare. The user stays in the CLI surface throughout —
// avoids the hit-binding_required-at-agent-invocation friction.
//
// When the server returns `connectors_missing` (issue #413), the CLI
// shows the resolved FQN, version, and hash for each missing
// connector and prompts the user; on confirmation it retries the
// install with `auto_install_connectors=true` so the server runs the
// connector pipeline transparently.
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
	resolvedFQN, resolvedVersion, perr := splitFQNVersion(fqnArg, *version)
	if perr != nil {
		fmt.Fprintf(stderr, "%v\n", perr)
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
		return 1
	}
	if previewStatus != http.StatusOK {
		fmt.Fprintf(stderr, "server returned %d: %s\n", previewStatus, string(previewRaw))
		return 1
	}
	var preview actionPreviewWire
	if err := json.Unmarshal(previewRaw, &preview); err != nil {
		fmt.Fprintf(stderr, "error parsing preview: %v\n", err)
		return 1
	}

	// Already-installed short-circuit: same name + same hash → no
	// prompt, no install. Mirrors the connector-install pattern.
	if preview.AlreadyInstalled {
		fmt.Fprintf(stdout, "Already installed: %s\n  hash: %s\n", preview.Name, preview.Hash)
		return 0
	}

	// Step 2: render the consent prompt. Shows action metadata plus
	// the connector deps split into "already installed" vs. "will
	// be installed alongside this action".
	renderActionPreview(stdout, &preview)

	// Step 3: prompt unless --yes.
	if !*yes {
		answer := strings.ToLower(strings.TrimSpace(promptLine(stdin, stdout, "Install? [y/N]: ")))
		if answer != "y" && answer != "yes" {
			fmt.Fprintln(stdout, "Cancelled.")
			return 0
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
	if *force {
		body["force"] = true
	}
	bodyJSON, _ := json.Marshal(body)
	status, respBody, err := bindingDoRequest(http.MethodPost, "/actions/install",
		strings.NewReader(string(bodyJSON)))
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	if status != http.StatusCreated && status != http.StatusOK {
		fmt.Fprintf(stderr, "server returned %d: %s\n", status, string(respBody))
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
	}
	if err := json.Unmarshal(respBody, &resp); err != nil {
		fmt.Fprintf(stderr, "error parsing response: %v\n", err)
		return 1
	}
	verb := "Added"
	if resp.AlreadyInstalled {
		verb = "Already installed"
	}
	fmt.Fprintf(stdout, "%s: %s\n  source: %s\n  path: %s\n",
		verb, resp.Name, resp.Source, resp.Path)

	if *noBind || len(resp.UnboundCapabilities) == 0 {
		if len(resp.UnboundCapabilities) > 0 {
			fmt.Fprintln(stdout, "")
			fmt.Fprintln(stdout, "Action has unbound credential capabilities:")
			for _, u := range resp.UnboundCapabilities {
				fmt.Fprintf(stdout, "  - %s (%s)\n", u.ConnectorFQN, u.Kind)
			}
			fmt.Fprintln(stdout, "Run `aileron binding setup <connector-FQN>` for each before invoking the action.")
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
	return 0
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
