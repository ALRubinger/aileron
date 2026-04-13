package launch_test

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/ALRubinger/aileron/core/launch"
	launchpolicy "github.com/ALRubinger/aileron/core/policy/launch"
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
	// Should position at row 22 (rows-2) for separator
	if !strings.Contains(out, "\033[22;1H") {
		t.Errorf("expected cursor move to row 22, got %q", out)
	}
	// Should position at row 23 (rows-1) for content
	if !strings.Contains(out, "\033[23;1H") {
		t.Errorf("expected cursor move to row 23, got %q", out)
	}
	// Should position at row 24 (rows) for hint
	if !strings.Contains(out, "\033[24;1H") {
		t.Errorf("expected cursor move to row 24, got %q", out)
	}
}

func TestStatusBar_Resize(t *testing.T) {
	bar := launch.NewStatusBar(24, 80, "test")
	var buf bytes.Buffer

	bar.Resize(&buf, 30, 120)
	out := buf.String()

	// After resize to 30 rows, separator should be at row 28 (rows-2)
	if !strings.Contains(out, "\033[28;1H") {
		t.Errorf("expected cursor move to row 28 after resize, got %q", out)
	}
}

func TestStatusBar_TooSmallTerminal(t *testing.T) {
	bar := launch.NewStatusBar(3, 80, "test")
	var buf bytes.Buffer
	bar.Render(&buf)
	// With only 3 rows, bar should not render (needs at least 4)
	if buf.Len() != 0 {
		t.Errorf("expected no output for 3-row terminal, got %q", buf.String())
	}
}

