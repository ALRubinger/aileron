package spec

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/flightplan/manifest"
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

// TestWorkedExampleDeclaresEnvironmentTools sharpens the drift guard on the
// environment block: the worked example must declare `aileron.environment`
// with at least one curated tool ref, and must not carry the retired
// `requires.executionEnvironment` key.
func TestWorkedExampleDeclaresEnvironmentTools(t *testing.T) {
	inst := frontmatter(t, examplePath(t))
	blk := inst["aileron"].(map[string]any)
	env, ok := blk["environment"].(map[string]any)
	if !ok {
		t.Fatalf("the worked example must declare aileron.environment, got %T", blk["environment"])
	}
	tools, ok := env["tools"].([]any)
	if !ok || len(tools) == 0 {
		t.Fatalf("the worked example must declare environment.tools, got %v", env["tools"])
	}
	requires := blk["requires"].(map[string]any)
	if _, lingering := requires["executionEnvironment"]; lingering {
		t.Error("the worked example must not carry the retired requires.executionEnvironment key")
	}
}

// toolNamePattern captures the closed tool-name alternation embedded in the
// schema's environment.tools item pattern (`^(name1|name2|...)@...`).
var toolNamePattern = regexp.MustCompile(`^\^\(([a-z0-9|-]+)\)@`)

// TestEnvironmentToolVocabularyMatchesCatalog is the tool-vocabulary drift
// guard: every tool name the schema's `environment.tools` pattern admits must
// exist in the curated catalog at images/sandbox-features/<name>/. This is a
// subset check by design; catalog entries the schema does not (yet) list,
// such as agent features, are fine.
func TestEnvironmentToolVocabularyMatchesCatalog(t *testing.T) {
	raw, err := os.ReadFile(schemaPath(t))
	if err != nil {
		t.Fatalf("read schema: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse schema JSON: %v", err)
	}
	defs := doc["$defs"].(map[string]any)
	env := defs["environment"].(map[string]any)
	tools := env["properties"].(map[string]any)["tools"].(map[string]any)
	pattern, ok := tools["items"].(map[string]any)["pattern"].(string)
	if !ok {
		t.Fatal("environment.tools.items.pattern missing from the schema")
	}
	m := toolNamePattern.FindStringSubmatch(pattern)
	if m == nil {
		t.Fatalf("tools item pattern %q does not open with a closed name alternation", pattern)
	}
	names := strings.Split(m[1], "|")
	if len(names) == 0 {
		t.Fatal("the tools pattern admits no names")
	}
	for _, name := range names {
		feature := filepath.Join(repoRoot(t), "images", "sandbox-features", name, "devcontainer-feature.json")
		if _, err := os.Stat(feature); err != nil {
			t.Errorf("schema admits tool %q but the catalog has no %s: %v", name, feature, err)
		}
	}
}

// agentFeatures are the sandbox-feature directories that provision an agent
// runtime rather than an invocable tool. They are intentionally absent from the
// schema's environment.tools vocabulary because a Flight Plan declares the tools
// its steps invoke, not the agent that runs it. The reverse drift guard below
// excludes them.
var agentFeatures = map[string]struct{}{
	"claude": {},
	"codex":  {},
}

