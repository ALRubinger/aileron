package augment

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/action"
)

// Stage 3 contract (#369, ratified by ADR-0008):
//   - Derive maps a manifest into the protocol-neutral DerivedAction.
//   - kebab-case → snake_case.
//   - Description is the manifest body with leading `# Heading` stripped,
//     falling back to Match.Intent.
//   - Parameters is a JSON Schema object built from [[inputs]].
//   - AugmentOpenAI / AugmentAnthropic append derived actions to
//     `tools` in their respective shapes.
//   - Forward collision: agent's tool wins; Aileron's gets `aileron.` prefix.
//   - Reverse collision: agent tool with `aileron.` prefix is renamed
//     to `agent.<name>` with a warning.

// --- captureLogger lets tests assert warnings ---

type captureLogger struct {
	calls []logCall
}

type logCall struct {
	msg  string
	args []any
}

func (c *captureLogger) Warn(msg string, args ...any) {
	c.calls = append(c.calls, logCall{msg: msg, args: args})
}

// --- helpers ---

func loadedAction(t *testing.T, name, body string, inputs []action.Input) action.LoadedAction {
	t.Helper()
	return action.LoadedAction{
		Manifest: &action.Manifest{
			Name:    name,
			Version: "1.0.0",
			Source:  "hub://aileron/" + name + "@1.0.0",
			Match:   action.Match{Intent: "do " + name},
			Inputs:  inputs,
			Body:    body,
		},
		Path: "/tmp/" + name + ".md",
	}
}

func required(b bool) *bool { return &b }

// --- Derive ---

func TestDerive_KebabToSnake(t *testing.T) {
	la := loadedAction(t, "ship-update", "Posts a ship update.", nil)
	got := Derive(la)
	if got.Name != "ship_update" {
		t.Errorf("Name = %q, want ship_update", got.Name)
	}
}

func TestDerive_StripsLeadingHeading(t *testing.T) {
	la := loadedAction(t, "x", "# Title\n\nThe real description.", nil)
	got := Derive(la)
	if got.Description != "The real description." {
		t.Errorf("Description = %q, want %q", got.Description, "The real description.")
	}
}

func TestDerive_BodyWithoutHeadingPassesThrough(t *testing.T) {
	la := loadedAction(t, "x", "Just prose, no heading.", nil)
	got := Derive(la)
	if got.Description != "Just prose, no heading." {
		t.Errorf("Description = %q", got.Description)
	}
}

func TestDerive_FallbackToIntentWhenBodyEmpty(t *testing.T) {
	la := loadedAction(t, "x", "", nil)
	got := Derive(la)
	if got.Description != "do x" {
		t.Errorf("Description = %q, want fallback to intent", got.Description)
	}
}

func TestDerive_FallbackToIntentWhenBodyOnlyHeading(t *testing.T) {
	la := loadedAction(t, "x", "# Just a heading", nil)
	got := Derive(la)
	if got.Description != "do x" {
		t.Errorf("Description = %q, want fallback to intent", got.Description)
	}
}

func TestDerive_ParametersWithNoInputs(t *testing.T) {
	la := loadedAction(t, "x", "body", nil)
	got := Derive(la)
	if got.Parameters["type"] != "object" {
		t.Errorf("Parameters[type] = %v, want object", got.Parameters["type"])
	}
	if _, hasProps := got.Parameters["properties"]; hasProps {
		t.Error("Parameters has properties; want none for action with no inputs")
	}
}

func TestDerive_ParametersWithInputs(t *testing.T) {
	la := loadedAction(t, "x", "body", []action.Input{
		{Name: "channel", Type: "string", Description: "Slack channel"},
		{Name: "max_lines", Type: "integer", Required: required(false), Description: "Max lines"},
	})
	got := Derive(la)

	if got.Parameters["type"] != "object" {
		t.Errorf("Parameters[type] = %v", got.Parameters["type"])
	}
	props, ok := got.Parameters["properties"].(map[string]any)
	if !ok {
		t.Fatalf("Parameters[properties] not a map: %T", got.Parameters["properties"])
	}
	channel, _ := props["channel"].(map[string]any)
	if channel["type"] != "string" || channel["description"] != "Slack channel" {
		t.Errorf("channel field = %v", channel)
	}
	maxLines, _ := props["max_lines"].(map[string]any)
	if maxLines["type"] != "integer" || maxLines["description"] != "Max lines" {
		t.Errorf("max_lines field = %v", maxLines)
	}

	req, ok := got.Parameters["required"].([]string)
	if !ok {
		t.Fatalf("Parameters[required] not []string: %T", got.Parameters["required"])
	}
	sort.Strings(req)
	if !reflect.DeepEqual(req, []string{"channel"}) {
		t.Errorf("required = %v, want [channel]", req)
	}
}

