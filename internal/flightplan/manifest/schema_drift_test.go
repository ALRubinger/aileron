package manifest

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// repoRoot walks up from this source file to the repository root (the
// directory that contains go.work). The normative schema lives outside
// this Go module under docs/, so go:embed cannot reach it; the embedded
// copy must be kept byte-identical and this test is the guard.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed; cannot locate the repo root")
	}
	dir := filepath.Dir(file)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.work")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("could not find go.work walking up from %s", filepath.Dir(file))
		}
		dir = parent
	}
}

// TestEmbeddedSchemaMatchesDoc is the drift guard: the schema embedded in
// this package must be byte-identical to the normative copy at
// docs/schema/flight-plan-manifest.schema.json. If the doc schema changes
// (for example #1580 hoisting actionRef into $defs), this test fails until
// the embedded copy is refreshed.
func TestEmbeddedSchemaMatchesDoc(t *testing.T) {
	docPath := filepath.Join(repoRoot(t), "docs", "schema", "flight-plan-manifest.schema.json")
	want, err := os.ReadFile(docPath)
	if err != nil {
		t.Fatalf("read normative schema: %v", err)
	}
	if !bytes.Equal(want, EmbeddedSchema()) {
		t.Fatalf("embedded schema has drifted from %s; refresh the copy at "+
			"internal/flightplan/manifest/flight-plan-manifest.schema.json", docPath)
	}
}

// TestSchemaDeclaresExampleAndPrompt is the positive counterpart to the
// byte-identity guard: the schema-is-source-of-truth contract (#2064) requires
// the input definition to declare the optional example and prompt fields. It
// decodes the embedded schema and asserts both properties exist under
// $defs/input.properties, so a regression that drops one fails here even though
// the two copies would still be byte-identical.
func TestSchemaDeclaresExampleAndPrompt(t *testing.T) {
	var doc map[string]any
	if err := json.Unmarshal(EmbeddedSchema(), &doc); err != nil {
		t.Fatalf("embedded schema is not valid JSON: %v", err)
	}
	defs, ok := doc["$defs"].(map[string]any)
	if !ok {
		t.Fatal("schema has no $defs object")
	}
	input, ok := defs["input"].(map[string]any)
	if !ok {
		t.Fatal("$defs.input is not an object")
	}
	props, ok := input["properties"].(map[string]any)
	if !ok {
		t.Fatal("$defs.input.properties is not an object")
	}
	if _, ok := props["example"]; !ok {
		t.Error("$defs.input.properties must declare example (#2064)")
	}
	prompt, ok := props["prompt"].(map[string]any)
	if !ok {
		t.Fatal("$defs.input.properties must declare prompt as an object (#2064)")
	}
	if prompt["type"] != "boolean" {
		t.Errorf("prompt must be typed boolean, got %v", prompt["type"])
	}
	// Neither optional field is required: required-ness stays derived from
	// "no default", per the resolved decision.
	req, _ := input["required"].([]any)
	for _, r := range req {
		if r == "example" || r == "prompt" {
			t.Errorf("%v must not be in $defs.input.required (both fields are optional)", r)
		}
	}
}
