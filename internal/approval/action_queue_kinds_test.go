package approval_test

import (
	"context"
	"testing"
	"time"

	"github.com/ALRubinger/aileron/internal/approval"
)

// Kind discriminator + EditedPayload contract (#428):
//
//   - Register defaults Kind to ApprovalKindAction.
//   - RegisterCommsSend / RegisterCommsDraft / RegisterHTTPRequest
//     stamp the right Kind and populate Args with the kind-specific
//     fields the webapp's per-kind cards rely on.
//   - Decide carries the edited payload through to Wait so the
//     CommsServer can dispatch the user-edited bytes.
//   - The resolved event broadcast on Subscribe carries the entry's
//     Kind so the webapp can route resolution UI per kind.

func TestRegister_DefaultsToActionKind(t *testing.T) {
	q := approval.NewActionApprovalQueue(nil, nil)
	a := q.Register("send-email", "github://x/y", "session-1", map[string]any{"to": "alice"})
	if a.Kind != approval.ApprovalKindAction {
		t.Errorf("Kind = %q, want %q", a.Kind, approval.ApprovalKindAction)
	}
}

func TestRegisterKind_EmptyKindFallsBackToAction(t *testing.T) {
	q := approval.NewActionApprovalQueue(nil, nil)
	a := q.RegisterKind("", "x", "", "", nil)
	if a.Kind != approval.ApprovalKindAction {
		t.Errorf("Kind = %q, want fallback to %q", a.Kind, approval.ApprovalKindAction)
	}
}

func TestRegisterCommsSend_StampsKindAndArgs(t *testing.T) {
	q := approval.NewActionApprovalQueue(nil, nil)
	a := q.RegisterCommsSend("slack", "#general", "hello", "session-1")
	if a.Kind != approval.ApprovalKindCommsSend {
		t.Errorf("Kind = %q, want %q", a.Kind, approval.ApprovalKindCommsSend)
	}
	if a.ActionName != "send_message" {
		t.Errorf("ActionName = %q, want \"send_message\"", a.ActionName)
	}
	if a.Args["service"] != "slack" || a.Args["channel"] != "#general" || a.Args["body"] != "hello" {
		t.Errorf("Args = %+v, want service=slack channel=#general body=hello", a.Args)
	}
}

func TestRegisterCommsDraft_StampsAllOriginalAndDraftFields(t *testing.T) {
	q := approval.NewActionApprovalQueue(nil, nil)
	a := q.RegisterCommsDraft("slack", "#general", "alice", "ping", "pong", "msg-1", "session-1")
	if a.Kind != approval.ApprovalKindCommsDraft {
		t.Errorf("Kind = %q, want %q", a.Kind, approval.ApprovalKindCommsDraft)
	}
	want := map[string]any{
		"service":         "slack",
		"channel":         "#general",
		"original_author": "alice",
		"original_body":   "ping",
		"draft_body":      "pong",
		"reply_to":        "msg-1",
	}
	for k, v := range want {
		if a.Args[k] != v {
			t.Errorf("Args[%q] = %v, want %v", k, a.Args[k], v)
		}
	}
}

func TestRegisterHTTPRequest_StampsSecretNameNotValue(t *testing.T) {
	q := approval.NewActionApprovalQueue(nil, nil)
	a := q.RegisterHTTPRequest("POST", "https://api.linear.app/graphql", `{"q":"x"}`, "bindings/api_key/linear/work", "session-1")
	if a.Kind != approval.ApprovalKindHTTPRequest {
		t.Errorf("Kind = %q, want %q", a.Kind, approval.ApprovalKindHTTPRequest)
	}
	if a.Args["method"] != "POST" || a.Args["url"] != "https://api.linear.app/graphql" {
		t.Errorf("method/url args missing: %+v", a.Args)
	}
	if a.Args["secret_name"] != "bindings/api_key/linear/work" {
		t.Errorf("secret_name = %v, want bindings/api_key/linear/work", a.Args["secret_name"])
	}
	// Sanity: never carry a `secret_value` field.
	if _, ok := a.Args["secret_value"]; ok {
		t.Error("Args carried secret_value; the daemon must never surface raw bytes to the webapp")
	}
}

func TestDecide_EditedPayloadFlowsToWait(t *testing.T) {
	q := approval.NewActionApprovalQueue(nil, nil)
	a := q.RegisterCommsDraft("slack", "#general", "alice", "ping", "pong", "msg-1", "session-1")

	go func() {
		// Decide on a goroutine so Wait below has something to receive.
		time.Sleep(5 * time.Millisecond)
		_ = q.Decide(a.ID, true, "", map[string]any{"body": "pong — got it"})
	}()

	decision, err := a.Wait(context.Background(), 1*time.Second)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if !decision.Approved {
		t.Error("Approved = false; want true")
	}
	body, ok := decision.EditedPayload["body"].(string)
	if !ok || body != "pong — got it" {
		t.Errorf("EditedPayload.body = %v, want \"pong — got it\"", decision.EditedPayload["body"])
	}
}

func TestSubscribe_ResolvedEventCarriesKind(t *testing.T) {
	q := approval.NewActionApprovalQueue(nil, nil)
	events, cancel := q.Subscribe()
	defer cancel()

	a := q.RegisterCommsSend("slack", "#general", "hi", "")
	// Drain the pending event; we're testing the resolved payload.
	<-events

	if err := q.Decide(a.ID, true, "", nil); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	select {
	case e := <-events:
		if e.Type != approval.ActionApprovalEventResolved {
			t.Fatalf("event type = %v, want resolved", e.Type)
		}
		if e.Resolved == nil {
			t.Fatal("Resolved is nil")
		}
		if e.Resolved.Kind != approval.ApprovalKindCommsSend {
			t.Errorf("Resolved.Kind = %q, want %q", e.Resolved.Kind, approval.ApprovalKindCommsSend)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("no resolved event after Decide")
	}
}
