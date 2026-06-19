package launch

import (
	"bytes"
	"context"
	"testing"
)

// defaultHostCodePrompter contract:
//
//   - An already-cancelled context returns the context error immediately
//     and never opens the terminal or paints a prompt (a cancelled
//     launch must not leave a stray prompt on the user's screen).
//   - With a live context but no controlling terminal (CI), it surfaces
//     a clear open error rather than hanging — mirroring the vault
//     prompter's no-terminal contract.

func TestDefaultHostCodePrompter_CancelledContextReturnsEarly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var buf bytes.Buffer
	got, err := defaultHostCodePrompter(ctx, &buf)
	if err == nil {
		t.Fatal("expected the cancelled-context error")
	}
	if got != "" {
		t.Errorf("got %q, want empty string on cancellation", got)
	}
	if buf.Len() != 0 {
		t.Errorf("prompt painted %q despite a cancelled context", buf.String())
	}
}

func TestDefaultHostCodePrompter_NoTerminalFailsGracefully(t *testing.T) {
	if f, err := openControllingTerminal(); err == nil {
		f.Close()
		t.Skip("controlling terminal present; skipping to avoid blocking on read")
	}
	var buf bytes.Buffer
	if _, err := defaultHostCodePrompter(context.Background(), &buf); err == nil {
		t.Fatal("expected an error when no controlling terminal is available")
	}
}
