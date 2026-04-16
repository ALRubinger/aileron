package comms_test

import (
	"context"
	"testing"

	"github.com/ALRubinger/aileron/core/comms"
)

func TestPostEphemeralDraft_EmptyBotToken(t *testing.T) {
	err := comms.PostEphemeralDraft(context.Background(), comms.SlackDraftMessage{
		BotToken: "",
		Channel:  "C123",
		UserID:   "U123",
		DraftID:  "dft_1",
		Draft:    "test draft",
	})
	if err == nil {
		t.Fatal("expected error for empty bot token")
	}
}