func TestStatusBar_BarHeight(t *testing.T) {
	bar := launch.NewStatusBar(24, 80, "test")
	if bar.BarHeight() != 3 {
		t.Errorf("expected bar height 3, got %d", bar.BarHeight())
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

func TestStatusBar_QuietHoursIndicator(t *testing.T) {
	bar := launch.NewStatusBar(24, 80, "branding")
	q := launch.NewNotifyQueue(10, nil)
	q.SetQuietHours(&launchpolicy.QuietHoursConfig{
		Start: "20:00",
		End:   "08:00",
	})
	// Simulate 23:00 — inside quiet hours.
	q.SetNowFunc(func() time.Time {
		return time.Date(2026, 1, 15, 23, 0, 0, 0, time.Local)
	})
	bar.SetQueue(q)

	// Push a normal-priority message (no high-priority unread).
	q.Push(launch.Message{ID: "1", Priority: "normal"})

	var buf bytes.Buffer
	bar.Render(&buf)
	out := buf.String()

	if !strings.Contains(out, "[quiet]") {
		t.Errorf("expected [quiet] indicator during quiet hours, got:\n%s", out)
	}
	if !strings.Contains(out, "branding") {
		t.Error("expected branding text alongside quiet indicator")
	}
	if strings.Contains(out, "unread") {
		t.Error("should not show unread count during quiet hours with no high-priority messages")
	}
}

func TestStatusBar_QuietHoursHighPriorityShowsUnread(t *testing.T) {
	bar := launch.NewStatusBar(24, 80, "branding")
	q := launch.NewNotifyQueue(10, nil)
	q.SetQuietHours(&launchpolicy.QuietHoursConfig{
		Start: "20:00",
		End:   "08:00",
	})
	q.SetNowFunc(func() time.Time {
		return time.Date(2026, 1, 15, 23, 0, 0, 0, time.Local)
	})
	bar.SetQueue(q)

	// Push a high-priority message — should bypass quiet indicator.
	q.Push(launch.Message{ID: "1", Priority: "high", Preview: "urgent"})

	var buf bytes.Buffer
	bar.Render(&buf)
	out := buf.String()

	if strings.Contains(out, "[quiet]") {
		t.Error("should not show [quiet] when high-priority unread messages exist")
	}
	if !strings.Contains(out, "1 unread") {
		t.Errorf("expected '1 unread' for high-priority message during quiet hours, got:\n%s", out)
	}
}

func TestStatusBar_QuietHoursNarrowTerminal(t *testing.T) {
	// Narrow terminal where gap would be < 1.
	bar := launch.NewStatusBar(24, 10, "branding")
	q := launch.NewNotifyQueue(10, nil)
	q.SetQuietHours(&launchpolicy.QuietHoursConfig{
		Start: "20:00",
		End:   "08:00",
	})
	q.SetNowFunc(func() time.Time {
		return time.Date(2026, 1, 15, 23, 0, 0, 0, time.Local)
	})
	bar.SetQueue(q)

	var buf bytes.Buffer
	bar.Render(&buf)
	out := buf.String()

	// Should render without panicking; [quiet] and branding both present.
	if !strings.Contains(out, "[quiet]") {
		t.Errorf("expected [quiet] even in narrow terminal, got:\n%s", out)
	}
}

func TestStatusBar_WithQueue_LongPreviewTruncated(t *testing.T) {
	// Use a terminal width where preview is shown but needs truncation.
	bar := launch.NewStatusBar(24, 50, "brand")
	q := launch.NewNotifyQueue(10, nil)
	bar.SetQueue(q)

	q.Push(launch.Message{ID: "1", Preview: "This is a very long preview message that exceeds available space"})

	var buf bytes.Buffer
	bar.Render(&buf)
	out := buf.String()

	if !strings.Contains(out, "1 unread") {
		t.Errorf("expected '1 unread', got:\n%s", out)
	}
	// Should contain truncated preview with "..."
	if !strings.Contains(out, "...") {
		t.Errorf("expected truncated preview with '...', got:\n%s", out)
	}
}

func TestStatusBar_WithQueue_NarrowNoPreview(t *testing.T) {
	// Terminal too narrow for preview (previewSpace <= 10).
	bar := launch.NewStatusBar(24, 25, "brand")
	q := launch.NewNotifyQueue(10, nil)
	bar.SetQueue(q)

	q.Push(launch.Message{ID: "1", Preview: "hello"})

	var buf bytes.Buffer
	bar.Render(&buf)
	out := buf.String()

	if !strings.Contains(out, "1 unread") {
		t.Errorf("expected '1 unread' even in narrow terminal, got:\n%s", out)
	}
}

func TestStatusBar_OutsideQuietHoursShowsNormal(t *testing.T) {
	bar := launch.NewStatusBar(24, 80, "branding")
	q := launch.NewNotifyQueue(10, nil)
	q.SetQuietHours(&launchpolicy.QuietHoursConfig{
		Start: "22:00",
		End:   "06:00",
	})
	// Simulate 12:00 — outside quiet hours.
	q.SetNowFunc(func() time.Time {
		return time.Date(2026, 1, 15, 12, 0, 0, 0, time.Local)
	})
	bar.SetQueue(q)

	q.Push(launch.Message{ID: "1", Preview: "hello"})

	var buf bytes.Buffer
	bar.Render(&buf)
	out := buf.String()

	if strings.Contains(out, "[quiet]") {
		t.Error("should not show [quiet] outside quiet hours")
	}
	if !strings.Contains(out, "1 unread") {
		t.Errorf("expected '1 unread' outside quiet hours, got:\n%s", out)
	}
}

func TestStatusBar_DraftPendingIndicator(t *testing.T) {
	bar := launch.NewStatusBar(24, 120, "brand")
	q := launch.NewNotifyQueue(10, nil)
	bar.SetQueue(q)

	// Auto-draft message without a draft yet.
	q.Push(launch.Message{ID: "1", Preview: "hello", AutoDraft: true})

	var buf bytes.Buffer
	bar.Render(&buf)
	out := buf.String()

	if !strings.Contains(out, "1 awaiting draft") {
		t.Errorf("expected '1 awaiting draft' indicator, got:\n%s", out)
	}
}

func TestStatusBar_DraftPendingDisappearsAfterDraft(t *testing.T) {
	bar := launch.NewStatusBar(24, 120, "brand")
	q := launch.NewNotifyQueue(10, nil)
	bar.SetQueue(q)

	q.Push(launch.Message{ID: "1", Preview: "hello", AutoDraft: true})
	q.SetDraft("1", "my reply")

	var buf bytes.Buffer
	bar.Render(&buf)
	out := buf.String()

	if strings.Contains(out, "awaiting draft") {
		t.Errorf("should not show 'awaiting draft' after draft is set, got:\n%s", out)
	}
}

func TestStatusBar_HintLineShown(t *testing.T) {
	bar := launch.NewStatusBar(24, 80, "brand")
	var buf bytes.Buffer
	bar.Render(&buf)
	out := buf.String()

	if !strings.Contains(out, "Ctrl-]") {
		t.Error("expected 'Ctrl-]' in hint line")
	}
	if !strings.Contains(out, "notifications") {
		t.Error("expected 'notifications' in hint line")
	}
}
