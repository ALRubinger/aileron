package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/ALRubinger/aileron/internal/action"
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
	noCredentials := flags.Bool("no-credentials", false, "skip the credential-env-var heuristic + prompt; manifest stays credential-free")
	var credentialOverrides stringList
	flags.Var(&credentialOverrides, "credential", "explicit credential env-var name to stash (repeatable; supplements the heuristic)")
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

	if hash, err := hashBinary(programPath); err == nil {
		spec.Program.Hash = hash
	} else {
		fmt.Fprintf(stderr, "warning: compute binary hash: %v (continuing without pin)\n", err)
	}

	credKeys := resolveCredentialKeys(helpText, credentialOverrides, *noCredentials)
	applyCredentialsToSpec(spec, credKeys)

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

	actionFiles, err := emitLocalActionFiles(spec, manifest)
	if err != nil {
		fmt.Fprintf(stderr, "warning: failed to emit action files (%v); the connector is installed but agent-facing tools aren't yet registered\n", err)
		actionFiles = nil
	}

	stashedCount := 0
	var stashErr error
	if len(credKeys) > 0 {
		stashedCount, stashErr = stashCredentialBindings(
			manifest.Connector.Name, resolvedName, credKeys,
			stdin, stdout, stderr,
		)
		if stashErr != nil {
			// Loud failure: the manifest is installed but the
			// binding never landed in the daemon's vault, so the
			// runtime will deny every call to this connector with
			// `binding_required` until the user re-runs the setup.
			// A scroll-by warning is exactly how previous users
			// (including the maintainer dogfooding `aileron pp add
			// linear`) ended up with a connector whose every action
			// 403'd. Surface it on stderr and exit non-zero so the
			// shell prompt's exit-status indicator and any wrapping
			// automation both notice.
			fmt.Fprintf(stderr, "error: credential stash failed: %v\n", stashErr)
			fmt.Fprintln(stderr, "       manifest is installed but no binding is stored; rerun `aileron binding setup "+manifest.Connector.Name+"` to recover")
		}
	}

	fmt.Fprintf(stdout, "\nInstalled local connector %s\n", manifest.Connector.Name)
	fmt.Fprintf(stdout, "  Store dir:   %s\n", dir)
	fmt.Fprintf(stdout, "  Operations:  %d\n", len(manifest.Capabilities.Spawn.Operations))
	if len(actionFiles) > 0 {
		fmt.Fprintf(stdout, "  Actions:     %d registered under %s\n", len(actionFiles), action.DefaultDir())
	}
	if len(credKeys) > 0 {
		fmt.Fprintf(stdout, "  Credentials: %d declared, %d stashed in vault\n", len(credKeys), stashedCount)
	}
	if stashErr != nil {
		return 1
	}
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
	// Read the manifest before removing so we know which action
	// files to delete. A failure to load doesn't block the remove
	// itself — the on-disk action files become orphans, which
	// `aileron action list` already tolerates by surfacing as
	// load errors.
	var actionFiles []string
	if m, loadErr := store.Load(name); loadErr == nil {
		actionFiles = localActionFilePaths(m, name)
	}
	if err := store.Remove(name); err != nil {
		fmt.Fprintf(stderr, "error: remove local connector: %v\n", err)
		return 1
	}
	for _, path := range actionFiles {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			fmt.Fprintf(stderr, "warning: remove action file %s: %v\n", path, err)
		}
	}
	fmt.Fprintf(stdout, "Removed local connector %s (%d action file(s) cleaned up)\n", name, len(actionFiles))
	return 0
}

