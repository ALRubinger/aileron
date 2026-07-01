package runtime

import (
	"encoding/json"
	"strings"
	"testing"
)

func planWithOutput(out Output) *Plan {
	return &Plan{Outputs: map[string]Output{out.Name: out}}
}

func fileMapStep(materializes string) Step {
	return Step{ID: "s", Kind: KindTransform, Outputs: []string{"file"}, MaterializesOutput: materializes}
}

func TestMaterialize_UTF8ToDeclaredPath(t *testing.T) {
	p := planWithOutput(Output{Name: "digest.csv", MimeType: "text/csv", Encoding: EncodingUTF8, Target: PublishFile, Path: "digest.csv"})
	step := fileMapStep("digest.csv")
	outputs := map[string]any{"file": map[string]any{
		"path": "digest.csv", "mimeType": "text/csv", "encoding": "utf-8", "content": "a,b\n1,2\n",
	}}
	art, err := materialize(p, step, outputs)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if string(art.Content) != "a,b\n1,2\n" {
		t.Errorf("content = %q", art.Content)
	}
	if art.Path != "digest.csv" || !art.Written {
		t.Errorf("artifact = %+v", art)
	}
}

func TestMaterialize_DigestFileMapArtifact(t *testing.T) {
	// A file-map artifact carries a `sha256:<hex>` digest over its exact bytes,
	// so the recorded output is independently verifiable (ADR-0027 snapshot
	// identifier). The expected value is the known sha256 of the content.
	p := planWithOutput(Output{Name: "digest.csv", MimeType: "text/csv", Encoding: EncodingUTF8, Target: PublishFile, Path: "digest.csv"})
	step := fileMapStep("digest.csv")
	outputs := map[string]any{"file": map[string]any{
		"path": "digest.csv", "mimeType": "text/csv", "encoding": "utf-8", "content": "a,b\n1,2\n",
	}}
	art, err := materialize(p, step, outputs)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	const want = "sha256:492d5ea496056f1a6a6592241032fab764c321596317930b4fa0e1e8bc3b7470"
	if art.Digest != want {
		t.Errorf("digest = %q, want %q", art.Digest, want)
	}
}

func TestMaterialize_DigestDataResultCarrier(t *testing.T) {
	// A plain data-result carrier (no `content` key) digests the canonical JSON
	// bytes that land on disk, matching the exact serialized content.
	p := planWithOutput(Output{Name: "rows.json", MimeType: "application/json", Encoding: EncodingUTF8, Target: PublishFile, Path: "rows.json"})
	step := fileMapStep("rows.json")
	outputs := map[string]any{"file": map[string]any{"dealer": "a"}}
	art, err := materialize(p, step, outputs)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if string(art.Content) != `{"dealer":"a"}` {
		t.Fatalf("content = %q", art.Content)
	}
	const want = "sha256:464ab4c951738bc4064d5236f3111bf65fe2997e617e06cf65fb90e0dc043011"
	if art.Digest != want {
		t.Errorf("digest = %q, want %q", art.Digest, want)
	}
}

func TestMaterialize_DigestEmptyContentDeterministic(t *testing.T) {
	// A target:none artifact with empty content still carries a digest: the
	// sha256 of zero bytes, so every materialized output is recordable.
	p := planWithOutput(Output{Name: "empty.txt", MimeType: "text/plain", Encoding: EncodingUTF8, Target: PublishNone})
	step := fileMapStep("empty.txt")
	outputs := map[string]any{"file": map[string]any{"content": "", "encoding": "utf-8"}}
	art, err := materialize(p, step, outputs)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	const want = "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if art.Digest != want {
		t.Errorf("empty-content digest = %q, want %q", art.Digest, want)
	}
}

func TestMaterialize_DigestDetectsTamper(t *testing.T) {
	// A one-byte change to the content produces a different digest: the
	// acceptance's "tampered output byte fails verification".
	p := planWithOutput(Output{Name: "d.csv", MimeType: "text/csv", Encoding: EncodingUTF8, Target: PublishFile, Path: "d.csv"})
	step := fileMapStep("d.csv")
	base := materializeContent(t, p, step, "a,b\n1,2\n")
	tampered := materializeContent(t, p, step, "a,b\n1,3\n")
	if base == tampered {
		t.Fatal("a one-byte content change must produce a different digest")
	}
}

