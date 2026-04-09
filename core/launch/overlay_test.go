package launch_test

import (
	"strings"
	"testing"
	"time"

	"github.com/ALRubinger/aileron/core/launch"
)

// testWriter captures output for overlay tests.
type testWriter struct {
	strings.Builder
}

func TestOverlay_ShowRendersAltScreen(t *testing.T) {
	q := launch.NewNotifyQueue(10, nil)
	var w testWriter
	o := launch.NewOverlay(q, nil, &w, 24, 80, nil)

	o.Show()
	out := w.String()

	// Should switch to alternate screen buffer.
	if !strings.Contains(out, "\033[?1049h") {
		t.Error("expected alternate screen buffer switch")
	}
	// Should render header.
	if !strings.Contains(out, "aileron notifications") {
		t.Error("expected header in overlay")
	}
	if !strings.Contains(out, "No notifications") {
		t.Error("expected 'No notifications' for empty queue")
	}
}

func TestOverlay_HideRestoresScreen(t *testing.T) {
	q := launch.NewNotifyQueue(10, nil)
	var w testWriter
	o := launch.NewOverlay(q, nil, &w, 24, 80, nil)

	o.Show()
	w.Reset()
	o.Hide()
	out := w.String()

	// Should restore original screen buffer.
	if !strings.Contains(out, "\033[?1049l") {
		t.Error("expected alternate screen restore")
	}
}

func TestOverlay_IsActive(t *testing.T) {
	q := launch.NewNotifyQueue(10, nil)
	var w testWriter
	o := launch.NewOverlay(q, nil, &w, 24, 80, nil)

	if o.IsActive() {
		t.Error("should not be active before Show")
	}
	o.Show()
	if !o.IsActive() {
		t.Error("should be active after Show")
	}
	o.Hide()
	if o.IsActive() {
		t.Error("should not be active after Hide")
	}
}

func TestOverlay_ShowWithMessages(t *testing.T) {
	q := launch.NewNotifyQueue(10, nil)
	q.Push(launch.Message{ID: "1", Source: "slack", Author: "Alice", Preview: "Hey there"})
	q.Push(launch.Message{ID: "2", Source: "discord", Author: "Bob", Preview: "PR looks good"})

	var w testWriter
	o := launch.NewOverlay(q, nil, &w, 24, 80, nil)
	o.Show()
	out := w.String()

	if !strings.Contains(out, "Alice") {
		t.Error("expected Alice in overlay")
	}
	if !strings.Contains(out, "Bob") {
		t.Error("expected Bob in overlay")
	}
	if !strings.Contains(out, "2 messages") {
		t.Error("expected message count in header")
	}
}

func TestOverlay_CursorNavigation(t *testing.T) {
	q := launch.NewNotifyQueue(10, nil)
	q.Push(launch.Message{ID: "1", Source: "slack", Author: "Alice", Preview: "first"})
	q.Push(launch.Message{ID: "2", Source: "slack", Author: "Bob", Preview: "second"})
	q.Push(launch.Message{ID: "3", Source: "slack", Author: "Carol", Preview: "third"})

	var w testWriter
	o := launch.NewOverlay(q, nil, &w, 24, 80, nil)
	o.Show()

	// Move down with 'j'.
	w.Reset()
	o.HandleKey('j')
	out := w.String()
	// The cursor indicator should be on the second message.
	if !strings.Contains(out, "second") {
		t.Error("expected second message visible after j")
	}

	// Move up with 'k'.
	w.Reset()
	o.HandleKey('k')
	// Should be back on first.
}

func TestOverlay_CursorBounds(t *testing.T) {
	q := launch.NewNotifyQueue(10, nil)
	q.Push(launch.Message{ID: "1", Preview: "only"})

	var w testWriter
	o := launch.NewOverlay(q, nil, &w, 24, 80, nil)
	o.Show()

	// Move up past the top — should not panic.
	o.HandleKey('k')
	o.HandleKey('k')

	// Move down past the bottom — should not panic.
	o.HandleKey('j')
	o.HandleKey('j')
}

func TestOverlay_DismissWithQ(t *testing.T) {
	q := launch.NewNotifyQueue(10, nil)
	var w testWriter
	dismissed := false
	o := launch.NewOverlay(q, nil, &w, 24, 80, func() {
		dismissed = true
	})

	o.Show()
	o.HandleKey('q')

	if o.IsActive() {
		t.Error("overlay should be inactive after 'q'")
	}
	if !dismissed {
		t.Error("onDismiss should have been called")
	}
}

func TestOverlay_DismissWithEsc(t *testing.T) {
	q := launch.NewNotifyQueue(10, nil)
	var w testWriter
	dismissed := false
	o := launch.NewOverlay(q, nil, &w, 24, 80, func() {
		dismissed = true
	})

	o.Show()
	// Send standalone ESC (no follow-up byte).
	o.HandleKey(0x1B)
	time.Sleep(200 * time.Millisecond) // wait for ESC timer

	if o.IsActive() {
		t.Error("overlay should be inactive after ESC")
	}
	if !dismissed {
		t.Error("onDismiss should have been called")
	}
}

func TestOverlay_ArrowKeys(t *testing.T) {
	q := launch.NewNotifyQueue(10, nil)
	q.Push(launch.Message{ID: "1", Preview: "first"})
	q.Push(launch.Message{ID: "2", Preview: "second"})

	var w testWriter
	o := launch.NewOverlay(q, nil, &w, 24, 80, nil)
	o.Show()

	// Arrow down: ESC [ B
	o.HandleKey(0x1B)
	o.HandleKey('[')
	o.HandleKey('B')

	// Arrow up: ESC [ A
	o.HandleKey(0x1B)
	o.HandleKey('[')
	o.HandleKey('A')

	// Should not panic or dismiss.
	if !o.IsActive() {
		t.Error("arrow keys should not dismiss overlay")
	}
}

func TestOverlay_DismissSelected(t *testing.T) {
	q := launch.NewNotifyQueue(10, nil)
	q.Push(launch.Message{ID: "1", Preview: "to dismiss"})

	var w testWriter
	o := launch.NewOverlay(q, nil, &w, 24, 80, nil)
	o.Show()

	o.HandleKey('d')

	if q.UnreadCount() != 0 {
		t.Errorf("expected 0 unread after dismiss, got %d", q.UnreadCount())
	}
}

func TestOverlay_Resize(t *testing.T) {
	q := launch.NewNotifyQueue(10, nil)
	q.Push(launch.Message{ID: "1", Preview: "msg"})

	var w testWriter
	o := launch.NewOverlay(q, nil, &w, 24, 80, nil)
	o.Show()
	w.Reset()

	o.Resize(30, 120)
	out := w.String()

	// Should re-render with the new dimensions.
	if !strings.Contains(out, "aileron notifications") {
		t.Error("expected re-render after resize")
	}
}
