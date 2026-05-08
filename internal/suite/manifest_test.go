package suite

import (
	"strings"
	"testing"
)

// TestParse_HappyPath verifies a well-formed manifest decodes to the
// expected struct shape. This is the contract the CLI's `-f` path
// reads against.
func TestParse_HappyPath(t *testing.T) {
	data := []byte(`
[suite]
name = "gmail-and-calendar"
description = "Read and draft Gmail; read and create calendar events"

[[actions]]
fqn = "github://owner/repo/actions/list-recent-emails@0.0.6"

[[actions]]
fqn = "github://owner/repo/actions/get-email@0.0.6"
`)
	m, err := Parse("test.toml", data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if m.Suite.Name != "gmail-and-calendar" {
		t.Errorf("Suite.Name = %q", m.Suite.Name)
	}
	if m.Suite.Description == "" {
		t.Error("Suite.Description should be set")
	}
	if len(m.Actions) != 2 {
		t.Fatalf("len(Actions) = %d, want 2", len(m.Actions))
	}
	if m.Actions[0].FQN != "github://owner/repo/actions/list-recent-emails@0.0.6" {
		t.Errorf("Actions[0].FQN = %q", m.Actions[0].FQN)
	}
}

// TestParse_RejectsUnknownTopLevelKey is the strict-mode contract:
// a typo at the top level fails fast rather than silently installing
// the wrong thing.
func TestParse_RejectsUnknownTopLevelKey(t *testing.T) {
	data := []byte(`
[suite]
name = "x"
description = "x"

[bogus]
key = "value"

[[actions]]
fqn = "github://owner/repo/actions/foo@1.0.0"
`)
	_, err := Parse("test.toml", data)
	if err == nil {
		t.Fatal("expected error for unknown top-level key")
	}
	if !strings.Contains(err.Error(), "unknown key") {
		t.Errorf("error should mention unknown key; got: %v", err)
	}
}

// TestParse_RejectsUnknownActionKey: per-action keys are equally
// closed. A user who types `name = "x"` (instead of editing the
// FQN) should see an error, not a silently-misnamed install.
func TestParse_RejectsUnknownActionKey(t *testing.T) {
	data := []byte(`
[suite]
name = "x"
description = "x"

[[actions]]
fqn = "github://owner/repo/actions/foo@1.0.0"
nickname = "foo-action"
`)
	_, err := Parse("test.toml", data)
	if err == nil {
		t.Fatal("expected error for unknown action key")
	}
	if !strings.Contains(err.Error(), "unknown key") {
		t.Errorf("error should mention unknown key; got: %v", err)
	}
}

// TestParse_RejectsUnknownSuiteKey: same closed-schema rule for the
// [suite] table itself.
func TestParse_RejectsUnknownSuiteKey(t *testing.T) {
	data := []byte(`
[suite]
name = "x"
description = "x"
maintainer = "alice"

[[actions]]
fqn = "github://owner/repo/actions/foo@1.0.0"
`)
	_, err := Parse("test.toml", data)
	if err == nil {
		t.Fatal("expected error for unknown suite key")
	}
	if !strings.Contains(err.Error(), "unknown key") {
		t.Errorf("error should mention unknown key; got: %v", err)
	}
}

// TestParse_RequiresSuiteName: missing required metadata fails.
func TestParse_RequiresSuiteName(t *testing.T) {
	data := []byte(`
[suite]
description = "x"

[[actions]]
fqn = "github://owner/repo/actions/foo@1.0.0"
`)
	_, err := Parse("test.toml", data)
	if err == nil {
		t.Fatal("expected error for missing suite.name")
	}
	if !strings.Contains(err.Error(), "[suite].name") {
		t.Errorf("error should mention [suite].name; got: %v", err)
	}
}

// TestParse_RequiresSuiteDescription: same for description.
func TestParse_RequiresSuiteDescription(t *testing.T) {
	data := []byte(`
[suite]
name = "x"

[[actions]]
fqn = "github://owner/repo/actions/foo@1.0.0"
`)
	_, err := Parse("test.toml", data)
	if err == nil {
		t.Fatal("expected error for missing suite.description")
	}
	if !strings.Contains(err.Error(), "[suite].description") {
		t.Errorf("error should mention [suite].description; got: %v", err)
	}
}

// TestParse_RejectsEmptyActions: a suite with zero actions has no
// semantic meaning; surface it loudly.
func TestParse_RejectsEmptyActions(t *testing.T) {
	data := []byte(`
[suite]
name = "x"
description = "x"
`)
	_, err := Parse("test.toml", data)
	if err == nil {
		t.Fatal("expected error for empty actions")
	}
	if !strings.Contains(err.Error(), "[[actions]]") {
		t.Errorf("error should mention [[actions]]; got: %v", err)
	}
}

// TestParse_RejectsActionWithoutVersion: floating versions break
// reproducibility — the same source could resolve to different
// bytes on different days. v1 requires `@<version>`.
func TestParse_RejectsActionWithoutVersion(t *testing.T) {
	data := []byte(`
[suite]
name = "x"
description = "x"

[[actions]]
fqn = "github://owner/repo/actions/foo"
`)
	_, err := Parse("test.toml", data)
	if err == nil {
		t.Fatal("expected error for action without @<version>")
	}
	if !strings.Contains(err.Error(), "@<version>") {
		t.Errorf("error should mention @<version>; got: %v", err)
	}
}

// TestParse_RejectsEmptyActionFQN: an action entry with an explicit
// empty fqn is a misconfigured manifest.
func TestParse_RejectsEmptyActionFQN(t *testing.T) {
	data := []byte(`
[suite]
name = "x"
description = "x"

[[actions]]
fqn = ""
`)
	_, err := Parse("test.toml", data)
	if err == nil {
		t.Fatal("expected error for empty action fqn")
	}
	if !strings.Contains(err.Error(), "fqn is required") {
		t.Errorf("error should mention fqn required; got: %v", err)
	}
}

// TestParse_RejectsMalformedTOML: surface TOML decode errors
// faithfully so users can locate the bad line.
func TestParse_RejectsMalformedTOML(t *testing.T) {
	data := []byte(`[suite
name = "x"`)
	_, err := Parse("test.toml", data)
	if err == nil {
		t.Fatal("expected error for malformed TOML")
	}
}

// TestParse_PreservesActionOrder: the install loop walks the list
// in declaration order. Verify the parser keeps that order.
func TestParse_PreservesActionOrder(t *testing.T) {
	data := []byte(`
[suite]
name = "x"
description = "x"

[[actions]]
fqn = "github://owner/repo/actions/aaa@1.0.0"

[[actions]]
fqn = "github://owner/repo/actions/bbb@1.0.0"

[[actions]]
fqn = "github://owner/repo/actions/ccc@1.0.0"
`)
	m, err := Parse("test.toml", data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := []string{"aaa", "bbb", "ccc"}
	for i, w := range want {
		if !strings.Contains(m.Actions[i].FQN, w) {
			t.Errorf("Actions[%d].FQN = %q, want substring %q", i, m.Actions[i].FQN, w)
		}
	}
}
