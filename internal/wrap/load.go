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

// FromHelpOptions carries optional inputs to [FromHelpWithOptions].
// Zero value matches the behavior of [FromHelp]; populated fields
// enrich the wrap with metadata the bare `--help` parser can't
// recover.
type FromHelpOptions struct {
	// AgentContext is the parsed PrintingPress `agent-context`
	// JSON, when the wrapped binary supports the convention. Used
	// to recover positionals that the Cobra `--help` Usage line
	// hides (e.g. `linear-pp-cli teams` is documented as
	// `teams <id>` in agent-context but as `teams [flags]` in
	// --help). When nil, FromHelp parses positionals from --help
	// alone — the pre-agent-context behavior.
	AgentContext *AgentContext
}

// FromHelp builds a Spec by introspecting `<program> --help` and
// every nested subcommand's `--help`. The output captures one
// SubcommandSpec per *leaf* command (a command with no further
// subcommands), with that leaf's required + optional flags as
// declared inputs and an argv template that uses `[...]` optional
// groups for the optional flags.
//
// For a Cobra CLI like linear-pp-cli:
//
//	linear-pp-cli                 (top-level)
//	└── issues                    (namespace — has Available Commands)
//	    ├── create                (leaf — has Flags only)   →  emits `issues-create`
//	    └── list                  (leaf)                     →  emits `issues-list`
//
// Namespace commands (those that route to nested subcommands) are
// not emitted as operations; only leaves are callable. This matches
// how the agent thinks about tool calls: "create an issue", not
// "navigate to the issues namespace and then create."
//
// `name` is the connector FQN to assign (`github://acme/foo` or
// `local://user/foo`).
// `version` is the connector's initial version.
// `program` is the absolute path of the binary to wrap.
// `runner` runs the help command; pass HelpRunnerExec in production.
//
// CLIs without any subcommands fall back to a single `run`
// operation invoking the bare binary — same behavior as before the
// recursion landed, so single-purpose wrapping paths are unchanged.
func FromHelp(ctx context.Context, runner HelpRunner, name, version, program string) (*Spec, error) {
	return FromHelpWithOptions(ctx, runner, name, version, program, FromHelpOptions{})
}

// FromHelpWithOptions is the option-bearing variant of [FromHelp].
// Callers that have already captured the binary's `agent-context`
// output (via [sandbox.RunIntrospect] + [ParseAgentContext]) pass
// it here so the wrap heuristic can prefer the structured
// positional declarations over `--help`-line parsing.
func FromHelpWithOptions(ctx context.Context, runner HelpRunner, name, version, program string, opts FromHelpOptions) (*Spec, error) {
	if !isAbsoluteOrTilde(program) {
		return nil, fmt.Errorf("program path %q must be absolute or ~/-anchored", program)
	}
	rootHelp, err := runner(ctx, program, []string{"--help"})
	if err != nil {
		// Some programs exit non-zero on --help (notably curl). The
		// captured output is still parseable; fall through.
		if rootHelp == "" {
			return nil, fmt.Errorf("invoke %s --help: %w", program, err)
		}
	}
	positionalOverrides := PositionalsByPath(opts.AgentContext)
	rootSubs := parseSubcommands(rootHelp)
	if len(rootSubs) == 0 {
		// CLIs without subcommands still get a single default
		// operation that maps to the bare program invocation.
		return &Spec{
			Connector: ConnectorSpec{Name: name, Version: version},
			Program:   ProgramSpec{Path: program},
			Subcommands: []SubcommandSpec{{
				Name:        "run",
				Description: "Default invocation",
				Argv:        baseName(program),
			}},
		}, nil
	}

	progBase := baseName(program)
	var leaves []SubcommandSpec
	for _, rs := range rootSubs {
		discovered, err := walkSubcommand(ctx, runner, program, progBase, []string{rs.Name}, rs.Description, 1, positionalOverrides)
		if err != nil {
			// A subcommand whose --help fails (rare, but happens on
			// stub commands) drops out of the wrap rather than
			// failing the whole install. The author can hand-add it
			// later via the YAML path.
			continue
		}
		leaves = append(leaves, discovered...)
	}
	if len(leaves) == 0 {
		// Every recursive walk failed to surface a leaf. Fall back
		// to the bare-subcommand emission so we don't ship a Spec
		// with zero operations (cstore.ValidateManifest rejects that).
		// Prepend the binary basename so each fallback op invokes
		// the wrapped CLI correctly (without this, argv[0] eats
		// the subcommand token at exec time and the wrapped CLI
		// runs as if called bare).
		leaves = make([]SubcommandSpec, len(rootSubs))
		for i, rs := range rootSubs {
			leaves[i] = SubcommandSpec{
				Name:        rs.Name,
				Description: rs.Description,
				Argv:        progBase + " " + rs.Name,
			}
		}
	}
	return &Spec{
		Connector: ConnectorSpec{Name: name, Version: version},
		Program:   ProgramSpec{Path: program},
		Subcommands: leaves,
	}, nil
}