// materializeContent materializes a utf-8 file-map with the given content and
// returns its digest, so a tamper test can compare two runs.
func materializeContent(t *testing.T, p *Plan, step Step, content string) string {
	t.Helper()
	outputs := map[string]any{"file": map[string]any{"encoding": "utf-8", "content": content}}
	art, err := materialize(p, step, outputs)
	if err != nil {
		t.Fatalf("materialize %q: %v", content, err)
	}
	return art.Digest
}

func TestMaterialize_Base64Refused(t *testing.T) {
	p := planWithOutput(Output{Name: "chart.png", MimeType: "image/png", Encoding: EncodingBase64, Target: PublishFile, Path: "chart.png"})
	step := fileMapStep("chart.png")
	outputs := map[string]any{"file": map[string]any{
		"path": "chart.png", "mimeType": "image/png", "encoding": "base64", "content": "AAAA",
	}}
	_, err := materialize(p, step, outputs)
	if err == nil {
		t.Fatal("base64/binary materialization must be refused in v1")
	}
	if !strings.Contains(err.Error(), "#1510") {
		t.Errorf("error must cite the deferred mount boundary (#1510), got %v", err)
	}
}

func TestMaterialize_MimeTypeMismatchRefused(t *testing.T) {
	p := planWithOutput(Output{Name: "digest.csv", MimeType: "text/csv", Encoding: EncodingUTF8, Target: PublishFile, Path: "digest.csv"})
	step := fileMapStep("digest.csv")
	outputs := map[string]any{"file": map[string]any{
		"mimeType": "application/json", "encoding": "utf-8", "content": "{}",
	}}
	if _, err := materialize(p, step, outputs); err == nil {
		t.Fatal("a file-map mimeType that disagrees with the declared output must be refused")
	}
}

func TestMaterialize_TargetNoneRecordedNotWritten(t *testing.T) {
	p := planWithOutput(Output{Name: "kept.json", MimeType: "application/json", Encoding: EncodingUTF8, Target: PublishNone})
	step := fileMapStep("kept.json")
	outputs := map[string]any{"file": map[string]any{"encoding": "utf-8", "content": "{}"}}
	art, err := materialize(p, step, outputs)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if art.Written {
		t.Error("a target:none artifact must not be written")
	}
	if art.Name != "kept.json" {
		t.Errorf("artifact name = %q", art.Name)
	}
}

func TestMaterialize_CarrierFromJSONString(t *testing.T) {
	p := planWithOutput(Output{Name: "d.json", MimeType: "application/json", Encoding: EncodingUTF8, Target: PublishFile, Path: "d.json"})
	step := fileMapStep("d.json")
	// The carrier is a JSON string the step emitted, not a decoded map.
	outputs := map[string]any{"file": `{"encoding":"utf-8","content":"{}","mimeType":"application/json"}`}
	art, err := materialize(p, step, outputs)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if string(art.Content) != "{}" {
		t.Errorf("content = %q", art.Content)
	}
}

func TestMaterialize_RawDataObjectSerialized(t *testing.T) {
	// A data-producing action-call step whose carrier is a plain object with no
	// `content` key (e.g. the Athena connector's run_query result) must
	// materialize the whole object as utf-8 JSON, never a 0-byte file (#1706).
	p := planWithOutput(Output{Name: "chains.json", MimeType: "application/json", Encoding: EncodingUTF8, Target: PublishFile, Path: "chains.json"})
	step := fileMapStep("chains.json")
	outputs := map[string]any{"file": map[string]any{
		"QueryExecutionId": "q-123",
		"ResultSet":        map[string]any{"rows": float64(2)},
	}}
	art, err := materialize(p, step, outputs)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if len(art.Content) == 0 {
		t.Fatal("a non-file-map data result must not materialize 0 bytes")
	}
	if !art.Written || art.Path != "chains.json" {
		t.Errorf("artifact = %+v", art)
	}
	var got map[string]any
	if err := json.Unmarshal(art.Content, &got); err != nil {
		t.Fatalf("content is not valid JSON: %v (%q)", err, art.Content)
	}
	if got["QueryExecutionId"] != "q-123" {
		t.Errorf("serialized result missing QueryExecutionId: %q", art.Content)
	}
}

