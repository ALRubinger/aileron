package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"text/tabwriter"

	"github.com/BurntSushi/toml"

	"github.com/ALRubinger/aileron/internal/cstore"
	"github.com/ALRubinger/aileron/internal/sandbox"
	"github.com/ALRubinger/aileron/internal/wrap"
)

// runCli dispatches `aileron cli <subcommand>` for the BYOCLI
// flow (issue #749). Sibling to runConnector / runAction; each
// subcommand owns its own flag set and exit-code handling.
func runCli(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, cliUsage)
		return 1
	}
	switch args[0] {
	case "add":
		return runCliAdd(args[1:], stdin, stdout, stderr)
	case "list", "ls":
		return runCliList(args[1:], stdout, stderr)
	case "remove", "rm":
		return runCliRemove(args[1:], stdout, stderr)
	case "refresh":
		return runCliRefresh(args[1:], stdin, stdout, stderr)
	case "help", "--help", "-h":
		fmt.Fprintln(stdout, cliUsage)
		return 0
	default:
		fmt.Fprintf(stderr, "unknown cli subcommand: %q\n", args[0])
		fmt.Fprintln(stderr, cliUsage)
		return 1
	}
}

const cliUsage = `usage:
  aileron cli add <path>           Wrap an installed CLI binary as a local connector
  aileron cli list                 List installed local-mode connectors
  aileron cli remove <name>        Unregister a local connector
  aileron cli refresh <name>       Re-introspect after a CLI upgrade

flags for ` + "`aileron cli add`" + `:
  --name=<name>      Override the on-disk connector name (default: binary basename)
  --version=<ver>    Set the initial connector version (default: 0.0.1)
  --dry-run          Print the inferred manifest and exit without writing
  --edit             Open the inferred manifest in $EDITOR before confirmation
  -y, --yes          Skip the confirmation prompt`

