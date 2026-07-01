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

func TestMaterialize_HTMLRenderProducesSealedArtifact(t *testing.T) {
	// Integration: the html-render transform's file-map entry drops straight
	// into materialize.go unchanged, producing a sealed, writable HTML artifact
	// whose bytes are a deterministic function of the template and data bindings.
	p := planWithOutput(Output{
		Name: "report.html", MimeType: "text/html", Encoding: EncodingUTF8,
		Target: PublishFile, Path: "report.html",
	})
	step := Step{
		ID: "render", Kind: KindTransform, Transform: "html-render",
		Outputs: []string{"report"}, MaterializesOutput: "report.html",
	}
	reg := NewTransformRegistry()
	outputs, err := reg.run(step, map[string]any{
		"template": "<h1>{{ .summary.title }}</h1><ul>{{ range .summary.items }}<li>{{ . }}</li>{{ end }}</ul>",
		"summary": map[string]any{
			"title": "Weekly Digest",
			"items": []any{"alpha", "beta"},
		},
	})
	if err != nil {
		t.Fatalf("html-render: %v", err)
	}
	art, err := materialize(p, step, outputs)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	want := "<h1>Weekly Digest</h1><ul><li>alpha</li><li>beta</li></ul>"
	if string(art.Content) != want {
		t.Errorf("content = %q, want %q", art.Content, want)
	}
	if art.MimeType != "text/html" {
		t.Errorf("mimeType = %q, want text/html", art.MimeType)
	}
	if !art.Written || art.Path != "report.html" {
		t.Errorf("artifact must be sealed to the declared path, got %+v", art)
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
