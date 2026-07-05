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
			Payload:   map[string]any{"aileron.failure.class": "binding_required"},
			Timestamp: time.Now(),
		},
		audit.Event{
			EventID:   "fail-2",
			EventType: model.EventTypeExecutionFailed,
			Payload:   map[string]any{"aileron.failure.class": "policy_denied"},
			Timestamp: time.Now(),
		},
		audit.Event{
			// Success event carries no failure class — must be filtered out.
			EventID:   "ok",
			EventType: model.EventTypeActionInstalled,
			Payload:   map[string]any{"aileron.action.name": "ship-update"},
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
			Payload:   map[string]any{"aileron.connector.fqn": fqn},
			Timestamp: time.Now(),
		},
		// Action-installed event keyed on the action's own FQN
		audit.Event{
			EventID:   "act",
			Payload:   map[string]any{"aileron.action.fqn": fqn},
			Timestamp: time.Now(),
		},
		// Failure event with nested details.connector
		audit.Event{
			EventID: "fail",
			Payload: map[string]any{
				"aileron.failure.class":   "binding_required",
				"aileron.failure.details": map[string]any{"connector": fqn},
			},
			Timestamp: time.Now(),
		},
		// Unrelated event
		audit.Event{
			EventID:   "other",
			Payload:   map[string]any{"aileron.connector.fqn": "github://x/y"},
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

func TestListEvents_FilterByOutputNameMatchesMaterialized(t *testing.T) {
	store := seedEvents(t,
		audit.Event{
			EventID:   "out-report",
			EventType: model.EventType("output.materialized"),
			Payload: map[string]any{
				"aileron.output.name":         "report.pdf",
				"aileron.output.content_hash": "sha256:aaa",
			},
			Timestamp: time.Now(),
		},
		audit.Event{
			EventID:   "out-summary",
			EventType: model.EventType("output.materialized"),
			Payload: map[string]any{
				"aileron.output.name":         "summary.txt",
				"aileron.output.content_hash": "sha256:bbb",
			},
			Timestamp: time.Now(),
		},
		audit.Event{
			// Non-output event carries no output name — must be excluded.
			EventID:   "install",
			EventType: model.EventTypeActionInstalled,
			Payload:   map[string]any{"aileron.action.name": "ship-update"},
			Timestamp: time.Now(),
		},
	)
	got, _ := store.ListEvents(context.Background(), audit.EventFilter{OutputName: "report.pdf"})
	if len(got) != 1 || got[0].EventID != "out-report" {
		t.Errorf("got = %+v; want only out-report", got)
	}
}

// TestListEvents_ToolStepOutputIsQueryableByExistingFilters proves the store is
// producer-agnostic (#1762): an output.materialized event produced by a rung-3
// tool step — carrying aileron.step.kind:"tool" and aileron.step.image alongside
// the standard output name/content_hash — is found by the SAME --content-hash and
// --output filters the connector path uses, with no new query path. A different
// content-hash does not match (negative).
func TestListEvents_ToolStepOutputIsQueryableByExistingFilters(t *testing.T) {
	const toolDigest = "sha256:0011223344556677"
	store := seedEvents(t,
		audit.Event{
			EventID:   "tool-out",
			EventType: model.EventType("output.materialized"),
			Payload: map[string]any{
				"aileron.output.name":         "extract.txt",
				"aileron.output.content_hash": toolDigest,
				"aileron.step.kind":           "tool",
				"aileron.step.image":          "registry.example.com/tool-a:1@sha256:beef",
			},
			Timestamp: time.Now(),
		},
	)

	byHash, _ := store.ListEvents(context.Background(), audit.EventFilter{ContentHash: toolDigest})
	if len(byHash) != 1 || byHash[0].EventID != "tool-out" {
		t.Errorf("--content-hash must surface the tool-step record, got %+v", byHash)
	}
	byName, _ := store.ListEvents(context.Background(), audit.EventFilter{OutputName: "extract.txt"})
	if len(byName) != 1 || byName[0].EventID != "tool-out" {
		t.Errorf("--output must surface the tool-step record, got %+v", byName)
	}
	miss, _ := store.ListEvents(context.Background(), audit.EventFilter{ContentHash: "sha256:deadbeef"})
	if len(miss) != 0 {
		t.Errorf("a different content-hash must not match, got %+v", miss)
	}
}

func TestListEvents_FilterByContentHashIsExact(t *testing.T) {
	const digest = "sha256:0123456789abcdef"
	store := seedEvents(t,
		audit.Event{
			EventID:   "match",
			EventType: model.EventType("output.materialized"),
			Payload: map[string]any{
				"aileron.output.name":         "report.pdf",
				"aileron.output.content_hash": digest,
			},
			Timestamp: time.Now(),
		},
		audit.Event{
			EventID:   "other-hash",
			EventType: model.EventType("output.materialized"),
			Payload: map[string]any{
				"aileron.output.name":         "report.pdf",
				"aileron.output.content_hash": "sha256:ffff",
			},
			Timestamp: time.Now(),
		},
	)
	got, _ := store.ListEvents(context.Background(), audit.EventFilter{ContentHash: digest})
	if len(got) != 1 || got[0].EventID != "match" {
		t.Errorf("got = %+v; want only match", got)
	}

	// A partial/prefix hash must not match — equality is exact.
	partial, _ := store.ListEvents(context.Background(), audit.EventFilter{ContentHash: "sha256:0123"})
	if len(partial) != 0 {
		t.Errorf("partial hash should not match, got %+v", partial)
	}

	// A hash present on no event returns empty.
	none, _ := store.ListEvents(context.Background(), audit.EventFilter{ContentHash: "sha256:deadbeef"})
	if len(none) != 0 {
		t.Errorf("unknown hash should return empty, got %+v", none)
	}
}

// TestListEvents_FilterByContentHashIgnoresPlanKey locks that the
// ContentHash filter matches only on `aileron.output.content_hash` and
// never on `aileron.plan.content_hash`. The plan-level content hash is a
// real key emitted by the flight-plan runtime (see
// internal/flightplan/runtime/audit.go), so a matcher that read it would
// return output.materialized events whose OUTPUT hash differs from the
// requested digest.
func TestListEvents_FilterByContentHashIgnoresPlanKey(t *testing.T) {
	const digest = "sha256:0123456789abcdef"
	store := seedEvents(t,
		// Plan hash equals the requested digest, but the output hash
		// differs — this event must NOT be returned.
		audit.Event{
			EventID:   "plan-hash-only",
			EventType: model.EventType("output.materialized"),
			Payload: map[string]any{
				"aileron.output.name":         "report.pdf",
				"aileron.plan.content_hash":   digest,
				"aileron.output.content_hash": "sha256:ffff",
			},
			Timestamp: time.Now(),
		},
	)
	got, _ := store.ListEvents(context.Background(), audit.EventFilter{ContentHash: digest})
	if len(got) != 0 {
		t.Errorf("plan-level content_hash must not match output filter, got %+v", got)
	}
}

func TestListEvents_OutputFiltersCompose(t *testing.T) {
	t0 := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	const digest = "sha256:cafe"
	store := seedEvents(t,
		// Match: right name, right hash, after Since.
		audit.Event{
			EventID: "want",
			Payload: map[string]any{
				"aileron.output.name":         "report.pdf",
				"aileron.output.content_hash": digest,
			},
			Timestamp: t0.Add(time.Hour),
		},
		// Right name + hash, but BEFORE Since.
		audit.Event{
			EventID: "too-old",
			Payload: map[string]any{
				"aileron.output.name":         "report.pdf",
				"aileron.output.content_hash": digest,
			},
			Timestamp: t0.Add(-time.Hour),
		},
		// Right name + Since, but different hash.
		audit.Event{
			EventID: "wrong-hash",
			Payload: map[string]any{
				"aileron.output.name":         "report.pdf",
				"aileron.output.content_hash": "sha256:beef",
			},
			Timestamp: t0.Add(time.Hour),
		},
	)
	got, _ := store.ListEvents(context.Background(), audit.EventFilter{
		Since:       t0,
		OutputName:  "report.pdf",
		ContentHash: digest,
	})
	if len(got) != 1 || got[0].EventID != "want" {
		t.Errorf("got = %+v; want one event 'want'", got)
	}
}

// TestListEvents_FilterByInvocationIDIsExact locks the InvocationID
// filter: it returns exactly the events carrying that launch-scoped id
// under `aileron.invocation.id`, excludes events from a different
// invocation, and matches by exact string equality (no prefix match,
// unknown id returns empty).
func TestListEvents_FilterByInvocationIDIsExact(t *testing.T) {
	const inv = "11111111-1111-1111-1111-111111111111"
	store := seedEvents(t,
		audit.Event{
			EventID:   "a-in-inv",
			EventType: model.EventType("output.materialized"),
			Payload:   map[string]any{"aileron.invocation.id": inv},
			Timestamp: time.Now(),
		},
		audit.Event{
			EventID:   "b-in-inv",
			EventType: model.EventTypeActionInstalled,
			Payload:   map[string]any{"aileron.invocation.id": inv},
			Timestamp: time.Now(),
		},
		audit.Event{
			EventID:   "other-inv",
			EventType: model.EventType("output.materialized"),
			Payload:   map[string]any{"aileron.invocation.id": "22222222-2222-2222-2222-222222222222"},
			Timestamp: time.Now(),
		},
		audit.Event{
			EventID:   "no-inv",
			EventType: model.EventTypeActionInstalled,
			Payload:   map[string]any{"aileron.action.name": "ship"},
			Timestamp: time.Now(),
		},
	)

	got, _ := store.ListEvents(context.Background(), audit.EventFilter{InvocationID: inv})
	if len(got) != 2 {
		t.Fatalf("want 2 events for the invocation, got %d: %+v", len(got), got)
	}
	seen := map[string]bool{}
	for _, e := range got {
		seen[e.EventID] = true
	}
	if !seen["a-in-inv"] || !seen["b-in-inv"] {
		t.Errorf("want both events of the invocation, got %+v", got)
	}

	// A prefix of the id must not match — equality is exact.
	partial, _ := store.ListEvents(context.Background(), audit.EventFilter{InvocationID: "11111111"})
	if len(partial) != 0 {
		t.Errorf("partial invocation id should not match, got %+v", partial)
	}

	// An unknown id returns empty.
	none, _ := store.ListEvents(context.Background(), audit.EventFilter{InvocationID: "deadbeef-0000-0000-0000-000000000000"})
	if len(none) != 0 {
		t.Errorf("unknown invocation id should return empty, got %+v", none)
	}
}

// TestListEvents_ReturnsLaunchAndReachAlongsideOutputs is the #1928 regression
// crossing the runtime→daemon-flatten→filter seam. The flightplan.launch and
// flightplan.launch.reach records now carry the launch-scoped
// aileron.invocation.id at the top level of their payload (the same place the
// daemon sink surfaces the runtime's flat Fields), so an invocation-filtered
// query returns them alongside the materialized outputs. Before the fix only the
// output.materialized record carried the id, so the launch and reach records were
// dropped from GET /v1/audit?invocation_id=<id> and invisible in the /audit
// Timeline. This asserts the filter contract those flat payloads rely on.
func TestListEvents_ReturnsLaunchAndReachAlongsideOutputs(t *testing.T) {
	const inv = "11111111-1111-1111-1111-111111111111"
	store := seedEvents(t,
		// output.materialized already carried the id before the fix.
		audit.Event{
			EventID:   "output",
			EventType: model.EventTypeOutputMaterialized,
			Payload:   map[string]any{"aileron.invocation.id": inv, "aileron.output.name": "digest.csv"},
			Timestamp: time.Now(),
		},
		// flightplan.launch.reach now carries the id at the top level (#1928).
		audit.Event{
			EventID:   "reach",
			EventType: model.EventTypeFlightPlanLaunchReach,
			Payload:   map[string]any{"aileron.invocation.id": inv, "aileron.step.id": "extract"},
			Timestamp: time.Now(),
		},
		// flightplan.launch (per-launch summary) now carries the id at the top
		// level, flat (no "fields" nesting) so the filter reads it (#1928).
		audit.Event{
			EventID:   "launch",
			EventType: model.EventTypeFlightPlanLaunch,
			Payload:   map[string]any{"aileron.invocation.id": inv, "sourceInputBindings": map[string]any{}},
			Timestamp: time.Now(),
		},
	)

	got, _ := store.ListEvents(context.Background(), audit.EventFilter{InvocationID: inv})
	if len(got) != 3 {
		t.Fatalf("want 3 events for the invocation (output+reach+launch), got %d: %+v", len(got), got)
	}
	seen := map[string]bool{}
	for _, e := range got {
		seen[e.EventID] = true
	}
	if !seen["output"] || !seen["reach"] || !seen["launch"] {
		t.Errorf("invocation filter dropped a record: got %+v, want output+reach+launch", seen)
	}
}

// TestListEvents_InvocationIDComposesWithOtherFilters proves the
// InvocationID filter AND-composes with Since and ContentHash: only the
// event matching all three is returned.
func TestListEvents_InvocationIDComposesWithOtherFilters(t *testing.T) {
	t0 := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	const inv = "11111111-1111-1111-1111-111111111111"
	const digest = "sha256:cafe"
	store := seedEvents(t,
		// Match: right invocation, right hash, after Since.
		audit.Event{
			EventID: "want",
			Payload: map[string]any{
				"aileron.invocation.id":       inv,
				"aileron.output.content_hash": digest,
			},
			Timestamp: t0.Add(time.Hour),
		},
		// Right invocation + hash, but BEFORE Since.
		audit.Event{
			EventID: "too-old",
			Payload: map[string]any{
				"aileron.invocation.id":       inv,
				"aileron.output.content_hash": digest,
			},
			Timestamp: t0.Add(-time.Hour),
		},
		// Right hash + Since, but a different invocation.
		audit.Event{
			EventID: "other-inv",
			Payload: map[string]any{
				"aileron.invocation.id":       "22222222-2222-2222-2222-222222222222",
				"aileron.output.content_hash": digest,
			},
			Timestamp: t0.Add(time.Hour),
		},
	)
	got, _ := store.ListEvents(context.Background(), audit.EventFilter{
		Since:        t0,
		InvocationID: inv,
		ContentHash:  digest,
	})
	if len(got) != 1 || got[0].EventID != "want" {
		t.Errorf("got = %+v; want one event 'want'", got)
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
			Payload:   map[string]any{"aileron.failure.class": "binding_required", "aileron.failure.details": map[string]any{"connector": fqn}},
			Timestamp: t0.Add(time.Hour),
		},
		// Right class + connector, but BEFORE Since.
		audit.Event{
			EventID:   "too-old",
			Payload:   map[string]any{"aileron.failure.class": "binding_required", "aileron.failure.details": map[string]any{"connector": fqn}},
			Timestamp: t0.Add(-time.Hour),
		},
		// Right class + Since, but different connector.
		audit.Event{
			EventID:   "wrong-fqn",
			Payload:   map[string]any{"aileron.failure.class": "binding_required", "aileron.failure.details": map[string]any{"connector": "github://x/y"}},
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