// runCliAdd implements `aileron cli add <path>` per #749.
//
// Flow:
//  1. Probe sandbox availability (refuse on unsupported platforms).
//  2. Resolve the binary path and derive an on-disk name.
//  3. Run --help inside the platform sandbox via sandbox.RunHelp.
//  4. Parse the captured help text into a wrap.Spec.
//  5. Apply the default XDG_CONFIG_HOME fs scope per #619.
//  6. Render the inferred manifest, prompt for confirmation
//     (or open in $EDITOR with --edit).
//  7. Write to the local connector store.
func runCliAdd(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("cli add", flag.ContinueOnError)
	flags.SetOutput(stderr)
	name := flags.String("name", "", "on-disk connector name (default: binary basename)")
	version := flags.String("version", "0.0.1", "initial connector version")
	dryRun := flags.Bool("dry-run", false, "print the inferred manifest and exit without writing")
	editFlag := flags.Bool("edit", false, "open the inferred manifest in $EDITOR before confirmation")
	yes := flags.Bool("yes", false, "skip the confirmation prompt")
	flags.BoolVar(yes, "y", false, "skip the confirmation prompt (alias for --yes)")
	if err := flags.Parse(args); err != nil {
		return 1
	}
	rest := flags.Args()
	if len(rest) == 0 {
		fmt.Fprintln(stderr, "error: a CLI binary path is required")
		fmt.Fprintln(stderr, cliUsage)
		return 1
	}

	programPath, err := resolveProgramPath(rest[0])
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}

	resolvedName := *name
	if resolvedName == "" {
		resolvedName = filepath.Base(programPath)
	}
	if err := validateConnectorName(resolvedName); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}

	if err := sandbox.SandboxAvailable(); err != nil {
		fmt.Fprintf(stderr, "error: spawn sandbox unavailable on this platform: %v\n", err)
		fmt.Fprintln(stderr, "aileron cli add requires kernel-enforced confinement; BYOCLI is not supported here.")
		return 1
	}

	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(stderr, "error: resolve cwd: %v\n", err)
		return 1
	}

	fmt.Fprintf(stdout, "Introspecting %s --help (sandboxed)…\n", programPath)
	ctx := context.Background()
	helpText, err := runHelpSandboxed(ctx, programPath, cwd)
	if err != nil {
		fmt.Fprintf(stderr, "error: run --help: %v\n", err)
		return 1
	}

	spec, err := buildSpecFromHelp(resolvedName, *version, programPath, helpText)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}

	manifest := buildLocalManifest(spec, resolvedName)
	if err := cstore.ValidateManifest(manifest, "manifest.toml"); err != nil {
		fmt.Fprintf(stderr, "error: emitted manifest does not validate: %v\n", err)
		return 1
	}

	if *editFlag {
		edited, err := editManifestInEditor(manifest)
		if err != nil {
			fmt.Fprintf(stderr, "error: %v\n", err)
			return 1
		}
		manifest = edited
		if err := cstore.ValidateManifest(manifest, "manifest.toml"); err != nil {
			fmt.Fprintf(stderr, "error: edited manifest does not validate: %v\n", err)
			return 1
		}
	}

	renderManifestPreview(stdout, manifest, programPath)

	if *dryRun {
		fmt.Fprintln(stdout, "\n--dry-run: not writing manifest.")
		return 0
	}

	if !*yes {
		if !confirm(stdin, stdout, "Install this local connector?") {
			fmt.Fprintln(stdout, "Aborted.")
			return 1
		}
	}

	store := cstore.NewLocalStore(cstore.DefaultLocalRoot())
	dir, err := store.Save(resolvedName, manifest)
	if err != nil {
		fmt.Fprintf(stderr, "error: save local manifest: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "\nInstalled local connector %s\n", manifest.Connector.Name)
	fmt.Fprintf(stdout, "  Store dir:  %s\n", dir)
	fmt.Fprintf(stdout, "  Operations: %d\n", len(manifest.Capabilities.Spawn.Operations))
	return 0
}

func runCliList(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		fmt.Fprintln(stderr, "usage: aileron cli list")
		return 1
	}
	store := cstore.NewLocalStore(cstore.DefaultLocalRoot())
	entries, err := store.List()
	if err != nil {
		fmt.Fprintf(stderr, "error: list local connectors: %v\n", err)
		return 1
	}
	if len(entries) == 0 {
		fmt.Fprintln(stdout, "No local connectors installed.")
		fmt.Fprintln(stdout, "Use `aileron cli add <path>` to wrap an installed CLI binary.")
		return 0
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tVERSION\tPROGRAM\tOPERATIONS")
	for _, e := range entries {
		if e.LoadErr != nil {
			fmt.Fprintf(tw, "%s\t(invalid)\t\t— %v\n", e.Name, e.LoadErr)
			continue
		}
		spawn := e.Manifest.Capabilities.Spawn
		program := ""
		if spawn != nil && len(spawn.Programs) > 0 {
			program = spawn.Programs[0].Path
		}
		opCount := 0
		if spawn != nil {
			opCount = len(spawn.Operations)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\n",
			e.Name,
			e.Manifest.Connector.Version,
			program,
			opCount,
		)
	}
	if err := tw.Flush(); err != nil {
		fmt.Fprintf(stderr, "error: flush table: %v\n", err)
		return 1
	}
	return 0
}

func runCliRemove(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: aileron cli remove <name>")
		return 1
	}
	name := args[0]
	store := cstore.NewLocalStore(cstore.DefaultLocalRoot())
	has, err := store.Has(name)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	if !has {
		fmt.Fprintf(stderr, "error: no local connector named %q\n", name)
		return 1
	}
	if err := store.Remove(name); err != nil {
		fmt.Fprintf(stderr, "error: remove local connector: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "Removed local connector %s\n", name)
	return 0
}

func runCliRefresh(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("cli refresh", flag.ContinueOnError)
	flags.SetOutput(stderr)
	yes := flags.Bool("yes", false, "skip the confirmation prompt")
	flags.BoolVar(yes, "y", false, "alias for --yes")
	if err := flags.Parse(args); err != nil {
		return 1
	}
	rest := flags.Args()
	if len(rest) == 0 {
		fmt.Fprintln(stderr, "usage: aileron cli refresh <name>")
		return 1
	}
	name := rest[0]
	store := cstore.NewLocalStore(cstore.DefaultLocalRoot())
	existing, err := store.Load(name)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			fmt.Fprintf(stderr, "error: no local connector named %q\n", name)
			return 1
		}
		fmt.Fprintf(stderr, "error: load existing manifest: %v\n", err)
		return 1
	}
	if existing.Capabilities.Spawn == nil || len(existing.Capabilities.Spawn.Programs) == 0 {
		fmt.Fprintf(stderr, "error: existing manifest for %q has no spawn program; cannot refresh\n", name)
		return 1
	}
	programPath := existing.Capabilities.Spawn.Programs[0].Path

	if err := sandbox.SandboxAvailable(); err != nil {
		fmt.Fprintf(stderr, "error: spawn sandbox unavailable: %v\n", err)
		return 1
	}
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(stderr, "error: resolve cwd: %v\n", err)
		return 1
	}

	fmt.Fprintf(stdout, "Re-introspecting %s --help (sandboxed)…\n", programPath)
	helpText, err := runHelpSandboxed(context.Background(), programPath, cwd)
	if err != nil {
		fmt.Fprintf(stderr, "error: run --help: %v\n", err)
		return 1
	}
	spec, err := buildSpecFromHelp(name, existing.Connector.Version, programPath, helpText)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	manifest := buildLocalManifest(spec, name)
	if err := cstore.ValidateManifest(manifest, "manifest.toml"); err != nil {
		fmt.Fprintf(stderr, "error: refreshed manifest does not validate: %v\n", err)
		return 1
	}
	renderManifestPreview(stdout, manifest, programPath)
	if !*yes {
		if !confirm(stdin, stdout, "Replace the existing manifest with this refreshed version?") {
			fmt.Fprintln(stdout, "Aborted.")
			return 1
		}
	}
	if _, err := store.Save(name, manifest); err != nil {
		fmt.Fprintf(stderr, "error: save refreshed manifest: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "\nRefreshed %s. Operations: %d. Credentials in vault are unchanged.\n",
		name, len(manifest.Capabilities.Spawn.Operations))
	return 0
}

