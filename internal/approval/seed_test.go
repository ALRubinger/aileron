package approval

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLoadSeedFile_BareArray verifies the seed loader accepts a top-
// level JSON array. This is the canonical fixture shape.
func TestLoadSeedFile_BareArray(t *testing.T) {
	path := writeSeedFile(t, `[
		{"kind":"action","action_name":"send-email","connector_fqn":"github://x/y","args":{"to":"a@b.com"}}
	]`)
	entries, err := LoadSeedFile(path)
	if err != nil {
		t.Fatalf("LoadSeedFile: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("len=%d want 1", len(entries))
	}
	if entries[0].Kind != ApprovalKindAction {
		t.Errorf("kind=%q want action", entries[0].Kind)
	}
	if entries[0].ActionName != "send-email" {
		t.Errorf("action_name=%q", entries[0].ActionName)
	}
	if entries[0].ConnectorFQN != "github://x/y" {
		t.Errorf("connector_fqn=%q", entries[0].ConnectorFQN)
	}
	if got, want := entries[0].Args["to"], "a@b.com"; got != want {
		t.Errorf("args.to=%v want %q", got, want)
	}
}

// TestLoadSeedFile_ItemsWrapper verifies the seed loader also accepts
// the {"items":[...]} shape so a fixture captured directly from
// `GET /v1/action-approvals` can be replayed.
func TestLoadSeedFile_ItemsWrapper(t *testing.T) {
	path := writeSeedFile(t, `{"items":[
		{"kind":"comms_send","action_name":"send_message","args":{"service":"slack","channel":"#dev","body":"hi"}}
	]}`)
	entries, err := LoadSeedFile(path)
	if err != nil {
		t.Fatalf("LoadSeedFile: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("len=%d want 1", len(entries))
	}
	if entries[0].Kind != ApprovalKindCommsSend {
		t.Errorf("kind=%q want comms_send", entries[0].Kind)
	}
}

// TestLoadSeedFile_MissingActionName rejects entries without an
// action_name — those would render as blank cards in the webapp.
func TestLoadSeedFile_MissingActionName(t *testing.T) {
	path := writeSeedFile(t, `[{"kind":"action"}]`)
	_, err := LoadSeedFile(path)
	if err == nil {
		t.Fatal("LoadSeedFile: want error, got nil")
	}
	if !strings.Contains(err.Error(), "action_name is required") {
		t.Errorf("error=%v want mention of action_name", err)
	}
}

// TestLoadSeedFile_UnknownKind rejects typo'd kinds at load time —
// the seed-path failure mode is a startup error, not a runtime
// surprise once the daemon is already running.
func TestLoadSeedFile_UnknownKind(t *testing.T) {
	path := writeSeedFile(t, `[{"kind":"approve_everything","action_name":"x"}]`)
	_, err := LoadSeedFile(path)
	if err == nil {
		t.Fatal("LoadSeedFile: want error, got nil")
	}
	if !strings.Contains(err.Error(), "unknown kind") {
		t.Errorf("error=%v want mention of unknown kind", err)
	}
}

// TestLoadSeedFile_MissingFile surfaces a useful filesystem error
// rather than a confusing unmarshal error.
func TestLoadSeedFile_MissingFile(t *testing.T) {
	_, err := LoadSeedFile(filepath.Join(t.TempDir(), "does-not-exist.json"))
	if err == nil {
		t.Fatal("LoadSeedFile: want error, got nil")
	}
	if !strings.Contains(err.Error(), "read seed file") {
		t.Errorf("error=%v want mention of read seed file", err)
	}
}

// TestLoadSeedFile_InvalidJSON rejects garbage. Verifies the
// fall-through error mentions both shape options so an operator
// debugging the failure knows what's expected.
func TestLoadSeedFile_InvalidJSON(t *testing.T) {
	path := writeSeedFile(t, `not even close to JSON`)
	_, err := LoadSeedFile(path)
	if err == nil {
		t.Fatal("LoadSeedFile: want error, got nil")
	}
	if !strings.Contains(err.Error(), "neither a JSON array nor an object") {
		t.Errorf("error=%v want a shape-hint message", err)
	}
}

// TestSeedQueue_RegistersAllEntries verifies SeedQueue puts each
// entry on the queue in input order and returns the minted ids,
// exercising the same Register code path production uses.
func TestSeedQueue_RegistersAllEntries(t *testing.T) {
	q := NewActionApprovalQueue(nil, nil)
	entries := []SeedEntry{
		{Kind: ApprovalKindAction, ActionName: "send-email", Args: map[string]any{"to": "a@b"}},
		{Kind: ApprovalKindCommsSend, ActionName: "send_message", Args: map[string]any{"service": "slack"}},
		{Kind: ApprovalKindHTTPRequest, ActionName: "http_request", Args: map[string]any{"method": "GET", "url": "http://x"}},
	}

	ids := SeedQueue(q, entries)

	if len(ids) != 3 {
		t.Fatalf("len(ids)=%d want 3", len(ids))
	}
	pending := q.List()
	if len(pending) != 3 {
		t.Fatalf("queue.List len=%d want 3", len(pending))
	}
	// All seeded entries must be retrievable by their returned id.
	for _, id := range ids {
		if _, ok := q.Get(id); !ok {
			t.Errorf("Get(%q) not found", id)
		}
	}
	// Kinds round-trip — the queue's RegisterKind preserves what
	// the seed handed it.
	gotKinds := map[ApprovalKind]int{}
	for _, p := range pending {
		gotKinds[p.Kind]++
	}
	for _, k := range []ApprovalKind{ApprovalKindAction, ApprovalKindCommsSend, ApprovalKindHTTPRequest} {
		if gotKinds[k] != 1 {
			t.Errorf("kind %q count=%d want 1", k, gotKinds[k])
		}
	}
}

// TestSeedQueue_EmptyKindDefaultsToAction mirrors the queue's own
// behavior — an unset kind in the JSON becomes ApprovalKindAction
// rather than failing — so fixtures written before #428 (or by
// hand without a kind field) still seed successfully.
func TestSeedQueue_EmptyKindDefaultsToAction(t *testing.T) {
	q := NewActionApprovalQueue(nil, nil)
	ids := SeedQueue(q, []SeedEntry{
		{ActionName: "legacy-action"},
	})
	if len(ids) != 1 {
		t.Fatalf("len(ids)=%d", len(ids))
	}
	a, _ := q.Get(ids[0])
	if a.Kind != ApprovalKindAction {
		t.Errorf("kind=%q want %q", a.Kind, ApprovalKindAction)
	}
}

func writeSeedFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "seed.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write seed: %v", err)
	}
	return path
}