// --- AugmentOpenAI ---

func TestAugmentOpenAI_NoActions_ReturnsBodyUnchanged(t *testing.T) {
	body := []byte(`{"model":"gpt-4","messages":[]}`)
	got, err := AugmentOpenAI(body, nil, nil)
	if err != nil {
		t.Fatalf("AugmentOpenAI: %v", err)
	}
	if string(got) != string(body) {
		t.Errorf("body modified despite no actions: %s", got)
	}
}

func TestAugmentOpenAI_AppendsAction(t *testing.T) {
	body := []byte(`{"model":"gpt-4","messages":[]}`)
	actions := []DerivedAction{{
		Name:        "ship_update",
		Description: "post a ship update",
		Parameters:  map[string]any{"type": "object"},
	}}
	got, err := AugmentOpenAI(body, actions, nil)
	if err != nil {
		t.Fatalf("AugmentOpenAI: %v", err)
	}

	var req map[string]any
	if err := json.Unmarshal(got, &req); err != nil {
		t.Fatalf("decode: %v", err)
	}
	tools, _ := req["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("got %d tools, want 1", len(tools))
	}
	tm := tools[0].(map[string]any)
	if tm["type"] != "function" {
		t.Errorf("tool type = %v, want function", tm["type"])
	}
	fn, _ := tm["function"].(map[string]any)
	if fn["name"] != "ship_update" {
		t.Errorf("name = %v, want ship_update", fn["name"])
	}
	if fn["description"] != "post a ship update" {
		t.Errorf("description = %v", fn["description"])
	}
}

func TestAugmentOpenAI_PreservesAgentTools(t *testing.T) {
	body := []byte(`{"model":"gpt-4","messages":[],"tools":[{"type":"function","function":{"name":"search","description":"agent search"}}]}`)
	actions := []DerivedAction{{Name: "ship_update", Description: "x", Parameters: map[string]any{"type": "object"}}}
	got, err := AugmentOpenAI(body, actions, nil)
	if err != nil {
		t.Fatalf("AugmentOpenAI: %v", err)
	}
	var req map[string]any
	json.Unmarshal(got, &req)
	tools, _ := req["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("got %d tools, want 2", len(tools))
	}
	fn0 := tools[0].(map[string]any)["function"].(map[string]any)
	if fn0["name"] != "search" {
		t.Errorf("agent tool name changed: %v", fn0["name"])
	}
}

func TestAugmentOpenAI_NameCollision_RenamesAileronAction(t *testing.T) {
	body := []byte(`{"model":"gpt-4","messages":[],"tools":[{"type":"function","function":{"name":"search"}}]}`)
	actions := []DerivedAction{{Name: "search", Description: "Aileron's search", Parameters: map[string]any{"type": "object"}}}
	got, err := AugmentOpenAI(body, actions, nil)
	if err != nil {
		t.Fatalf("AugmentOpenAI: %v", err)
	}
	var req map[string]any
	json.Unmarshal(got, &req)
	tools := req["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("got %d tools, want 2", len(tools))
	}
	agentName := tools[0].(map[string]any)["function"].(map[string]any)["name"]
	if agentName != "search" {
		t.Errorf("agent tool renamed unexpectedly: %v", agentName)
	}
	aileronName := tools[1].(map[string]any)["function"].(map[string]any)["name"]
	if aileronName != "aileron.search" {
		t.Errorf("Aileron action name = %v, want aileron.search", aileronName)
	}
}

