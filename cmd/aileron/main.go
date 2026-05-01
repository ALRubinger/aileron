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
	"path/filepath"
	"strings"
	"time"

	"github.com/ALRubinger/aileron/internal/audit"
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
		return runConnector(args[1:], stdout, stderr)
	case "action":
		return runAction(args[1:], os.Stdin, stdout, stderr)
	case "status":
		return runStatus(args[1:], stdout, stderr)
	case "log":
		return runLog(args[1:], stdout, stderr)
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
	fmt.Fprintln(w, "  aileron action add <FQN>           Install an action template from its FQN")
	fmt.Fprintln(w, "  aileron status [section]           Show merged config (policy, env, notifications, vault)")
	fmt.Fprintln(w, "  aileron log [flags]                View the audit trail")
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

// bindingAPIBaseURL returns the server's binding API base URL,
// overridable via AILERON_API_URL for tests and non-default ports.
func bindingAPIBaseURL() string {
	if u := os.Getenv("AILERON_API_URL"); u != "" {
		return strings.TrimRight(u, "/")
	}
	return "http://localhost:8721/v1"
}

// bindingDoRequest issues an HTTP request to the server and returns
// the parsed body. Status codes are surfaced to callers.
func bindingDoRequest(method, path string, body io.Reader) (int, []byte, error) {
	req, err := http.NewRequest(method, bindingAPIBaseURL()+path, body)
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
		fmt.Fprintln(stdout, "No bindings.")
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
		showStatusPolicy(dir, stdout)
		fmt.Fprintln(stdout)
		showStatusEnv(dir, stdout)
		fmt.Fprintln(stdout)
		showStatusNotifications(dir, stdout)
		fmt.Fprintln(stdout)
		showStatusVault(dir, stdout)
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
		fmt.Fprintln(stderr, "usage: aileron status [policy|env|notifications|vault]")
		return 1
	}
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

func showStatusNotifications(dir string, w io.Writer) {
	fmt.Fprintln(w, "\033[1mNotifications\033[0m")

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

	if merged.Notifications == nil {
		fmt.Fprintln(w, "  No notifications configured.")
		return
	}

	if cfg := merged.Notifications.Slack; cfg != nil {
		fmt.Fprintln(w, "  Slack:")
		fmt.Fprintf(w, "    app_token: %s\n", tokenStatus(cfg.AppToken))
		fmt.Fprintf(w, "    bot_token: %s\n", tokenStatus(cfg.BotToken))
		for _, ch := range cfg.Channels {
			draft := ""
			if ch.AutoDraft {
				draft = " (auto-draft)"
			}
			fmt.Fprintf(w, "    channel: %s [show=%s]%s\n", ch.Name, ch.Show, draft)
		}
	}

	if cfg := merged.Notifications.Discord; cfg != nil {
		fmt.Fprintln(w, "  Discord:")
		fmt.Fprintf(w, "    bot_token: %s\n", tokenStatus(cfg.BotToken))
		for _, ch := range cfg.Channels {
			fmt.Fprintf(w, "    channel: %s [show=%s]\n", ch.Name, ch.Show)
		}
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
	if launch.IsVaultRef(value) {
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
  aileron connector install <FQN> [--version=<v>] [--hash=<sha256:...>] [--force]`

const actionUsage = `usage:
  aileron action add <FQN> [--version=<v>] [--force]`

// runConnector dispatches `aileron connector <subcommand>`.
func runConnector(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, connectorUsage)
		return 1
	}
	switch args[0] {
	case "install":
		return runConnectorInstall(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown connector command: %q\n", args[0])
		fmt.Fprintln(stderr, connectorUsage)
		return 1
	}
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

// runConnectorInstall posts to /v1/connectors/install and renders the
// returned envelope. Accepts either a bare FQN (`github://...`) or an
// FQN+version (`github://...@1.0.0`); --version is required when the
// bare form is used.
func runConnectorInstall(args []string, stdout, stderr io.Writer) int {
	fqnArg, rest, ok := extractPositional(args)
	if !ok {
		fmt.Fprintln(stderr, connectorUsage)
		return 1
	}
	flags := flag.NewFlagSet("connector install", flag.ContinueOnError)
	flags.SetOutput(stderr)
	version := flags.String("version", "", "strict SemVer to install (required if FQN omits @<version>)")
	hash := flags.String("hash", "", "expected sha256:<hex>; install aborts if computed hash does not match")
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
	body := map[string]any{"fqn": resolvedFQN, "version": resolvedVersion}
	if *hash != "" {
		body["expected_hash"] = *hash
	}
	bodyJSON, _ := json.Marshal(body)
	status, respBody, err := bindingDoRequest(http.MethodPost, "/connectors/install",
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
		Fqn              string `json:"fqn"`
		Version          string `json:"version"`
		Hash             string `json:"hash"`
		EntryDir         string `json:"entry_dir"`
		AlreadyInstalled bool   `json:"already_installed"`
	}
	if err := json.Unmarshal(respBody, &resp); err != nil {
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

// runActionAdd posts to /v1/actions/install, renders the envelope,
// and (per ADR-0006 v1 UX) prompts the user to drop into the binding
// setup flow for any unbound credential capabilities the action's
// connectors declare. The user stays in the CLI surface throughout —
// avoids the hit-binding_required-at-agent-invocation friction.
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
	if err := flags.Parse(rest); err != nil {
		return 1
	}
	resolvedFQN, resolvedVersion, perr := splitFQNVersion(fqnArg, *version)
	if perr != nil {
		fmt.Fprintf(stderr, "%v\n", perr)
		return 1
	}
	body := map[string]any{"fqn": resolvedFQN, "version": resolvedVersion}
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
