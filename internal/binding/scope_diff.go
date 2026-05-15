package binding

// MissingScopes returns the elements of required that are not present
// in granted, preserving the order of required. Used by drift
// detection to decide whether a binding's recorded grant satisfies an
// upgraded connector manifest's scope set.
//
// Semantics are strict-superset on required: if granted has a scope
// that required does not, that's not drift — the connector dropped a
// scope, which is benign and does not warrant forcing a reauthorize.
// The reverse — required has something granted lacks — is drift.
//
// Returns nil when nothing is missing. Callers should compare with
// len(missing) > 0 rather than nil-checking.
func MissingScopes(required, granted []string) []string {
	if len(required) == 0 {
		return nil
	}
	have := make(map[string]struct{}, len(granted))
	for _, g := range granted {
		have[g] = struct{}{}
	}
	var missing []string
	for _, r := range required {
		if _, ok := have[r]; !ok {
			missing = append(missing, r)
		}
	}
	return missing
}
