package audit_test

import (
	"context"
	"testing"
	"time"

	"github.com/ALRubinger/aileron/internal/audit"
	"github.com/ALRubinger/aileron/internal/model"
)

// MemStore.ListEvents contract:
//   - Returns flat events newest-first.
//   - Empty filter returns every event.
//   - Each filter field independently narrows the result.
//   - Connector matching looks under three known payload keys.
//   - Limit caps the result; zero or negative means no cap.

func seedEvents(t *testing.T, ev ...audit.Event) *audit.MemStore {
	t.Helper()
	store := audit.NewMemStore()
	for _, e := range ev {
		if err := store.Append(context.Background(), e); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	return store
}

func TestListEvents_OrdersNewestFirst(t *testing.T) {
	now := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	store := seedEvents(t,
		audit.Event{EventID: "old", Timestamp: now},
		audit.Event{EventID: "new", Timestamp: now.Add(time.Hour)},
		audit.Event{EventID: "mid", Timestamp: now.Add(time.Minute)},
	)
	got, err := store.ListEvents(context.Background(), audit.EventFilter{})
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	want := []string{"new", "mid", "old"}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d (%+v)", len(got), len(want), got)
	}
	for i, g := range got {
		if g.EventID != want[i] {
			t.Errorf("got[%d].EventID = %q, want %q", i, g.EventID, want[i])
		}
	}
}

func TestListEvents_FilterSinceIsInclusive(t *testing.T) {
	t0 := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	store := seedEvents(t,
		audit.Event{EventID: "e0", Timestamp: t0},
		audit.Event{EventID: "e1", Timestamp: t0.Add(time.Hour)},
		audit.Event{EventID: "e2", Timestamp: t0.Add(2 * time.Hour)},
	)
	got, _ := store.ListEvents(context.Background(), audit.EventFilter{Since: t0.Add(time.Hour)})
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].EventID != "e2" || got[1].EventID != "e1" {
		t.Errorf("ids = %q,%q; want e2,e1", got[0].EventID, got[1].EventID)
	}
}

func TestListEvents_FilterByEventID(t *testing.T) {
	store := seedEvents(t,
		audit.Event{EventID: "alpha", Timestamp: time.Now()},
		audit.Event{EventID: "beta", Timestamp: time.Now()},
	)
	got, _ := store.ListEvents(context.Background(), audit.EventFilter{EventID: "beta"})
	if len(got) != 1 || got[0].EventID != "beta" {
		t.Errorf("got = %+v; want one event with id beta", got)
	}
	none, _ := store.ListEvents(context.Background(), audit.EventFilter{EventID: "missing"})
	if len(none) != 0 {
		t.Errorf("missing id should return empty, got %+v", none)
	}
}

func TestListEvents_FilterByClassMatchesFailures(t *testing.T) {
	store := seedEvents(t,
		audit.Event{
			EventID:   "fail-1",
			EventType: model.EventTypeExecutionFailed,
			Payload:   map[string]any{"class": "binding_required"},
			Timestamp: time.Now(),
		},
		audit.Event{
			EventID:   "fail-2",
			EventType: model.EventTypeExecutionFailed,
			Payload:   map[string]any{"class": "policy_denied"},
			Timestamp: time.Now(),
		},
		audit.Event{
			// Success event has no `class` — must be filtered out.
			EventID:   "ok",
			EventType: model.EventTypeActionInstalled,
			Payload:   map[string]any{"name": "ship-update"},
			Timestamp: time.Now(),
		},
	)
	got, _ := store.ListEvents(context.Background(), audit.EventFilter{Class: "binding_required"})
	if len(got) != 1 || got[0].EventID != "fail-1" {
		t.Errorf("got = %+v; want only fail-1", got)
	}
}

func TestListEvents_FilterByConnectorFQNAcrossKnownKeys(t *testing.T) {
	const fqn = "github://aileron/slack"
	store := seedEvents(t,
		// Binding-lifecycle event
		audit.Event{
			EventID:   "bind",
			Payload:   map[string]any{"connector_fqn": fqn},
			Timestamp: time.Now(),
		},
		// Action-installed event
		audit.Event{
			EventID:   "act",
			Payload:   map[string]any{"fqn": fqn},
			Timestamp: time.Now(),
		},
		// Failure event with details.connector
		audit.Event{
			EventID: "fail",
			Payload: map[string]any{
				"class":   "binding_required",
				"details": map[string]any{"connector": fqn},
			},
			Timestamp: time.Now(),
		},
		// Unrelated event
		audit.Event{
			EventID:   "other",
			Payload:   map[string]any{"connector_fqn": "github://x/y"},
			Timestamp: time.Now(),
		},
	)
	got, _ := store.ListEvents(context.Background(), audit.EventFilter{ConnectorFQN: fqn})
	ids := map[string]bool{}
	for _, e := range got {
		ids[e.EventID] = true
	}
	for _, want := range []string{"bind", "act", "fail"} {
		if !ids[want] {
			t.Errorf("missing %q in result", want)
		}
	}
	if ids["other"] {
		t.Error("unrelated event leaked into result")
	}
}

func TestListEvents_LimitTruncatesAfterFiltering(t *testing.T) {
	now := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	var seed []audit.Event
	for i := 0; i < 5; i++ {
		seed = append(seed, audit.Event{
			EventID:   string(rune('a' + i)),
			Timestamp: now.Add(time.Duration(i) * time.Hour),
		})
	}
	store := seedEvents(t, seed...)
	got, _ := store.ListEvents(context.Background(), audit.EventFilter{Limit: 2})
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	// newest-first: 'e' then 'd'
	if got[0].EventID != "e" || got[1].EventID != "d" {
		t.Errorf("got = %q,%q; want e,d", got[0].EventID, got[1].EventID)
	}

	all, _ := store.ListEvents(context.Background(), audit.EventFilter{Limit: 0})
	if len(all) != 5 {
		t.Errorf("limit=0 should be uncapped, got %d", len(all))
	}
}

func TestListEvents_FiltersCompose(t *testing.T) {
	const fqn = "github://aileron/slack"
	t0 := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	store := seedEvents(t,
		// Match: right class, right connector, after Since.
		audit.Event{
			EventID:   "want",
			Payload:   map[string]any{"class": "binding_required", "details": map[string]any{"connector": fqn}},
			Timestamp: t0.Add(time.Hour),
		},
		// Right class + connector, but BEFORE Since.
		audit.Event{
			EventID:   "too-old",
			Payload:   map[string]any{"class": "binding_required", "details": map[string]any{"connector": fqn}},
			Timestamp: t0.Add(-time.Hour),
		},
		// Right class + Since, but different connector.
		audit.Event{
			EventID:   "wrong-fqn",
			Payload:   map[string]any{"class": "binding_required", "details": map[string]any{"connector": "github://x/y"}},
			Timestamp: t0.Add(time.Hour),
		},
	)
	got, _ := store.ListEvents(context.Background(), audit.EventFilter{
		Since:        t0,
		Class:        "binding_required",
		ConnectorFQN: fqn,
	})
	if len(got) != 1 || got[0].EventID != "want" {
		t.Errorf("got = %+v; want one event 'want'", got)
	}
}
