package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ALRubinger/aileron/internal/audit"
	"github.com/ALRubinger/aileron/internal/launch"
	"github.com/ALRubinger/aileron/internal/launch/agents"
	"github.com/ALRubinger/aileron/internal/model"
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
		return runBinding(args[1:], stdout, stderr)
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

// runBinding handles `aileron binding <subcommand>`.
func runBinding(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: aileron binding <list>")
		return 1
	}
	switch args[0] {
	case "list":
		return runBindingList(stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown binding command: %q\n", args[0])
		fmt.Fprintln(stderr, "usage: aileron binding <list>")
		return 1
	}
}

// runBindingList prints every entry in the local vault as a binding
// row: path, type, and labels. Per ADR-0011, this works without the
// vault being unlocked — metadata is stored plaintext alongside the
// encrypted value, so listing requires no KEK. The use of a binding
// (e.g., resolving its credential during action execution) is the
// step that triggers an unlock prompt.
func runBindingList(stdout, stderr io.Writer) int {
	vaultPath := launch.DefaultVaultPath()
	fv, err := vault.NewFileVault(vaultPath)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	names := fv.Names()
	if len(names) == 0 {
		fmt.Fprintln(stdout, "No bindings stored.")
		return 0
	}

	// Resolve each entry's metadata via Get. The FileVault stores
	// metadata plaintext, so this works even though the vault is not
	// formally "unlocked" with a KEK — Get on a FileVault returns
	// the raw stored bytes (the EncryptedVault decorator is what
	// would attempt decryption, and we deliberately read the inner
	// FileVault directly here).
	fmt.Fprintf(stdout, "%-40s  %-12s  %s\n", "PATH", "TYPE", "LABELS")
	for _, name := range names {
		s, err := fv.Get(context.Background(), name)
		if err != nil {
			fmt.Fprintf(stdout, "%-40s  %-12s  (metadata unreadable: %v)\n", name, "?", err)
			continue
		}
		labels := formatLabels(s.Metadata.Labels)
		typ := s.Metadata.Type
		if typ == "" {
			typ = "-"
		}
		fmt.Fprintf(stdout, "%-40s  %-12s  %s\n", name, typ, labels)
	}
	return 0
}

// formatLabels renders a label map as `k=v,k=v` for terminal display.
func formatLabels(m map[string]string) string {
	if len(m) == 0 {
		return "-"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+m[k])
	}
	return strings.Join(parts, ",")
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
