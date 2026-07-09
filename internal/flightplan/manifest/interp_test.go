package manifest

import (
	"strings"
	"testing"
)

// TestCommandInputRefs_SingleToken proves a lone token yields its input name.
func TestCommandInputRefs_SingleToken(t *testing.T) {
	refs, err := CommandInputRefs("{{ inputs.region }}")
	if err != nil {
		t.Fatalf("CommandInputRefs: %v", err)
	}
	if len(refs) != 1 || refs[0] != "region" {
		t.Errorf("refs = %v, want [region]", refs)
	}
}

// TestCommandInputRefs_MultipleAndAdjacent proves multiple and adjacent tokens
// are all extracted in order, and a literal-embedded token is recognized.
func TestCommandInputRefs_MultipleAndAdjacent(t *testing.T) {
	cases := map[string][]string{
		"--region={{inputs.region}} --env={{inputs.env}}": {"region", "env"},
		"{{inputs.a}}{{inputs.b}}":                         {"a", "b"},
		"prefix-{{ inputs.name }}-suffix":                  {"name"},
		"{{inputs.only}}":                                  {"only"},
	}
	for in, want := range cases {
		refs, err := CommandInputRefs(in)
		if err != nil {
			t.Fatalf("CommandInputRefs(%q): %v", in, err)
		}
		if strings.Join(refs, ",") != strings.Join(want, ",") {
			t.Errorf("CommandInputRefs(%q) = %v, want %v", in, refs, want)
		}
	}
}

// TestCommandInputRefs_InternalWhitespace proves surrounding whitespace inside
// the braces is tolerated: the token body is trimmed before matching.
func TestCommandInputRefs_InternalWhitespace(t *testing.T) {
	refs, err := CommandInputRefs("{{    inputs.spaced    }}")
	if err != nil {
		t.Fatalf("CommandInputRefs: %v", err)
	}
	if len(refs) != 1 || refs[0] != "spaced" {
		t.Errorf("refs = %v, want [spaced]", refs)
	}
}

// TestCommandInputRefs_NoTokens proves a token-free string is valid and yields
// no refs (a plain literal argv element is the common case).
func TestCommandInputRefs_NoTokens(t *testing.T) {
	for _, in := range []string{"extract-tool", "--mode=csv", "", "a}b{c"} {
		refs, err := CommandInputRefs(in)
		if err != nil {
			t.Fatalf("CommandInputRefs(%q): %v", in, err)
		}
		if len(refs) != 0 {
			t.Errorf("CommandInputRefs(%q) = %v, want no refs", in, refs)
		}
	}
}

// TestCommandInputRefs_MalformedBraces proves an unbalanced or stray brace
// shape is rejected rather than silently swallowed.
func TestCommandInputRefs_MalformedBraces(t *testing.T) {
	for _, in := range []string{
		"{{ inputs.x }",   // no closing }}
		"{ inputs.x }}",   // no opening {{
		"{{ inputs.x",     // unbalanced opener
		"inputs.x }}",     // stray closer
		"{{ inputs.a {{ inputs.b }}", // nested opener
	} {
		if _, err := CommandInputRefs(in); err == nil {
			t.Errorf("CommandInputRefs(%q) = nil error, want a malformed-brace error", in)
		}
	}
}

// TestCommandInputRefs_NonInputsBody proves a token whose body is not
// inputs.<name> (a steps.… reference or arbitrary text) is rejected: the
// embedded grammar admits ONLY input references.
func TestCommandInputRefs_NonInputsBody(t *testing.T) {
	for _, in := range []string{
		"{{ steps.a.b }}",
		"{{ hello }}",
		"{{ inputs. }}",
		"{{ }}",
		"{{ inputs.a.b }}",
	} {
		if _, err := CommandInputRefs(in); err == nil {
			t.Errorf("CommandInputRefs(%q) = nil error, want a bad-token error", in)
		}
	}
}

// TestPromptBindingRefs_SingleInput proves a lone inputs token yields one input
// ref.
func TestPromptBindingRefs_SingleInput(t *testing.T) {
	refs, err := PromptBindingRefs("{{ inputs.region }}")
	if err != nil {
		t.Fatalf("PromptBindingRefs: %v", err)
	}
	if len(refs) != 1 || refs[0].Kind != PromptRefInput || refs[0].Input != "region" {
		t.Errorf("refs = %+v, want one input ref for region", refs)
	}
}

// TestPromptBindingRefs_SingleStep proves a lone steps token yields one step ref
// carrying the correct step id and output.
func TestPromptBindingRefs_SingleStep(t *testing.T) {
	refs, err := PromptBindingRefs("{{ steps.query_metrics.series }}")
	if err != nil {
		t.Fatalf("PromptBindingRefs: %v", err)
	}
	if len(refs) != 1 || refs[0].Kind != PromptRefStep ||
		refs[0].StepID != "query_metrics" || refs[0].Output != "series" {
		t.Errorf("refs = %+v, want one step ref query_metrics.series", refs)
	}
}

