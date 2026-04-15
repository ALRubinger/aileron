package comms_test

import (
	"context"
	"testing"

	"github.com/ALRubinger/aileron/core/comms"
)

func TestSendSlackMessage_EmptyToken(t *testing.T) {
	err := comms.SendSlackMessage(context.Background(), "", "C123", "hello")
	if err == nil {
		t.Fatal("expected error for empty token")
	}
}

func TestSendSlackMessage_EmptyChannel(t *testing.T) {
	err := comms.SendSlackMessage(context.Background(), "xoxp-test", "", "hello")
	if err == nil {
		t.Fatal("expected error for empty channel")
	}
}
