package audit

import (
	"fmt"
	"strings"
)

// Typed accessors over the flat `aileron.*` provenance keys that the
// runtime emits into an [Event]'s Payload map. These mirror the webapp's
// `webapp/src/lib/audit/payload.ts` getters so the server-assembled walk
// reads exactly the keys the client walk does. Each getter is defensive
// about a missing or wrong-typed value, because a payload that has been
// JSON round-tripped through `POST /v1/audit` carries `float64` numbers
// and `[]any` / `map[string]any` shapes rather than the runtime's native
// Go types.

// StepInput is one `aileron.step.inputs[]` entry: a producing binding,
// the reference it resolved, and (for a hashed input) the content hash
// of the value it carried. A literal input has no ContentHash.
type StepInput struct {
	Binding          string
	Source           string
	ContentHash      string
	QueryExecutionID string
}

// payloadString returns the non-empty string at key, or "".
func payloadStr(p map[string]any, key string) string {
	if v, ok := p[key].(string); ok {
		return v
	}
	return ""
}

// payloadNum returns the numeric value at key coerced to int, reporting
// whether a number was present. JSON round-tripping yields float64, so
// both float64 and int are accepted.
func payloadNum(p map[string]any, key string) (int, bool) {
	switch v := p[key].(type) {
	case float64:
		return int(v), true
	case int:
		return v, true
	case int64:
		return int(v), true
	default:
		return 0, false
	}
}

func outputName(e Event) string        { return payloadStr(e.Payload, "aileron.output.name") }
func outputMime(e Event) string        { return payloadStr(e.Payload, "aileron.output.mime") }
func outputContentHash(e Event) string { return payloadStr(e.Payload, "aileron.output.content_hash") }
func outputBytes(e Event) (int, bool)  { return payloadNum(e.Payload, "aileron.output.bytes") }

func stepID(e Event) string        { return payloadStr(e.Payload, "aileron.step.id") }
func stepKind(e Event) string      { return payloadStr(e.Payload, "aileron.step.kind") }
func stepTransform(e Event) string { return payloadStr(e.Payload, "aileron.step.transform") }
func stepCommand(e Event) string   { return payloadStr(e.Payload, "aileron.step.command") }

// stepInputs reads the `aileron.step.inputs[]` walk-back list, coercing
// each entry defensively. A non-array or malformed value yields an empty
// list. An entry without a usable binding name is skipped, matching
// payload.ts. The list may arrive as `[]any` of `map[string]any` (JSON
// round-tripped) or as the runtime's native `[]map[string]any`.
func stepInputs(e Event) []StepInput {
	raw, ok := e.Payload["aileron.step.inputs"]
	if !ok {
		return nil
	}
	items := toAnySlice(raw)
	if items == nil {
		return nil
	}
	out := make([]StepInput, 0, len(items))
	for _, item := range items {
		obj, ok := item.(map[string]any)
		if !ok {
			continue
		}
		binding, ok := obj["binding"].(string)
		if !ok || binding == "" {
			continue
		}
		entry := StepInput{Binding: binding}
		if s, ok := obj["source"].(string); ok {
			entry.Source = s
		}
		if s, ok := obj["content_hash"].(string); ok {
			entry.ContentHash = s
		}
		if s, ok := obj["query_execution_id"].(string); ok {
			entry.QueryExecutionID = s
		}
		out = append(out, entry)
	}
	return out
}

// toAnySlice normalizes the two concrete slice shapes a step-inputs
// value can carry into a `[]any` the caller can range over. It returns
// nil for any non-slice value.
func toAnySlice(v any) []any {
	switch s := v.(type) {
	case []any:
		return s
	case []map[string]any:
		out := make([]any, len(s))
		for i := range s {
			out[i] = s[i]
		}
		return out
	default:
		return nil
	}
}

func planSkill(e Event) string { return payloadStr(e.Payload, "aileron.plan.skill") }

// actorIdentityLabel prefers the normalized Actor.IdentityLabel (lifted
// at ingest by handlers_audit.go), falling back to the flat payload key.
func actorIdentityLabel(e Event) string {
	if e.Actor.IdentityLabel != "" {
		return e.Actor.IdentityLabel
	}
	return payloadStr(e.Payload, "aileron.actor.identity_label")
}

// --- title/subtitle helpers, mirroring provenance.ts ---

func artifactTitle(e Event) string {
	if n := outputName(e); n != "" {
		return n
	}
	return "(unnamed artifact)"
}

func artifactSubtitle(e Event) string {
	parts := make([]string, 0, 3)
	if m := outputMime(e); m != "" {
		parts = append(parts, m)
	}
	if b, ok := outputBytes(e); ok {
		if s := formatBytes(b); s != "" {
			parts = append(parts, s)
		}
	}
	if h := shortHash(outputContentHash(e)); h != "" {
		parts = append(parts, h)
	}
	return strings.Join(parts, " · ")
}

func stepSubtitle(e Event) string {
	parts := make([]string, 0, 2)
	if k := stepKind(e); k != "" {
		parts = append(parts, k)
	}
	detail := stepTransform(e)
	if detail == "" {
		detail = stepCommand(e)
	}
	if detail != "" {
		parts = append(parts, detail)
	}
	return strings.Join(parts, " · ")
}

// shortHash shortens a `sha256:<hex>` (or bare hex) content hash for
// display, keeping the algorithm prefix and the first/last few hex
// chars. Mirrors payload.ts `shortHash`.
func shortHash(hash string) string {
	if hash == "" {
		return ""
	}
	algo, hex := "", hash
	if i := strings.Index(hash, ":"); i >= 0 {
		algo, hex = hash[:i], hash[i+1:]
	}
	if len(hex) <= 12 {
		return hash
	}
	abbreviated := hex[:8] + "…" + hex[len(hex)-4:]
	if algo != "" {
		return algo + ":" + abbreviated
	}
	return abbreviated
}

// formatBytes renders a base-1024 human-readable size. Mirrors
// payload.ts `formatBytes`.
func formatBytes(bytes int) string {
	if bytes < 0 {
		return ""
	}
	if bytes < 1024 {
		return fmt.Sprintf("%d B", bytes)
	}
	units := []string{"KB", "MB", "GB", "TB"}
	value := float64(bytes) / 1024
	unit := 0
	for value >= 1024 && unit < len(units)-1 {
		value /= 1024
		unit++
	}
	if value < 10 {
		return fmt.Sprintf("%.1f %s", value, units[unit])
	}
	return fmt.Sprintf("%d %s", int(value+0.5), units[unit])
}
