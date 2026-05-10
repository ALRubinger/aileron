package wrap

import (
	"bufio"
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// LoadYAML decodes a Spec from raw YAML bytes. Returns a structured
// error pointing at the source path on parse failure.
func LoadYAML(path string, data []byte) (*Spec, error) {
	var s Spec
	if err := yaml.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := validate(&s, path); err != nil {
		return nil, err
	}
	return &s, nil
}

// validate enforces the post-parse invariants the YAML schema does
// not express: required fields, path shapes, env-key shapes, and the
// CredentialEnvKeys ⊆ EnvPassthrough rule.
func validate(s *Spec, path string) error {
	if s.Connector.Name == "" {
		return fmt.Errorf("%s: connector.name is required", path)
	}
	if s.Connector.Version == "" {
		return fmt.Errorf("%s: connector.version is required", path)
	}
	if s.Program.Path == "" {
		return fmt.Errorf("%s: program.path is required", path)
	}
	if !isAbsoluteOrTilde(s.Program.Path) {
		return fmt.Errorf("%s: program.path %q must be absolute or ~/-anchored", path, s.Program.Path)
	}
	if len(s.Subcommands) == 0 {
		return fmt.Errorf("%s: at least one subcommand is required", path)
	}
	for i, sub := range s.Subcommands {
		if sub.Name == "" {
			return fmt.Errorf("%s: subcommands[%d].name is required", path, i)
		}
		if strings.TrimSpace(sub.Argv) == "" {
			return fmt.Errorf("%s: subcommands[%d].argv is required", path, i)
		}
	}
	envSet := make(map[string]struct{}, len(s.EnvPassthrough))
	for i, k := range s.EnvPassthrough {
		if !envKeyRe.MatchString(k) {
			return fmt.Errorf("%s: env_passthrough[%d] %q is not a valid env name", path, i, k)
		}
		envSet[k] = struct{}{}
	}
	for i, k := range s.CredentialEnvKeys {
		if !envKeyRe.MatchString(k) {
			return fmt.Errorf("%s: credential_env_keys[%d] %q is not a valid env name", path, i, k)
		}
		if _, ok := envSet[k]; !ok {
			return fmt.Errorf(
				"%s: credential_env_keys[%d] %q is not in env_passthrough — every credential env key must be allowlisted",
				path, i, k)
		}
	}
	if s.Cwd != "" && !isAbsoluteOrTilde(s.Cwd) {
		return fmt.Errorf("%s: cwd %q must be absolute or ~/-anchored", path, s.Cwd)
	}
	for i, p := range s.FSRead {
		if !isAbsoluteOrTilde(p) {
			return fmt.Errorf("%s: fs_read[%d] %q must be absolute or ~/-anchored", path, i, p)
		}
	}
	for i, p := range s.FSWrite {
		if !isAbsoluteOrTilde(p) {
			return fmt.Errorf("%s: fs_write[%d] %q must be absolute or ~/-anchored", path, i, p)
		}
	}
	return nil
}

// envKeyRe is the POSIX-shape env name pattern.
var envKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// isAbsoluteOrTilde reports whether p is absolute or ~/-anchored.
func isAbsoluteOrTilde(p string) bool {
	if p == "" {
		return false
	}
	if strings.HasPrefix(p, "/") {
		return true
	}
	return p == "~" || strings.HasPrefix(p, "~/")
}

// HelpRunner runs `<program> --help` and returns the output. The
// production implementation is HelpRunnerExec; tests inject a fake.
type HelpRunner func(ctx context.Context, program string, args []string) (string, error)

// HelpRunnerExec runs the program with `args` (typically `--help`)
// and returns combined stdout/stderr. Subject to a 5-second timeout
// so a hung help command doesn't block the wrap operation.
func HelpRunnerExec(ctx context.Context, program string, args []string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, program, args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// FromHelp builds a Spec by invoking `<program> --help` and parsing
// the output heuristically. The returned Spec is a scaffold the
// connector author edits — the parser captures the top-level
// subcommand list and a placeholder argv per subcommand. Per-flag
// arity, parameter types, and descriptions need hand-editing in the
// emitted YAML or directly in the manifest.
//
// `name` is the FQN to assign (`github://acme/foo`).
// `version` is the connector's initial version (`0.0.1`).
// `program` is the absolute path of the binary to wrap.
// `runner` runs the help command; pass HelpRunnerExec in production.
func FromHelp(ctx context.Context, runner HelpRunner, name, version, program string) (*Spec, error) {
	if !isAbsoluteOrTilde(program) {
		return nil, fmt.Errorf("program path %q must be absolute or ~/-anchored", program)
	}
	help, err := runner(ctx, program, []string{"--help"})
	if err != nil {
		// Some programs exit non-zero on --help (notably curl). The
		// captured output is still parseable; fall through.
		if help == "" {
			return nil, fmt.Errorf("invoke %s --help: %w", program, err)
		}
	}
	subs := parseSubcommands(help)
	if len(subs) == 0 {
		// CLIs without subcommands still get a single default
		// operation that maps to the bare program invocation.
		subs = []SubcommandSpec{{
			Name:        "run",
			Description: "Default invocation",
			Argv:        baseName(program),
		}}
	}
	prog := program
	return &Spec{
		Connector: ConnectorSpec{Name: name, Version: version},
		Program:   ProgramSpec{Path: prog},
		Subcommands: subs,
	}, nil
}

// parseSubcommands extracts subcommands from a `--help` output. The
// heuristic looks for a "Commands:" or "Subcommands:" section header
// followed by indented `<name> <description>` lines. This shape is
// what cobra, urfave/cli, and most modern CLI frameworks produce.
//
// Lines that do not look like subcommand entries are ignored. The
// caller treats this as a scaffold rather than a contract.
func parseSubcommands(help string) []SubcommandSpec {
	var out []SubcommandSpec
	scanner := bufio.NewScanner(strings.NewReader(help))
	inSection := false
	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		if !inSection {
			if isSubcommandHeader(trimmed) {
				inSection = true
			}
			continue
		}
		if trimmed == "" {
			// Blank line ends the subcommand block in most CLIs.
			if len(out) > 0 {
				return out
			}
			continue
		}
		// A subcommand line typically starts with significant
		// whitespace then `<name>  <description>`. Sections after
		// the subcommand block (`Flags:`, `Options:`) start at column
		// zero and are skipped.
		if !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") {
			if len(out) > 0 {
				return out
			}
			continue
		}
		name, desc := splitSubcommandLine(trimmed)
		if name == "" {
			continue
		}
		out = append(out, SubcommandSpec{
			Name:        name,
			Description: desc,
			Argv:        baseName(name),
		})
	}
	return out
}

// isSubcommandHeader recognizes the section headers cobra,
// urfave/cli, and clap-rs typically emit.
func isSubcommandHeader(line string) bool {
	low := strings.ToLower(strings.TrimSuffix(line, ":"))
	switch low {
	case "commands", "available commands", "subcommands", "available subcommands":
		return true
	}
	return false
}

// splitSubcommandLine splits a `name  description` row into its two
// halves. Whitespace runs of two or more separate the name from the
// description.
var twoOrMoreSpaces = regexp.MustCompile(`\s{2,}`)

func splitSubcommandLine(line string) (name, desc string) {
	parts := twoOrMoreSpaces.Split(line, 2)
	switch len(parts) {
	case 0:
		return "", ""
	case 1:
		return parts[0], ""
	default:
		return parts[0], strings.TrimSpace(parts[1])
	}
}

// baseName returns the last path segment of `p` so a program path
// like `/usr/bin/git` yields the argv[0] `git` the runtime expects
// in the spawn envelope.
func baseName(p string) string {
	idx := strings.LastIndex(p, "/")
	if idx < 0 {
		return p
	}
	return p[idx+1:]
}
