package audit_test

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ALRubinger/aileron/internal/audit"
	"github.com/ALRubinger/aileron/internal/model"
)

// FileStore contract:
//   - Round-trip: Append → close → reopen → ListEvents / GetByEventID
//     surfaces the same data the writer recorded.
//   - Trace-aware Append (AppendToTrace) round-trips via ListTraces.
//   - Concurrent appends from N goroutines all land — none are lost.
//   - A malformed line in the file does not abort replay; remaining
//     entries are loaded and the bad line is logged.
//   - Filter/limit semantics match MemStore (delegation, but pinned).

func newTestFileStore(t *testing.T) (*audit.FileStore, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")
	store, err := audit.NewFileStore(path, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	return store, path
}

func TestFileStore_AppendThenReopen_RoundTrip(t *testing.T) {
	store, path := newTestFileStore(t)
	t0 := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	ev := audit.Event{
		EventID:   "audit-xyz",
		EventType: model.EventTypeActionInstalled,
		Actor:     model.ActorRef{Type: model.ActorTypeHuman, ID: "user"},
		Payload:   map[string]any{"aileron.action.name": "ship-update", "aileron.action.fqn": "github://aileron/slack"},
		Timestamp: t0,
	}
	if err := store.Append(context.Background(), ev); err != nil {
		t.Fatalf("Append: %v", err)
	}

	// Reopen — the new store must replay every appended entry.
	reopened, err := audit.NewFileStore(path, slog.Default())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got, ok := reopened.GetByEventID("audit-xyz")
	if !ok {
		t.Fatalf("event not found after reopen")
	}
	if got.EventID != "audit-xyz" || got.EventType != model.EventTypeActionInstalled {
		t.Errorf("got = %+v", got)
	}
	if got.Payload["aileron.action.name"] != "ship-update" || got.Payload["aileron.action.fqn"] != "github://aileron/slack" {
		t.Errorf("payload = %+v", got.Payload)
	}
	if !got.Timestamp.Equal(t0) {
		t.Errorf("timestamp = %v, want %v", got.Timestamp, t0)
	}
	if got.Actor.Type != model.ActorTypeHuman || got.Actor.ID != "user" {
		t.Errorf("actor = %+v", got.Actor)
	}

	listed, err := reopened.ListEvents(context.Background(), audit.EventFilter{})
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(listed) != 1 || listed[0].EventID != "audit-xyz" {
		t.Errorf("ListEvents = %+v, want one event xyz", listed)
	}
}

func TestFileStore_AppendToTrace_RoundTrip(t *testing.T) {
	store, path := newTestFileStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if err := store.AppendToTrace(ctx, "trace-1", "intent-1", "ws-1", audit.Event{
		EventID:   "e-1",
		EventType: model.EventTypeIntentSubmitted,
		Actor:     model.ActorRef{ID: "u1"},
		Timestamp: now,
	}); err != nil {
		t.Fatalf("AppendToTrace: %v", err)
	}
	if err := store.AppendToTrace(ctx, "trace-1", "intent-1", "ws-1", audit.Event{
		EventID:   "e-2",
		EventType: model.EventTypeExecutionSucceeded,
		Actor:     model.ActorRef{ID: "agent-x"},
		Timestamp: now.Add(time.Second),
	}); err != nil {
		t.Fatalf("AppendToTrace: %v", err)
	}

	reopened, err := audit.NewFileStore(path, slog.Default())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	traces, err := reopened.ListTraces(ctx, audit.Filter{IntentID: "intent-1"})
	if err != nil {
		t.Fatalf("ListTraces: %v", err)
	}
	if len(traces) != 1 {
		t.Fatalf("traces = %d, want 1", len(traces))
	}
	if traces[0].WorkspaceID != "ws-1" || traces[0].IntentID != "intent-1" {
		t.Errorf("trace meta = %+v", traces[0])
	}
	if len(traces[0].Events) != 2 {
		t.Errorf("events = %d, want 2", len(traces[0].Events))
	}

	got, err := reopened.GetTrace(ctx, "trace-1")
	if err != nil {
		t.Fatalf("GetTrace: %v", err)
	}
	if got.WorkspaceID != "ws-1" {
		t.Errorf("GetTrace = %+v", got)
	}
}

func TestFileStore_ConcurrentAppends_AllLand(t *testing.T) {
	store, path := newTestFileStore(t)
	const writers = 8
	const perWriter = 25
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				id := fmt.Sprintf("w%d-i%d", w, i)
				if err := store.Append(context.Background(), audit.Event{
					EventID:   id,
					Timestamp: time.Now().UTC(),
					Payload:   map[string]any{"writer": w, "i": i},
				}); err != nil {
					t.Errorf("Append: %v", err)
					return
				}
			}
		}(w)
	}
	wg.Wait()

	want := writers * perWriter
	listed, err := store.ListEvents(context.Background(), audit.EventFilter{})
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(listed) != want {
		t.Errorf("in-memory count = %d, want %d", len(listed), want)
	}

	// Re-open: the file is the source of truth, so the same count
	// must surface from a cold read.
	reopened, err := audit.NewFileStore(path, slog.Default())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	relisted, _ := reopened.ListEvents(context.Background(), audit.EventFilter{})
	if len(relisted) != want {
		t.Errorf("reopened count = %d, want %d", len(relisted), want)
	}

	// Spot-check that every (writer, i) pair survived.
	seen := map[string]bool{}
	for _, e := range relisted {
		seen[e.EventID] = true
	}
	for w := 0; w < writers; w++ {
		for i := 0; i < perWriter; i++ {
			id := fmt.Sprintf("w%d-i%d", w, i)
			if !seen[id] {
				t.Errorf("missing event %q after reopen", id)
			}
		}
	}
}

