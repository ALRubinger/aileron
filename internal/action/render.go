package action

import (
	"fmt"
	"strings"
)

// ResolveFieldPath walks `response` along the dotted `path` and returns
// the resolved leaf as a string plus ok=true. When any path segment
// fails to resolve, returns the empty string and ok=false; callers
// (the approval-preview renderer) surface a per-field "n/a" indicator
// in that case per ADR-0016.
//
// Header-style array shorthand: when a path segment lands on an array
// of `{"name": "X", "value": "..."}` entries and the next segment is
// the header name (e.g. `message.payload.headers.Subject`), the
// resolver finds the entry whose `name` equals the segment and returns
// the matching `value`. This matches Gmail's draft shape, where headers
// are positional rather than keyed. For other arrays the path is
// expected to use a numeric index.
//
// Leaf values are rendered via fmt.Sprintf("%v", ...) so strings,
// numbers, and bools all serialise sensibly. The renderer does not
// pretty-print nested maps or arrays — preview is a flat "label: value"
// list per ADR-0016's worked example.
func ResolveFieldPath(response map[string]any, path string) (string, bool) {
	if path == "" || response == nil {
		return "", false
	}
	segments := strings.Split(path, ".")
	var current any = response
	for _, seg := range segments {
		switch node := current.(type) {
		case map[string]any:
			next, ok := node[seg]
			if !ok {
				return "", false
			}
			current = next
		case []any:
			// Try the header-style shorthand first: array of objects each
			// carrying `name` + `value`, where `seg` matches one of the
			// `name` fields. This is the Gmail draft shape and is the
			// motivating case for ADR-0016.
			value, found := headerLookup(node, seg)
			if found {
				current = value
				continue
			}
			// Fall back to numeric index (e.g. `attachments.0.filename`).
			idx, err := parseIndex(seg)
			if err != nil || idx < 0 || idx >= len(node) {
				return "", false
			}
			current = node[idx]
		default:
			// We hit a leaf with path segments left to consume.
			return "", false
		}
	}
	if current == nil {
		return "", false
	}
	return fmt.Sprintf("%v", current), true
}

// headerLookup implements the header-style array shorthand. Returns the
// `value` of the first entry in `arr` whose `name` field equals `key`,
// plus found=true. When `arr` is not an array of {name, value} objects
// or no entry matches, returns nil + false.
func headerLookup(arr []any, key string) (any, bool) {
	for _, item := range arr {
		obj, ok := item.(map[string]any)
		if !ok {
			return nil, false
		}
		nameVal, hasName := obj["name"]
		if !hasName {
			return nil, false
		}
		name, ok := nameVal.(string)
		if !ok {
			return nil, false
		}
		if name == key {
			val, hasValue := obj["value"]
			if !hasValue {
				return nil, false
			}
			return val, true
		}
	}
	return nil, false
}

// parseIndex parses a non-negative integer string for the array-index
// path segment fallback. Returns -1 on any non-numeric input so the
// caller treats it as a path miss rather than a runtime error.
func parseIndex(s string) (int, error) {
	if s == "" {
		return -1, fmt.Errorf("empty index")
	}
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return -1, fmt.Errorf("non-numeric segment %q", s)
		}
		n = n*10 + int(r-'0')
	}
	return n, nil
}

// InterpolatePreviewArgs walks `template` (which TOML decodes to any of
// string, []any, or map[string]any) and substitutes `${args.NAME}`
// tokens with the corresponding entry from `args`. Mirrors the
// [[execute]] step interpolation in interpolateArgs so the preview
// directive shares one convention with the gated action's execute
// chain. Unresolved references are left as the literal `${args.X}`
// token; the manifest validator catches references that the gated
// action's inputs do not declare, so unresolved-at-runtime here means
// the agent failed to supply a declared input — surfacing the literal
// to the connector is the same fallback the execute path uses.
func InterpolatePreviewArgs(template map[string]any, args map[string]any) map[string]any {
	out := make(map[string]any, len(template))
	for k, v := range template {
		out[k] = interpolateArgs(v, args)
	}
	return out
}
