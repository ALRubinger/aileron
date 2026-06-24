package spec

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"gopkg.in/yaml.v3"
)

// These tests guard the Flight Plan manifest spec artifacts under docs/ against
// drift. They validate the contract (the JSON Schema), not any implementation
// internals. The artifacts live outside this Go module, so go:embed cannot
// reach them; the paths are resolved from the repo root computed via
// runtime.Caller.

// repoRoot walks up from this source file to the repository root (the directory
// that contains go.work).
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

func schemaPath(t *testing.T) string {
	return filepath.Join(repoRoot(t), "docs", "schema", "flight-plan-manifest.schema.json")
}

func examplePath(t *testing.T) string {
	return filepath.Join(repoRoot(t), "docs", "schema", "flight-plan-manifest.example.skill.md")
}

// compileSchema compiles the committed Flight Plan manifest schema.
func compileSchema(t *testing.T) *jsonschema.Schema {
	t.Helper()
	f, err := os.Open(schemaPath(t))
	if err != nil {
		t.Fatalf("open schema: %v", err)
	}
	defer f.Close()

	doc, err := jsonschema.UnmarshalJSON(f)
	if err != nil {
		t.Fatalf("parse schema JSON: %v", err)
	}

	c := jsonschema.NewCompiler()
	const loc = "https://withaileron.ai/schema/flight-plan-manifest.schema.json"
	if err := c.AddResource(loc, doc); err != nil {
		t.Fatalf("add schema resource: %v", err)
	}
	sch, err := c.Compile(loc)
	if err != nil {
		t.Fatalf("compile schema: %v", err)
	}
	return sch
}

// frontmatter extracts the YAML frontmatter from a SKILL.md document and returns
// it decoded as a JSON-shaped value the validator can consume, plus the raw
// frontmatter map.
func frontmatter(t *testing.T, mdPath string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(mdPath)
	if err != nil {
		t.Fatalf("read %s: %v", mdPath, err)
	}
	front, _, ok := splitFrontmatter(raw)
	if !ok {
		t.Fatalf("no YAML frontmatter found in %s", mdPath)
	}

	var m map[string]any
	if err := yaml.Unmarshal(front, &m); err != nil {
		t.Fatalf("parse frontmatter YAML: %v", err)
	}
	return jsonifyMap(t, m)
}

// splitFrontmatter splits a Markdown document into its YAML frontmatter block
// and the remaining body. It expects the document to open with a `---` fence and
// closes on the first standalone `---` line, so a body line that merely starts
// with three dashes (a thematic break, for example) is not mistaken for the
// closing fence.
func splitFrontmatter(raw []byte) (front, body []byte, ok bool) {
	const fence = "---"
	text := strings.ReplaceAll(string(raw), "\r\n", "\n")
	lines := strings.Split(text, "\n")
	if len(lines) == 0 || lines[0] != fence {
		return nil, nil, false
	}
	for i := 1; i < len(lines); i++ {
		if lines[i] == fence {
			front = []byte(strings.Join(lines[1:i], "\n"))
			body = []byte(strings.Join(lines[i+1:], "\n"))
			return front, body, true
		}
	}
	return nil, nil, false
}

// jsonifyMap round-trips a YAML-decoded value through JSON so that the schema
// validator sees the same value model it expects (string keys, float64 numbers,
// []any and map[string]any containers).
func jsonifyMap(t *testing.T, m map[string]any) map[string]any {
	t.Helper()
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(m); err != nil {
		t.Fatalf("re-encode frontmatter to JSON: %v", err)
	}
	out, err := jsonschema.UnmarshalJSON(&buf)
	if err != nil {
		t.Fatalf("decode frontmatter as JSON: %v", err)
	}
	asMap, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("frontmatter did not decode to a JSON object, got %T", out)
	}
	return asMap
}

// TestWorkedExampleValidates is the happy path: the committed worked example
// validates against the committed schema. This is the drift guard.
func TestWorkedExampleValidates(t *testing.T) {
	sch := compileSchema(t)
	inst := frontmatter(t, examplePath(t))
	if err := sch.Validate(inst); err != nil {
		t.Fatalf("worked example must validate against the schema:\n%v", err)
	}
}