func TestFileStore_MalformedLineSkippedWithWarning(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")

	// Pre-seed the file: one good entry, one corrupt line, one good entry.
	good1 := `{"event":{"EventID":"good-1","EventType":"action.installed","Actor":{"ID":"user","Type":"human"},"Payload":{"name":"ship-update"},"Timestamp":"2026-05-01T00:00:00Z"}}`
	good2 := `{"event":{"EventID":"good-2","EventType":"action.installed","Actor":{"ID":"user","Type":"human"},"Payload":{"name":"send-email"},"Timestamp":"2026-05-01T00:01:00Z"}}`
	corrupt := `{this is not json`
	contents := strings.Join([]string{good1, corrupt, good2, ""}, "\n")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	var logBuf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	store, err := audit.NewFileStore(path, log)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}

	// Both good entries must have loaded; the corrupt line must not abort replay.
	if _, ok := store.GetByEventID("good-1"); !ok {
		t.Error("good-1 missing after replay")
	}
	if _, ok := store.GetByEventID("good-2"); !ok {
		t.Error("good-2 missing after replay")
	}
	listed, _ := store.ListEvents(context.Background(), audit.EventFilter{})
	if len(listed) != 2 {
		t.Errorf("loaded = %d, want 2", len(listed))
	}

	// Warning must surface so operators can find the bad line.
	logged := logBuf.String()
	if !strings.Contains(logged, "malformed line") {
		t.Errorf("expected 'malformed line' warning; got: %s", logged)
	}
	if !strings.Contains(logged, "skipped") {
		t.Errorf("expected post-replay 'skipped' tally; got: %s", logged)
	}
}

func TestFileStore_NewOnMissingFileIsEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "deeper", "audit.jsonl")
	store, err := audit.NewFileStore(path, slog.Default())
	if err != nil {
		t.Fatalf("NewFileStore on missing path: %v", err)
	}
	listed, _ := store.ListEvents(context.Background(), audit.EventFilter{})
	if len(listed) != 0 {
		t.Errorf("listed = %d, want 0", len(listed))
	}

	// Append must create the file under the directory we asked for,
	// proving MkdirAll happened in New.
	if err := store.Append(context.Background(), audit.Event{
		EventID: "after-mkdir", Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("file not created at %s: %v", path, err)
	}
}

func TestFileStore_FilterAndLimitDelegateToMem(t *testing.T) {
	// FileStore delegates ListEvents to MemStore — pin behavior so
	// future refactors that re-implement the path don't silently drop
	// filter semantics.
	store, _ := newTestFileStore(t)
	ctx := context.Background()
	t0 := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		_ = store.Append(ctx, audit.Event{
			EventID:   string(rune('a' + i)),
			Timestamp: t0.Add(time.Duration(i) * time.Hour),
			Payload:   map[string]any{"aileron.failure.class": "binding_required"},
		})
	}
	got, err := store.ListEvents(ctx, audit.EventFilter{
		Class: "binding_required",
		Limit: 2,
	})
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(got) != 2 || got[0].EventID != "e" || got[1].EventID != "d" {
		t.Errorf("got = %+v; want [e, d]", got)
	}
}

func TestFileStore_PathReturnsConfiguredPath(t *testing.T) {
	store, path := newTestFileStore(t)
	if store.Path() != path {
		t.Errorf("Path() = %q, want %q", store.Path(), path)
	}
}
