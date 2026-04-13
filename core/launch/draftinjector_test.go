package launch_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/core/launch"
)

func TestDraftInjector_Inject(t *testing.T) {
	var buf bytes.Buffer
	di := launch.NewDraftInjector(&buf)

	di.Inject(launch.Message{
		ID:      "msg-1",
		Source:  "slack",
		Channel: "#backend",
		Author:  "Sarah",
		Body:    "Does the new auth middleware change the JWT claims?",
	})

	out := buf.String()

	if !strings.Contains(out, "msg-1") {
		t.Error("expected message ID in injected prompt")
	}
	if !strings.Contains(out, "read_messages") {
		t.Error("expected read_messages tool reference in injected prompt")
	}
	if !strings.Contains(out, "draft_reply") {
		t.Error("expected draft_reply tool reference in injected prompt")
	}
	if !strings.HasSuffix(out, "\n") {
		t.Error("expected trailing newline to submit as user input")
	}
}

func TestDraftInjector_InjectMultiple(t *testing.T) {
	var buf bytes.Buffer
	di := launch.NewDraftInjector(&buf)

	di.Inject(launch.Message{ID: "1", Source: "slack", Channel: "#general", Author: "Alice", Body: "first"})
	di.Inject(launch.Message{ID: "2", Source: "discord", Channel: "dev-chat", Author: "Bob", Body: "second"})

	out := buf.String()
	if strings.Count(out, "\n") != 2 {
		t.Errorf("expected 2 newlines for 2 injections, got %d", strings.Count(out, "\n"))
	}
}

func TestDraftInjector_InjectRevision(t *testing.T) {
	var buf bytes.Buffer
	di := launch.NewDraftInjector(&buf)

	di.InjectRevision(launch.Message{
		ID:      "msg-1",
		Source:  "slack",
		Channel: "#backend",
		Author:  "Sarah",
		Body:    "Does the new auth middleware change the JWT claims?",
		Draft:   "Yes, we added a role claim.",
	}, "make it more casual and mention the PR number")

	out := buf.String()

	if !strings.Contains(out, "msg-1") {
		t.Error("expected message ID in revision prompt")
	}
	if !strings.Contains(out, "make it more casual") {
		t.Error("expected user feedback in revision prompt")
	}
	if !strings.Contains(out, "draft_reply") {
		t.Error("expected draft_reply tool reference in revision prompt")
	}
	if !strings.HasSuffix(out, "\n") {
		t.Error("expected trailing newline to submit as user input")
	}
}
