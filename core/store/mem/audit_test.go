package mem

import (
	"context"
	"testing"
	"time"

	api "github.com/ALRubinger/aileron/core/api/gen"
)

func TestTraceStore_AppendAndGet(t *testing.T) {
	s := NewTraceStore()
	ctx := context.Background()

	event1 := api.TraceEvent{
		EventId:   "evt_1",
		EventType: "intent.submitted",
		Actor:     api.ActorRef{Id: "agent_1", Type: api.Agent},
		Timestamp: time.Now().UTC(),
	}
	event2 := api.TraceEvent{
		EventId:   "evt_2",
		EventType: "policy.evaluated",
		Actor:     api.ActorRef{Id: "aileron", Type: api.Service},
		Timestamp: time.Now().UTC(),
	}

	s.Append(ctx, "int_1", "ws_1", "trc_1", event1)
	s.Append(ctx, "int_1", "ws_1", "trc_1", event2)

	trace, err := s.GetByIntent(ctx, "int_1")
	if err != nil {
		t.Fatalf("GetByIntent: %v", err)
	}
	if trace.TraceId != "trc_1" {
		t.Errorf("TraceId = %q, want %q", trace.TraceId, "trc_1")
	}
	if len(trace.Events) != 2 {
		t.Fatalf("Events = %d, want 2", len(trace.Events))
	}
	if trace.Events[0].EventType != "intent.submitted" {
		t.Errorf("Events[0].EventType = %q, want %q", trace.Events[0].EventType, "intent.submitted")
	}
	if trace.Events[1].EventType != "policy.evaluated" {
		t.Errorf("Events[1].EventType = %q, want %q", trace.Events[1].EventType, "policy.evaluated")
	}
}

func TestTraceStore_GetNotFound(t *testing.T) {
	s := NewTraceStore()
	_, err := s.GetByIntent(context.Background(), "nonexistent")
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestTraceStore_List(t *testing.T) {
	s := NewTraceStore()
	ctx := context.Background()

	s.Append(ctx, "int_1", "ws_1", "trc_1", api.TraceEvent{
		EventId: "evt_1", EventType: "intent.submitted",
		Actor: api.ActorRef{Id: "a", Type: api.Agent}, Timestamp: time.Now().UTC(),
	})
	s.Append(ctx, "int_2", "ws_2", "trc_2", api.TraceEvent{
		EventId: "evt_2", EventType: "intent.submitted",
		Actor: api.ActorRef{Id: "a", Type: api.Agent}, Timestamp: time.Now().UTC(),
	})

	all, _ := s.List(ctx, TraceFilter{})
	if len(all) != 2 {
		t.Errorf("List(all): got %d, want 2", len(all))
	}

	ws1, _ := s.List(ctx, TraceFilter{WorkspaceID: "ws_1"})
	if len(ws1) != 1 {
		t.Errorf("List(ws_1): got %d, want 1", len(ws1))
	}
}
