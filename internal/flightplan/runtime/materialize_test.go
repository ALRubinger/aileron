package runtime

import (
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