// TestStrippedManifestStillValid asserts the lossless-if-stripped guarantee at
// the schema level: removing the entire `aileron` block leaves a document that
// the schema does not reject. A host without Aileron reads a valid skill.
func TestStrippedManifestStillValid(t *testing.T) {
	sch := compileSchema(t)
	inst := frontmatter(t, examplePath(t))
	delete(inst, "aileron")
	if err := sch.Validate(inst); err != nil {
		t.Fatalf("a stripped manifest (no aileron block) must not be rejected by the schema:\n%v", err)
	}
}

// TestStrippedBodyRemainsCoherentSkill confirms the worked example's Markdown
// body remains a self-contained skill with the Aileron frontmatter block
// removed: it keeps a name and description and a non-empty body.
func TestStrippedBodyRemainsCoherentSkill(t *testing.T) {
	raw, err := os.ReadFile(examplePath(t))
	if err != nil {
		t.Fatalf("read example: %v", err)
	}
	front, body, ok := splitFrontmatter(raw)
	if !ok {
		t.Fatal("no frontmatter")
	}
	var m map[string]any
	if err := yaml.Unmarshal(front, &m); err != nil {
		t.Fatalf("parse frontmatter: %v", err)
	}
	if _, hasName := m["name"]; !hasName {
		t.Error("stripped skill must still carry a name")
	}
	if _, hasDesc := m["description"]; !hasDesc {
		t.Error("stripped skill must still carry a description")
	}
	if strings.TrimSpace(string(body)) == "" {
		t.Error("stripped skill must still carry a non-empty body")
	}
}

// mutate returns a deep-ish copy of the worked example's instance with a
// mutation applied, so each contract-failure case starts from a valid base.
func validExampleInstance(t *testing.T) map[string]any {
	return frontmatter(t, examplePath(t))
}

func aileronBlock(t *testing.T, inst map[string]any) map[string]any {
	t.Helper()
	blk, ok := inst["aileron"].(map[string]any)
	if !ok {
		t.Fatalf("aileron block missing or not an object, got %T", inst["aileron"])
	}
	return blk
}

// TestMissingInputsRejected: a manifest with no `inputs` block fails, because
// inputs are REQUIRED (the determinism correction in #1523).
func TestMissingInputsRejected(t *testing.T) {
	sch := compileSchema(t)
	inst := validExampleInstance(t)
	delete(aileronBlock(t, inst), "inputs")
	if err := sch.Validate(inst); err == nil {
		t.Fatal("a manifest missing the required inputs block must be rejected")
	}
}

// TestMissingOutputsRejected: a manifest with no `outputs` block fails, because
// outputs are REQUIRED (the output contract correction in #1519).
func TestMissingOutputsRejected(t *testing.T) {
	sch := compileSchema(t)
	inst := validExampleInstance(t)
	delete(aileronBlock(t, inst), "outputs")
	if err := sch.Validate(inst); err == nil {
		t.Fatal("a manifest missing the required outputs block must be rejected")
	}
}

// TestBadEffectRejected: an out-of-enum operation effect fails. The effect enum
// is closed and aligned to ADR-0003.
func TestBadEffectRejected(t *testing.T) {
	sch := compileSchema(t)
	inst := validExampleInstance(t)
	blk := aileronBlock(t, inst)
	requires := blk["requires"].(map[string]any)
	actions := requires["actions"].([]any)
	first := actions[0].(map[string]any)
	tc := first["trustContract"].(map[string]any)
	tc["effect"] = "mutate-everything"
	if err := sch.Validate(inst); err == nil {
		t.Fatal("an out-of-enum effect must be rejected")
	}
}

// TestBadEncodingRejected: an unknown output encoding fails. The encoding enum
// is closed to utf-8 and base64.
func TestBadEncodingRejected(t *testing.T) {
	sch := compileSchema(t)
	inst := validExampleInstance(t)
	blk := aileronBlock(t, inst)
	outputs := blk["outputs"].([]any)
	first := outputs[0].(map[string]any)
	first["encoding"] = "utf-16"
	if err := sch.Validate(inst); err == nil {
		t.Fatal("an unknown output encoding must be rejected")
	}
}

// TestReservedBase64EncodingAccepted: base64 is reserved in the interface, so
// the schema must accept it even though v1 implements utf-8 only. The
// text-only restriction is an implementation property, not the declared
// contract.
func TestReservedBase64EncodingAccepted(t *testing.T) {
	sch := compileSchema(t)
	inst := validExampleInstance(t)
	blk := aileronBlock(t, inst)
	outputs := blk["outputs"].([]any)
	first := outputs[0].(map[string]any)
	first["encoding"] = "base64"
	if err := sch.Validate(inst); err != nil {
		t.Fatalf("base64 encoding must be accepted as a reserved interface value:\n%v", err)
	}
}

