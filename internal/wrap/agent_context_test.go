package wrap

import (
	"context"
	"strings"
	"testing"
)

// TestParseAgentContext_DecodesPPCatalogShape exercises the subset
// of the PrintingPress agent-context schema the wrap heuristic
// reads. Fields the parser doesn't consult (schema_version-only
// shape, auth blocks, flag metadata) are still required to decode
// without erroring — the struct uses json tags that ignore them.
func TestParseAgentContext_DecodesPPCatalogShape(t *testing.T) {
	in := `{
		"schema_version": "3",
		"cli": {"name": "linear-pp-cli", "version": "1.0.0"},
		"commands": [
			{
				"name": "attachments",
				"use": "attachments <id>",
				"short": "Get a single attachment"
			},
			{
				"name": "issues",
				"use": "issues [ID]",
				"subcommands": [
					{"name": "create", "use": "create"},
					{"name": "list", "use": "list"}
				]
			}
		]
	}`
	got, err := ParseAgentContext([]byte(in))
	if err != nil {
		t.Fatalf("ParseAgentContext: %v", err)
	}
	if got == nil {
		t.Fatal("ParseAgentContext returned nil")
	}
	if got.SchemaVersion != "3" {
		t.Errorf("SchemaVersion = %q, want %q", got.SchemaVersion, "3")
	}
	if len(got.Commands) != 2 {
		t.Fatalf("Commands len = %d, want 2", len(got.Commands))
	}
	if got.Commands[0].Use != "attachments <id>" {
		t.Errorf("Commands[0].Use = %q", got.Commands[0].Use)
	}
	if len(got.Commands[1].Subcommands) != 2 {
		t.Errorf("issues.Subcommands len = %d, want 2", len(got.Commands[1].Subcommands))
	}
}

// TestParseAgentContext_EmptyInputReturnsNil pins the convention:
// an empty body means "binary doesn't implement agent-context";
// callers fall back to --help-only parsing. Returning nil instead
// of an error keeps the call site terse.
func TestParseAgentContext_EmptyInputReturnsNil(t *testing.T) {
	for _, in := range [][]byte{nil, {}, []byte("   \n")} {
		got, err := ParseAgentContext(in)
		if err != nil {
			t.Errorf("ParseAgentContext(%q): err = %v, want nil", in, err)
		}
		if got != nil {
			t.Errorf("ParseAgentContext(%q): got = %+v, want nil", in, got)
		}
	}
}

// TestParseAgentContext_TolerateLeadingBanner covers the wild
// case where a CLI prints a one-line banner before the JSON. The
// parser strips up to the first `{` so a stray progress line or
// shell-color reset doesn't fail the whole introspection.
func TestParseAgentContext_TolerateLeadingBanner(t *testing.T) {
	in := "warming up...\n{\"schema_version\": \"3\", \"commands\": []}"
	got, err := ParseAgentContext([]byte(in))
	if err != nil {
		t.Fatalf("ParseAgentContext: %v", err)
	}
	if got == nil || got.SchemaVersion != "3" {
		t.Errorf("got = %+v, want SchemaVersion=3", got)
	}
}

// TestParseAgentContext_MalformedJSONErrors keeps the error path
// honest — a non-empty body that isn't JSON returns an error
// rather than nil, so the caller sees the problem rather than
// silently degrading to --help-only.
func TestParseAgentContext_MalformedJSONErrors(t *testing.T) {
	_, err := ParseAgentContext([]byte("{not json"))
	if err == nil {
		t.Errorf("ParseAgentContext on malformed input returned nil error")
	}
}

// TestPositionalsByPath_FlattenedKeyShape pins the verb-path
// shape walkSubcommand uses to look up overrides — space-joined,
// no binary prefix. Without this stability guarantee, a refactor
// of the path representation would silently disconnect the
// override path from the lookup path.
func TestPositionalsByPath_FlattenedKeyShape(t *testing.T) {
	ctx := &AgentContext{
		Commands: []AgentCommand{
			{
				Name: "attachments",
				Use:  "attachments <id>",
			},
			{
				Name: "issues",
				Use:  "issues [ID]",
				Subcommands: []AgentCommand{
					{Name: "create", Use: "create"},
					{Name: "get", Use: "get <issue_id>"},
				},
			},
		},
	}
	got := PositionalsByPath(ctx)
	if got == nil {
		t.Fatal("PositionalsByPath returned nil")
	}
	// "attachments" → [{id, required}]
	if specs, ok := got["attachments"]; !ok || len(specs) != 1 || specs[0].Name != "id" || !specs[0].Required {
		t.Errorf("attachments override = %+v; want [{id, required}]", specs)
	}
	// "issues" → [{id, optional}] (square brackets)
	if specs, ok := got["issues"]; !ok || len(specs) != 1 || specs[0].Name != "id" || specs[0].Required {
		t.Errorf("issues override = %+v; want [{id, optional}]", specs)
	}
	// "issues get" → [{issue_id, required}]
	if specs, ok := got["issues get"]; !ok || len(specs) != 1 || specs[0].Name != "issue_id" || !specs[0].Required {
		t.Errorf("issues get override = %+v; want [{issue_id, required}]", specs)
	}
	// "issues create" has no positional in its use string; the
	// PP convention puts flags in the separate `flags` array, not
	// inline in `use`. The override map should therefore not
	// carry an entry for this path — its absence is what tells
	// walkSubcommand to keep using the --help-derived positionals
	// (which for a flags-only leaf are empty).
	if _, ok := got["issues create"]; ok {
		t.Errorf("issues create unexpectedly has positional override; verb-only `use` should not register an entry")
	}
}