func TestAugmentOpenAI_ReverseCollision_RenamesAgentTool(t *testing.T) {
	body := []byte(`{"model":"gpt-4","messages":[],"tools":[{"type":"function","function":{"name":"aileron.search","description":"agent tool with reserved prefix"}}]}`)
	actions := []DerivedAction{{Name: "ship_update", Description: "x", Parameters: map[string]any{"type": "object"}}}
	log := &captureLogger{}
	got, err := AugmentOpenAI(body, actions, log)
	if err != nil {
		t.Fatalf("AugmentOpenAI: %v", err)
	}
	var req map[string]any
	json.Unmarshal(got, &req)
	tools := req["tools"].([]any)
	agentName := tools[0].(map[string]any)["function"].(map[string]any)["name"]
	if agentName != "agent.search" {
		t.Errorf("agent tool name = %v, want agent.search", agentName)
	}
	if len(log.calls) != 1 {
		t.Errorf("got %d warnings, want 1", len(log.calls))
	}
}

// --- AugmentAnthropic ---

func TestAugmentAnthropic_AppendsActionInAnthropicShape(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4-6","max_tokens":1024,"messages":[]}`)
	actions := []DerivedAction{{
		Name:        "ship_update",
		Description: "post a ship update",
		Parameters:  map[string]any{"type": "object"},
	}}
	got, err := AugmentAnthropic(body, actions, nil)
	if err != nil {
		t.Fatalf("AugmentAnthropic: %v", err)
	}
	var req map[string]any
	json.Unmarshal(got, &req)
	tools, _ := req["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("got %d tools, want 1", len(tools))
	}
	tm := tools[0].(map[string]any)
	// Anthropic shape: flat name/description/input_schema, no `function` wrapper.
	if _, hasFn := tm["function"]; hasFn {
		t.Error("Anthropic tool incorrectly wrapped in `function`")
	}
	if tm["name"] != "ship_update" {
		t.Errorf("name = %v, want ship_update", tm["name"])
	}
	if tm["description"] != "post a ship update" {
		t.Errorf("description = %v", tm["description"])
	}
	if _, ok := tm["input_schema"].(map[string]any); !ok {
		t.Errorf("input_schema missing or wrong type: %T", tm["input_schema"])
	}
}

func TestAugmentAnthropic_NameCollision_RenamesAileronAction(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4-6","max_tokens":1,"messages":[],"tools":[{"name":"search","description":"agent search","input_schema":{"type":"object"}}]}`)
	actions := []DerivedAction{{Name: "search", Description: "Aileron's", Parameters: map[string]any{"type": "object"}}}
	got, err := AugmentAnthropic(body, actions, nil)
	if err != nil {
		t.Fatalf("AugmentAnthropic: %v", err)
	}
	var req map[string]any
	json.Unmarshal(got, &req)
	tools := req["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("got %d tools, want 2", len(tools))
	}
	if tools[0].(map[string]any)["name"] != "search" {
		t.Errorf("agent tool renamed unexpectedly")
	}
	if tools[1].(map[string]any)["name"] != "aileron.search" {
		t.Errorf("Aileron action name = %v, want aileron.search", tools[1].(map[string]any)["name"])
	}
}

func TestAugmentAnthropic_ReverseCollision_RenamesAgentTool(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4-6","max_tokens":1,"messages":[],"tools":[{"name":"aileron.search","description":"agent","input_schema":{"type":"object"}}]}`)
	actions := []DerivedAction{{Name: "ship_update", Description: "x", Parameters: map[string]any{"type": "object"}}}
	log := &captureLogger{}
	got, err := AugmentAnthropic(body, actions, log)
	if err != nil {
		t.Fatalf("AugmentAnthropic: %v", err)
	}
	var req map[string]any
	json.Unmarshal(got, &req)
	tools := req["tools"].([]any)
	if tools[0].(map[string]any)["name"] != "agent.search" {
		t.Errorf("agent tool name = %v, want agent.search", tools[0].(map[string]any)["name"])
	}
	if len(log.calls) != 1 {
		t.Errorf("got %d warnings, want 1", len(log.calls))
	}
}

// --- Errors ---

func TestAugmentOpenAI_InvalidJSON_ReturnsError(t *testing.T) {
	_, err := AugmentOpenAI([]byte(`{not-json`), []DerivedAction{{Name: "x"}}, nil)
	if err == nil {
		t.Fatal("expected error decoding invalid JSON")
	}
	if !strings.Contains(err.Error(), "invalid") && !strings.Contains(err.Error(), "character") {
		// message just needs to be a json decode error
		t.Logf("error message: %v", err)
	}
}
