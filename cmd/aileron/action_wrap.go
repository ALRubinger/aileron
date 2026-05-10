package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/ALRubinger/aileron/internal/action"
	"github.com/ALRubinger/aileron/internal/cstore"
	"github.com/ALRubinger/aileron/internal/sandbox/forwarder"
	"github.com/ALRubinger/aileron/internal/wrap"
)

// actionWrapHelpRunner is the HelpRunner used in production. Tests
// substitute a static fake so they don't depend on a real binary.
var actionWrapHelpRunner = wrap.HelpRunnerExec

// actionWrapForwarder is the forwarder WASM bytes the --install path
// hands to cstore.Store.InstallForwarderManifest. Indirected so tests
// can swap in a small fake without dragging in the 3 MB binary.
var actionWrapForwarder = forwarder.WASM

// runActionWrap implements `aileron action wrap <CLI>`.
//
// Three modes:
//
//	--config=<file> --install   Load a YAML spec and install the
//	                            resulting manifest into the daemon's
//	                            connector store. Action files land in
//	                            ~/.aileron/actions/. No source tree
//	                            on disk; the wrap is live immediately.
//
//	--config=<file>             YAML mode without --install: write a
//	                            slim source tree (connector/manifest.toml
//	                            + actions/<op>.md) under --out for
//	                            review, editing, or publishing.
//
//	(no --config)               Invoke <CLI> --help and emit a scaffold
//	                            from the parsed subcommand list. Quick
//	                            path. Pair with --install to run
//	                            immediately; without --install, the
//	                            scaffold lands under --out.
func runActionWrap(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("action wrap", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", "", "YAML spec describing the connector (precise mode)")
	outDir := flags.String("out", "./aileron-connector", "directory to write the connector source tree into (ignored with --install)")
	name := flags.String("name", "", "FQN of the connector to scaffold (--help mode); ignored when --config is set")
	version := flags.String("version", "0.0.1", "initial version string (--help mode); ignored when --config is set")
	force := flags.Bool("force", false, "overwrite existing files")
	install := flags.Bool("install", false, "write the manifest directly into the daemon's connector store and action files into ~/.aileron/actions (instead of writing a source tree under --out)")
	if err := flags.Parse(args); err != nil {
		return 1
	}
	rest := flags.Args()

	spec, code := loadSpec(*configPath, *name, *version, rest, stderr)
	if code != 0 {
		return code
	}
	if *install {
		return runInstall(spec, *force, stdout, stderr)
	}
	return runEmit(spec, *outDir, *force, stdout, stderr)
}

// loadSpec produces a wrap.Spec from either --config (YAML mode) or
// the positional CLI arg + --name + --version (--help-parsing mode).
// Returns the spec plus 0 on success, or nil + non-zero exit on a
// flag-shape error already reported to stderr.
func loadSpec(configPath, name, version string, rest []string, stderr io.Writer) (*wrap.Spec, int) {
	if configPath != "" {
		data, err := os.ReadFile(configPath)
		if err != nil {
			fmt.Fprintf(stderr, "error: read %s: %v\n", configPath, err)
			return nil, 1
		}
		spec, err := wrap.LoadYAML(configPath, data)
		if err != nil {
			fmt.Fprintf(stderr, "error: %v\n", err)
			return nil, 1
		}
		return spec, 0
	}
	if len(rest) == 0 {
		fmt.Fprintln(stderr, "error: a CLI binary path is required (or pass --config=<yaml>)")
		fmt.Fprintln(stderr, actionUsage)
		return nil, 1
	}
	if name == "" {
		fmt.Fprintln(stderr, "error: --name=<FQN> is required in --help-parsing mode")
		return nil, 1
	}
	spec, err := wrap.FromHelp(context.Background(), actionWrapHelpRunner, name, version, rest[0])
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return nil, 1
	}
	return spec, 0
}

// runEmit writes the connector source tree under outDir. Default
// mode when --install is not set.
func runEmit(spec *wrap.Spec, outDir string, force bool, stdout, stderr io.Writer) int {
	if err := wrap.Emit(spec, outDir, force); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "Wrote connector source tree to %s\n", outDir)
	fmt.Fprintf(stdout, "Operations: %d\n", len(spec.Subcommands))
	fmt.Fprintln(stdout, "Next steps:")
	fmt.Fprintln(stdout, "  - Inspect connector/manifest.toml and actions/<op>.md.")
	fmt.Fprintln(stdout, "  - Run `aileron action wrap --config=... --install` to load directly into the daemon store.")
	return 0
}

// runInstall writes the manifest directly into the daemon's
// connector store and emits action files under ~/.aileron/actions/.
// No source tree on disk.
func runInstall(spec *wrap.Spec, force bool, stdout, stderr io.Writer) int {
	store := cstore.NewStore(cstore.DefaultRoot())
	if err := store.LoadIndex(); err != nil {
		// The index is a cache; a missing or unreadable index is not
		// fatal. We still install; subsequent index rebuilds pick it
		// up. Print a hint so the operator sees it.
		fmt.Fprintf(stderr, "note: store index unreadable (%v); proceeding with fresh install\n", err)
	}
	actionsDir := action.DefaultDir()
	res, err := wrap.Install(spec, store, actionsDir, actionWrapForwarder, force)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	if res.AlreadyInstalled {
		fmt.Fprintf(stdout, "Connector already installed at %s\n", res.Hash)
	} else {
		fmt.Fprintf(stdout, "Installed connector %s\n", spec.Connector.Name)
		fmt.Fprintf(stdout, "  Hash:       %s\n", res.Hash)
		fmt.Fprintf(stdout, "  Store dir:  %s\n", res.EntryDir)
	}
	fmt.Fprintf(stdout, "Action files: %d under %s\n", len(res.ActionFiles), actionsDir)
	for _, p := range res.ActionFiles {
		fmt.Fprintf(stdout, "  %s\n", p)
	}
	return 0
}
