package main

import (
	"bufio"
	"regexp"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/flightplan/runtime"
)

// newWalker builds a launchInputWalker over a scripted stdin and a capture
// buffer, returning both so a test drives the walk and inspects the rendered
// prompts.
func newWalker(input string) (launchInputWalker, *strings.Builder) {
	var out strings.Builder
	return launchInputWalker{stdin: bufio.NewReader(strings.NewReader(input)), stdout: &out}, &out
}

func litDefault(name, desc string, def any) runtime.Input {
	return runtime.Input{Name: name, Description: desc, Resolution: runtime.Resolution{Rule: runtime.ResolutionLiteral, HasDefault: true, Default: def}}
}

func litRequired(name, desc string) runtime.Input {
	return runtime.Input{Name: name, Description: desc, Resolution: runtime.Resolution{Rule: runtime.ResolutionLiteral}}
}

// The walk prompts literals in declaration order.
func TestWalk_DeclarationOrder(t *testing.T) {
	w, out := newWalker("\n\n\n") // accept all three defaults
	inputs := []runtime.Input{
		litDefault("alpha", "first", "a"),
		litDefault("bravo", "second", "b"),
		litDefault("charlie", "third", "c"),
	}
	if _, err := w.Walk(inputs, nil); err != nil {
		t.Fatalf("Walk: %v", err)
	}
	s := out.String()
	iA, iB, iC := strings.Index(s, "alpha"), strings.Index(s, "bravo"), strings.Index(s, "charlie")
	if !(iA >= 0 && iA < iB && iB < iC) {
		t.Errorf("prompts must appear in declaration order, got:\n%s", s)
	}
}

// A required input renders [required]; an optional (defaulted) input renders
// [optional] with the default and an Enter-to-accept note.
func TestWalk_RequiredOptionalMarkers(t *testing.T) {
	w, out := newWalker("typed\n\n") // required: "typed", optional: Enter-accept
	inputs := []runtime.Input{
		litRequired("req", "a required one"),
		litDefault("opt", "an optional one", "d7"),
	}
	if _, err := w.Walk(inputs, nil); err != nil {
		t.Fatalf("Walk: %v", err)
	}
	s := out.String()
	if !strings.Contains(s, "[required]") {
		t.Errorf("required input must render [required]:\n%s", s)
	}
	if !strings.Contains(s, "[optional]") || !strings.Contains(s, "default: d7") || !strings.Contains(s, "Enter to accept") {
		t.Errorf("optional input must render [optional] + default + Enter note:\n%s", s)
	}
}

// Enter on an optional input injects the declared default as its NATIVE typed
// value (a number stays a number); a typed entry enters as a string.
func TestWalk_EnterAcceptsTypedDefault(t *testing.T) {
	w, _ := newWalker("\ncustom\n") // accept default, then type a value
	inputs := []runtime.Input{
		litDefault("window_days", "days", 7), // native number default
		litRequired("account", "acct"),
	}
	got, err := w.Walk(inputs, nil)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if got["window_days"] != 7 {
		t.Errorf("Enter-accepted default must inject the native typed value 7, got %#v", got["window_days"])
	}
	if got["account"] != "custom" {
		t.Errorf("typed entry must enter as a string, got %#v", got["account"])
	}
}

// A literal already supplied via --input keeps its value, is shown as already
// set, and is never prompted.
func TestWalk_OverrideSkipsPrompt(t *testing.T) {
	w, out := newWalker("") // no stdin needed: the only input is pre-set
	inputs := []runtime.Input{litDefault("account", "acct", "default-acct")}
	got, err := w.Walk(inputs, runtime.LaunchArgs{"account": "given"})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if got["account"] != "given" {
		t.Errorf("override must be preserved, got %#v", got["account"])
	}
	if !strings.Contains(out.String(), "already set") {
		t.Errorf("an overridden input must render an already-set line:\n%s", out.String())
	}
}

