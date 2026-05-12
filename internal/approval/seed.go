package approval

import (
	"encoding/json"
	"fmt"
	"os"
)

// SeedEntry is the JSON-deserialized shape consumed by the daemon's
// `--dev-seed-approvals` flag. Each entry becomes a pending action
// approval on queue startup so developer / Playwright integration
// tests have something to list, approve, and deny without driving a
// real agent through a policy-gated tool call.
//
// Not part of any production wire contract — exists strictly for the
// developer-facing seed path. ID and RequestedAt are not user-supplied;
// the queue mints them via the same path production registrations use.
type SeedEntry struct {
	Kind         ApprovalKind   `json:"kind"`
	ActionName   string         `json:"action_name"`
	ConnectorFQN string         `json:"connector_fqn,omitempty"`
	Args         map[string]any `json:"args,omitempty"`
	SessionID    string         `json:"session_id,omitempty"`
}

// LoadSeedFile reads the seed JSON from path and returns the validated
// entries. The file may be either a top-level JSON array of entries or
// an object with an `items` key holding the array — the latter matches
// the shape `GET /v1/action-approvals` returns, so the same fixture
// can be saved off a running daemon and replayed later.
func LoadSeedFile(path string) ([]SeedEntry, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read seed file: %w", err)
	}
	// Try {"items":[...]} first — that's the response shape from the
	// list endpoint. Fall back to a bare array on failure.
	var wrapped struct {
		Items []SeedEntry `json:"items"`
	}
	if err := json.Unmarshal(raw, &wrapped); err == nil && wrapped.Items != nil {
		return validateSeedEntries(wrapped.Items)
	}
	var arr []SeedEntry
	if err := json.Unmarshal(raw, &arr); err != nil {
		return nil, fmt.Errorf("seed file is neither a JSON array nor an object with an items array: %w", err)
	}
	return validateSeedEntries(arr)
}

// SeedQueue registers each entry on the queue using its kind-specific
// Register* code path — the same one production callers go through.
// Returns the minted approval IDs in input order so the seed caller
// (and integration tests) can correlate input entries with the ids
// the queue assigned.
func SeedQueue(q *ActionApprovalQueue, entries []SeedEntry) []string {
	ids := make([]string, 0, len(entries))
	for _, e := range entries {
		a := q.RegisterKind(e.Kind, e.ActionName, e.ConnectorFQN, e.SessionID, e.Args)
		ids = append(ids, a.ID)
	}
	return ids
}

// validateSeedEntries rejects malformed entries before they hit the
// queue. Anything that would silently produce a confusing empty card
// in the webapp (missing action_name) or an unknown kind (typo in the
// fixture file) is caught here so the seed-path failure mode is a
// startup error with a line-number-able message rather than a runtime
// surprise.
func validateSeedEntries(entries []SeedEntry) ([]SeedEntry, error) {
	for i, e := range entries {
		if e.ActionName == "" {
			return nil, fmt.Errorf("seed entry %d: action_name is required", i)
		}
		switch e.Kind {
		case "", ApprovalKindAction, ApprovalKindCommsSend, ApprovalKindCommsDraft, ApprovalKindHTTPRequest:
			// ok
		default:
			return nil, fmt.Errorf("seed entry %d: unknown kind %q (want one of: action, comms_send, comms_draft, http_request)", i, e.Kind)
		}
	}
	return entries, nil
}