// localActionFilePaths returns the absolute paths of every
// action.md file `aileron cli remove` should delete for a given
// connector. Mirrors the file naming used by emitLocalActionFiles,
// but reads the operation list off the parsed manifest so the
// caller doesn't need a wrap.Spec.
func localActionFilePaths(m *cstore.Manifest, name string) []string {
	if m == nil || m.Capabilities.Spawn == nil {
		return nil
	}
	actionsDir := action.DefaultDir()
	out := make([]string, 0, len(m.Capabilities.Spawn.Operations))
	for op := range m.Capabilities.Spawn.Operations {
		out = append(out, filepath.Join(actionsDir, name+"-"+op+".md"))
	}
	return out
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

	// Re-pin the binary hash after a `go install`/upgrade.
	// Intentional rotation: a different binary at the same path
	// is a new artifact and its hash should replace the prior
	// pin per #619's binary-integrity decision.
	if hash, hashErr := hashBinary(programPath); hashErr == nil {
		spec.Program.Hash = hash
	}

	// Preserve credential capability from the existing manifest:
	// `cli refresh` must not silently demote a connector from
	// credential-bearing to credential-less because the new
	// `--help` happened to omit the env var mention. Re-run
	// detection on the new help text and union the result with
	// the existing keys, then keep the existing credential kind
	// declaration when one exists.
	preservedKeys := existing.Capabilities.Spawn.CredentialEnvKeys
	newKeys := wrap.DetectCredentialEnvKeys(helpText)
	credKeys := mergeStringSets(preservedKeys, newKeys)
	applyCredentialsToSpec(spec, credKeys)

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
	actionFiles, err := emitLocalActionFiles(spec, manifest)
	if err != nil {
		fmt.Fprintf(stderr, "warning: emit refreshed action files: %v\n", err)
	}
	fmt.Fprintf(stdout, "\nRefreshed %s. Operations: %d. Actions: %d. Credentials in vault are unchanged.\n",
		name, len(manifest.Capabilities.Spawn.Operations), len(actionFiles))
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
//
// `subcommandPath` is the verb chain prepended before `--help`.
// Nil/empty yields `<binary> --help`; `["issues", "create"]`
// yields `<binary> issues create --help` so the wrap recursion
// can walk Cobra-style nested verb trees.
func runHelpSandboxed(ctx context.Context, programPath, cwd string, subcommandPath ...string) (string, error) {
	res, err := sandbox.RunHelp(ctx, programPath, cwd, subcommandPath...)
	if err != nil {
		// Surface the captured output anyway — `curl --help`-style
		// CLIs exit non-zero but still print usage.
		if len(res.Stdout) == 0 && len(res.Stderr) == 0 {
			return "", err
		}
	}
	return string(res.Stdout) + string(res.Stderr), nil
}

// buildSpecFromHelp turns the root --help text into a wrap.Spec
// by handing wrap.FromHelp a sandboxed runner that re-invokes
// `<binary> <subcommand path...> --help` for each subcommand the
// recursion encounters.
//
// The root help text is pre-captured by the caller — re-invoking
// the sandbox for it would be a wasted round trip — so the runner
// short-circuits when the path is empty and returns the cached
// text directly. Every nested path takes a fresh sandboxed call;
// this is what makes the recursion's per-leaf flag extraction
// faithful to the real CLI's verb tree (a static-text runner
// would feed the root help back at every level and explode the
// recursion against any non-trivial subcommand set, hence the
// per-path-sandbox plumbing rather than the v0 single-shot
// closure that lived here pre-#wrap-recurse-cobra).
func buildSpecFromHelp(name, version, programPath, rootHelpText string) (*wrap.Spec, error) {
	fqn, err := cstore.LocalFQN(name)
	if err != nil {
		return nil, err
	}
	cwd, _ := os.Getwd()
	if cwd == "" {
		cwd = "/"
	}
	runner := func(ctx context.Context, _ string, args []string) (string, error) {
		// `args` is `<path...> --help`. The root invocation has
		// args=["--help"]; everything else is a nested path.
		if len(args) <= 1 {
			return rootHelpText, nil
		}
		subPath := args[:len(args)-1] // drop the trailing "--help"
		return runHelpSandboxed(ctx, programPath, cwd, subPath...)
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

// hashBinary returns the canonical `sha256:<hex>` content hash
// of the file at `path`. Populated into the local-mode manifest's
// `programs[].hash` field so the spawn runtime can detect a
// later-installed binary at the same path (per [#619]'s
// binary-integrity decision). The runtime refuses to spawn when
// the on-disk bytes don't match the pinned hash; `aileron cli
// refresh` is the documented way to rotate the pin after an
// intentional upgrade.
//
// [#619]: https://github.com/ALRubinger/aileron/issues/619
func hashBinary(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

// emitLocalActionFiles writes one action.md per spawn operation
// under ~/.aileron/actions/<connector-leaf>-<op>.md. Mirrors the
// hub-wrap path's `wrap.Install` action emission so local
// connectors are visible to `aileron action list` and to the
// daemon's tool-surface layer.
//
// Hash field on the action's `[[requires.connectors]]` carries
// the `sha256:bound-at-install` placeholder — the action
// executor's local-FQN router (#749) bypasses the hash lookup
// and resolves via [cstore.LocalStore] instead. Returns the
// absolute paths of the written files so the caller can display
// them.
//
// Overwrite-safe: `aileron cli refresh` re-introspects and
// re-emits, deliberately stomping the existing files so a
// removed CLI subcommand drops out of the action surface.
func emitLocalActionFiles(spec *wrap.Spec, m *cstore.Manifest) ([]string, error) {
	actionsDir := action.DefaultDir()
	if err := os.MkdirAll(actionsDir, 0o755); err != nil {
		return nil, err
	}
	written := make([]string, 0, len(spec.Subcommands))
	for _, sub := range spec.Subcommands {
		path := filepath.Join(actionsDir, wrap.ActionFileName(spec, sub))
		body := wrap.RenderActionMD(spec, sub, m, "")
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			return written, fmt.Errorf("write action file %s: %w", path, err)
		}
		written = append(written, path)
	}
	return written, nil
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

// stringList is the repeatable-flag value type for `--credential`.
// Implements flag.Value; each invocation appends to the slice.
type stringList []string

// String renders the list as a comma-separated string for flag
// usage; the empty-string default matches flag.Var's contract.
func (s *stringList) String() string { return strings.Join(*s, ",") }

// Set appends the value, deduplicated. Caller controls
// ordering by argument order.
func (s *stringList) Set(v string) error {
	for _, existing := range *s {
		if existing == v {
			return nil
		}
	}
	*s = append(*s, v)
	return nil
}

// resolveCredentialKeys combines the env-var heuristic on the
// captured --help text with any user-supplied `--credential` flag
// overrides. Returns nil when `--no-credentials` is set so the
// caller skips both manifest mutation and the vault stash. The
// merge is sorted + deduplicated so the manifest's
// EnvPassthrough order is stable across runs.
func resolveCredentialKeys(helpText string, overrides stringList, skip bool) []string {
	if skip {
		return nil
	}
	detected := wrap.DetectCredentialEnvKeys(helpText)
	return mergeStringSets(detected, []string(overrides))
}

// mergeStringSets returns the sorted union of two slices,
// deduplicated. The helper takes []string rather than a typed
// set because the call sites here all carry plain string lists.
func mergeStringSets(a, b []string) []string {
	seen := make(map[string]struct{}, len(a)+len(b))
	for _, v := range a {
		seen[v] = struct{}{}
	}
	for _, v := range b {
		seen[v] = struct{}{}
	}
	if len(seen) == 0 {
		return nil
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	// Reuse sort.Strings via the wrap package's idiom — keep
	// the manifest stable across re-runs of `cli add` against
	// the same binary.
	sortStrings(out)
	return out
}

// sortStrings is a thin wrapper around sort.Strings, declared
// here so the import list stays focused on the call sites that
// need real sort behavior (one site so far).
func sortStrings(s []string) {
	if len(s) < 2 {
		return
	}
	// Inline bubble for very small lists; the typical case is
	// 1-3 credential env vars, well under the threshold where a
	// real sort beats N^2.
	for i := 0; i < len(s); i++ {
		for j := i + 1; j < len(s); j++ {
			if s[j] < s[i] {
				s[i], s[j] = s[j], s[i]
			}
		}
	}
}

// applyCredentialsToSpec sets the wrap.Spec fields that drive
// the manifest's [capabilities.credential] block and the
// [capabilities.spawn].credential_env_keys list. Idempotent: an
// empty keys slice leaves the spec untouched. The credential
// kind is hardcoded to api_key — v1 of the wrap heuristic only
// catches API-key-shaped env vars; OAuth2 still goes through
// the manifest-author path.
func applyCredentialsToSpec(spec *wrap.Spec, keys []string) {
	if spec == nil || len(keys) == 0 {
		return
	}
	spec.EnvPassthrough = mergeStringSets(spec.EnvPassthrough, keys)
	spec.CredentialEnvKeys = mergeStringSets(spec.CredentialEnvKeys, keys)
	if spec.Credential == nil {
		spec.Credential = &wrap.CredentialSpec{Kind: cstore.CredentialKindAPIKey}
	}
}

// stashCredentialBindings prompts the user once for a credential
// value and dispatches the write to the daemon via
// `POST /v1/bindings/setup`. The daemon already holds the
// unlocked vault (the state machine guarantees this before any
// dispatched handler runs), so we never re-prompt for a
// passphrase here — eliminating the dual-prompt UX trap where
// the credential-value prompt and the vault-passphrase prompt
// looked identical and the user routinely missed the second
// one, leaving the manifest installed but no binding written.
//
// Returns 1 when the binding was created (or already existed
// and was skipped), 0 with nil error when the user intentionally
// left the value blank, and 0 with a non-nil error when the
// daemon rejected the request. Callers surface the error
// prominently — a silent "credential stash incomplete" warning
// at the end of `pp add` is the exact failure mode this rewrite
// is closing.
//
// v1 limitation: a connector that genuinely has multiple
// distinct credentials (e.g. an API token and a separate webhook
// secret) gets only one binding. The other env keys are still
// declared in EnvPassthrough so the user can set them in the
// surrounding shell, but the vault doesn't hold them. Multi-
// credential connectors fall back to the manual wrap path.
func stashCredentialBindings(connectorFQN, connectorName string, keys []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	if len(keys) == 0 {
		return 0, nil
	}
	fmt.Fprintf(stdout, "\nCredential capture:\n")
	if len(keys) == 1 {
		fmt.Fprintf(stdout, "  Detected env var: %s\n", keys[0])
	} else {
		fmt.Fprintf(stdout, "  Detected env vars: %s\n", strings.Join(keys, ", "))
		fmt.Fprintln(stdout, "  Note: v1 binds one credential per connector; the same value is injected into every detected key.")
	}
	fmt.Fprint(stdout, "  Credential value (leave blank to skip): ")
	value, err := promptPassphrase("", nil)
	if err != nil {
		fmt.Fprintln(stdout)
		return 0, fmt.Errorf("read credential value: %w", err)
	}
	fmt.Fprintln(stdout)
	if value == "" {
		fmt.Fprintln(stdout, "  (no credential stashed; manifest declares the env keys but no binding is set)")
		return 0, nil
	}

	body, err := json.Marshal(map[string]any{
		"connector_fqn": connectorFQN,
		"bindings": []map[string]any{{
			"identity": defaultBindingIdentity,
			"source":   map[string]any{"kind": "api_key", "value": value},
		}},
	})
	if err != nil {
		return 0, fmt.Errorf("marshal binding request: %w", err)
	}
	status, respBody, err := bindingDoRequestFn(http.MethodPost, "/bindings/setup", strings.NewReader(string(body)))
	if err != nil {
		return 0, fmt.Errorf("post bindings/setup: %w", err)
	}
	if status != http.StatusCreated {
		return 0, fmt.Errorf("daemon rejected binding setup (HTTP %d): %s", status, strings.TrimSpace(string(respBody)))
	}
	var resp struct {
		Created []struct {
			Name string `json:"name"`
		} `json:"created"`
		Skipped []string `json:"skipped"`
	}
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return 0, fmt.Errorf("parse bindings/setup response: %w", err)
	}
	for _, b := range resp.Created {
		fmt.Fprintf(stdout, "  Stashed credential under binding %s\n", b.Name)
	}
	for _, name := range resp.Skipped {
		fmt.Fprintf(stdout, "  Binding %s already exists; left untouched\n", name)
	}
	return len(resp.Created) + len(resp.Skipped), nil
}

// defaultBindingIdentity is the identity segment paired with
// every credential `aileron pp add` / `aileron cli add` stashes.
// Matches the historical local-vault binding name shape
// `api_key/<connector-name>/default` so existing manifests and
// the runtime credential resolver keep resolving the same key.
const defaultBindingIdentity = "default"

// bindingDoRequestFn is the HTTP seam tests swap with a fake
// daemon transport. Production points at [bindingDoRequest] in
// main.go, which resolves the daemon URL and submits a real
// request.
var bindingDoRequestFn = bindingDoRequest

// timeNow is the wall clock the credential binding writes use
// for CreatedAt stamps. Indirected as a package-level seam so
// the few tests that assert on the timestamp can swap in a
// fixed value without monkey-patching time.
var timeNow = func() time.Time { return time.Now() }
