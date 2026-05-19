package wrap

import (
	"encoding/json"
	"strings"
)

// AgentContext is the subset of the PrintingPress `agent-context`
// JSON envelope the wrap heuristic consults to recover positional
// arguments that a CLI's Cobra `--help` Usage block hides. The full
// schema (versioned by `schema_version`) carries flag types,
// defaults, pp:endpoint annotations, and auth metadata — none of
// which the introspector needs in v1, so the decoder ignores them.
//
// Why a separate source: Cobra's "shortcut" convention emits a
// Usage line that elides the positional even when the underlying
// command requires one (linear-pp-cli's `attachments` is a
// shortcut for `attachments get <id>`, but `--help` shows
// `attachments <id> [flags]` only for the leaf — the shortcut
// row in `agent-context` is what reliably carries the `<id>`).
// Preferring the `use` string when present means a PP-style CLI
// that documents its positionals in agent-context produces a
// working wrap even when its `--help` output is ambiguous.
//
// Schema reference: github.com/mvanhorn/printing-press-library
// (the catalog the `aileron pp add` installer reads from).
type AgentContext struct {
	SchemaVersion string         `json:"schema_version"`
	Commands      []AgentCommand `json:"commands"`
}

// AgentCommand is one entry in the agent-context `commands` /
// `subcommands` tree. Recursive — namespace commands carry a
// `subcommands` array, leaves don't.
type AgentCommand struct {
	Name        string         `json:"name"`
	Use         string         `json:"use"`
	Short       string         `json:"short"`
	Subcommands []AgentCommand `json:"subcommands,omitempty"`
}

// ParseAgentContext decodes the captured `agent-context` stdout
// into the [AgentContext] subset the wrap heuristic uses. Returns
// nil with no error when `data` is empty (the binary doesn't
// implement the agent-context convention; callers should fall
// back to `--help` parsing). Returns an error only for malformed
// JSON — every other defect (missing fields, unknown schema
// version) decodes to zero values and the caller continues.
func ParseAgentContext(data []byte) (*AgentContext, error) {
	if len(data) == 0 {
		return nil, nil
	}
	// Tolerate leading whitespace / BOM that some CLIs emit. The
	// JSON object itself starts at the first `{`.
	trimmed := stripUntilJSON(data)
	if len(trimmed) == 0 {
		return nil, nil
	}
	var ctx AgentContext
	if err := json.Unmarshal(trimmed, &ctx); err != nil {
		return nil, err
	}
	return &ctx, nil
}

// stripUntilJSON trims bytes preceding the first `{` so a CLI
// that prefixes its JSON with a banner or progress line still
// parses. Returns the byte slice from `{` onward, or nil if no
// `{` is present.
func stripUntilJSON(data []byte) []byte {
	for i, b := range data {
		if b == '{' {
			return data[i:]
		}
	}
	return nil
}

// PositionalsByPath returns a map from verb-path (joined by " ")
// to the positionals declared in each command's `use` string.
// Empty when the context is nil or carries no commands.
//
// Example: linear-pp-cli's agent-context describes an
// `attachments` command with `"use": "attachments <id>"` →
// PositionalsByPath returns `{"attachments": [{Name:"id",
// Required:true}]}`. The `--help` parser found nothing for the
// same path because the Usage line was `attachments [flags]` (the
// shortcut convention); the wrap emitter then prefers this
// positional set.
//
// Variadic positionals (`<ARGS...>` / `[ARGS...]`) are skipped to
// match parsePositionals — the argv-pattern grammar doesn't
// represent variadic slots in v1.
func PositionalsByPath(ctx *AgentContext) map[string][]positionalSpec {
	if ctx == nil || len(ctx.Commands) == 0 {
		return nil
	}
	out := map[string][]positionalSpec{}
	for _, c := range ctx.Commands {
		walkAgentCommands(out, nil, c)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// walkAgentCommands recursively flattens an [AgentCommand] tree
// into a verb-path → positionals map. `parent` is the chain of
// verbs accumulated above this node (e.g. `["issues"]` when
// visiting `issues.create`). Empty parent + cmd.Name "attachments"
// yields the key "attachments"; parent `["issues"]` + cmd.Name
// "create" yields "issues create".
func walkAgentCommands(out map[string][]positionalSpec, parent []string, cmd AgentCommand) {
	if cmd.Name == "" {
		return
	}
	path := append(append([]string(nil), parent...), cmd.Name)
	key := strings.Join(path, " ")
	if positionals := positionalsFromUse(cmd.Use, cmd.Name); len(positionals) > 0 {
		out[key] = positionals
	}
	for _, sub := range cmd.Subcommands {
		walkAgentCommands(out, path, sub)
	}
}

// positionalsFromUse parses an agent-context `use` string into the
// positionals it declares. The `use` field is a single line with
// the same `[ARG]` / `<ARG>` markers Cobra's Usage block uses, but
// without the binary prefix and the trailing `[flags]` token.
//
// Examples:
//
//	"attachments <id>"            → [{Name:"id", Required:true}]
//	"issues [ID]"                 → [{Name:"id", Required:false}]
//	"create"                      → nil
//	""                            → nil
//
// `verbName` is the leaf verb's own name (e.g. "attachments") so
// the parser can skip it as the leading non-marker token rather
// than mistaking a literal positional.
func positionalsFromUse(use, verbName string) []positionalSpec {
	use = strings.TrimSpace(use)
	if use == "" {
		return nil
	}
	// Strip the leading verb name so the bracket-marker extractor
	// doesn't have to know about it. `extractPositionalsFromUsageLine`
	// only looks at `[...]` and `<...>` shapes; any leading literal
	// tokens are ignored by that pass, so this trim is belt-and-
	// suspenders for shapes like `<verb> <ID>` where the verb might
	// resemble a positional.
	if verbName != "" && strings.HasPrefix(use, verbName) {
		use = strings.TrimSpace(use[len(verbName):])
	}
	return extractPositionalsFromUsageLine(use)
}
