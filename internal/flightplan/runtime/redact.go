package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
)

// applyRedaction applies the trust-contract redaction rules to a dispatch
// result IN PLACE on a deep copy, BEFORE the result surfaces into the graph
// or any audit summary. Each rule names a field path and how it is redacted:
// drop removes it, mask replaces it with a fixed mask, hash replaces it with
// a stable hash (ADR-0027). The original result is never mutated.
//
// Field paths use dotted segments with a `[]` element-wildcard for arrays,
// matching the schema's example shape (for example series[].labels.account_id
// or assignee.email). A path that matches nothing is a no-op: redaction is a
// declared safeguard, not a required-field assertion.
func applyRedaction(result map[string]any, rules []RedactionRule) map[string]any {
	out := deepCopyMap(result)
	for _, r := range rules {
		segs := parsePath(r.Field)
		redactPath(out, segs, r.Rule)
	}
	return out
}

// pathSeg is one segment of a redaction path. Wildcard marks a `[]` array
// element traversal.
type pathSeg struct {
	key      string
	wildcard bool
}

// parsePath splits a dotted field path into segments, treating a `[]` suffix
// on a segment as an array-element wildcard that applies the remaining path to
// every element.
func parsePath(path string) []pathSeg {
	var segs []pathSeg
	for _, raw := range strings.Split(path, ".") {
		if raw == "" {
			continue
		}
		if strings.HasSuffix(raw, "[]") {
			segs = append(segs, pathSeg{key: strings.TrimSuffix(raw, "[]"), wildcard: true})
		} else {
			segs = append(segs, pathSeg{key: raw})
		}
	}
	return segs
}

// redactPath walks segs into m and applies the rule at the leaf. It recurses
// through wildcard array elements so a single rule covers every element of a
// declared array field.
func redactPath(node any, segs []pathSeg, rule RedactionKind) {
	if len(segs) == 0 {
		return
	}
	m, ok := node.(map[string]any)
	if !ok {
		return
	}
	seg := segs[0]
	last := len(segs) == 1

	if last && !seg.wildcard {
		applyRule(m, seg.key, rule)
		return
	}

	child, present := m[seg.key]
	if !present {
		return
	}

	if seg.wildcard {
		// child should be an array; apply the remainder to each element. When
		// the wildcard is the leaf segment, redact each element value
		// directly under its parent slice.
		arr, ok := child.([]any)
		if !ok {
			return
		}
		rest := segs[1:]
		if last {
			for i := range arr {
				arr[i] = redactValue(arr[i], rule)
			}
			return
		}
		for _, el := range arr {
			redactPath(el, rest, rule)
		}
		return
	}

	redactPath(child, segs[1:], rule)
}

// applyRule redacts key in m per rule.
func applyRule(m map[string]any, key string, rule RedactionKind) {
	v, present := m[key]
	if !present {
		return
	}
	switch rule {
	case RedactDrop:
		delete(m, key)
	default:
		m[key] = redactValue(v, rule)
	}
}

// redactValue returns the redacted form of a value. drop is handled by the
// caller (it removes the key); mask and hash transform the value.
func redactValue(v any, rule RedactionKind) any {
	switch rule {
	case RedactMask:
		return "***"
	case RedactHash:
		// Hash a canonical, type-preserving JSON encoding so distinct inputs
		// (a number vs the string of that number, or two different objects)
		// never collide to the same hash. A value that cannot encode falls
		// back to the mask so a redacted field is never surfaced in the clear.
		encoded, err := json.Marshal(v)
		if err != nil {
			return "***"
		}
		sum := sha256.Sum256(encoded)
		return "sha256:" + hex.EncodeToString(sum[:])
	case RedactDrop:
		// In an array context drop replaces the element with nil since a slice
		// cannot remove an index by key.
		return nil
	default:
		return v
	}
}

// deepCopyMap returns a deep copy of a JSON-shaped map so redaction never
// mutates the dispatcher's result.
func deepCopyMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = deepCopyValue(v)
	}
	return out
}

func deepCopyValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		return deepCopyMap(t)
	case []any:
		arr := make([]any, len(t))
		for i, el := range t {
			arr[i] = deepCopyValue(el)
		}
		return arr
	default:
		return v
	}
}
