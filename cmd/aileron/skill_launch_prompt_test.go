package main

import (
	"bytes"
	"regexp"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/flightplan/runtime"
)

// newLaunchInputPrompter wires an interactive prompter only when stdin is a
// TTY; a non-interactive launch (piped stdin, CI) gets nil so the runtime keeps
// its fail-fast error for a missing required input.
func TestNewLaunchInputPrompter_TTYGate(t *testing.T) {
	origTTY := isTTYFn
	t.Cleanup(func() { isTTYFn = origTTY })

	isTTYFn = func() bool { return false }
	if p := newLaunchInputPrompter(&bytes.Buffer{}); p != nil {
		t.Errorf("non-TTY launch must get a nil prompter (fail-fast), got %T", p)
	}

	isTTYFn = func() bool { return true }
	if p := newLaunchInputPrompter(&bytes.Buffer{}); p == nil {
		t.Error("interactive launch must get a non-nil prompter")
	}
}

// linePrompter reads one line for the missing input and echoes the name,
// description, and the human-readable enum hint so the operator knows what to
// type.
func TestLinePrompter_PromptInput(t *testing.T) {
	var out bytes.Buffer
	p := linePrompter{stdin: strings.NewReader("us-east-1\n"), stdout: &out}
	in := runtime.Input{
		Name:        "region",
		Description: "target AWS region",
		Constraint:  &runtime.Constraint{Enum: []string{"us-east-1", "us-west-2"}},
	}
	got, err := p.PromptInput(in)
	if err != nil {
		t.Fatalf("PromptInput: %v", err)
	}
	if got != "us-east-1" {
		t.Errorf("prompted value = %q, want %q", got, "us-east-1")
	}
	prompt := out.String()
	for _, want := range []string{"region", "target AWS region", "[one of: us-east-1, us-west-2]"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt %q missing %q", prompt, want)
		}
	}
}

// A pattern-constrained input must NOT echo the raw regex into the prompt: it is
// meaningless noise to an operator (#2130). The pattern is still enforced on
// submit by the runtime validator; the prompt just no longer prints it.
func TestLinePrompter_PatternHidden(t *testing.T) {
	var out bytes.Buffer
	p := linePrompter{stdin: strings.NewReader("us-east-1\n"), stdout: &out}
	in := runtime.Input{
		Name:        "region",
		Description: "target AWS region",
		Constraint:  &runtime.Constraint{Pattern: regexp.MustCompile("^us-[a-z]+-[0-9]$")},
	}
	if _, err := p.PromptInput(in); err != nil {
		t.Fatalf("PromptInput: %v", err)
	}
	prompt := out.String()
	if strings.Contains(prompt, "^us-[a-z]+-[0-9]$") || strings.Contains(prompt, "[matching") {
		t.Errorf("prompt %q must not surface the raw pattern", prompt)
	}
}

// A drained stdin makes PromptInput propagate a read error rather than silently
// returning an empty string: the shared promptLine collapses EOF to "", but the
// interactive prompter uses the EOF-aware readPromptLine so a closed or drained
// terminal fails the launch instead of resolving a required input to "" (#2063).
func TestLinePrompter_EOFPropagatesError(t *testing.T) {
	var out bytes.Buffer
	p := linePrompter{stdin: strings.NewReader(""), stdout: &out} // immediate EOF
	in := runtime.Input{Name: "req", Description: "a required value"}
	if _, err := p.PromptInput(in); err == nil {
		t.Fatal("a drained stdin must propagate a read error, not a silent empty string")
	}
}

func TestInputConstraintHint(t *testing.T) {
	cases := []struct {
		name string
		c    *runtime.Constraint
		want string
	}{
		{"nil", nil, ""},
		{"enum", &runtime.Constraint{Enum: []string{"prod", "staging"}}, "[one of: prod, staging]"},
		// A pattern-only constraint renders no hint: the raw regex is meaningless
		// noise to an operator (#2130). The pattern is still enforced on submit;
		// this only suppresses its display.
		{"pattern-suppressed", &runtime.Constraint{Pattern: regexp.MustCompile("^x$")}, ""},
		// Enum takes precedence and is still shown even when a pattern coexists.
		{"enum-wins-over-pattern", &runtime.Constraint{Enum: []string{"a", "b"}, Pattern: regexp.MustCompile("^x$")}, "[one of: a, b]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := inputConstraintHint(tc.c); got != tc.want {
				t.Errorf("inputConstraintHint = %q, want %q", got, tc.want)
			}
		})
	}
}
