package action

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/ALRubinger/aileron/internal/cstore"
)

// installPreviewAction writes an action manifest that declares both an
// approval-required execute step and an [approval.preview] directive
// targeting the same connector. Returns the action store. Mirrors the
// `send-draft` worked example from ADR-0016 against a synthetic
// connector so the executor's preview path can be exercised without
// spinning up real WASM.
func installPreviewAction(t *testing.T, fqn, version, hash string) *Store {
	t.Helper()
	dir := t.TempDir()
	manifest := `+++
name = "send-draft"
version = "1.0.0"
source = "hub://test/send-draft@1.0.0"

[[requires.connectors]]
name = "` + fqn + `"
version = "` + version + `"
hash = "` + hash + `"
capabilities = ["send_draft", "get_draft"]

[match]
intent = "send a draft"

[[inputs]]
name = "draft_id"
type = "string"
description = "Draft id"

[[execute]]
id = "send"
connector = "` + fqn + `"
op = "send_draft"

[approval]
required = true

[approval.preview]
op = "get_draft"
args = { id = "${args.draft_id}" }

[approval.preview.render]
To = "message.payload.headers.To"
Subject = "message.payload.headers.Subject"
Preview = "message.snippet"
+++

# Send Draft
`
	if err := os.WriteFile(filepath.Join(dir, "send-draft.md"), []byte(manifest), 0o644); err != nil {
		t.Fatalf("write action: %v", err)
	}
	store := NewStore(dir)
	if _, err := store.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	return store
}

// TestSandboxExecutor_InvokePreview_HappyPath verifies the full ADR-0016
// preview flow against a Gmail-shaped response: the runtime calls the
// preview op with interpolated args, walks the manifest's render paths
// (including the header-array shorthand for headers.To / Subject), and
// returns the {label, value} fields in the manifest's declared order.
// Asserts the wire-shape contract the approval prompt depends on.
func TestSandboxExecutor_InvokePreview_HappyPath(t *testing.T) {
	cstoreDir := t.TempDir()
	store := cstore.NewStore(cstoreDir)
	const fqn = "github://test/google"
	hash := installFakeConnector(t, store, fqn, "1.0.0")
	actions := installPreviewAction(t, fqn, "1.0.0", hash)

	gmailResponse := map[string]any{
		"message": map[string]any{
			"payload": map[string]any{
				"headers": []any{
					map[string]any{"name": "To", "value": "alice@example.com"},
					map[string]any{"name": "Subject", "value": "Weekly recap"},
				},
			},
			"snippet": "Here's the recap from this week's standup...",
		},
	}
	rt := &fakeRuntime{}
	rt.connectors = map[string]*fakeConnector{fqn: {resp: gmailResponse}}

	exec := NewSandboxExecutor(actions, store, rt, nil)
	t.Cleanup(func() { _ = exec.Close(context.Background()) })

	loaded, err := actions.Get("send-draft")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	got := exec.InvokePreview(context.Background(), loaded.Manifest, map[string]any{"draft_id": "r-12345"})
	if got.Unavailable != "" {
		t.Fatalf("Unavailable = %q, want empty (happy path)", got.Unavailable)
	}

	// Order from manifest: To, Subject, Preview.
	want := []PreviewField{
		{Label: "To", Value: "alice@example.com"},
		{Label: "Subject", Value: "Weekly recap"},
		{Label: "Preview", Value: "Here's the recap from this week's standup..."},
	}
	if len(got.Fields) != len(want) {
		t.Fatalf("Fields length = %d, want %d (got %+v)", len(got.Fields), len(want), got.Fields)
	}
	for i, w := range want {
		if got.Fields[i] != w {
			t.Errorf("Fields[%d] = %+v, want %+v", i, got.Fields[i], w)
		}
	}

	// Preview op was invoked with the interpolated draft id.
	calls := rt.connectors[fqn].calls
	if len(calls) != 1 {
		t.Fatalf("connector calls = %d, want 1", len(calls))
	}
	if calls[0].Op != "get_draft" {
		t.Errorf("call op = %q, want %q", calls[0].Op, "get_draft")
	}
	if calls[0].Args["id"] != "r-12345" {
		t.Errorf("call args[id] = %v, want %q (after ${args.draft_id} interpolation)", calls[0].Args["id"], "r-12345")
	}
}

// TestSandboxExecutor_InvokePreview_ConnectorError surfaces the
// runtime's "Preview unavailable: <reason>" fallback (ADR-0016) when
// the upstream op returns an error. The approval still proceeds, so
// the result's Unavailable field is the user-facing reason and
// Fields is nil. Without this, a 404 from Gmail would silently render
// an empty preview that looks like the draft exists.
func TestSandboxExecutor_InvokePreview_ConnectorError(t *testing.T) {
	cstoreDir := t.TempDir()
	store := cstore.NewStore(cstoreDir)
	const fqn = "github://test/google"
	hash := installFakeConnector(t, store, fqn, "1.0.0")
	actions := installPreviewAction(t, fqn, "1.0.0", hash)

	rt := &fakeRuntime{}
	rt.connectors = map[string]*fakeConnector{fqn: {err: errors.New("Gmail API returned 404")}}

	exec := NewSandboxExecutor(actions, store, rt, nil)
	t.Cleanup(func() { _ = exec.Close(context.Background()) })

	loaded, err := actions.Get("send-draft")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	got := exec.InvokePreview(context.Background(), loaded.Manifest, map[string]any{"draft_id": "wrong-id"})
	if got.Unavailable == "" {
		t.Fatalf("Unavailable empty; want a 'preview unavailable: ...' fallback")
	}
	if len(got.Fields) != 0 {
		t.Errorf("Fields = %+v, want empty on wholesale failure", got.Fields)
	}
}