// An entry violating a constraint re-prompts; a subsequent valid entry succeeds
// with no final-pass failure.
func TestWalk_InvalidReprompts(t *testing.T) {
	w, out := newWalker("dev\nprod\n") // "dev" is rejected, "prod" accepted
	inputs := []runtime.Input{{
		Name:       "env",
		Resolution: runtime.Resolution{Rule: runtime.ResolutionLiteral},
		Constraint: &runtime.Constraint{Enum: []string{"prod", "staging"}},
	}}
	got, err := w.Walk(inputs, nil)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if got["env"] != "prod" {
		t.Errorf("re-prompt must accept the valid entry, got %#v", got["env"])
	}
	// The rejected value's constraint message must have surfaced.
	if !strings.Contains(out.String(), "allowed values") {
		t.Errorf("an invalid entry must print the constraint failure:\n%s", out.String())
	}
}

// A drained reader terminates the walk with an error rather than spinning.
func TestWalk_EOFTerminates(t *testing.T) {
	w, _ := newWalker("") // required input, immediate EOF
	inputs := []runtime.Input{litRequired("req", "needs a value")}
	if _, err := w.Walk(inputs, nil); err == nil {
		t.Fatal("a drained reader on a required input must return an error, not spin")
	}
}

// Dynamic and source inputs are never prompted: the walk renders a read-only
// line and reads no stdin for them.
func TestWalk_DynamicAndSourceNeverPrompted(t *testing.T) {
	w, out := newWalker("") // no stdin: nothing may read
	inputs := []runtime.Input{
		{Name: "as_of", Resolution: runtime.Resolution{Rule: runtime.ResolutionDynamic, DynamicValue: "now"}},
		{Name: "live", Resolution: runtime.Resolution{Rule: runtime.ResolutionSource, SourceActionRef: "aileron:m.q"}},
	}
	got, err := w.Walk(inputs, nil)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if _, ok := got["as_of"]; ok {
		t.Error("a dynamic input must not be collected into the walk args")
	}
	s := out.String()
	if !strings.Contains(s, "as_of") || !strings.Contains(s, "resolved at launch") {
		t.Errorf("dynamic input must render a read-only line:\n%s", s)
	}
	if !strings.Contains(s, "resolved from source") {
		t.Errorf("source input must render a read-only line:\n%s", s)
	}
}

// A default whose string form is large renders as a type+size summary, not the
// raw value.
func TestWalk_LargeDefaultSummarized(t *testing.T) {
	big := strings.Repeat("x", maxDefaultInlineLen+1)
	w, out := newWalker("\n") // accept the large default
	inputs := []runtime.Input{litDefault("blob", "a big one", big)}
	if _, err := w.Walk(inputs, nil); err != nil {
		t.Fatalf("Walk: %v", err)
	}
	s := out.String()
	if strings.Contains(s, big) {
		t.Errorf("a large default must not be rendered raw:\n%s", s)
	}
	if !strings.Contains(s, "<string,") {
		t.Errorf("a large default must render as a type+size summary:\n%s", s)
	}
}

// Enter on a required-no-default literal routes to "a value is required" and
// re-prompts; an empty string can never be entered for such an input
// (documented intentional, #2063).
func TestWalk_EmptyRequiredReprompts(t *testing.T) {
	w, out := newWalker("\nfinally\n") // Enter (rejected), then a value
	inputs := []runtime.Input{litRequired("token", "a secret")}
	got, err := w.Walk(inputs, nil)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if got["token"] != "finally" {
		t.Errorf("re-prompt must accept the eventual value, got %#v", got["token"])
	}
	if !strings.Contains(out.String(), "a value is required") {
		t.Errorf("an empty entry on a required input must print the required message:\n%s", out.String())
	}
}

// A pattern-constrained input shows its hint in the prompt.
func TestWalk_ConstraintHintShown(t *testing.T) {
	w, out := newWalker("us-east-1\n")
	inputs := []runtime.Input{{
		Name:       "region",
		Resolution: runtime.Resolution{Rule: runtime.ResolutionLiteral},
		Constraint: &runtime.Constraint{Pattern: regexp.MustCompile("^us-[a-z]+-[0-9]$")},
	}}
	if _, err := w.Walk(inputs, nil); err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if !strings.Contains(out.String(), "^us-[a-z]+-[0-9]$") {
		t.Errorf("prompt must carry the pattern hint:\n%s", out.String())
	}
}
