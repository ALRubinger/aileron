// Package augment converts installed actions into LLM tool definitions
// and appends them to outbound chat-completion / messages requests so
// the LLM can choose them alongside the agent's own tools.
//
// The contract is ratified by [ADR-0008]. Two protocol shapes are
// supported: OpenAI Chat Completions and Anthropic Messages.
//
// [ADR-0008]: https://docs.withaileron.ai/adr/0008-intent-matching
package augment

import (
	"encoding/json"
	"strings"

	"github.com/ALRubinger/aileron/internal/action"
)

// Logger is the minimal subset of slog.Logger used by this package. The
// indirection keeps augmentation decoupled from any concrete logger.
type Logger interface {
	Warn(msg string, args ...any)
}

// nopLogger discards messages; used when callers pass nil.
type nopLogger struct{}

func (nopLogger) Warn(string, ...any) {}

// DerivedAction is the protocol-neutral form of an installed action,
// ready for insertion into either an OpenAI or Anthropic tools array.
type DerivedAction struct {
	// Name is the LLM-facing function name (snake_case, mapped from the
	// manifest's kebab-case `name`).
	Name string

	// Description is the prose the LLM sees when choosing among tools —
	// the action's Markdown body with a leading `# Heading` line
	// stripped, falling back to `Match.Intent` if the body is empty.
	Description string

	// Parameters is the JSON Schema object derived from the manifest's
	// `[[inputs]]` declarations. Always at least
	// `{ "type": "object" }`; populated `properties` and `required`
	// entries are added when inputs exist.
	Parameters map[string]any
}

// Derive converts an action's manifest into the protocol-neutral form.
func Derive(la action.LoadedAction) DerivedAction {
	m := la.Manifest
	return DerivedAction{
		Name:        toolName(m.Name),
		Description: deriveDescription(m),
		Parameters:  deriveParameters(m),
	}
}

// DeriveAll is a convenience wrapper that derives every loaded action
// in a slice. Order is preserved.
func DeriveAll(actions []action.LoadedAction) []DerivedAction {
	out := make([]DerivedAction, 0, len(actions))
	for _, la := range actions {
		out = append(out, Derive(la))
	}
	return out
}

// toolName maps a manifest's kebab-case `name` to the snake_case
// identifier the LLM sees, per ADR-0008.
func toolName(manifestName string) string {
	return strings.ReplaceAll(manifestName, "-", "_")
}

// deriveDescription extracts the LLM-facing description from the
// manifest. If the body starts with a `# Heading` line, that line is
// stripped (the LLM doesn't need the title — it has the function
// name). When the body is empty, fall back to Match.Intent so the LLM
// always has *something* to read.
func deriveDescription(m *action.Manifest) string {
	body := strings.TrimSpace(m.Body)
	if body == "" {
		return strings.TrimSpace(m.Match.Intent)
	}
	lines := strings.SplitN(body, "\n", 2)
	if len(lines) > 0 && strings.HasPrefix(strings.TrimSpace(lines[0]), "# ") {
		if len(lines) == 1 {
			return strings.TrimSpace(m.Match.Intent)
		}
		return strings.TrimSpace(lines[1])
	}
	return body
}