// maxSubcommandDepth caps how deep [walkSubcommand] recurses into a
// subcommand tree. Cobra-shaped CLIs in PrintingPress's catalog top
// out at depth 2 (`linear-pp-cli issues create`); gh/kubectl reach
// 3 (`gh api graphql query`). Cap at 5 — anything deeper is either
// a CLI authoring pathology or a runner that returns identical help
// for every path (which the test fixtures used to do, and which
// would otherwise spin forever). When the cap is hit the current
// node is emitted as a leaf with whatever flags its help declares,
// rather than continuing to recurse — the agent loses access to
// genuinely-deep verbs, but the install completes.
const maxSubcommandDepth = 5

// walkSubcommand recursively explores a subcommand path. Returns
// the leaf SubcommandSpecs discovered at this path (one when the
// command is itself a leaf, many when it's a namespace whose
// children are leaves, transitively).
//
// `path` is the chain of verbs from the root binary to this
// subcommand (e.g. `["issues", "create"]`). The Cobra runtime
// invokes `<binary> <path[0]> <path[1]> --help` to discover the
// next layer.
//
// `description` is the line of prose the parent command's --help
// associated with this subcommand. Plumbed down so leaves carry
// the agent-facing prose from their parent's row when their own
// --help body doesn't have a one-line summary at the top.
//
// `depth` is the current recursion depth (1 at the top-level
// subcommand, 2 at its children, ...). Bounded by
// [maxSubcommandDepth].
func walkSubcommand(ctx context.Context, runner HelpRunner, program, progBase string, path []string, description string, depth int, positionalOverrides map[string][]positionalSpec) ([]SubcommandSpec, error) {
	args := append(append([]string(nil), path...), "--help")
	help, err := runner(ctx, program, args)
	if err != nil {
		if help == "" {
			return nil, fmt.Errorf("invoke %s %s: %w", program, strings.Join(args, " "), err)
		}
		// Tolerate non-zero exit when the output is still parseable.
	}
	subs := parseSubcommands(help)
	if len(subs) > 0 && depth < maxSubcommandDepth {
		// Namespace: recurse into each child. The namespace itself
		// is not emitted as an operation — agents don't call
		// "issues" without a verb; they call "issues create".
		var out []SubcommandSpec
		for _, s := range subs {
			child := append(append([]string(nil), path...), s.Name)
			discovered, err := walkSubcommand(ctx, runner, program, progBase, child, s.Description, depth+1, positionalOverrides)
			if err != nil {
				continue
			}
			out = append(out, discovered...)
		}
		return out, nil
	}
	// Leaf (or depth cap reached): parse positionals + flags and
	// emit a SubcommandSpec. Positionals come from the Usage block
	// (`<binary> verb [POS] [flags]`); flags from the Flags + Global
	// Flags sections.
	//
	// Agent-context overrides win when present, even if --help
	// already parsed positionals — the structured source is more
	// reliable than free-form Usage scraping when the CLI provides
	// it. CLIs without agent-context fall back to the --help
	// parser as before.
	positionals := parsePositionals(help)
	if override, ok := positionalOverrides[strings.Join(path, " ")]; ok && len(override) > 0 {
		positionals = override
	}
	flags := parseFlags(help)
	leaf := buildLeafSpec(progBase, path, description, positionals, flags)
	return []SubcommandSpec{leaf}, nil
}