func TestMaterialize_RawDataArraySerialized(t *testing.T) {
	// A data result that is a JSON array must also serialize, not error.
	p := planWithOutput(Output{Name: "rows.json", MimeType: "application/json", Encoding: EncodingUTF8, Target: PublishFile, Path: "rows.json"})
	step := fileMapStep("rows.json")
	outputs := map[string]any{"file": []any{
		map[string]any{"dealer": "a"}, map[string]any{"dealer": "b"},
	}}
	art, err := materialize(p, step, outputs)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if string(art.Content) != `[{"dealer":"a"},{"dealer":"b"}]` {
		t.Errorf("content = %q", art.Content)
	}
}

func TestMaterialize_RawDataFromJSONString(t *testing.T) {
	// A carrier emitted as a JSON string for a plain data object also serializes
	// the underlying object (canonicalized), not the quoted string.
	p := planWithOutput(Output{Name: "d.json", MimeType: "application/json", Encoding: EncodingUTF8, Target: PublishFile, Path: "d.json"})
	step := fileMapStep("d.json")
	outputs := map[string]any{"file": `{"id":7,"name":"x"}`}
	art, err := materialize(p, step, outputs)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if len(art.Content) == 0 {
		t.Fatal("a JSON-string data result must not materialize 0 bytes")
	}
	var got map[string]any
	if err := json.Unmarshal(art.Content, &got); err != nil {
		t.Fatalf("content is not valid JSON: %v", err)
	}
	if got["name"] != "x" {
		t.Errorf("serialized result = %q", art.Content)
	}
}

func TestMaterialize_EmptyFileMapContentStaysEmpty(t *testing.T) {
	// A genuine file-map that explicitly emits empty content keeps writing an
	// empty file: the `content`-key discriminator preserves the ability to
	// produce a deliberately-empty artifact.
	p := planWithOutput(Output{Name: "empty.txt", MimeType: "text/plain", Encoding: EncodingUTF8, Target: PublishFile, Path: "empty.txt"})
	step := fileMapStep("empty.txt")
	outputs := map[string]any{"file": map[string]any{"content": "", "encoding": "utf-8"}}
	art, err := materialize(p, step, outputs)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if len(art.Content) != 0 {
		t.Errorf("explicit empty file-map content must stay empty, got %q", art.Content)
	}
}

func TestMaterialize_NonObjectCarrierRefused(t *testing.T) {
	p := planWithOutput(Output{Name: "d.csv", MimeType: "text/csv", Encoding: EncodingUTF8, Target: PublishNone})
	step := fileMapStep("d.csv")
	outputs := map[string]any{"file": 42} // not a map or JSON string
	if _, err := materialize(p, step, outputs); err == nil {
		t.Fatal("a non-object, non-JSON-string carrier must be refused")
	}
}

func TestMaterialize_NoCarrierRefused(t *testing.T) {
	p := planWithOutput(Output{Name: "d.csv", MimeType: "text/csv", Encoding: EncodingUTF8, Target: PublishNone})
	step := Step{ID: "s", Kind: KindTransform, Outputs: nil, MaterializesOutput: "d.csv"}
	if _, err := materialize(p, step, map[string]any{}); err == nil {
		t.Fatal("a step with no carrier output must be refused")
	}
}

func TestMaterialize_MalformedJSONCarrierRefused(t *testing.T) {
	p := planWithOutput(Output{Name: "d.csv", MimeType: "text/csv", Encoding: EncodingUTF8, Target: PublishNone})
	step := fileMapStep("d.csv")
	outputs := map[string]any{"file": "{not json"}
	if _, err := materialize(p, step, outputs); err == nil {
		t.Fatal("a malformed JSON carrier must be refused")
	}
}
