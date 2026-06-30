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

	carrier, err := carrierFor(step, outputs)
	if err != nil {
		return Artifact{}, fmt.Errorf("flightplan: step %q materialize %q: %w", step.ID, out.Name, err)
	}

	entry, rawContent, isFileMap, err := decodeCarrier(carrier)
	if err != nil {
		return Artifact{}, fmt.Errorf("flightplan: step %q materialize %q: %w", step.ID, out.Name, err)
	}

	var content []byte
	if isFileMap {
		// The carrier is a file-map entry (#1519): it carries the bytes in
		// `content` plus its own encoding/mimeType transport detail.

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
		content = []byte(entry.Content)
	} else {
		// The carrier is a plain data result (no `content` key), e.g. an
		// action-call step returning {QueryExecutionId, ResultSet}. Materialize
		// the whole carrier as utf-8 JSON rather than extracting a missing
		// `content` field and silently writing 0 bytes (#1706). decodeCarrier
		// has already canonicalized it to JSON bytes.
		content = rawContent
	}

	art := Artifact{
		Name:     out.Name,
		MimeType: out.MimeType,
		Content:  content,
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

// carrierFor selects a step's carrier output value. It prefers the output whose
// name matches the materialized output (the conventional carrier), then falls
// back to the step's first declared output. The carrier value may be a
// map[string]any (already decoded), a []any, a JSON string the step emitted, or
// any other JSON-serializable result.
func carrierFor(step Step, outputs map[string]any) (any, error) {
	var carrier any
	if c, ok := outputs[step.MaterializesOutput]; ok {
		carrier = c
	} else if len(step.Outputs) > 0 {
		carrier = outputs[step.Outputs[0]]
	}
	if carrier == nil {
		return nil, fmt.Errorf("step produced no carrier output")
	}
	return carrier, nil
}

// decodeCarrier classifies a step's carrier output as either a file-map entry
// or a plain data result, and returns the bytes to materialize.
//
// A carrier is treated as a file-map (#1519) iff it decodes to a JSON object
// carrying a "content" key — the only field that conveys the artifact bytes.
// In that case the typed fileMapEntry is returned (isFileMap=true) and the
// caller validates its encoding/mimeType transport detail. Without a "content"
// key the carrier is a plain data result (e.g. an action-call returning
// {QueryExecutionId, ResultSet}); the whole carrier is re-serialized to
// canonical JSON and returned as rawContent (isFileMap=false), so a
// data-producing step materializes its result instead of silently writing 0
// bytes (#1706).
//
// A carrier that decodes to a JSON object with a "content" key but is genuinely
// a data result is the one ambiguous case; "content" is the file-map's defining
// byte-carrying field, so it is the correct discriminator. A scalar or
// otherwise undecodable carrier is a typed error.
func decodeCarrier(carrier any) (entry fileMapEntry, rawContent []byte, isFileMap bool, err error) {
	// Canonicalize the carrier to JSON bytes. A string carrier is taken as the
	// JSON it emitted; any other value is marshaled.
	var raw []byte
	if s, ok := carrier.(string); ok {
		raw = []byte(s)
	} else {
		b, mErr := json.Marshal(carrier)
		if mErr != nil {
			return fileMapEntry{}, nil, false, fmt.Errorf("encode carrier: %w", mErr)
		}
		raw = b
	}

	// Inspect the generic shape to classify file-map vs. data result.
	var generic any
	if uErr := json.Unmarshal(raw, &generic); uErr != nil {
		return fileMapEntry{}, nil, false, fmt.Errorf("decode carrier: %w", uErr)
	}

	obj, isObject := generic.(map[string]any)
	if isObject {
		if _, hasContent := obj["content"]; hasContent {
			var fm fileMapEntry
			if uErr := json.Unmarshal(raw, &fm); uErr != nil {
				return fileMapEntry{}, nil, false, fmt.Errorf("decode file-map entry: %w", uErr)
			}
			return fm, nil, true, nil
		}
	}

	// A plain data result is materialized only when it is structured (a JSON
	// object or array). A bare scalar carrier is not a plausible artifact and
	// signals a wiring error rather than a result to serialize.
	switch generic.(type) {
	case map[string]any, []any:
		canonical, mErr := json.Marshal(generic)
		if mErr != nil {
			return fileMapEntry{}, nil, false, fmt.Errorf("encode data result: %w", mErr)
		}
		return fileMapEntry{}, canonical, false, nil
	default:
		return fileMapEntry{}, nil, false, fmt.Errorf("carrier is %T, want a file-map {path,mimeType,encoding,content} object or a JSON data object/array", carrier)
	}
}
