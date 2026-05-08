package suite

import (
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/cstore"
)

// --- Parse ---

// TestParse_HappyPath verifies a well-formed v2 manifest decodes to
// the expected struct shape. This is the contract the CLI's
// `add-suite` paths read against.
func TestParse_HappyPath(t *testing.T) {
	data := []byte(`
name = "gmail-and-calendar"
description = "Read and draft Gmail; read and create calendar events"

actions = [
  "actions/list-recent-emails",
  "actions/get-email",
]
`)
	m, err := Parse("test.toml", data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if m.Name != "gmail-and-calendar" {
		t.Errorf("Name = %q", m.Name)
	}
	if m.Description == "" {
		t.Error("Description should be set")
	}
	if len(m.Actions) != 2 {
		t.Fatalf("len(Actions) = %d, want 2", len(m.Actions))
	}
	if m.Actions[0] != "actions/list-recent-emails" {
		t.Errorf("Actions[0] = %q", m.Actions[0])
	}
}

// TestParse_RejectsUnknownTopLevelKey is the strict-mode contract:
// a typo at the top level fails fast rather than silently installing
// the wrong thing.
func TestParse_RejectsUnknownTopLevelKey(t *testing.T) {
	data := []byte(`
name = "x"
description = "x"
maintainer = "alice"

actions = ["actions/foo"]
`)
	_, err := Parse("test.toml", data)
	if err == nil {
		t.Fatal("expected error for unknown top-level key")
	}
	if !strings.Contains(err.Error(), "unknown key") {
		t.Errorf("error should mention unknown key; got: %v", err)
	}
}

// TestParse_RejectsLegacySuiteTable: v1's `[suite]` block is no
// longer accepted. Surface it as an unknown key so authors who
// migrate get a clear "remove the table" signal.
func TestParse_RejectsLegacySuiteTable(t *testing.T) {
	data := []byte(`
[suite]
name = "x"
description = "x"

actions = ["actions/foo"]
`)
	_, err := Parse("test.toml", data)
	if err == nil {
		t.Fatal("expected error for legacy [suite] table")
	}
	if !strings.Contains(err.Error(), "unknown key") {
		t.Errorf("error should mention unknown key for [suite]; got: %v", err)
	}
}

// TestParse_RequiresName: missing required metadata fails.
func TestParse_RequiresName(t *testing.T) {
	data := []byte(`
description = "x"
actions = ["actions/foo"]
`)
	_, err := Parse("test.toml", data)
	if err == nil {
		t.Fatal("expected error for missing name")
	}
	if !strings.Contains(err.Error(), "name is required") {
		t.Errorf("error should mention name; got: %v", err)
	}
}

// TestParse_RequiresDescription: same for description.
func TestParse_RequiresDescription(t *testing.T) {
	data := []byte(`
name = "x"
actions = ["actions/foo"]
`)
	_, err := Parse("test.toml", data)
	if err == nil {
		t.Fatal("expected error for missing description")
	}
	if !strings.Contains(err.Error(), "description is required") {
		t.Errorf("error should mention description; got: %v", err)
	}
}

// TestParse_RejectsEmptyActions: a suite with zero actions has no
// semantic meaning; surface it loudly.
func TestParse_RejectsEmptyActions(t *testing.T) {
	data := []byte(`
name = "x"
description = "x"
actions = []
`)
	_, err := Parse("test.toml", data)
	if err == nil {
		t.Fatal("expected error for empty actions")
	}
	if !strings.Contains(err.Error(), "actions") {
		t.Errorf("error should mention actions; got: %v", err)
	}
}

// TestParse_RejectsEmptyEntry: a blank string in the array is a
// misconfigured manifest.
func TestParse_RejectsEmptyEntry(t *testing.T) {
	data := []byte(`
name = "x"
description = "x"
actions = ["actions/foo", "  "]
`)
	_, err := Parse("test.toml", data)
	if err == nil {
		t.Fatal("expected error for empty entry")
	}
	if !strings.Contains(err.Error(), "actions[1]") {
		t.Errorf("error should name actions[1]; got: %v", err)
	}
}

// TestParse_RejectsMalformedTOML: surface TOML decode errors
// faithfully so users can locate the bad line.
func TestParse_RejectsMalformedTOML(t *testing.T) {
	data := []byte(`name =
`)
	_, err := Parse("test.toml", data)
	if err == nil {
		t.Fatal("expected error for malformed TOML")
	}
}

// TestParse_PreservesActionOrder: the install loop walks the list
// in declaration order. Verify the parser keeps that order.
func TestParse_PreservesActionOrder(t *testing.T) {
	data := []byte(`
name = "x"
description = "x"

actions = [
  "actions/aaa",
  "actions/bbb",
  "actions/ccc",
]
`)
	m, err := Parse("test.toml", data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := []string{"actions/aaa", "actions/bbb", "actions/ccc"}
	for i, w := range want {
		if m.Actions[i] != w {
			t.Errorf("Actions[%d] = %q, want %q", i, m.Actions[i], w)
		}
	}
}

// --- Resolve ---

// TestResolve_PathFormInherits the suite's repo and version; the
// composed Ref points at <suiteFQN.Authority()>/<path>@<suiteVersion>.
// This is the headline ergonomic of v2.
func TestResolve_PathFormInheritsSuiteRefAndRepo(t *testing.T) {
	m := &Manifest{
		Name: "x", Description: "x",
		Actions: []string{"actions/foo", "actions/bar"},
	}
	suiteFQN := cstore.FQN{Scheme: "github", Owner: "acme", Repo: "conn"}
	refs, err := Resolve(m, suiteFQN, "0.0.6")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(refs) != 2 {
		t.Fatalf("len(refs) = %d, want 2", len(refs))
	}
	if got := refs[0].FQN.String(); got != "github://acme/conn/actions/foo" {
		t.Errorf("refs[0].FQN = %q", got)
	}
	if refs[0].Version != "0.0.6" {
		t.Errorf("refs[0].Version = %q, want 0.0.6", refs[0].Version)
	}
}

// TestResolve_FQNFormUnchanged: an FQN-form entry skips the
// inheritance machinery entirely; cstore.ParseRef does the work.
func TestResolve_FQNFormUnchanged(t *testing.T) {
	m := &Manifest{
		Name: "x", Description: "x",
		Actions: []string{"github://other/repo/actions/foo@1.2.3"},
	}
	refs, err := Resolve(m, cstore.FQN{Scheme: "github", Owner: "acme", Repo: "conn"}, "0.0.6")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got := refs[0].FQN.String(); got != "github://other/repo/actions/foo" {
		t.Errorf("refs[0].FQN = %q", got)
	}
	if refs[0].Version != "1.2.3" {
		t.Errorf("refs[0].Version = %q, want 1.2.3", refs[0].Version)
	}
}

// TestResolve_MixedEntries: path-form and FQN-form coexist in one
// manifest. Path form inherits; FQN form uses its own version.
func TestResolve_MixedEntries(t *testing.T) {
	m := &Manifest{
		Name: "x", Description: "x",
		Actions: []string{
			"actions/foo",
			"github://other/repo/actions/bar@1.0.0",
		},
	}
	suiteFQN := cstore.FQN{Scheme: "github", Owner: "acme", Repo: "conn"}
	refs, err := Resolve(m, suiteFQN, "0.0.6")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if refs[0].Version != "0.0.6" {
		t.Errorf("path-form should inherit 0.0.6; got %q", refs[0].Version)
	}
	if refs[1].Version != "1.0.0" {
		t.Errorf("FQN-form should use 1.0.0; got %q", refs[1].Version)
	}
}

// TestResolve_PathFormRequiresSuiteContext: a path-form entry in a
// local manifest (zero suiteFQN/version) is a hard error so the
// user sees the actual problem rather than installing under the
// wrong publisher.
func TestResolve_PathFormRequiresSuiteContext(t *testing.T) {
	m := &Manifest{
		Name: "x", Description: "x",
		Actions: []string{"actions/foo"},
	}
	_, err := Resolve(m, cstore.FQN{}, "")
	if err == nil {
		t.Fatal("expected error for path-form in local mode")
	}
	if !strings.Contains(err.Error(), "path-form") {
		t.Errorf("error should mention path-form; got: %v", err)
	}
}

// TestResolve_FQNFormWithoutVersionErrors: matches cstore.ParseRef's
// strict-SemVer rule. A v2 manifest can't omit @<version> on FQN-
// form entries.
func TestResolve_FQNFormWithoutVersionErrors(t *testing.T) {
	m := &Manifest{
		Name: "x", Description: "x",
		Actions: []string{"github://acme/conn/actions/foo"},
	}
	_, err := Resolve(m, cstore.FQN{Scheme: "github", Owner: "acme", Repo: "conn"}, "0.0.6")
	if err == nil {
		t.Fatal("expected error for FQN without @<version>")
	}
}

// TestResolve_PathFormStripsLeadingSlash: be forgiving of "/actions/foo"
// vs. "actions/foo"; both compose to the same result.
func TestResolve_PathFormStripsLeadingSlash(t *testing.T) {
	m := &Manifest{
		Name: "x", Description: "x",
		Actions: []string{"/actions/foo"},
	}
	suiteFQN := cstore.FQN{Scheme: "github", Owner: "acme", Repo: "conn"}
	refs, err := Resolve(m, suiteFQN, "0.0.6")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got := refs[0].FQN.Subpath; got != "actions/foo" {
		t.Errorf("subpath = %q, want actions/foo", got)
	}
}
