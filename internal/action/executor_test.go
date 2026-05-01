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

// --- interpolateArgs / mergeArgs unit tests ---
//
// Per ADR-0003, `[[execute]] inputs` values may reference call-time
// args via `${args.<name>}` interpolation. The executor's
// interpolateArgs walks the (string | []any | map[string]any) shape
// TOML decodes step inputs into and substitutes references against
// the call-time args map.

func TestInterpolateArgs_StringSubstitution(t *testing.T) {
	got := interpolateArgs("Hello, ${args.name}!", map[string]any{"name": "world"})
	if got != "Hello, world!" {
		t.Errorf("got %q, want %q", got, "Hello, world!")
	}
}

func TestInterpolateArgs_MultipleTokensInOneString(t *testing.T) {
	got := interpolateArgs("${args.greeting}, ${args.subject}!", map[string]any{
		"greeting": "Hello", "subject": "world",
	})
	if got != "Hello, world!" {
		t.Errorf("got %q", got)
	}
}

func TestInterpolateArgs_StringWithoutTokens(t *testing.T) {
	got := interpolateArgs("plain string", map[string]any{"x": "y"})
	if got != "plain string" {
		t.Errorf("got %q, want unchanged", got)
	}
}

func TestInterpolateArgs_UnknownArgLeavesLiteral(t *testing.T) {
	// Validation has already rejected references that don't resolve to
	// a declared input; if a missing arg slips through at runtime the
	// safe behavior is to leave the literal in place so downstream
	// surfaces (the connector's own validator, the LLM) can name the
	// problem rather than substituting an empty string silently.
	got := interpolateArgs("${args.missing}", map[string]any{"other": "value"})
	if got != "${args.missing}" {
		t.Errorf("got %q, want literal preserved", got)
	}
}

func TestInterpolateArgs_FormatsNonStringValues(t *testing.T) {
	cases := []struct {
		input string
		args  map[string]any
		want  string
	}{
		{"${args.n}", map[string]any{"n": 42}, "42"},
		{"${args.b}", map[string]any{"b": true}, "true"},
		{"${args.f}", map[string]any{"f": 3.14}, "3.14"},
	}
	for _, c := range cases {
		got := interpolateArgs(c.input, c.args)
		if got != c.want {
			t.Errorf("interpolateArgs(%q) = %v, want %q", c.input, got, c.want)
		}
	}
}

func TestInterpolateArgs_SliceWalksRecursively(t *testing.T) {
	args := map[string]any{"a": "alpha", "b": "beta"}
	got := interpolateArgs([]any{"${args.a}", "${args.b}", "static"}, args)
	want := []any{"alpha", "beta", "static"}
	gotSlice, ok := got.([]any)
	if !ok {
		t.Fatalf("got type %T, want []any", got)
	}
	if len(gotSlice) != len(want) {
		t.Fatalf("len = %d, want %d", len(gotSlice), len(want))
	}
	for i := range want {
		if gotSlice[i] != want[i] {
			t.Errorf("[%d] = %v, want %v", i, gotSlice[i], want[i])
		}
	}
}

func TestInterpolateArgs_MapWalksRecursively(t *testing.T) {
	args := map[string]any{"channel": "#eng", "user": "alr"}
	got := interpolateArgs(map[string]any{
		"channel": "${args.channel}",
		"text":    "Hi ${args.user}",
	}, args)
	gotMap, ok := got.(map[string]any)
	if !ok {
		t.Fatalf("got type %T, want map[string]any", got)
	}
	if gotMap["channel"] != "#eng" {
		t.Errorf("channel = %v", gotMap["channel"])
	}
	if gotMap["text"] != "Hi alr" {
		t.Errorf("text = %v", gotMap["text"])
	}
}

func TestInterpolateArgs_NestedStructures(t *testing.T) {
	// Nested map-in-slice-in-map — the shape an action might use to
	// build a Slack block payload.
	args := map[string]any{"channel": "#eng", "user": "alr"}
	got := interpolateArgs(map[string]any{
		"target": "${args.channel}",
		"blocks": []any{
			map[string]any{"type": "section", "text": "Hi ${args.user}"},
		},
	}, args)
	gotMap := got.(map[string]any)
	if gotMap["target"] != "#eng" {
		t.Errorf("target = %v", gotMap["target"])
	}
	blocks := gotMap["blocks"].([]any)
	block := blocks[0].(map[string]any)
	if block["text"] != "Hi alr" {
		t.Errorf("nested text = %v", block["text"])
	}
}

func TestInterpolateArgs_PassesThroughNonStandardTypes(t *testing.T) {
	// Numbers, bools, nil at leaf positions return unchanged. Tested
	// separately from the formatting path because here the input is
	// the leaf, not an interpolation token.
	cases := []any{42, true, 3.14, nil}
	for _, c := range cases {
		got := interpolateArgs(c, map[string]any{})
		if got != c {
			t.Errorf("interpolateArgs(%v) = %v, want unchanged", c, got)
		}
	}
}

func TestInterpolateArgs_EmptyArgsMapLeavesAllLiteral(t *testing.T) {
	got := interpolateArgs("${args.x} and ${args.y}", map[string]any{})
	if got != "${args.x} and ${args.y}" {
		t.Errorf("got %q, want all literal", got)
	}
}

func TestMergeArgs_CallTimeWinsOverStaticInputs(t *testing.T) {
	// Per the comment on mergeArgs: caller args (the LLM's choice) take
	// precedence over the action author's static defaults.
	out := mergeArgs(
		map[string]any{"channel": "#run-time"},
		map[string]any{"channel": "#default", "team": "eng"},
	)
	if out["channel"] != "#run-time" {
		t.Errorf("channel = %v, want #run-time", out["channel"])
	}
	if out["team"] != "eng" {
		t.Errorf("team default lost: %v", out["team"])
	}
}

func TestMergeArgs_InterpolatesStaticInputsAgainstCallTimeArgs(t *testing.T) {
	// Static input declares `text = "Hi ${args.user}"`; call-time arg
	// supplies `user = "alr"`. mergeArgs interpolates the static input
	// before merging, so the connector sees `text: "Hi alr"`.
	out := mergeArgs(
		map[string]any{"user": "alr"},
		map[string]any{"text": "Hi ${args.user}"},
	)
	if out["text"] != "Hi alr" {
		t.Errorf("text = %v, want 'Hi alr'", out["text"])
	}
	if out["user"] != "alr" {
		t.Errorf("user = %v", out["user"])
	}
}

func TestMergeArgs_EmptyInputs(t *testing.T) {
	if got := mergeArgs(nil, nil); len(got) != 0 {
		t.Errorf("empty merge = %v, want empty", got)
	}
	if got := mergeArgs(map[string]any{"x": 1}, nil); got["x"] != 1 {
		t.Errorf("args-only merge dropped %v", got)
	}
	if got := mergeArgs(nil, map[string]any{"x": 1}); got["x"] != 1 {
		t.Errorf("inputs-only merge dropped %v", got)
	}
}
