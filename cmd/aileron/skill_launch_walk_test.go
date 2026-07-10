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

// A plan with zero declared inputs is a clean silent no-op: the walk writes
// nothing to stdout (no banner, no prompts) and returns the caller's args
// byte-stable, so a future refactor that emits a spurious walk header is caught
// (#2063).
func TestWalk_ZeroInputsSilentNoOp(t *testing.T) {
	w, out := newWalker("") // no stdin may be read
	args := runtime.LaunchArgs{"already": "set"}
	got, err := w.Walk(nil, args)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if out.String() != "" {
		t.Errorf("a zero-input walk must write nothing to stdout, got %q", out.String())
	}
	if len(got) != 1 || got["already"] != "set" {
		t.Errorf("a zero-input walk must return the args unchanged, got %#v", got)
	}
}

// A no-default input renders [required]; a defaulted input renders
// [required · has default] with the default and an Enter-to-accept note. The
// value is always required either way; the default only lets the operator skip
// typing it, so both markers say [required].
func TestWalk_RequiredOptionalMarkers(t *testing.T) {
	w, out := newWalker("typed\n\n") // no-default: "typed", defaulted: Enter-accept
	inputs := []runtime.Input{
		litRequired("req", "a required one"),
		litDefault("opt", "a defaulted one", "d7"),
	}
	if _, err := w.Walk(inputs, nil); err != nil {
		t.Fatalf("Walk: %v", err)
	}
	s := out.String()
	if !strings.Contains(s, "[required]") {
		t.Errorf("no-default input must render [required]:\n%s", s)
	}
	if strings.Contains(s, "[optional]") {
		t.Errorf("no input may render the retired [optional] marker:\n%s", s)
	}
	if !strings.Contains(s, "[required · has default]") || !strings.Contains(s, "default: d7") || !strings.Contains(s, "Enter to accept") {
		t.Errorf("defaulted input must render [required · has default] + default + Enter note:\n%s", s)
	}
}

// Enter on an optional input records NO override for it, so its declared default
// resolves natively downstream (parity with --accept-defaults, #2063); a typed
// entry enters as a string override.
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
	if _, ok := got["window_days"]; ok {
		t.Errorf("an Enter-accepted default must not be written into the args (it resolves natively downstream), got %#v", got["window_days"])
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

// --- example + prompt: false fields (#2064) ---

// A declared example renders inline on the prompt line so an example never has
// to be jammed into the description.
func TestWalk_ExampleRenderedInPrompt(t *testing.T) {
	w, out := newWalker("\n") // accept the default
	in := litDefault("region", "the AWS region", "us-east-1")
	in.HasExample = true
	in.Example = "us-west-2"
	if _, err := w.Walk([]runtime.Input{in}, nil); err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if !strings.Contains(out.String(), "e.g. us-west-2") {
		t.Errorf("a declared example must render inline on the prompt line:\n%s", out.String())
	}
}

// A large example is capped through the same type+size summary the default
// uses, so a big example never floods the terminal.
func TestWalk_LargeExampleSummarized(t *testing.T) {
	big := strings.Repeat("y", maxDefaultInlineLen+1)
	w, out := newWalker("\n")
	in := litDefault("blob", "a big one", "small-default")
	in.HasExample = true
	in.Example = big
	if _, err := w.Walk([]runtime.Input{in}, nil); err != nil {
		t.Fatalf("Walk: %v", err)
	}
	s := out.String()
	if strings.Contains(s, big) {
		t.Errorf("a large example must not be rendered raw:\n%s", s)
	}
	if !strings.Contains(s, "e.g. <string,") {
		t.Errorf("a large example must render as a type+size summary:\n%s", s)
	}
}

// An input marked prompt: false is skipped by the walk: no prompt is emitted,
// no stdin line is consumed, and no value is written so the declared default
// applies silently downstream.
func TestWalk_NoPromptSkipsAndKeepsDefault(t *testing.T) {
	// The scripted stdin carries exactly one line for the promptable input. If
	// the skipped input wrongly consumed a line, the second prompt would read the
	// wrong value.
	w, out := newWalker("visible-value\n")
	skipped := litDefault("dashboard_template", "big inlined template", "BIG-DEFAULT")
	skipped.NoPrompt = true
	inputs := []runtime.Input{
		skipped,
		litRequired("account", "the account"),
	}
	got, err := w.Walk(inputs, nil)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if _, ok := got["dashboard_template"]; ok {
		t.Error("a prompt: false input must not be written into the walk args, so its declared default applies downstream")
	}
	if got["account"] != "visible-value" {
		t.Errorf("the skipped input must not consume the promptable input's stdin line, got account=%#v", got["account"])
	}
	s := out.String()
	if !strings.Contains(s, "advanced") {
		t.Errorf("a skipped input must render an informational advanced line:\n%s", s)
	}
	// The skip must be non-interactive: its default value is not solicited, so
	// the big default never appears as an Enter-to-accept prompt.
	if strings.Contains(s, "Enter to accept") && strings.Contains(s, "dashboard_template") {
		t.Errorf("a prompt: false input must not render an interactive default prompt:\n%s", s)
	}
}

// A prompt: false input supplied via --input is shown as already set and keeps
// the override: the skip check sits after the already-set check, so an explicit
// override still wins and stays --input-overridable.
func TestWalk_NoPromptStillOverridable(t *testing.T) {
	w, out := newWalker("") // no stdin: the only input is pre-set
	in := litDefault("dashboard_template", "big inlined template", "BIG-DEFAULT")
	in.NoPrompt = true
	got, err := w.Walk([]runtime.Input{in}, runtime.LaunchArgs{"dashboard_template": "OVERRIDE"})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if got["dashboard_template"] != "OVERRIDE" {
		t.Errorf("a prompt: false input supplied via --input must keep the override, got %#v", got["dashboard_template"])
	}
	s := out.String()
	if !strings.Contains(s, "already set") {
		t.Errorf("an overridden prompt: false input must render an already-set line:\n%s", s)
	}
	if strings.Contains(s, "advanced") {
		t.Errorf("an overridden input must not also render the advanced skip line:\n%s", s)
	}
}

// The literal prompt brackets the input name ([name]) and renders the
// declared description bare after a leading space, with no parentheses around
// it. The trailing "(default: X, Enter to accept)" note keeps its parenthetical
// form. This guards the interactive-launch prompt format specifically.
func TestWalk_LiteralPromptBracketsNameBareDescription(t *testing.T) {
	w, out := newWalker("\n") // accept the default
	inputs := []runtime.Input{litDefault("aws_region", "The AWS region to target", "us-east-1")}
	if _, err := w.Walk(inputs, nil); err != nil {
		t.Fatalf("Walk: %v", err)
	}
	s := out.String()
	if !strings.Contains(s, "[aws_region] The AWS region to target") {
		t.Errorf("prompt must bracket the name and render the description bare:\n%s", s)
	}
	if strings.Contains(s, "(The AWS region to target)") {
		t.Errorf("the description must not be wrapped in parentheses:\n%s", s)
	}
	if !strings.Contains(s, "(default: us-east-1, Enter to accept)") {
		t.Errorf("the default-note parenthetical must be preserved:\n%s", s)
	}
}
