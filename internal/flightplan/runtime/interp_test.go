package runtime

import (
	"strings"
	"testing"
)

// TestInstantiateCommand_SingleToken proves an element with one token resolves
// to the input's string value and is marked derived.
func TestInstantiateCommand_SingleToken(t *testing.T) {
	resolved, derived, err := instantiateCommand(
		[]string{"query", "--region={{ inputs.region }}"},
		map[string]any{"region": "us-east-1"},
	)
	if err != nil {
		t.Fatalf("instantiateCommand: %v", err)
	}
	if strings.Join(resolved, " ") != "query --region=us-east-1" {
		t.Errorf("resolved = %v, want the interpolated argv", resolved)
	}
	if len(derived) != 1 || derived[0] != 1 {
		t.Errorf("derived = %v, want [1]", derived)
	}
}

// TestInstantiateCommand_AdjacentTokens proves adjacent tokens in one element
// both resolve and the element is marked derived once.
func TestInstantiateCommand_AdjacentTokens(t *testing.T) {
	resolved, derived, err := instantiateCommand(
		[]string{"{{inputs.a}}{{inputs.b}}"},
		map[string]any{"a": "x", "b": "y"},
	)
	if err != nil {
		t.Fatalf("instantiateCommand: %v", err)
	}
	if resolved[0] != "xy" {
		t.Errorf("resolved[0] = %q, want xy", resolved[0])
	}
	if len(derived) != 1 || derived[0] != 0 {
		t.Errorf("derived = %v, want [0]", derived)
	}
}

// TestInstantiateCommand_TokenFreePassthrough proves a token-free argv passes
// through verbatim with no derived indices (the regression that a plan with no
// interpolation execs exactly what it declared).
func TestInstantiateCommand_TokenFreePassthrough(t *testing.T) {
	resolved, derived, err := instantiateCommand(
		[]string{"extract-tool", "--mode", "csv"},
		map[string]any{"region": "us-east-1"},
	)
	if err != nil {
		t.Fatalf("instantiateCommand: %v", err)
	}
	if strings.Join(resolved, " ") != "extract-tool --mode csv" {
		t.Errorf("resolved = %v, want the argv verbatim", resolved)
	}
	if len(derived) != 0 {
		t.Errorf("derived = %v, want none", derived)
	}
}

// TestInstantiateCommand_NonStringValue proves a non-string resolved value is
// substituted via its %v string form, matching EnforceConstraint.
func TestInstantiateCommand_NonStringValue(t *testing.T) {
	resolved, _, err := instantiateCommand(
		[]string{"--limit={{inputs.n}}"},
		map[string]any{"n": 42},
	)
	if err != nil {
		t.Fatalf("instantiateCommand: %v", err)
	}
	if resolved[0] != "--limit=42" {
		t.Errorf("resolved[0] = %q, want --limit=42", resolved[0])
	}
}

// TestInstantiateCommand_MissingInput proves a token naming an input with no
// resolved value is a hard error, never a silent empty substitution.
func TestInstantiateCommand_MissingInput(t *testing.T) {
	_, _, err := instantiateCommand(
		[]string{"--region={{inputs.region}}"},
		map[string]any{},
	)
	if err == nil {
		t.Fatal("instantiateCommand with a missing resolved input must error")
	}
}

// TestInstantiateCommand_DerivedIndicesOrdered proves the derived-index set
// names exactly the token-bearing elements, in argv order.
func TestInstantiateCommand_DerivedIndicesOrdered(t *testing.T) {
	_, derived, err := instantiateCommand(
		[]string{"tool", "{{inputs.a}}", "lit", "{{inputs.b}}"},
		map[string]any{"a": "1", "b": "2"},
	)
	if err != nil {
		t.Fatalf("instantiateCommand: %v", err)
	}
	if len(derived) != 2 || derived[0] != 1 || derived[1] != 3 {
		t.Errorf("derived = %v, want [1 3]", derived)
	}
}
