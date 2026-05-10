package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/ALRubinger/aileron/internal/wrap"
)

// actionWrapHelpRunner is the HelpRunner used in production. Tests
// substitute a static fake so they don't depend on a real binary.
var actionWrapHelpRunner = wrap.HelpRunnerExec

// runActionWrap implements `aileron action wrap <CLI>`.
//
// Two input modes:
//
//	--config=<file>    Load a YAML spec the connector author maintains
//	                   in their source repo. Precise control over the
//	                   exposed subcommands and parameters.
//
//	(no --config)      Invoke <CLI> --help and emit a scaffold from the
//	                   parsed subcommand list. Quick path; the
//	                   connector author edits the emitted YAML or the
//	                   manifest before publishing.
//
// In both modes the emitter writes a connector source tree under
// --out (default: ./aileron-connector). Existing files are preserved
// unless --force is passed.
func runActionWrap(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("action wrap", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", "", "YAML spec describing the connector (precise mode)")
	outDir := flags.String("out", "./aileron-connector", "directory to write the connector source tree into")
	name := flags.String("name", "", "FQN of the connector to scaffold (--help mode); ignored when --config is set")
	version := flags.String("version", "0.0.1", "initial version string (--help mode); ignored when --config is set")
	force := flags.Bool("force", false, "overwrite existing files in --out")
	if err := flags.Parse(args); err != nil {
		return 1
	}
	rest := flags.Args()
	if *configPath != "" {
		// YAML mode. Positional arg is allowed but ignored — the
		// YAML carries everything.
		_ = rest
		return runWrapFromConfig(*configPath, *outDir, *force, stdout, stderr)
	}
	if len(rest) == 0 {
		fmt.Fprintln(stderr, "error: a CLI binary path is required (or pass --config=<yaml>)")
		fmt.Fprintln(stderr, actionUsage)
		return 1
	}
	if *name == "" {
		fmt.Fprintln(stderr, "error: --name=<FQN> is required in --help-parsing mode")
		return 1
	}
	return runWrapFromHelp(rest[0], *name, *version, *outDir, *force, stdout, stderr)
}

// runWrapFromConfig loads a YAML Spec and emits the connector tree.
func runWrapFromConfig(path, outDir string, force bool, stdout, stderr io.Writer) int {
	data, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(stderr, "error: read %s: %v\n", path, err)
		return 1
	}
	spec, err := wrap.LoadYAML(path, data)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	if err := wrap.Emit(spec, outDir, force); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "Wrote connector source tree to %s\n", outDir)
	fmt.Fprintf(stdout, "Subcommands: %d\n", len(spec.Subcommands))
	fmt.Fprintln(stdout, "Next steps:")
	fmt.Fprintln(stdout, "  1. Review connector/manifest.toml (especially [capabilities.spawn]).")
	fmt.Fprintln(stdout, "  2. Generate a publisher signing key under keys/ (see keys/README.md).")
	fmt.Fprintln(stdout, "  3. Build the WASM connector and tag a release.")
	return 0
}

// runWrapFromHelp invokes `<program> --help`, parses the output, and
// emits a scaffold the connector author edits.
func runWrapFromHelp(program, name, version, outDir string, force bool, stdout, stderr io.Writer) int {
	spec, err := wrap.FromHelp(context.Background(), actionWrapHelpRunner, name, version, program)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	if err := wrap.Emit(spec, outDir, force); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "Scaffolded connector for %s at %s\n", program, outDir)
	fmt.Fprintf(stdout, "Subcommands detected: %d\n", len(spec.Subcommands))
	fmt.Fprintln(stdout, "This is a scaffold. Edit connector/manifest.toml to:")
	fmt.Fprintln(stdout, "  - Refine each subcommand's argv pattern (placeholders match {name} tokens).")
	fmt.Fprintln(stdout, "  - Declare env_passthrough, fs_read, and fs_write scopes.")
	fmt.Fprintln(stdout, "  - Wire a credential capability if the binary needs one.")
	return 0
}
