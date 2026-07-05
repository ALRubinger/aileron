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