// TestPositionalsByPath_NilContextReturnsNil pins the no-op path:
// callers can pass nil (e.g. when the binary doesn't implement
// agent-context) and the lookup is a no-op rather than a nil
// dereference.
func TestPositionalsByPath_NilContextReturnsNil(t *testing.T) {
	if got := PositionalsByPath(nil); got != nil {
		t.Errorf("PositionalsByPath(nil) = %v, want nil", got)
	}
	if got := PositionalsByPath(&AgentContext{}); got != nil {
		t.Errorf("PositionalsByPath(empty) = %v, want nil", got)
	}
}

// TestFromHelpWithOptions_AgentContextRecoversHiddenPositional is
// the load-bearing test for `aileron pp add linear`: linear-pp-cli's
// `attachments --help` Usage line is `attachments [flags]` (the
// shortcut convention drops the `<id>` positional), but the
// agent-context output declares it explicitly. Without the
// override path, the wrap emits `linear-pp-cli attachments` with
// no input — and the Linear API rejects the malformed GraphQL
// query with a 400 CSRF response that looks like an auth failure
// to the agent. With the override, the leaf gets the `{id}`
// placeholder.
func TestFromHelpWithOptions_AgentContextRecoversHiddenPositional(t *testing.T) {
	rootHelp := `linear-pp-cli — Linear from the terminal.

Available Commands:
  attachments   Get a single attachment
`
	// The shortcut convention: Usage line elides the positional.
	leafHelp := `Shortcut for 'attachments get'. Get a single attachment

Usage:
  linear-pp-cli attachments [flags]
`
	runner := pathRunner(map[string]string{
		"":            rootHelp,
		"attachments": leafHelp,
	})
	agentCtx := &AgentContext{
		Commands: []AgentCommand{
			{Name: "attachments", Use: "attachments <id>"},
		},
	}
	s, err := FromHelpWithOptions(context.Background(), runner,
		"local://user/linear", "0.0.1", "/usr/local/bin/linear-pp-cli",
		FromHelpOptions{AgentContext: agentCtx})
	if err != nil {
		t.Fatalf("FromHelpWithOptions: %v", err)
	}
	if len(s.Subcommands) != 1 {
		t.Fatalf("subcommands = %d, want 1", len(s.Subcommands))
	}
	leaf := s.Subcommands[0]
	if !strings.Contains(leaf.Argv, "{id}") {
		t.Errorf("argv = %q; expected {id} positional (recovered from agent-context)", leaf.Argv)
	}
	if len(leaf.Params) != 1 || leaf.Params[0].Name != "id" || !leaf.Params[0].Required {
		t.Errorf("params = %+v; want one required `id` input", leaf.Params)
	}
}

// TestFromHelpWithOptions_AgentContextOverridesHelpPositional pins
// the precedence: when both --help and agent-context declare
// positionals, agent-context wins. The structured source is
// authoritative; --help parsing is a heuristic fallback.
func TestFromHelpWithOptions_AgentContextOverridesHelpPositional(t *testing.T) {
	rootHelp := `Available Commands:
  fetch   Fetch a resource
`
	// --help declares one optional positional.
	leafHelp := `Fetch a resource.

Usage:
  mycli fetch [URL] [flags]
`
	runner := pathRunner(map[string]string{
		"":      rootHelp,
		"fetch": leafHelp,
	})
	// agent-context declares it as REQUIRED with a different name.
	agentCtx := &AgentContext{
		Commands: []AgentCommand{
			{Name: "fetch", Use: "fetch <target_url>"},
		},
	}
	s, err := FromHelpWithOptions(context.Background(), runner,
		"local://user/mycli", "0.0.1", "/usr/local/bin/mycli",
		FromHelpOptions{AgentContext: agentCtx})
	if err != nil {
		t.Fatalf("FromHelpWithOptions: %v", err)
	}
	leaf := s.Subcommands[0]
	if len(leaf.Params) != 1 || leaf.Params[0].Name != "target_url" || !leaf.Params[0].Required {
		t.Errorf("params = %+v; agent-context should override --help (want required target_url)", leaf.Params)
	}
}

// TestFromHelp_NoAgentContextPreservesHelpParsing pins the
// backwards-compatibility floor: callers using bare FromHelp (no
// agent-context) get exactly the pre-#agent-context behavior. A
// shortcut-style binary still ends up with an empty positional
// list — the bug stays as it was, but only for connectors that
// can't supply agent-context.
func TestFromHelp_NoAgentContextPreservesHelpParsing(t *testing.T) {
	rootHelp := `Available Commands:
  attachments   Get a single attachment
`
	leafHelp := `Usage:
  mycli attachments [flags]
`
	runner := pathRunner(map[string]string{
		"":            rootHelp,
		"attachments": leafHelp,
	})
	s, err := FromHelp(context.Background(), runner,
		"local://user/mycli", "0.0.1", "/usr/local/bin/mycli")
	if err != nil {
		t.Fatalf("FromHelp: %v", err)
	}
	leaf := s.Subcommands[0]
	if len(leaf.Params) != 0 {
		t.Errorf("params = %+v; bare FromHelp must not synthesize positionals", leaf.Params)
	}
}
