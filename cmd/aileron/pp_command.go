package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// runPp dispatches `aileron pp <subcommand>`. The `pp` namespace
// is PrintingPress's adopted prefix (used across `<name>-pp-cli`,
// `<name>-pp-mcp` binaries, `/pp-<name>` slash skills). Naming the
// Aileron command group the same way keeps the convention
// recognizable to PrintingPress users without inventing a new
// abstraction.
//
// v1 ships one subcommand — `add` — sized to install a single
// PrintingPress CLI in one command. Future subcommands (search,
// update, uninstall, list) are intentionally deferred until real
// user demand surfaces; the underlying `aileron cli` commands
// already cover the lifecycle for installed connectors.
func runPp(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, ppUsage)
		return 1
	}
	switch args[0] {
	case "add":
		return runPpAdd(args[1:], stdin, stdout, stderr)
	case "help", "--help", "-h":
		fmt.Fprintln(stdout, ppUsage)
		return 0
	default:
		fmt.Fprintf(stderr, "unknown pp subcommand: %q\n", args[0])
		fmt.Fprintln(stderr, ppUsage)
		return 1
	}
}

const ppUsage = `usage:
  aileron pp add <name>          Install a CLI from PrintingPress's catalog and wrap it

flags for ` + "`aileron pp add`" + `:
  --dry-run               Print the install plan and exit without running ` + "`go install`" + `
  --edit                  Open the inferred manifest in $EDITOR before confirmation
  -y, --yes               Skip the confirmation prompt
  --no-credentials        Skip credential detection + prompt
  --passphrase-file <p>   Read the vault passphrase from <p> (newline-terminated)`

// runPpAdd implements `aileron pp add <name>`. The flow:
//
//  1. Verify `go` is on $PATH. The install path is `go install`;
//     bail loud rather than surfacing a confusing error from a
//     subprocess that the user can't intercept.
//  2. Fetch PrintingPress's `registry.json` from
//     mvanhorn/printing-press-library. Live; no local cache.
//  3. Look up <name> in the entries array.
//  4. Derive the Go module path from the entry's `path` field —
//     `github.com/mvanhorn/printing-press-library/<path>`.
//  5. Print the install plan (module + source URL + binary name)
//     and require confirmation (unless --yes).
//  6. Run `go install <module>@latest`. Output streams to the
//     user's stdout/stderr so they see the toolchain's progress.
//  7. Locate the installed binary at $GOBIN/<name>-pp-cli (with
//     fallbacks).
//  8. Hand off to runCliAdd against that binary. The introspector
//     + credential capture from #755 + #759 do the rest.
func runPpAdd(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("pp add", flag.ContinueOnError)
	flags.SetOutput(stderr)
	dryRun := flags.Bool("dry-run", false, "print the install plan and exit without running go install")
	editFlag := flags.Bool("edit", false, "open the inferred manifest in $EDITOR before confirmation")
	yes := flags.Bool("yes", false, "skip the confirmation prompt")
	flags.BoolVar(yes, "y", false, "alias for --yes")
	noCredentials := flags.Bool("no-credentials", false, "skip the credential prompt path entirely")
	passphraseFile := flags.String("passphrase-file", "", "read the vault passphrase from this path")
	if err := flags.Parse(args); err != nil {
		return 1
	}
	rest := flags.Args()
	if len(rest) == 0 {
		fmt.Fprintln(stderr, "error: a PrintingPress CLI name is required (e.g. `aileron pp add linear`)")
		fmt.Fprintln(stderr, ppUsage)
		return 1
	}
	name := rest[0]

	if err := requireGoOnPath(); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}

	fmt.Fprintf(stdout, "Fetching PrintingPress catalog…\n")
	entry, err := lookupPPEntry(context.Background(), name)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}

	modulePath := ppModulePath(entry.Path)
	packagePath := ppPackagePath(entry.Path, name)
	sourceURL := ppSourceURL(entry.Path)
	binaryName := name + "-pp-cli"

	fmt.Fprintln(stdout)
	fmt.Fprintf(stdout, "Install plan for %s:\n", name)
	fmt.Fprintf(stdout, "  Module:   %s\n", modulePath)
	fmt.Fprintf(stdout, "  Package:  %s@latest\n", packagePath)
	fmt.Fprintf(stdout, "  Source:   %s\n", sourceURL)
	fmt.Fprintf(stdout, "  Binary:   %s (installed to $GOBIN)\n", binaryName)
	fmt.Fprintf(stdout, "  Wrap as:  local://user/%s\n", name)
	if entry.Description != "" {
		fmt.Fprintf(stdout, "  About:    %s\n", entry.Description)
	}

	if *dryRun {
		fmt.Fprintln(stdout, "\n--dry-run: catalog resolved, nothing installed.")
		return 0
	}
	if !*yes {
		if !confirm(stdin, stdout, "Run `go install` and wrap as a local connector?") {
			fmt.Fprintln(stdout, "Aborted.")
			return 1
		}
	}

	fmt.Fprintf(stdout, "\nRunning `go install %s@latest`…\n", packagePath)
	if err := runGoInstall(packagePath, stdout, stderr); err != nil {
		fmt.Fprintf(stderr, "error: go install: %v\n", err)
		return 1
	}

	binPath, err := resolveInstalledBinary(binaryName)
	if err != nil {
		fmt.Fprintf(stderr, "error: locate installed binary: %v\n", err)
		fmt.Fprintln(stderr, "Check that $GOBIN (or $(go env GOPATH)/bin) is on your $PATH.")
		return 1
	}

	// Hand off to `cli add`. Use --name=<short-name> so the on-disk
	// local-store entry is `linear`, not `linear-pp-cli`; the user
	// types `aileron pp add linear` and expects the connector to be
	// reachable as `linear` afterwards.
	cliArgs := []string{"--name", name}
	if *yes {
		cliArgs = append(cliArgs, "--yes")
	}
	if *editFlag {
		cliArgs = append(cliArgs, "--edit")
	}
	if *noCredentials {
		cliArgs = append(cliArgs, "--no-credentials")
	}
	if *passphraseFile != "" {
		cliArgs = append(cliArgs, "--passphrase-file", *passphraseFile)
	}
	cliArgs = append(cliArgs, binPath)
	return runCliAdd(cliArgs, stdin, stdout, stderr)
}

