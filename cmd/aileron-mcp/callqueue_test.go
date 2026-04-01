package main

import "testing"

func TestCallQueue_EnqueueAndDequeue(t *testing.T) {
	q := newCallQueue()

	call := queuedCall{
		ServerName:    "github",
		ToolName:      "create_pr",
		QualifiedName: "github__create_pr",
		Arguments:     map[string]any{"base": "main"},
	}

	q.enqueue("apr_1", call)

	if q.len() != 1 {
		t.Errorf("len = %d, want 1", q.len())
	}

	got, ok := q.dequeue("apr_1")
	if !ok {
		t.Fatal("expected to find queued call")
	}
	if got.ToolName != "create_pr" {
		t.Errorf("ToolName = %q, want %q", got.ToolName, "create_pr")
	}
	if got.ServerName != "github" {
		t.Errorf("ServerName = %q, want %q", got.ServerName, "github")
	}

	// Should be removed after dequeue.
	if q.len() != 0 {
		t.Errorf("len after dequeue = %d, want 0", q.len())
	}

	_, ok = q.dequeue("apr_1")
	if ok {
		t.Error("expected dequeue to return false after removal")
	}
}

func TestCallQueue_DequeueNotFound(t *testing.T) {
	q := newCallQueue()

	_, ok := q.dequeue("nonexistent")
	if ok {
		t.Error("expected false for nonexistent approval ID")
	}
}

func TestCallQueue_Peek(t *testing.T) {
	q := newCallQueue()

	call := queuedCall{
		ServerName: "stripe",
		ToolName:   "create_charge",
	}
	q.enqueue("apr_2", call)

	got, ok := q.peek("apr_2")
	if !ok {
		t.Fatal("expected to find queued call via peek")
	}
	if got.ToolName != "create_charge" {
		t.Errorf("ToolName = %q, want %q", got.ToolName, "create_charge")
	}

	// Peek should not remove.
	if q.len() != 1 {
		t.Errorf("len after peek = %d, want 1", q.len())
	}
}

func TestQueuedCall_Summary(t *testing.T) {
	call := queuedCall{
		ServerName: "github",
		ToolName:   "create_pr",
	}
	want := "create_pr (via github)"
	if got := call.summary(); got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
}
