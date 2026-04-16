package comms_test

import (
	"context"
	"strings"
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

func TestPostEphemeralDraft_EmptyChannel(t *testing.T) {
	err := comms.PostEphemeralDraft(context.Background(), comms.SlackDraftMessage{
		BotToken: "xoxb-test",
		Channel:  "",
		UserID:   "U123",
	})
	if err == nil {
		t.Fatal("expected error for empty channel")
	}
}

func TestPostEphemeralDraft_EmptyUserID(t *testing.T) {
	err := comms.PostEphemeralDraft(context.Background(), comms.SlackDraftMessage{
		BotToken: "xoxb-test",
		Channel:  "C123",
		UserID:   "",
	})
	if err == nil {
		t.Fatal("expected error for empty user ID")
	}
}

func TestBuildDraftBlocks_Basic(t *testing.T) {
	blocks := comms.BuildDraftBlocks(comms.SlackDraftMessage{
		Author:  "Sarah",
		DraftID: "dft_123",
		Draft:   "No, the claims stay the same.",
	})

	if len(blocks) != 2 {
		t.Fatalf("expected 2 blocks (section + actions), got %d", len(blocks))
	}
}

func TestBuildDraftBlocks_LongDraftTruncated(t *testing.T) {
	longDraft := strings.Repeat("x", 600)
	blocks := comms.BuildDraftBlocks(comms.SlackDraftMessage{
		Author:  "Sarah",
		DraftID: "dft_123",
		Draft:   longDraft,
	})

	// Should still produce valid blocks without error.
	if len(blocks) != 2 {
		t.Fatalf("expected 2 blocks, got %d", len(blocks))
	}
}

func TestBuildDraftBlocks_EmptyDraft(t *testing.T) {
	blocks := comms.BuildDraftBlocks(comms.SlackDraftMessage{
		Author:  "Sarah",
		DraftID: "dft_123",
		Draft:   "",
	})

	if len(blocks) != 2 {
		t.Fatalf("expected 2 blocks even with empty draft, got %d", len(blocks))
	}
}
