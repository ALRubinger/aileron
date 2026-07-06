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
// description, and constraint hint so the operator knows what to type.
func TestLinePrompter_PromptInput(t *testing.T) {
	var out bytes.Buffer
	p := linePrompter{stdin: strings.NewReader("us-east-1\n"), stdout: &out}
	in := runtime.Input{
		Name:        "region",
		Description: "target AWS region",
		Constraint:  &runtime.Constraint{Pattern: regexp.MustCompile("^us-[a-z]+-[0-9]$")},
	}
	got, err := p.PromptInput(in)
	if err != nil {
		t.Fatalf("PromptInput: %v", err)
	}
	if got != "us-east-1" {
		t.Errorf("prompted value = %q, want %q", got, "us-east-1")
	}
	prompt := out.String()
	for _, want := range []string{"region", "target AWS region", "^us-[a-z]+-[0-9]$"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt %q missing %q", prompt, want)
		}
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
		{"pattern", &runtime.Constraint{Pattern: regexp.MustCompile("^x$")}, "[matching ^x$]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := inputConstraintHint(tc.c); got != tc.want {
				t.Errorf("inputConstraintHint = %q, want %q", got, tc.want)
			}
		})
	}
}
