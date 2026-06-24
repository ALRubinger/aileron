package runtime

import (
	"encoding/json"
	"fmt"
)

// Artifact is one materialized output: the declared output's name, the path it
// was written to, its mime type, and the bytes. The runtime records it in the
// audit by name; the bytes are written to disk by the run orchestration when
// the publish target is `file`.
type Artifact struct {
	Name     string
	Path     string
	MimeType string
	// Content is the materialized utf-8 bytes. Empty for a target:none
	// artifact (recorded but not written).
	Content []byte
	// Written reports whether the artifact should be written to disk
	// (publish target `file`) vs retained in the run record only (`none`).
	Written bool
}

// fileMapEntry is one entry of the typed JSON file-map transport a
// materializesOutput step produces: {path, mimeType, encoding, content}
// (#1519). Materialization is pure deterministic code; no LLM interprets the
// file-map.
type fileMapEntry struct {
	Path     string `json:"path"`
	MimeType string `json:"mimeType"`
	Encoding string `json:"encoding"`
	Content  string `json:"content"`
}

// materialize parses the typed JSON file-map from a materializesOutput step's
// result, validates it against the declared output, and returns the artifact.
// v1 materializes utf-8 only; a base64/binary entry is a hard error citing the
// deferred mount boundary (#1510). The materialized name must match the
// declared output and the mime type must match the declared mimeType.
//
// The step result for a materializing step is expected to carry the file-map
// under the step's first declared output (the conventional carrier), shaped as
// a single file-map entry. A step may produce the entry directly as a map.
func materialize(p *Plan, step Step, outputs map[string]any) (Artifact, error) {
	out, ok := p.Outputs[step.MaterializesOutput]
	if !ok {
		// Decode already guards this; defensive.
		return Artifact{}, fmt.Errorf("flightplan: step %q materializes undeclared output %q", step.ID, step.MaterializesOutput)
	}

	entry, err := fileMapFor(step, outputs)
	if err != nil {
		return Artifact{}, fmt.Errorf("flightplan: step %q materialize %q: %w", step.ID, out.Name, err)
	}

	// Validate the encoding: v1 is utf-8 only.
	switch entry.Encoding {
	case "", string(EncodingUTF8):
		// utf-8 (default) is materialized.
	case string(EncodingBase64):
		return Artifact{}, fmt.Errorf(
			"flightplan: step %q output %q declares base64/binary encoding, which is deferred to the mount / run-and-collect boundary (#1510); v1 materializes utf-8 only",
			step.ID, out.Name)
	default:
		return Artifact{}, fmt.Errorf("flightplan: step %q output %q has unknown file-map encoding %q", step.ID, out.Name, entry.Encoding)
	}

	// The mime type, when the file-map declares one, must match the declared
	// output's mimeType so the materialized artifact matches its contract.
	if entry.MimeType != "" && entry.MimeType != out.MimeType {
		return Artifact{}, fmt.Errorf(
			"flightplan: step %q output %q file-map mimeType %q does not match the declared %q",
			step.ID, out.Name, entry.MimeType, out.MimeType)
	}

	art := Artifact{
		Name:     out.Name,
		MimeType: out.MimeType,
		Content:  []byte(entry.Content),
	}
	if out.Target == PublishFile {
		// The publish path comes from the declared output, not the file-map,
		// so the runtime controls where artifacts land (the file-map path is
		// advisory transport detail).
		art.Path = out.Path
		art.Written = true
	}
	return art, nil
}

// fileMapFor extracts the file-map entry from a step's outputs. It accepts the
// entry under the step's materializing output name (the conventional carrier),
// or — when the step produced exactly one output — under that single output.
// The carrier value may be a map[string]any (already decoded) or a JSON string
// the step emitted; both decode to a fileMapEntry.
func fileMapFor(step Step, outputs map[string]any) (fileMapEntry, error) {
	var carrier any
	// Prefer an output whose name matches the materialized output, else the
	// step's first declared output.
	if len(step.Outputs) > 0 {
		carrier = outputs[step.Outputs[0]]
	}
	if carrier == nil {
		return fileMapEntry{}, fmt.Errorf("step produced no file-map carrier output")
	}
	return decodeFileMapEntry(carrier)
}

// decodeFileMapEntry coerces a carrier value into a fileMapEntry. A decoded
// map or a JSON string are both accepted; anything else is a typed error.
func decodeFileMapEntry(carrier any) (fileMapEntry, error) {
	var raw []byte
	switch c := carrier.(type) {
	case map[string]any:
		b, err := json.Marshal(c)
		if err != nil {
			return fileMapEntry{}, err
		}
		raw = b
	case string:
		raw = []byte(c)
	default:
		return fileMapEntry{}, fmt.Errorf("file-map carrier is %T, want a {path,mimeType,encoding,content} object or its JSON", carrier)
	}
	var entry fileMapEntry
	if err := json.Unmarshal(raw, &entry); err != nil {
		return fileMapEntry{}, fmt.Errorf("decode file-map entry: %w", err)
	}
	return entry, nil
}
