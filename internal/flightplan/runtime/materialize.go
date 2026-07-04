package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// Artifact is one materialized output: the declared output's name, the path it
// was written to, its mime type, and the bytes. The runtime records it in the
// audit by name and content digest; the bytes are written to disk by the run
// orchestration when the publish target is `file`.
type Artifact struct {
	Name     string
	Path     string
	MimeType string
	// Content is the materialized utf-8 bytes. Empty for a target:none
	// artifact (recorded but not written).
	Content []byte
	// Digest is the sha256 of Content, formatted `sha256:<hex>`. It is computed
	// at construction over the exact bytes writeArtifacts lands on disk, so an
	// operator can independently verify a loose output file against the digest
	// recorded in the per-launch audit (ADR-0027 snapshot identifier). Empty
	// content hashes deterministically to the sha256 of zero bytes, so the
	// digest is always recordable — including for a target:none artifact that
	// is retained but never written.
	Digest string
	// Written reports whether the artifact should be written to disk
	// (publish target `file`) vs retained in the run record only (`none`).
	Written bool
}

// contentDigest returns the `sha256:<hex>` digest of the materialized bytes.
// It is the audit-safe snapshot identifier for an artifact: it references the
// exact content without carrying it inline (ADR-0027 audit boundary).
func contentDigest(content []byte) string {
	sum := sha256.Sum256(content)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// canonicalValueDigest returns the `sha256:<hex>` digest of a bound value's
// canonical JSON form. It reuses decodeCarrier's canonicalization
// (marshal → unmarshal to a generic value → marshal) so object keys are
// emitted in Go's sorted order: the digest is therefore reproducible across
// runs for an equal value. This is the input-side snapshot identifier the
// per-output audit record records for each binding, so a materialized output
// walks back to the exact inputs by hash without inlining the dataset
// (ADR-0027 audit boundary, issue #1753).
//
// This digest is over the whole bound value object. It equals a downstream
// carrier's digest only for a plain-data carrier that hashes its entire
// content the same way; it does not equal a file-map carrier's Digest, which
// materialize() computes over each entry.Content rather than the enclosing
// {path, mimeType, encoding, content} object.
func canonicalValueDigest(v any) (string, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("encode bound value: %w", err)
	}
	var generic any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return "", fmt.Errorf("decode bound value: %w", err)
	}
	canonical, err := json.Marshal(generic)
	if err != nil {
		return "", fmt.Errorf("re-encode bound value: %w", err)
	}
	return contentDigest(canonical), nil
}

// inputContentDigest returns the `sha256:<hex>` snapshot identifier the input
// walk-back records for a resolved binding value, in the SAME digest-space as
// the producer's `aileron.output.content_hash`.
//
// A file-map carrier (a JSON object with a `content` key, #1519) is digested
// over its carried `entry.Content` bytes — the exact bytes materialize() digests
// into Artifact.Digest — so a downstream input walks back to the producing
// `output.materialized` record by an equal hash (#1891/#1912). A plain-data
// carrier keeps canonicalValueDigest over the whole value object, which already
// equals the producer's digest for a plain-data carrier (materialize() digests
// the same canonical bytes). On any decode error the value falls back to
// canonicalValueDigest so non-file-map / edge values behave exactly as before.
func inputContentDigest(v any) (string, error) {
	entry, _, isFileMap, err := decodeCarrier(v)
	if err == nil && isFileMap {
		return contentDigest([]byte(entry.Content)), nil
	}
	return canonicalValueDigest(v)
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
		Digest:   contentDigest(content),
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
