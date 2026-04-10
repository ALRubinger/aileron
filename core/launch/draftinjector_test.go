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
		Source:  "slack",
		Channel: "#backend",
		Author:  "Sarah",
		Body:    "Does the new auth middleware change the JWT claims?",
	})

	out := buf.String()

	if !strings.Contains(out, "#backend") {
		t.Error("expected channel in injected prompt")
	}
	if !strings.Contains(out, "Sarah") {
		t.Error("expected author in injected prompt")
	}
	if !strings.Contains(out, "JWT claims") {
		t.Error("expected message body in injected prompt")
	}
	if !strings.Contains(out, "send_message") {
		t.Error("expected send_message tool reference in injected prompt")
	}
	if !strings.Contains(out, `service="slack"`) {
		t.Error("expected service parameter in injected prompt")
	}
	if !strings.Contains(out, `channel="#backend"`) {
		t.Error("expected channel parameter in injected prompt")
	}
	if !strings.HasSuffix(out, "\n") {
		t.Error("expected trailing newline to submit as user input")
	}
}

func TestDraftInjector_InjectMultiple(t *testing.T) {
	var buf bytes.Buffer
	di := launch.NewDraftInjector(&buf)

	di.Inject(launch.Message{Source: "slack", Channel: "#general", Author: "Alice", Body: "first"})
	di.Inject(launch.Message{Source: "discord", Channel: "dev-chat", Author: "Bob", Body: "second"})

	out := buf.String()
	if strings.Count(out, "\n") != 2 {
		t.Errorf("expected 2 newlines for 2 injections, got %d", strings.Count(out, "\n"))
	}
	if !strings.Contains(out, "Alice") {
		t.Error("expected first author")
	}
	if !strings.Contains(out, "Bob") {
		t.Error("expected second author")
	}
}