// TestNoSecretValueField asserts the load-bearing invariant that no field in the
// credential contract can hold a secret value. The credential block declares
// kind and placement only. Adding an obvious secret-bearing key makes the
// closed object invalid.
func TestNoSecretValueField(t *testing.T) {
	sch := compileSchema(t)
	inst := validExampleInstance(t)
	blk := aileronBlock(t, inst)
	requires := blk["requires"].(map[string]any)
	actions := requires["actions"].([]any)
	first := actions[0].(map[string]any)
	tc := first["trustContract"].(map[string]any)
	cred := tc["credential"].(map[string]any)
	cred["value"] = "super-secret-token"
	if err := sch.Validate(inst); err == nil {
		t.Fatal("the credential block must reject any secret-bearing field; additionalProperties is closed")
	}
}

// TestExecutionEnvironmentRequiresExactlyOneRung: an execution environment that
// declares both rung1Image and rung2CapabilityUnits, or neither, fails. The
// prose states exactly one rung is declared (ADR-0027 execution rungs).
func TestExecutionEnvironmentRequiresExactlyOneRung(t *testing.T) {
	sch := compileSchema(t)

	bothRungs := func(env map[string]any) {
		env["rung1Image"] = map[string]any{"ref": "registry.example.com/runner:1.4"}
		env["rung2CapabilityUnits"] = map[string]any{"features": []any{"ghcr.io/example/feature:1"}}
	}
	emptyEnv := func(env map[string]any) {
		delete(env, "rung1Image")
		delete(env, "rung2CapabilityUnits")
	}

	for name, mutate := range map[string]func(map[string]any){
		"both rungs": bothRungs,
		"no rung":    emptyEnv,
	} {
		t.Run(name, func(t *testing.T) {
			inst := validExampleInstance(t)
			blk := aileronBlock(t, inst)
			requires := blk["requires"].(map[string]any)
			env := requires["executionEnvironment"].(map[string]any)
			mutate(env)
			if err := sch.Validate(inst); err == nil {
				t.Fatalf("an execution environment with %q must be rejected", name)
			}
		})
	}
}

// TestOAuthRequiredForOAuth2Credential: an oauth2 credential without its oauth
// block fails. The prose couples the two.
func TestOAuthRequiredForOAuth2Credential(t *testing.T) {
	sch := compileSchema(t)
	inst := validExampleInstance(t)
	blk := aileronBlock(t, inst)
	actions := blk["requires"].(map[string]any)["actions"].([]any)
	// The second action carries the oauth2 credential in the worked example.
	var found bool
	for _, a := range actions {
		tc := a.(map[string]any)["trustContract"].(map[string]any)
		cred := tc["credential"].(map[string]any)
		if cred["kind"] == "oauth2" {
			delete(tc, "oauth")
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected the worked example to carry an oauth2 credential")
	}
	if err := sch.Validate(inst); err == nil {
		t.Fatal("an oauth2 credential without an oauth block must be rejected")
	}
}

// TestFilePublishTargetRequiresPath: a file publish target without a path fails.
func TestFilePublishTargetRequiresPath(t *testing.T) {
	sch := compileSchema(t)
	inst := validExampleInstance(t)
	blk := aileronBlock(t, inst)
	outputs := blk["outputs"].([]any)
	first := outputs[0].(map[string]any)
	publish := first["publish"].(map[string]any)
	if publish["target"] != "file" {
		t.Fatal("expected the first output to publish to a file target")
	}
	delete(publish, "path")
	if err := sch.Validate(inst); err == nil {
		t.Fatal("a file publish target without a path must be rejected")
	}
}

// TestNoneCredentialNeedsNoPlacement: a credential of kind none validates without
// a placement, because an unauthenticated call has no wire placement.
func TestNoneCredentialNeedsNoPlacement(t *testing.T) {
	sch := compileSchema(t)
	inst := validExampleInstance(t)
	blk := aileronBlock(t, inst)
	actions := blk["requires"].(map[string]any)["actions"].([]any)
	tc := actions[0].(map[string]any)["trustContract"].(map[string]any)
	// Replace the first action's credential with a placement-free `none` kind.
	tc["credential"] = map[string]any{"kind": "none"}
	if err := sch.Validate(inst); err != nil {
		t.Fatalf("a credential of kind none must validate without a placement:\n%v", err)
	}
}