// TestSandboxExecutor_InvokePreview_MultilineFlag asserts that fields
// named in the manifest's `multiline` list (ADR-0016) carry
// Multiline=true through to the rendered PreviewResult, so the
// approval surface can render them as scrollable blockquotes rather
// than inline `key: value` rows. The flag must propagate regardless
// of whether the field's path resolved (a missing multiline field is
// still flagged so the UI renders the empty blockquote position
// consistently).
func TestSandboxExecutor_InvokePreview_MultilineFlag(t *testing.T) {
	cstoreDir := t.TempDir()
	store := cstore.NewStore(cstoreDir)
	const fqn = "github://test/google"
	hash := installFakeConnector(t, store, fqn, "1.0.0")

	dir := t.TempDir()
	manifest := `+++
name = "send-draft"
version = "1.0.0"
source = "hub://test/send-draft@1.0.0"

[[requires.connectors]]
name = "` + fqn + `"
version = "1.0.0"
hash = "` + hash + `"
capabilities = ["send_draft", "get_draft"]

[match]
intent = "send a draft"

[[inputs]]
name = "draft_id"
type = "string"
description = "Draft id"

[[execute]]
id = "send"
connector = "` + fqn + `"
op = "send_draft"

[approval]
required = true

[approval.preview]
op = "get_draft"
args = { id = "${args.draft_id}" }
multiline = ["Body"]

[approval.preview.render]
To = "message.payload.headers.To"
Body = "message.snippet"
+++

# Send Draft
`
	if err := os.WriteFile(filepath.Join(dir, "send-draft.md"), []byte(manifest), 0o644); err != nil {
		t.Fatalf("write action: %v", err)
	}
	actions := NewStore(dir)
	if _, err := actions.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}

	gmailResponse := map[string]any{
		"message": map[string]any{
			"payload": map[string]any{
				"headers": []any{
					map[string]any{"name": "To", "value": "alice@example.com"},
				},
			},
			"snippet": "Hi Alice — here is the weekly recap. Lots of detail below.",
		},
	}
	rt := &fakeRuntime{}
	rt.connectors = map[string]*fakeConnector{fqn: {resp: gmailResponse}}

	exec := NewSandboxExecutor(actions, store, rt, nil)
	t.Cleanup(func() { _ = exec.Close(context.Background()) })

	loaded, err := actions.Get("send-draft")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	got := exec.InvokePreview(context.Background(), loaded.Manifest, map[string]any{"draft_id": "r-1"})
	if got.Unavailable != "" {
		t.Fatalf("Unavailable = %q, want empty", got.Unavailable)
	}

	var to, body *PreviewField
	for i := range got.Fields {
		switch got.Fields[i].Label {
		case "To":
			to = &got.Fields[i]
		case "Body":
			body = &got.Fields[i]
		}
	}
	if to == nil || body == nil {
		t.Fatalf("missing fields in result: got %+v", got.Fields)
	}
	if to.Multiline {
		t.Errorf("To.Multiline = true, want false (not in `multiline` list)")
	}
	if !body.Multiline {
		t.Errorf("Body.Multiline = false, want true (`multiline = [\"Body\"]`)")
	}
	if body.Value == "" {
		t.Errorf("Body.Value empty, want resolved snippet")
	}
}

// TestSandboxExecutor_InvokePreview_PathMissPerField asserts that a
// successful preview call whose response does not contain every
// declared render path surfaces the missing fields with Missing=true
// rather than failing the whole preview. The user sees "n/a" for
// those rows and still gets the resolved rows; the approval proceeds.
func TestSandboxExecutor_InvokePreview_PathMissPerField(t *testing.T) {
	cstoreDir := t.TempDir()
	store := cstore.NewStore(cstoreDir)
	const fqn = "github://test/google"
	hash := installFakeConnector(t, store, fqn, "1.0.0")
	actions := installPreviewAction(t, fqn, "1.0.0", hash)

	// Response is missing the Subject header and the snippet.
	gmailResponse := map[string]any{
		"message": map[string]any{
			"payload": map[string]any{
				"headers": []any{
					map[string]any{"name": "To", "value": "alice@example.com"},
				},
			},
		},
	}
	rt := &fakeRuntime{}
	rt.connectors = map[string]*fakeConnector{fqn: {resp: gmailResponse}}

	exec := NewSandboxExecutor(actions, store, rt, nil)
	t.Cleanup(func() { _ = exec.Close(context.Background()) })

	loaded, err := actions.Get("send-draft")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	got := exec.InvokePreview(context.Background(), loaded.Manifest, map[string]any{"draft_id": "r-1"})
	if got.Unavailable != "" {
		t.Fatalf("Unavailable = %q, want empty (per-field misses don't fail the whole preview)", got.Unavailable)
	}
	if len(got.Fields) != 3 {
		t.Fatalf("Fields length = %d, want 3", len(got.Fields))
	}
	want := []PreviewField{
		{Label: "To", Value: "alice@example.com"},
		{Label: "Subject", Missing: true},
		{Label: "Preview", Missing: true},
	}
	for i, w := range want {
		if got.Fields[i] != w {
			t.Errorf("Fields[%d] = %+v, want %+v", i, got.Fields[i], w)
		}
	}
}