// deriveParameters builds the JSON Schema parameters object from the
// manifest's [[inputs]] declarations. ADR-0003 promises `{ "type":
// "object" }` even when no inputs are declared.
func deriveParameters(m *action.Manifest) map[string]any {
	schema := map[string]any{
		"type": "object",
	}
	if len(m.Inputs) == 0 {
		return schema
	}
	props := make(map[string]any, len(m.Inputs))
	var required []string
	for _, in := range m.Inputs {
		field := map[string]any{"type": in.Type}
		if in.Description != "" {
			field["description"] = in.Description
		}
		props[in.Name] = field
		if in.IsRequired() {
			required = append(required, in.Name)
		}
	}
	schema["properties"] = props
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

// Augmented is the result of running [AugmentOpenAI] or
// [AugmentAnthropic]. Body is the re-encoded request payload to
// forward upstream; OurNames is the post-collision list of tool names
// the gateway now owns. The interception layer uses OurNames to
// decide whether a tool call coming back from the upstream LLM should
// be intercepted (Aileron-owned) or passed through unchanged
// (agent-declared).
type Augmented struct {
	Body     []byte
	OurNames []string
}

// AugmentOpenAI parses an OpenAI Chat Completions request body, applies
// collision rules, appends the derived actions to the `tools` array,
// and returns the re-encoded body alongside the post-collision list
// of Aileron-owned tool names. When `actions` is empty the body is
// returned untouched and OurNames is empty.
func AugmentOpenAI(body []byte, actions []DerivedAction, log Logger) (Augmented, error) {
	if len(actions) == 0 {
		return Augmented{Body: body}, nil
	}
	if log == nil {
		log = nopLogger{}
	}
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		return Augmented{}, err
	}

	tools, _ := req["tools"].([]any)
	declared := agentDeclaredOpenAI(tools, log)

	ourNames := make([]string, 0, len(actions))
	for _, a := range actions {
		finalName := a.Name
		if declared[finalName] {
			finalName = "aileron." + finalName
		}
		ourNames = append(ourNames, finalName)
		tools = append(tools, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        finalName,
				"description": a.Description,
				"parameters":  a.Parameters,
			},
		})
	}
	req["tools"] = tools
	out, err := json.Marshal(req)
	if err != nil {
		return Augmented{}, err
	}
	return Augmented{Body: out, OurNames: ourNames}, nil
}

// AugmentAnthropic parses an Anthropic Messages request body, applies
// collision rules, appends the derived actions to the `tools` array
// (Anthropic shape: name/description/input_schema), and returns the
// re-encoded body alongside the post-collision list of Aileron-owned
// tool names.
func AugmentAnthropic(body []byte, actions []DerivedAction, log Logger) (Augmented, error) {
	if len(actions) == 0 {
		return Augmented{Body: body}, nil
	}
	if log == nil {
		log = nopLogger{}
	}
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		return Augmented{}, err
	}

	tools, _ := req["tools"].([]any)
	declared := agentDeclaredAnthropic(tools, log)

	ourNames := make([]string, 0, len(actions))
	for _, a := range actions {
		finalName := a.Name
		if declared[finalName] {
			finalName = "aileron." + finalName
		}
		ourNames = append(ourNames, finalName)
		tools = append(tools, map[string]any{
			"name":         finalName,
			"description":  a.Description,
			"input_schema": a.Parameters,
		})
	}
	req["tools"] = tools
	out, err := json.Marshal(req)
	if err != nil {
		return Augmented{}, err
	}
	return Augmented{Body: out, OurNames: ourNames}, nil
}

// agentDeclaredOpenAI walks the OpenAI-shape tools array, applies the
// reverse-collision rule (agent tools using the reserved `aileron.`
// prefix are renamed to `agent.<original>`), and returns the set of
// agent-declared names.
func agentDeclaredOpenAI(tools []any, log Logger) map[string]bool {
	declared := map[string]bool{}
	for _, t := range tools {
		tm, ok := t.(map[string]any)
		if !ok {
			continue
		}
		fn, ok := tm["function"].(map[string]any)
		if !ok {
			continue
		}
		name, _ := fn["name"].(string)
		if name == "" {
			continue
		}
		name = renameIfReserved(name, fn, log)
		declared[name] = true
	}
	return declared
}

// agentDeclaredAnthropic mirrors agentDeclaredOpenAI for Anthropic's
// flat tool shape.
func agentDeclaredAnthropic(tools []any, log Logger) map[string]bool {
	declared := map[string]bool{}
	for _, t := range tools {
		tm, ok := t.(map[string]any)
		if !ok {
			continue
		}
		name, _ := tm["name"].(string)
		if name == "" {
			continue
		}
		name = renameIfReserved(name, tm, log)
		declared[name] = true
	}
	return declared
}

// renameIfReserved enforces ADR-0008's reverse-collision rule: an
// agent-declared tool whose name starts with the reserved `aileron.`
// prefix is renamed to `agent.<original-name>` and a warning is
// emitted. The provided map is mutated in place so the rename is
// visible in the forwarded request.
func renameIfReserved(name string, target map[string]any, log Logger) string {
	if !strings.HasPrefix(name, "aileron.") {
		return name
	}
	renamed := "agent." + strings.TrimPrefix(name, "aileron.")
	log.Warn("agent tool used reserved aileron. prefix; renaming",
		"from", name, "to", renamed)
	target["name"] = renamed
	return renamed
}