// buildLeafSpec assembles a SubcommandSpec for a leaf command. The
// operation name is the verb path joined by `-` (matches manifest
// op-name shape `^[a-z][a-z0-9_-]*$`). The argv template lays out
// the leaf invocation in the order Cobra expects it on the
// command line:
//
//   - binary basename as argv[0] (the runtime exec uses
//     `Argv[1:]` as the subprocess args, treating Argv[0] as the
//     conventional binary-name slot — see
//     `internal/sandbox/spawn.go`'s `exec.CommandContext(ctx,
//     env.Program, env.Argv[1:]...)`. Omitting the basename here
//     would cause the first verb token to be eaten by argv[0] and
//     the wrapped CLI would run as if called bare)
//   - verb path tokens (literals)
//   - required positionals as bare placeholders `{name}`
//   - optional positionals wrapped in `[{name}]` optional groups
//   - required flags as `--flag {name}`
//   - optional flags wrapped in `[--flag {name}]` optional groups
//
// Example for `linear-pp-cli sql [SQL] [flags]` with a `--rate-limit`
// optional flag, wrapped as `linear-pp-cli`:
//
//	Name:        "sql"
//	Argv:        "linear-pp-cli sql [{sql}] [--rate-limit {rate_limit}]"
//	Params:      [{sql, string, optional}, {rate_limit, number, optional}]
//
// Flag and positional names containing dashes (`--data-source`,
// `<FILE-PATH>`) are translated to underscore form (`data_source`,
// `file_path`) for the action input name AND the argv placeholder,
// since action input names are JSON Schema property names that
// must match `[a-z][a-z0-9_]*` per the manifest validator. The
// argv *literal* for flag names keeps the original spelling — that's
// what the wrapped CLI's flag parser expects.
func buildLeafSpec(progBase string, path []string, description string, positionals []positionalSpec, flags []flagSpec) SubcommandSpec {
	name := strings.Join(path, "-")
	var argv strings.Builder
	argv.WriteString(progBase)
	for _, p := range path {
		argv.WriteByte(' ')
		argv.WriteString(p)
	}

	// Positionals follow Cobra ordering: they appear before flags
	// on the command line. Required positionals are bare
	// placeholders; optional positionals get the `[...]` group
	// treatment so the agent can omit them and still produce a
	// valid argv (the bare-invocation behavior Cobra preserves
	// for `[ARG]`-shaped slots).
	var params []ParamSpec
	for _, p := range positionals {
		if p.Required {
			argv.WriteString(" {")
			argv.WriteString(p.Name)
			argv.WriteString("}")
		} else {
			argv.WriteString(" [{")
			argv.WriteString(p.Name)
			argv.WriteString("}]")
		}
		params = append(params, ParamSpec{
			Name:        p.Name,
			Type:        "string",
			Description: positionalDescription(p),
			Required:    p.Required,
		})
	}

	// Required flags first (stable order — order they appeared in
	// the help, which Cobra alphabetizes). Then optional flags in
	// the same order. The agent's tool catalog renders the JSON
	// schema's `properties` map by declaration order; required-
	// first keeps the call shape predictable.
	required := make([]flagSpec, 0, len(flags))
	optional := make([]flagSpec, 0, len(flags))
	for _, f := range flags {
		if f.Required {
			required = append(required, f)
		} else {
			optional = append(optional, f)
		}
	}
	for _, f := range required {
		inputName := flagInputName(f.Name)
		argv.WriteString(" --")
		argv.WriteString(f.Name)
		argv.WriteString(" {")
		argv.WriteString(inputName)
		argv.WriteString("}")
		params = append(params, ParamSpec{
			Name:        inputName,
			Type:        inputType(f.Type),
			Description: f.Description,
			Required:    true,
		})
	}
	for _, f := range optional {
		inputName := flagInputName(f.Name)
		argv.WriteString(" [--")
		argv.WriteString(f.Name)
		argv.WriteString(" {")
		argv.WriteString(inputName)
		argv.WriteString("}]")
		params = append(params, ParamSpec{
			Name:        inputName,
			Type:        inputType(f.Type),
			Description: f.Description,
			Required:    false,
		})
	}
	return SubcommandSpec{
		Name:        name,
		Description: description,
		Argv:        argv.String(),
		Params:      params,
	}
}

// positionalDescription returns a human-readable description for a
// positional input. Cobra's --help doesn't carry per-positional
// prose the way it does for flags (`--flag string  Description`),
// so we synthesize a minimal description from the positional's
// name and required-ness. The author can hand-edit it later via
// `--edit`.
func positionalDescription(p positionalSpec) string {
	if p.Required {
		return "Positional argument: " + p.Name + " (required)"
	}
	return "Positional argument: " + p.Name + " (optional)"
}

// flagInputName translates a Cobra long-form flag name into the
// shape an action-manifest input requires. The manifest validator
// enforces `^[a-z][a-z0-9_]*$` on input names (JSON Schema property
// shape, snake_case convention — `internal/action/validate.go`),
// but Cobra flags conventionally use kebab-case (`--data-source`,
// `--auth-set-api-key`). We translate dashes to underscores so the
// emitted action.md validates while the argv literal — what the
// wrapped CLI's flag parser actually reads — keeps the original
// dashed spelling.
//
// This is a one-way translation; the agent's call carries
// `data_source` as the input name and the spawn-pattern
// placeholder also uses `data_source`. The connector never sees
// the underscore form because the manifest's argv-template literal
// is `--data-source`.
func flagInputName(flagName string) string {
	return strings.ReplaceAll(flagName, "-", "_")
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
		// Defense in depth against parser surprises: skip names
		// that won't survive the manifest schema's POSIX-shape
		// rule. The introspector should never emit a bad name
		// after the splitter relaxation below, but a row whose
		// "name" column happens to contain a number-led token
		// or punctuation the regex rejects would otherwise fail
		// the entire install rather than just dropping the
		// outlier subcommand.
		if !subcommandNameRe.MatchString(name) {
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

// subcommandNameRe matches the operation-name shape the manifest
// schema accepts (`^[a-z][a-z0-9_-]*$` per [cstore.ValidateManifest]).
// Centralized here so the heuristic parser can drop malformed
// rows before they reach the manifest validator and fail the
// whole install.
var subcommandNameRe = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)

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

// splitSubcommandLine splits a `name <gap> description` row into
// its two halves. Cobra and urfave/cli space the columns with
// two or more characters; some other generators (the Linear
// CLI's reflection-based runtime, for example) use a single
// space. The splitter treats any run of whitespace as the
// column separator — operation names are constrained to
// `[a-z][a-z0-9_-]*` by the manifest schema, so they never
// contain internal whitespace, and a single-space gap can't
// occur inside a real name.
var splitterRe = regexp.MustCompile(`\s+`)

func splitSubcommandLine(line string) (name, desc string) {
	parts := splitterRe.Split(line, 2)
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
