package manifest

import (
	"bytes"
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