// ppCatalogURL is the live registry.json URL. Declared as a var
// so tests can swap in a local fixture without spinning up an
// HTTP server.
var ppCatalogURL = "https://raw.githubusercontent.com/mvanhorn/printing-press-library/main/registry.json"

// ppCatalogTimeout caps how long a catalog fetch waits before
// surfacing a network failure. The catalog is small (~30 KiB);
// anything beyond this points at connectivity rather than
// transfer time.
const ppCatalogTimeout = 10 * time.Second

// ppEntry is the subset of a PrintingPress catalog entry the
// installer reads. The full v2 schema carries fields the
// installer doesn't need (printer, category, mcp metadata),
// so we decode only the relevant slots and let the rest
// pass through silently.
type ppEntry struct {
	Name        string `json:"name"`
	Path        string `json:"path"`
	Description string `json:"description"`
}

// ppRegistry decodes registry.json's top-level shape (v2).
type ppRegistry struct {
	SchemaVersion int       `json:"schema_version"`
	Entries       []ppEntry `json:"entries"`
}

// lookupPPEntry fetches the live catalog and returns the entry
// matching `name`. Returns a wrapped error suitable for direct
// CLI display when the catalog can't be fetched, can't be
// parsed, or doesn't contain the name. The unknown-name error
// includes a short hint listing similarly-named entries so the
// user can recover from typos without re-reading the website.
func lookupPPEntry(ctx context.Context, name string) (*ppEntry, error) {
	if name == "" {
		return nil, errors.New("pp: catalog lookup name is empty")
	}
	ctx, cancel := context.WithTimeout(ctx, ppCatalogTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ppCatalogURL, nil)
	if err != nil {
		return nil, fmt.Errorf("pp: build catalog request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("pp: fetch catalog %s: %w", ppCatalogURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("pp: fetch catalog: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("pp: read catalog body: %w", err)
	}
	var reg ppRegistry
	if err := json.Unmarshal(body, &reg); err != nil {
		return nil, fmt.Errorf("pp: decode catalog: %w", err)
	}
	for i := range reg.Entries {
		if reg.Entries[i].Name == name {
			if reg.Entries[i].Path == "" {
				return nil, fmt.Errorf("pp: entry %q has empty path field; catalog format unexpected", name)
			}
			return &reg.Entries[i], nil
		}
	}
	return nil, ppUnknownEntryError(name, reg.Entries)
}

// ppUnknownEntryError produces the "not found" error with a
// short hint listing entries whose names share the input's
// leading characters. Optimized for typo recovery — when the
// user runs `aileron pp add lienar` they see `linear` in the
// suggestion list.
func ppUnknownEntryError(name string, entries []ppEntry) error {
	suggestions := make([]string, 0, 4)
	prefix := name
	if len(prefix) > 2 {
		prefix = prefix[:2]
	}
	prefix = strings.ToLower(prefix)
	for _, e := range entries {
		if strings.HasPrefix(strings.ToLower(e.Name), prefix) {
			suggestions = append(suggestions, e.Name)
			if len(suggestions) == 4 {
				break
			}
		}
	}
	msg := fmt.Sprintf("pp: %q is not in the PrintingPress catalog", name)
	if len(suggestions) > 0 {
		msg += fmt.Sprintf(" (did you mean one of: %s?)", strings.Join(suggestions, ", "))
	} else {
		msg += " (browse the catalog at https://printingpress.dev)"
	}
	return errors.New(msg)
}

// ppModulePath derives the Go module path from a catalog entry's
// `path` field. The convention is fixed: each catalog entry has
// its own `go.mod` at `library/<category>/<name>/go.mod`, and
// the module path declared there is the repo root plus the
// entry's `path` field.
//
// Note: this is the *module* path, not the runnable *package*
// path. The CLI binary lives at `<modulePath>/cmd/<name>-pp-cli`;
// use [ppPackagePath] for the value passed to `go install`.
func ppModulePath(entryPath string) string {
	return "github.com/mvanhorn/printing-press-library/" + strings.TrimPrefix(entryPath, "/")
}

// ppPackagePath returns the import path of the CLI variant's
// main package — the value passed to `go install`. Every
// PrintingPress entry follows the `cmd/<name>-pp-cli` /
// `cmd/<name>-pp-mcp` layout per their authoring convention;
// BYOCLI wraps the `-pp-cli` variant (the agent calls it
// through subcommands), not the `-pp-mcp` variant (which Aileron
// would speak MCP to, a separate integration point).
//
// Centralizing the `cmd/<name>-pp-cli` suffix here keeps the
// PrintingPress directory-layout assumption in one place.
func ppPackagePath(entryPath, name string) string {
	return ppModulePath(entryPath) + "/cmd/" + name + "-pp-cli"
}

// ppSourceURL renders a browser-friendly URL pointing at the
// entry's source directory in the library repo. Surfaced in the
// install plan so users can audit the source before
// authorizing `go install`.
func ppSourceURL(entryPath string) string {
	return "https://github.com/mvanhorn/printing-press-library/tree/main/" + strings.TrimPrefix(entryPath, "/")
}

// requireGoOnPath verifies the `go` toolchain is available before
// invoking `go install`. Returns a clear error with a remediation
// hint when not — anyone running `aileron pp add` is already in
// dev-tooling territory and can install Go without further
// guidance, so the hint is short.
func requireGoOnPath() error {
	if _, err := exec.LookPath("go"); err != nil {
		return errors.New("pp: `go` is not on $PATH; install Go (https://go.dev/dl) and retry")
	}
	return nil
}

// runGoInstall shells out to `go install <module>@latest` with
// the user's full environment, streaming output to the caller's
// stdout/stderr. The user sees the toolchain's progress and any
// build error directly. Per family rule "trust internal code,"
// the function relies on exec.LookPath having already verified
// `go` is present.
//
// Indirected through a package-level var so tests can swap in a
// fake without forking a real `go` process.
var runGoInstall = func(modulePath string, stdout, stderr io.Writer) error {
	cmd := exec.Command("go", "install", modulePath+"@latest")
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.Env = os.Environ()
	return cmd.Run()
}

// resolveInstalledBinary returns the absolute path of the
// just-installed binary. Walks the precedence Go's install
// pipeline writes to:
//
//  1. $GOBIN (set explicitly by user)
//  2. $(go env GOPATH)/bin (the default install destination)
//  3. exec.LookPath fallback ($PATH lookup)
//
// Returns an error when none of the three locations contain the
// expected binary, so the caller can surface a remediation hint
// rather than handing a malformed path to the wrapper.
func resolveInstalledBinary(binaryName string) (string, error) {
	if bin := os.Getenv("GOBIN"); bin != "" {
		path := filepath.Join(bin, binaryName)
		if fileExists(path) {
			return path, nil
		}
	}
	if gopath, err := goEnvGOPATH(); err == nil && gopath != "" {
		path := filepath.Join(gopath, "bin", binaryName)
		if fileExists(path) {
			return path, nil
		}
	}
	if path, err := exec.LookPath(binaryName); err == nil {
		return path, nil
	}
	return "", fmt.Errorf("binary %q not found in $GOBIN, $(go env GOPATH)/bin, or $PATH after install", binaryName)
}

// fileExists is a stat-and-discard helper. Doesn't distinguish
// permission errors from missing-file — the caller treats either
// as "binary not yet installed at this path."
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// goEnvGOPATH returns the Go toolchain's reported GOPATH, the
// fallback location `go install` writes to when GOBIN is unset.
// Indirected so tests can mock without forking `go env`.
var goEnvGOPATH = func() (string, error) {
	cmd := exec.Command("go", "env", "GOPATH")
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