// TestCatalogToolVocabularyMatchesSchema is the reverse-direction tool-vocabulary
// drift guard, the complement of TestEnvironmentToolVocabularyMatchesCatalog:
// every non-agent catalog entry at images/sandbox-features/<name>/ must appear in
// the schema's environment.tools name alternation. Without this direction, adding
// a feature directory to the catalog leaves it silently unusable in a Flight Plan
// until someone also edits the schema pattern (and its doc copy); this test makes
// that omission fail loudly at the moment the catalog grows.
func TestCatalogToolVocabularyMatchesSchema(t *testing.T) {
	// Collect the tool names the schema admits.
	raw, err := os.ReadFile(schemaPath(t))
	if err != nil {
		t.Fatalf("read schema: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse schema JSON: %v", err)
	}
	defs := doc["$defs"].(map[string]any)
	env := defs["environment"].(map[string]any)
	tools := env["properties"].(map[string]any)["tools"].(map[string]any)
	pattern, ok := tools["items"].(map[string]any)["pattern"].(string)
	if !ok {
		t.Fatal("environment.tools.items.pattern missing from the schema")
	}
	m := toolNamePattern.FindStringSubmatch(pattern)
	if m == nil {
		t.Fatalf("tools item pattern %q does not open with a closed name alternation", pattern)
	}
	admitted := make(map[string]struct{})
	for _, name := range strings.Split(m[1], "|") {
		admitted[name] = struct{}{}
	}

	// Walk the curated catalog and require every non-agent feature to be admitted.
	catalogDir := filepath.Join(repoRoot(t), "images", "sandbox-features")
	entries, err := os.ReadDir(catalogDir)
	if err != nil {
		t.Fatalf("read catalog dir %s: %v", catalogDir, err)
	}
	var checked int
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		// Only count directories that are actually curated features.
		if _, err := os.Stat(filepath.Join(catalogDir, name, "devcontainer-feature.json")); err != nil {
			continue
		}
		if _, isAgent := agentFeatures[name]; isAgent {
			continue
		}
		checked++
		if _, ok := admitted[name]; !ok {
			t.Errorf("catalog feature %q is not admitted by the schema environment.tools pattern %q; add it to the schema (and its doc copy) or mark it an agent feature", name, pattern)
		}
	}
	if checked == 0 {
		t.Fatal("no non-agent catalog features were checked; the reverse drift guard is inert")
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

// TestEnvironmentUnknownToolRejected: an environment.tools entry outside the
// curated catalog vocabulary fails. Unknown-tool rejection is encoded in the
// schema itself, so it fires at validation, before freeze.
func TestEnvironmentUnknownToolRejected(t *testing.T) {
	sch := compileSchema(t)
	inst := validExampleInstance(t)
	blk := aileronBlock(t, inst)
	env := blk["environment"].(map[string]any)
	env["tools"] = []any{"nmap@7.1"}
	if err := sch.Validate(inst); err == nil {
		t.Fatal("an unknown tool name must be rejected by the closed tools vocabulary")
	}
}

// TestEnvironmentEmptyRejected: a present-but-empty environment block fails.
// Omitting the key, not an empty block, is how a skill says it has no
// environment needs.
func TestEnvironmentEmptyRejected(t *testing.T) {
	sch := compileSchema(t)
	inst := validExampleInstance(t)
	blk := aileronBlock(t, inst)
	blk["environment"] = map[string]any{}
	if err := sch.Validate(inst); err == nil {
		t.Fatal("an empty environment block must be rejected")
	}
}

// TestRetiredExecutionEnvironmentRejected: the retired three-rung
// requires.executionEnvironment key fails validation. The environment block
// replaced it with no compatibility window.
func TestRetiredExecutionEnvironmentRejected(t *testing.T) {
	sch := compileSchema(t)
	inst := validExampleInstance(t)
	blk := aileronBlock(t, inst)
	requires := blk["requires"].(map[string]any)
	requires["executionEnvironment"] = map[string]any{
		"rung2CapabilityUnits": map[string]any{"features": []any{"ghcr.io/example/feature:1"}},
	}
	if err := sch.Validate(inst); err == nil {
		t.Fatal("the retired requires.executionEnvironment key must be rejected")
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

// steps returns the worked example's step graph as a slice of step maps.
func steps(t *testing.T, inst map[string]any) []map[string]any {
	t.Helper()
	blk := aileronBlock(t, inst)
	raw, ok := blk["steps"].([]any)
	if !ok {
		t.Fatalf("steps block missing or not an array, got %T", blk["steps"])
	}
	out := make([]map[string]any, 0, len(raw))
	for i, s := range raw {
		m, ok := s.(map[string]any)
		if !ok {
			t.Fatalf("step %d is not an object, got %T", i, s)
		}
		out = append(out, m)
	}
	return out
}

// TestStepGraphExampleValidates asserts the worked example carries a non-empty
// step graph and that it validates against the schema. TestWorkedExampleValidates
// already validates the whole document; this sharpens the guard so the example
// cannot quietly drop its composition and still pass.
func TestStepGraphExampleValidates(t *testing.T) {
	sch := compileSchema(t)
	inst := validExampleInstance(t)
	if got := len(steps(t, inst)); got == 0 {
		t.Fatal("the worked example must carry a non-empty steps graph")
	}
	if err := sch.Validate(inst); err != nil {
		t.Fatalf("the worked example with its step graph must validate:\n%v", err)
	}
}

// TestUnknownStepKindRejected: a step with an out-of-enum kind fails. The kind
// enum is closed to action-call, transform, and llm-seam.
func TestUnknownStepKindRejected(t *testing.T) {
	sch := compileSchema(t)
	inst := validExampleInstance(t)
	steps(t, inst)[0]["kind"] = "frobnicate"
	if err := sch.Validate(inst); err == nil {
		t.Fatal("a step with an unknown kind must be rejected; the kind enum is closed")
	}
}

// TestStepSecretFieldRejected: adding a secret-bearing key to a step makes the
// closed step object invalid, mirroring TestNoSecretValueField for the
// credential block. No step field may carry a secret.
func TestStepSecretFieldRejected(t *testing.T) {
	sch := compileSchema(t)
	inst := validExampleInstance(t)
	steps(t, inst)[0]["value"] = "super-secret-token"
	if err := sch.Validate(inst); err == nil {
		t.Fatal("a step must reject any extra field; additionalProperties is closed on every step kind")
	}
}

// TestStepLiteralBindingRejected: a binding that carries a literal value rather
// than a reference is rejected. The binding grammar is closed to inputs.<name>
// and steps.<id>.<output>, so a literal secret cannot be embedded in the wiring.
func TestStepLiteralBindingRejected(t *testing.T) {
	sch := compileSchema(t)
	inst := validExampleInstance(t)
	mutated := false
	for _, s := range steps(t, inst) {
		if s["kind"] == "action-call" {
			if args, ok := s["args"].(map[string]any); ok {
				for k := range args {
					args[k] = "super-secret-token"
					mutated = true
					break
				}
				break
			}
		}
	}
	if !mutated {
		t.Fatal("expected an action-call step with at least one arg to mutate in the worked example")
	}
	if err := sch.Validate(inst); err == nil {
		t.Fatal("a step arg that is a literal value rather than a binding reference must be rejected")
	}
}

// TestLLMSeamMarkAccepted: an llm-seam step validates. The marked seam is a
// first-class kind, not an error.
func TestLLMSeamMarkAccepted(t *testing.T) {
	sch := compileSchema(t)
	inst := validExampleInstance(t)
	var found bool
	for _, s := range steps(t, inst) {
		if s["kind"] == "llm-seam" {
			found = true
		}
	}
	if !found {
		t.Fatal("expected the worked example to carry an llm-seam step")
	}
	if err := sch.Validate(inst); err != nil {
		t.Fatalf("an llm-seam step must validate as the marked seam:\n%v", err)
	}
}

// seamStep returns the single llm-seam step from the worked example, failing
// the test if none is present. The #2099 prompt/model tests mutate it.
func seamStep(t *testing.T, inst map[string]any) map[string]any {
	t.Helper()
	for _, s := range steps(t, inst) {
		if s["kind"] == "llm-seam" {
			return s
		}
	}
	t.Fatal("expected the worked example to carry an llm-seam step")
	return nil
}

// TestLLMSeamPromptAndModelAccepted: the seam accepts a `prompt` template and a
// `model` target (#2099). Both are first-class fields on kind: llm-seam. The
// worked example already declares them; this test also proves a mutated seam
// carrying fresh values still validates.
func TestLLMSeamPromptAndModelAccepted(t *testing.T) {
	sch := compileSchema(t)
	inst := validExampleInstance(t)
	seam := seamStep(t, inst)
	seam["prompt"] = "Summarize {{ steps.render_csv.csv }} for {{ inputs.window_days }} days."
	seam["model"] = "anthropic:claude-haiku-4-5"
	if err := sch.Validate(inst); err != nil {
		t.Fatalf("an llm-seam carrying prompt and model must validate:\n%v", err)
	}
}

// TestSeamPromptEmptyRejected: an empty `prompt` on the seam is rejected. The
// schema requires minLength:1, so a present-but-empty template is invalid.
func TestSeamPromptEmptyRejected(t *testing.T) {
	sch := compileSchema(t)
	inst := validExampleInstance(t)
	seam := seamStep(t, inst)
	seam["prompt"] = ""
	if err := sch.Validate(inst); err == nil {
		t.Fatal("an empty prompt on the seam must be rejected (minLength:1)")
	}
}

// TestPromptOnActionCallRejected: `prompt` on an action-call step is rejected.
// The prompt field is permitted only on kind: llm-seam; every other kind is a
// closed object that forbids it.
func TestPromptOnActionCallRejected(t *testing.T) {
	sch := compileSchema(t)
	inst := validExampleInstance(t)
	mutated := false
	for _, s := range steps(t, inst) {
		if s["kind"] == "action-call" {
			s["prompt"] = "reach an LLM here"
			mutated = true
			break
		}
	}
	if !mutated {
		t.Fatal("expected an action-call step in the worked example to mutate")
	}
	if err := sch.Validate(inst); err == nil {
		t.Fatal("prompt on an action-call step must be rejected; it is llm-seam only")
	}
}

// TestModelOnTransformRejected: `model` on a transform step is rejected. The
// model field is permitted only on kind: llm-seam.
func TestModelOnTransformRejected(t *testing.T) {
	sch := compileSchema(t)
	inst := validExampleInstance(t)
	mutated := false
	for _, s := range steps(t, inst) {
		if s["kind"] == "transform" {
			s["model"] = "anthropic:claude-haiku-4-5"
			mutated = true
			break
		}
	}
	if !mutated {
		t.Fatal("expected a transform step in the worked example to mutate")
	}
	if err := sch.Validate(inst); err == nil {
		t.Fatal("model on a transform step must be rejected; it is llm-seam only")
	}
}

// TestLLMSeamIsOnlySeamKind: llm-seam is the ONLY kind that carries the seam
// mark. The no-LLM-outside-a-seam guarantee is structural: every non-seam step
// is a deterministic kind, so a language model is reachable only at an
// explicitly marked seam. A plan may declare one or MORE seams (#2100 overturned
// the single-seam constraint of ADR-0027); this test asserts at least one seam
// exists and that no other kind is a seam, but it no longer caps the count.
func TestLLMSeamIsOnlySeamKind(t *testing.T) {
	inst := validExampleInstance(t)
	seams := 0
	for _, s := range steps(t, inst) {
		switch s["kind"] {
		case "llm-seam":
			seams++
		case "action-call", "transform", "tool":
			// deterministic kinds
		default:
			t.Fatalf("unexpected step kind %v", s["kind"])
		}
	}
	if seams < 1 {
		t.Fatalf("the worked example must carry at least one llm-seam, got %d", seams)
	}
}

// TestMultipleSeamsValidate proves the schema permits MORE than one llm-seam
// step (#2100). The single-seam ceiling was only ever asserted in tests, docs,
// and the freeze lint's prose; the schema itself never capped the count. A
// second seam that mirrors the example's seam shape and binds an earlier step's
// output validates, so the runtime's multi-seam suspend/resume support has a
// schema-valid shape to run.
func TestMultipleSeamsValidate(t *testing.T) {
	sch := compileSchema(t)
	inst := validExampleInstance(t)
	blk := aileronBlock(t, inst)
	raw := blk["steps"].([]any)
	// Clone the existing seam and re-key it as a second, distinct seam that binds
	// the first seam's output, so both are structurally valid and dependency-
	// ordered.
	first := seamStep(t, inst)
	firstID, _ := first["id"].(string)
	second := map[string]any{
		"id":       firstID + "_2",
		"kind":     "llm-seam",
		"bindings": map[string]any{"prior": "steps." + firstID + "." + firstSeamOutput(t, first)},
		"outputs":  []any{"refined"},
	}
	blk["steps"] = append(raw, second)
	if err := sch.Validate(inst); err != nil {
		t.Fatalf("a plan with two llm-seam steps must validate (multi-seam, #2100):\n%v", err)
	}
	// And it carries two seams now.
	seams := 0
	for _, s := range steps(t, inst) {
		if s["kind"] == "llm-seam" {
			seams++
		}
	}
	if seams != 2 {
		t.Fatalf("expected two seams after adding one, got %d", seams)
	}
}

// firstSeamOutput returns a seam step's first declared output name, for building
// a downstream binding in the multi-seam test.
func firstSeamOutput(t *testing.T, seam map[string]any) string {
	t.Helper()
	outs, ok := seam["outputs"].([]any)
	if !ok || len(outs) == 0 {
		t.Fatal("the example seam must declare at least one output")
	}
	name, _ := outs[0].(string)
	return name
}

// TestStepBindingsResolve: every binding in the worked example resolves to a
// declared input or a prior step's named output. This is a contract-level walk
// of the graph references; it survives refactoring of the schema internals.
func TestStepBindingsResolve(t *testing.T) {
	inst := validExampleInstance(t)
	blk := aileronBlock(t, inst)

	declaredInputs := map[string]bool{}
	for _, in := range blk["inputs"].([]any) {
		declaredInputs[in.(map[string]any)["name"].(string)] = true
	}

	priorOutputs := map[string]bool{} // "stepId.outputName"
	for _, s := range steps(t, inst) {
		for _, b := range stepBindings(s) {
			if strings.HasPrefix(b, "inputs.") {
				name := strings.TrimPrefix(b, "inputs.")
				if !declaredInputs[name] {
					t.Errorf("binding %q references an undeclared input", b)
				}
				continue
			}
			if strings.HasPrefix(b, "steps.") {
				ref := strings.TrimPrefix(b, "steps.")
				if !priorOutputs[ref] {
					t.Errorf("binding %q references a step output not produced by a prior step", b)
				}
				continue
			}
			t.Errorf("binding %q has an unrecognized form", b)
		}
		// After this step is processed, its own outputs become available to
		// later steps. Processing in declared order is the topological order
		// the runtime executes (asserted acyclic by TestStepGraphIsAcyclic).
		id := s["id"].(string)
		for _, o := range stepOutputNames(s) {
			priorOutputs[id+"."+o] = true
		}
	}
}

// TestStepGraphIsAcyclic: the worked example's step references form a directed
// acyclic graph. The runtime executes the graph in topological order; this
// asserts a valid order exists. A JSON Schema cannot express acyclicity, so the
// drift guard checks it as a contract invariant.
func TestStepGraphIsAcyclic(t *testing.T) {
	inst := validExampleInstance(t)
	all := steps(t, inst)

	// Build the dependency edges: a step depends on each prior step it binds.
	deps := map[string]map[string]bool{}
	for _, s := range all {
		id := s["id"].(string)
		if _, exists := deps[id]; exists {
			t.Fatalf("duplicate step id %q in the worked example", id)
		}
		deps[id] = map[string]bool{}
		for _, b := range stepBindings(s) {
			if strings.HasPrefix(b, "steps.") {
				parts := strings.SplitN(strings.TrimPrefix(b, "steps."), ".", 2)
				deps[id][parts[0]] = true
			}
		}
	}

	// Kahn's algorithm: repeatedly remove a node with no unresolved deps.
	resolved := map[string]bool{}
	for progress := true; progress; {
		progress = false
		for id, d := range deps {
			if resolved[id] {
				continue
			}
			ready := true
			for dep := range d {
				if !resolved[dep] {
					ready = false
					break
				}
			}
			if ready {
				resolved[id] = true
				progress = true
			}
		}
	}
	if len(resolved) != len(deps) {
		t.Fatalf("the step graph is not acyclic: only %d of %d steps could be ordered", len(resolved), len(deps))
	}
}

// TestStepActionRefsDeclared: every action-call step's actionRef matches a
// declared requires.actions[].ref. The schema cannot express cross-array
// membership, so the drift guard asserts it on the example.
func TestStepActionRefsDeclared(t *testing.T) {
	inst := validExampleInstance(t)
	blk := aileronBlock(t, inst)

	declared := map[string]bool{}
	for _, a := range blk["requires"].(map[string]any)["actions"].([]any) {
		declared[a.(map[string]any)["ref"].(string)] = true
	}

	for _, s := range steps(t, inst) {
		if s["kind"] != "action-call" {
			continue
		}
		ref := s["actionRef"].(string)
		if !declared[ref] {
			t.Errorf("action-call step %v references undeclared action %q", s["id"], ref)
		}
	}
}

// TestMalformedActionRefRejected: an action reference that violates the
// aileron:<connector>.<action> pattern is rejected at every site that carries
// it. The three sites (requires.actions[].ref, an input resolution
// source.actionRef, and an action-call step's actionRef) share one
// $defs.actionRef definition, so this exercises the shared pattern at each
// place it is referenced.
func TestMalformedActionRefRejected(t *testing.T) {
	sch := compileSchema(t)
	const bad = "metrics.query_series" // missing the required aileron: prefix

	atActionRequirement := func(inst map[string]any) {
		actions := aileronBlock(t, inst)["requires"].(map[string]any)["actions"].([]any)
		actions[0].(map[string]any)["ref"] = bad
	}
	atActionCallStep := func(inst map[string]any) {
		for _, s := range steps(t, inst) {
			if s["kind"] == "action-call" {
				s["actionRef"] = bad
				return
			}
		}
		t.Fatal("expected the worked example to carry at least one action-call step")
	}
	atResolutionSource := func(inst map[string]any) {
		for _, in := range aileronBlock(t, inst)["inputs"].([]any) {
			res, ok := in.(map[string]any)["resolution"].(map[string]any)
			if !ok {
				continue
			}
			src, ok := res["source"].(map[string]any)
			if !ok {
				continue
			}
			if _, has := src["actionRef"]; !has {
				continue
			}
			src["actionRef"] = bad
			return
		}
		t.Skip("worked example carries no source-resolved input to mutate")
	}

	for name, mutate := range map[string]func(map[string]any){
		"requires.actions[].ref":      atActionRequirement,
		"action-call step actionRef":  atActionCallStep,
		"resolution source actionRef": atResolutionSource,
	} {
		t.Run(name, func(t *testing.T) {
			inst := validExampleInstance(t)
			mutate(inst)
			if err := sch.Validate(inst); err == nil {
				t.Fatalf("a malformed actionRef %q at %s must be rejected by the shared pattern", bad, name)
			}
		})
	}
}

// TestStrippedManifestDropsSteps: removing the aileron block drops the step
// graph with it, so the lossless-if-stripped guarantee covers the composition.
// TestStrippedManifestStillValid already deletes the whole block; this sharpens
// the regression by asserting the steps surface specifically disappears.
func TestStrippedManifestDropsSteps(t *testing.T) {
	sch := compileSchema(t)
	inst := validExampleInstance(t)
	if _, ok := aileronBlock(t, inst)["steps"]; !ok {
		t.Fatal("expected the worked example to carry a steps block before stripping")
	}
	delete(inst, "aileron")
	if _, ok := inst["aileron"]; ok {
		t.Fatal("stripping the aileron block must drop the steps graph with it")
	}
	if err := sch.Validate(inst); err != nil {
		t.Fatalf("a stripped manifest (steps dropped with the block) must still validate:\n%v", err)
	}
}

// stepBindings returns every binding reference a step carries, across the
// kind-specific args/bindings maps AND the `steps.<id>.<output>` /
// `inputs.<name>` refs embedded in an llm-seam's `prompt` template (#2120). A
// prompt-only dependency on a prior step is otherwise invisible to the
// resolve-existence and acyclicity graph walks; collecting its refs here makes
// those edges participate. Malformed tokens are dropped (the freeze lint owns
// malformed rejection; this walk is an existence/acyclicity contract check).
func stepBindings(s map[string]any) []string {
	var out []string
	collect := func(key string) {
		if m, ok := s[key].(map[string]any); ok {
			for _, v := range m {
				if sv, ok := v.(string); ok {
					out = append(out, sv)
				}
			}
		}
	}
	collect("args")
	collect("bindings")
	if prompt, ok := s["prompt"].(string); ok {
		refs, err := manifest.PromptBindingRefs(prompt)
		if err == nil {
			for _, ref := range refs {
				switch ref.Kind {
				case manifest.PromptRefInput:
					out = append(out, "inputs."+ref.Input)
				case manifest.PromptRefStep:
					out = append(out, "steps."+ref.StepID+"."+ref.Output)
				}
			}
		}
	}
	return out
}

// stepOutputNames returns the named outputs a step produces.
func stepOutputNames(s map[string]any) []string {
	var out []string
	if raw, ok := s["outputs"].([]any); ok {
		for _, o := range raw {
			if so, ok := o.(string); ok {
				out = append(out, so)
			}
		}
	}
	return out
}