// TestPromptBindingRefs_MixedAndPadded proves multiple, adjacent, and
// whitespace-padded mixed tokens are all extracted in order.
func TestPromptBindingRefs_MixedAndPadded(t *testing.T) {
	refs, err := PromptBindingRefs("Summarize {{ steps.render.csv }} for {{inputs.window}}{{ steps.a.b }}")
	if err != nil {
		t.Fatalf("PromptBindingRefs: %v", err)
	}
	if len(refs) != 3 {
		t.Fatalf("want 3 refs, got %d: %+v", len(refs), refs)
	}
	if refs[0].Kind != PromptRefStep || refs[0].StepID != "render" || refs[0].Output != "csv" {
		t.Errorf("refs[0] = %+v, want step render.csv", refs[0])
	}
	if refs[1].Kind != PromptRefInput || refs[1].Input != "window" {
		t.Errorf("refs[1] = %+v, want input window", refs[1])
	}
	if refs[2].Kind != PromptRefStep || refs[2].StepID != "a" || refs[2].Output != "b" {
		t.Errorf("refs[2] = %+v, want step a.b", refs[2])
	}
}

// TestPromptBindingRefs_NoTokens proves a token-free prompt is valid and yields
// no refs.
func TestPromptBindingRefs_NoTokens(t *testing.T) {
	for _, in := range []string{"Just a plain instruction.", "", "no braces here"} {
		refs, err := PromptBindingRefs(in)
		if err != nil {
			t.Fatalf("PromptBindingRefs(%q): %v", in, err)
		}
		if len(refs) != 0 {
			t.Errorf("PromptBindingRefs(%q) = %+v, want no refs", in, refs)
		}
	}
}

// TestPromptBindingRefs_MalformedBraces proves an unbalanced, stray, or nested
// brace shape is rejected with a malformed error.
func TestPromptBindingRefs_MalformedBraces(t *testing.T) {
	for _, in := range []string{
		"{{ steps.a.b }",             // no closing }}
		"{ inputs.x }}",              // no opening {{
		"{{ inputs.x",                // unbalanced opener
		"steps.a.b }}",              // stray closer
		"{{ steps.a {{ inputs.b }}", // nested opener
	} {
		if _, err := PromptBindingRefs(in); err == nil {
			t.Errorf("PromptBindingRefs(%q) = nil error, want a malformed-brace error", in)
		} else if !strings.Contains(err.Error(), "malformed") {
			t.Errorf("PromptBindingRefs(%q) error %q should mention malformed", in, err)
		}
	}
}

// TestPromptBindingRefs_BadBody proves a token body that is neither
// inputs.<name> nor steps.<id>.<output> is rejected, and the error names the
// bad body.
func TestPromptBindingRefs_BadBody(t *testing.T) {
	for _, in := range []string{
		"{{ outputs.x }}",   // not a binding namespace
		"{{ steps.x }}",     // steps missing output
		"{{ steps.a.b.c }}", // too many segments
		"{{ inputs. }}",     // empty input name
		"{{ hello }}",       // arbitrary text
		"{{ }}",             // empty body
	} {
		if _, err := PromptBindingRefs(in); err == nil {
			t.Errorf("PromptBindingRefs(%q) = nil error, want a bad-token error", in)
		}
	}
}

// TestCommandInputRefs_RejectsStepsBody is a regression pin proving the
// command/host grammar stays SEPARATE from the prompt grammar: a
// `{{ steps.a.b }}` body is a valid prompt token but must remain a malformed
// command token (command/host interpolate inputs only).
func TestCommandInputRefs_RejectsStepsBody(t *testing.T) {
	if _, err := CommandInputRefs("{{ steps.a.b }}"); err == nil {
		t.Fatal("CommandInputRefs must still reject a steps.<id>.<output> body")
	}
	// And the prompt grammar accepts the same token, confirming the two
	// grammars are genuinely independent.
	if _, err := PromptBindingRefs("{{ steps.a.b }}"); err != nil {
		t.Fatalf("PromptBindingRefs must accept a steps token: %v", err)
	}
}

// TestSubstituteInputs_HappyPath proves tokens are replaced by looked-up values
// and literal text is preserved verbatim.
func TestSubstituteInputs_HappyPath(t *testing.T) {
	lookup := func(name string) (string, bool) {
		return map[string]string{"region": "us-east-1", "env": "prod"}[name], true
	}
	got, err := SubstituteInputs("--region={{inputs.region}} --env={{ inputs.env }}", lookup)
	if err != nil {
		t.Fatalf("SubstituteInputs: %v", err)
	}
	if got != "--region=us-east-1 --env=prod" {
		t.Errorf("SubstituteInputs = %q, want the interpolated string", got)
	}
}

// TestSubstituteInputs_UnknownName proves a token whose name the lookup rejects
// is a hard error, never a silent empty substitution.
func TestSubstituteInputs_UnknownName(t *testing.T) {
	lookup := func(string) (string, bool) { return "", false }
	if _, err := SubstituteInputs("{{ inputs.missing }}", lookup); err == nil {
		t.Fatal("SubstituteInputs with an unresolvable name must error")
	}
}

// TestSubstituteInputs_Malformed proves the same grammar errors surface through
// the substitution entry point (it shares the scanner).
func TestSubstituteInputs_Malformed(t *testing.T) {
	lookup := func(string) (string, bool) { return "x", true }
	if _, err := SubstituteInputs("{{ inputs.x }", lookup); err == nil {
		t.Fatal("SubstituteInputs on a malformed token must error")
	}
}
