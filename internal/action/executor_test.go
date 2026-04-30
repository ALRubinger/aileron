package action

import (
	"context"
	"encoding/json"
	"testing"
)

// StubExecutor must always succeed (no Go error), produce a JSON
// payload the LLM can parse, and reflect the action name and args.
// Per ADR-0010, action-side errors should be Results carrying a
// non-nil *failure.Failure, not error returns — the stub never
// errors.

func TestStubExecutor_ReturnsPlaceholderJSON(t *testing.T) {
	res, err := StubExecutor{}.Execute(context.Background(), "ship_update",
		map[string]any{"channel": "#engineering"})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if res.Failure != nil {
		t.Errorf("StubExecutor unexpectedly marked Result as failure: %v", res.Failure)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(res.Content), &payload); err != nil {
		t.Fatalf("decode result content: %v\n%s", err, res.Content)
	}
	if payload["action"] != "ship_update" {
		t.Errorf("action = %v, want ship_update", payload["action"])
	}
	if payload["stub"] != true {
		t.Errorf("stub = %v, want true", payload["stub"])
	}
	args, _ := payload["args"].(map[string]any)
	if args["channel"] != "#engineering" {
		t.Errorf("args.channel = %v, want #engineering", args["channel"])
	}
}

func TestStubExecutor_HandlesNilArgs(t *testing.T) {
	res, err := StubExecutor{}.Execute(context.Background(), "x", nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(res.Content), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	args, _ := payload["args"].(map[string]any)
	if args == nil {
		t.Error("args missing in stub payload")
	}
}