// resolveProgramPath turns a user-supplied path into an absolute
// path and verifies the file exists and is executable. Rejects
// directories and non-executable regular files so the introspector
// fails before invoking the sandbox on a bad target.
func resolveProgramPath(input string) (string, error) {
	expanded := input
	if strings.HasPrefix(expanded, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("expand ~: %w", err)
		}
		expanded = filepath.Join(home, expanded[2:])
	}
	abs, err := filepath.Abs(expanded)
	if err != nil {
		return "", fmt.Errorf("resolve absolute path: %w", err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("stat %s: %w", abs, err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("%s is a directory, not a binary", abs)
	}
	if runtime.GOOS != "windows" && info.Mode()&0o111 == 0 {
		return "", fmt.Errorf("%s is not executable (mode %v)", abs, info.Mode())
	}
	return abs, nil
}

// validateConnectorName fronts cstore.LocalNameForFQN's shape rule
// so the CLI surfaces the error before the store call.
func validateConnectorName(name string) error {
	if _, err := cstore.LocalFQN(name); err != nil {
		return err
	}
	return nil
}

// runHelpSandboxed adapts sandbox.RunHelp to return a single
// merged help string. Many CLIs split --help between stdout and
// stderr; the wrap parser is heuristic enough that handing it
// both streams concatenated produces the same subcommand list.
func runHelpSandboxed(ctx context.Context, programPath, cwd string) (string, error) {
	res, err := sandbox.RunHelp(ctx, programPath, cwd)
	if err != nil {
		// Surface the captured output anyway — `curl --help`-style
		// CLIs exit non-zero but still print usage.
		if len(res.Stdout) == 0 && len(res.Stderr) == 0 {
			return "", err
		}
	}
	return string(res.Stdout) + string(res.Stderr), nil
}

// buildSpecFromHelp turns captured help text into a wrap.Spec.
// Reuses wrap.FromHelp's subcommand parser via a static
// HelpRunner so the introspection heuristic stays in one place.
func buildSpecFromHelp(name, version, programPath, helpText string) (*wrap.Spec, error) {
	fqn, err := cstore.LocalFQN(name)
	if err != nil {
		return nil, err
	}
	runner := func(context.Context, string, []string) (string, error) {
		return helpText, nil
	}
	spec, err := wrap.FromHelp(context.Background(), runner, fqn, version, programPath)
	if err != nil {
		return nil, err
	}
	spec.FSRead = defaultFSReadScope(name)
	spec.FSWrite = defaultFSWriteScope(name)
	return spec, nil
}

// defaultFSReadScope returns the default `[capabilities.spawn].fs_read`
// scope per #619: `$XDG_CONFIG_HOME/<name>` only. Users opt into
// broader reads at install time via --edit or by editing the
// emitted manifest. Never includes `$HOME` or the cwd by default.
func defaultFSReadScope(name string) []string {
	return []string{filepath.Join(xdgConfigHome(), name)}
}

// defaultFSWriteScope mirrors defaultFSReadScope. Per #619 both
// reads and writes start scoped to the connector's own XDG dir;
// CLIs that need broader writes (e.g. a cache directory) require
// explicit confirmation.
func defaultFSWriteScope(name string) []string {
	return []string{filepath.Join(xdgConfigHome(), name)}
}

// xdgConfigHome resolves the platform's per-user config root.
// Honors $XDG_CONFIG_HOME on POSIX; falls back to ~/.config on
// Linux/macOS and %APPDATA% on Windows.
func xdgConfigHome() string {
	if v := os.Getenv("XDG_CONFIG_HOME"); v != "" {
		return v
	}
	if runtime.GOOS == "windows" {
		if v := os.Getenv("APPDATA"); v != "" {
			return v
		}
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config")
}

// buildLocalManifest converts a populated wrap.Spec into a
// cstore.Manifest with Origin=local and the daemon-embedded
// forwarder forwarder declaration. Equivalent of wrap.BuildManifest
// for the hub-publisher path, specialized for the BYOCLI shape:
// the manifest is the connector's complete identity (no binary
// hash, no per-publisher signature).
func buildLocalManifest(s *wrap.Spec, name string) *cstore.Manifest {
	m := wrap.BuildManifest(s)
	m.Connector.Origin = cstore.OriginLocal
	// Ensure forwarder is set; BuildManifest always sets it, but
	// keep the invariant explicit so future BuildManifest changes
	// don't silently weaken local-mode manifests.
	m.Connector.Forwarder = cstore.BuiltinForwarderSpawn
	return m
}

// renderManifestPreview prints the inferred [capabilities.spawn]
// block to stdout per the UX spec in
// docs/src/content/docs/guides/wrapping-a-cli.md. Highlights
// scopes that look broad (the full home dir, an unrestricted
// host) so the user sees them at a glance.
func renderManifestPreview(w io.Writer, m *cstore.Manifest, programPath string) {
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Inferred manifest:")
	fmt.Fprintf(w, "  Name:        %s\n", m.Connector.Name)
	fmt.Fprintf(w, "  Version:     %s\n", m.Connector.Version)
	fmt.Fprintf(w, "  Origin:      %s\n", m.Connector.Origin)
	fmt.Fprintf(w, "  Program:     %s\n", programPath)
	spawn := m.Capabilities.Spawn
	if spawn != nil {
		fmt.Fprintf(w, "  Operations:  %d\n", len(spawn.Operations))
		for op := range spawn.Operations {
			fmt.Fprintf(w, "    - %s  (argv: %s)\n", op, spawn.Operations[op].Argv)
		}
		if len(spawn.FSRead) > 0 {
			fmt.Fprintln(w, "  FS read:")
			for _, p := range spawn.FSRead {
				fmt.Fprintf(w, "    - %s%s\n", p, scopeBadge(p))
			}
		}
		if len(spawn.FSWrite) > 0 {
			fmt.Fprintln(w, "  FS write:")
			for _, p := range spawn.FSWrite {
				fmt.Fprintf(w, "    - %s%s\n", p, scopeBadge(p))
			}
		}
		if len(spawn.EnvPassthrough) > 0 {
			fmt.Fprintf(w, "  Env passthrough: %s\n", strings.Join(spawn.EnvPassthrough, ", "))
		}
	}
}

// scopeBadge appends a `[broad]` marker per the UX spec when a
// path looks suspiciously wide: bare $HOME, root, or a single-
// segment top-level directory. The taxonomy is intentionally
// conservative — users prompted to acknowledge a [broad] scope
// is better than silently authorizing one.
func scopeBadge(path string) string {
	home, _ := os.UserHomeDir()
	switch path {
	case home, "/", "~":
		return "  [broad]"
	}
	if strings.Count(strings.Trim(path, "/"), "/") == 0 {
		return "  [broad]"
	}
	return ""
}

// confirm prints prompt to w and reads a y/N response from r.
// Treats anything other than y/yes (case-insensitive) as no.
// Used by `cli add` and `cli refresh` for the install
// confirmation step.
func confirm(r io.Reader, w io.Writer, prompt string) bool {
	fmt.Fprintf(w, "\n%s [y/N]: ", prompt)
	var resp string
	if _, err := fmt.Fscanln(r, &resp); err != nil {
		// Empty line / EOF / read error → treat as no.
		return false
	}
	resp = strings.ToLower(strings.TrimSpace(resp))
	return resp == "y" || resp == "yes"
}

// editManifestInEditor opens the given manifest in $EDITOR for
// user-driven narrowing of scopes / env keys / operations.
// Round-trips through TOML so any edits are validated by the
// same parser the daemon uses at load time.
func editManifestInEditor(m *cstore.Manifest) (*cstore.Manifest, error) {
	editor := os.Getenv("EDITOR")
	if editor == "" {
		return nil, errors.New("$EDITOR is not set; cannot --edit")
	}
	tmp, err := os.CreateTemp("", "aileron-cli-manifest-*.toml")
	if err != nil {
		return nil, err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if err := toml.NewEncoder(tmp).Encode(m); err != nil {
		tmp.Close()
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}

	cmd := exec.Command(editor, tmpName)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = os.Environ()
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("$EDITOR exited non-zero: %w", err)
	}

	body, err := os.ReadFile(tmpName)
	if err != nil {
		return nil, err
	}
	edited, err := cstore.ParseManifest(tmpName, body)
	if err != nil {
		return nil, err
	}
	return edited, nil
}

