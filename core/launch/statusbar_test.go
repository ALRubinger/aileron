package launch_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/core/launch"
)

func TestStatusBar_Render(t *testing.T) {
	bar := launch.NewStatusBar(24, 80, "Flying ✈️ withaileron.ai ")
	var buf bytes.Buffer
	bar.Render(&buf)
	out := buf.String()

	// Should contain cursor save/restore
	if !strings.Contains(out, "\0337") {
		t.Error("expected cursor save escape")
	}
	if !strings.Contains(out, "\0338") {
		t.Error("expected cursor restore escape")
	}
	// Should contain the text
	if !strings.Contains(out, "withaileron.ai") {
		t.Error("expected status bar text in output")
	}
	// Should contain separator character
	if !strings.Contains(out, "─") {
		t.Error("expected separator line")
	}
	// Should position at row 23 (rows-1) for separator
	if !strings.Contains(out, "\033[23;1H") {
		t.Errorf("expected cursor move to row 23, got %q", out)
	}
	// Should position at row 24 (rows) for text
	if !strings.Contains(out, "\033[24;1H") {
		t.Errorf("expected cursor move to row 24, got %q", out)
	}
}

func TestStatusBar_Resize(t *testing.T) {
	bar := launch.NewStatusBar(24, 80, "test")
	var buf bytes.Buffer

	bar.Resize(&buf, 30, 120)
	out := buf.String()

	// After resize to 30 rows, separator should be at row 29
	if !strings.Contains(out, "\033[29;1H") {
		t.Errorf("expected cursor move to row 29 after resize, got %q", out)
	}
}

func TestStatusBar_TooSmallTerminal(t *testing.T) {
	bar := launch.NewStatusBar(2, 80, "test")
	var buf bytes.Buffer
	bar.Render(&buf)
	// With only 2 rows, bar should not render (needs at least 3)
	if buf.Len() != 0 {
		t.Errorf("expected no output for 2-row terminal, got %q", buf.String())
	}
}

func TestStatusBar_BarHeight(t *testing.T) {
	bar := launch.NewStatusBar(24, 80, "test")
	if bar.BarHeight() != 2 {
		t.Errorf("expected bar height 2, got %d", bar.BarHeight())
	}
}

func TestStatusBar_EmojiWidth(t *testing.T) {
	// Use a supplementary plane emoji (U+1F6E9, small airplane) which should
	// count as 2 columns wide in displayWidth.
	bar := launch.NewStatusBar(24, 80, "Flying 🛩 test")
	var buf bytes.Buffer
	bar.Render(&buf)
	// Should render without issues
	if !strings.Contains(buf.String(), "test") {
		t.Error("expected status bar with emoji to render")
	}
}

func TestStatusBar_NarrowTerminal(t *testing.T) {
	// Text wider than terminal — should truncate
	bar := launch.NewStatusBar(24, 5, "this text is way too long")
	var buf bytes.Buffer
	bar.Render(&buf)
	out := buf.String()
	// Should still render without panicking
	if !strings.Contains(out, "\0337") {
		t.Error("expected cursor save even with narrow terminal")
	}
}

func TestStatusBar_WithQueue_NoUnread(t *testing.T) {
	bar := launch.NewStatusBar(24, 80, "branding")
	q := launch.NewNotifyQueue(10, nil)
	bar.SetQueue(q)

	var buf bytes.Buffer
	bar.Render(&buf)
	out := buf.String()

	// No unread → branding only, right-aligned.
	if !strings.Contains(out, "branding") {
		t.Error("expected branding text")
	}
	if strings.Contains(out, "unread") {
		t.Error("should not show unread count when queue is empty")
	}
}

func TestStatusBar_WithQueue_Unread(t *testing.T) {
	bar := launch.NewStatusBar(24, 80, "branding")
	q := launch.NewNotifyQueue(10, nil)
	bar.SetQueue(q)

	q.Push(launch.Message{ID: "1", Preview: "Hey, is the deploy blocked?"})
	q.Push(launch.Message{ID: "2", Preview: "PR looks good"})

	var buf bytes.Buffer
	bar.Render(&buf)
	out := buf.String()

	if !strings.Contains(out, "2 unread") {
		t.Errorf("expected '2 unread' in output, got:\n%s", out)
	}
	if !strings.Contains(out, "branding") {
		t.Error("expected branding text on the right")
	}
}

func TestStatusBar_WithQueue_PreviewShown(t *testing.T) {
	bar := launch.NewStatusBar(24, 120, "brand")
	q := launch.NewNotifyQueue(10, nil)
	bar.SetQueue(q)

	q.Push(launch.Message{ID: "1", Preview: "Latest message preview"})

	var buf bytes.Buffer
	bar.Render(&buf)
	out := buf.String()

	if !strings.Contains(out, "Latest message") {
		t.Errorf("expected preview text, got:\n%s", out)
	}
}

func TestStatusBar_WithQueue_AfterMarkAllRead(t *testing.T) {
	bar := launch.NewStatusBar(24, 80, "branding")
	q := launch.NewNotifyQueue(10, nil)
	bar.SetQueue(q)

	q.Push(launch.Message{ID: "1", Preview: "hello"})
	q.MarkAllRead()

	var buf bytes.Buffer
	bar.Render(&buf)
	out := buf.String()

	if strings.Contains(out, "unread") {
		t.Error("should not show unread count after MarkAllRead")
	}
}

func TestSetScrollRegion(t *testing.T) {
	var buf bytes.Buffer
	launch.SetScrollRegion(&buf, 1, 22)
	if buf.String() != "\033[1;22r" {
		t.Errorf("expected scroll region escape, got %q", buf.String())
	}
}

func TestResetScrollRegion(t *testing.T) {
	var buf bytes.Buffer
	launch.ResetScrollRegion(&buf)
	if buf.String() != "\033[r" {
		t.Errorf("expected reset scroll region escape, got %q", buf.String())
	}
}
