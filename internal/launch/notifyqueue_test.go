package launch_test

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ALRubinger/aileron/internal/launch"
	launchpolicy "github.com/ALRubinger/aileron/internal/policy/launch"
)

func TestNotifyQueue_PushAndMessages(t *testing.T) {
	q := launch.NewNotifyQueue(10, nil)
	q.Push(launch.Message{ID: "1", Author: "Alice", Preview: "hello"})
	q.Push(launch.Message{ID: "2", Author: "Bob", Preview: "world"})

	msgs := q.Messages()
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgs))
	}
	if msgs[0].Author != "Alice" {
		t.Errorf("msgs[0].Author = %q, want Alice", msgs[0].Author)
	}
	if msgs[1].Author != "Bob" {
		t.Errorf("msgs[1].Author = %q, want Bob", msgs[1].Author)
	}
}

func TestNotifyQueue_Overflow(t *testing.T) {
	q := launch.NewNotifyQueue(3, nil)
	for i := 0; i < 5; i++ {
		q.Push(launch.Message{ID: string(rune('a' + i)), Preview: string(rune('a' + i))})
	}

	msgs := q.Messages()
	if len(msgs) != 3 {
		t.Fatalf("expected 3 messages after overflow, got %d", len(msgs))
	}
	// Oldest two (a, b) should be dropped; remaining: c, d, e
	if msgs[0].ID != "c" {
		t.Errorf("oldest should be 'c', got %q", msgs[0].ID)
	}
	if msgs[2].ID != "e" {
		t.Errorf("newest should be 'e', got %q", msgs[2].ID)
	}
}

func TestNotifyQueue_Len(t *testing.T) {
	q := launch.NewNotifyQueue(10, nil)
	if q.Len() != 0 {
		t.Errorf("expected 0, got %d", q.Len())
	}
	q.Push(launch.Message{ID: "1"})
	q.Push(launch.Message{ID: "2"})
	if q.Len() != 2 {
		t.Errorf("expected 2, got %d", q.Len())
	}
}

func TestNotifyQueue_UnreadCount(t *testing.T) {
	q := launch.NewNotifyQueue(10, nil)
	q.Push(launch.Message{ID: "1"})
	q.Push(launch.Message{ID: "2"})
	q.Push(launch.Message{ID: "3", Read: true})

	if got := q.UnreadCount(); got != 2 {
		t.Errorf("UnreadCount = %d, want 2", got)
	}
}

func TestNotifyQueue_Latest(t *testing.T) {
	q := launch.NewNotifyQueue(10, nil)

	_, ok := q.Latest()
	if ok {
		t.Error("expected false for empty queue")
	}

	q.Push(launch.Message{ID: "1", Preview: "first"})
	q.Push(launch.Message{ID: "2", Preview: "second"})

	msg, ok := q.Latest()
	if !ok {
		t.Fatal("expected true")
	}
	if msg.Preview != "second" {
		t.Errorf("Latest = %q, want 'second'", msg.Preview)
	}
}

func TestNotifyQueue_MarkRead(t *testing.T) {
	q := launch.NewNotifyQueue(10, nil)
	q.Push(launch.Message{ID: "1"})
	q.Push(launch.Message{ID: "2"})

	q.MarkRead("1")

	if got := q.UnreadCount(); got != 1 {
		t.Errorf("UnreadCount after MarkRead = %d, want 1", got)
	}

	msgs := q.Messages()
	if !msgs[0].Read {
		t.Error("message 1 should be read")
	}
	if msgs[1].Read {
		t.Error("message 2 should be unread")
	}
}

func TestNotifyQueue_MarkAllRead(t *testing.T) {
	q := launch.NewNotifyQueue(10, nil)
	q.Push(launch.Message{ID: "1"})
	q.Push(launch.Message{ID: "2"})
	q.Push(launch.Message{ID: "3"})

	q.MarkAllRead()

	if got := q.UnreadCount(); got != 0 {
		t.Errorf("UnreadCount after MarkAllRead = %d, want 0", got)
	}
}

func TestNotifyQueue_MarkReadNonexistent(t *testing.T) {
	q := launch.NewNotifyQueue(10, nil)
	q.Push(launch.Message{ID: "1"})

	// Should not panic or change anything.
	q.MarkRead("nonexistent")

	if got := q.UnreadCount(); got != 1 {
		t.Errorf("UnreadCount = %d, want 1", got)
	}
}

func TestNotifyQueue_OnChange(t *testing.T) {
	var count atomic.Int32
	q := launch.NewNotifyQueue(10, func() {
		count.Add(1)
	})

	q.Push(launch.Message{ID: "1"})
	q.MarkRead("1")
	q.MarkAllRead()

	if got := count.Load(); got != 3 {
		t.Errorf("onChange called %d times, want 3", got)
	}
}

func TestNotifyQueue_MessagesReturnsSnapshot(t *testing.T) {
	q := launch.NewNotifyQueue(10, nil)
	q.Push(launch.Message{ID: "1", Preview: "original"})

	msgs := q.Messages()
	msgs[0].Preview = "mutated"

	// Queue should be unaffected.
	fresh := q.Messages()
	if fresh[0].Preview != "original" {
		t.Error("Messages should return a copy, not a reference")
	}
}

func TestNotifyQueue_AutoDraftCallbackFires(t *testing.T) {
	q := launch.NewNotifyQueue(10, nil)
	var got []string
	q.SetOnAutoDraft(func(msg launch.Message) {
		got = append(got, msg.ID)
	})

	q.Push(launch.Message{ID: "1", AutoDraft: true})
	q.Push(launch.Message{ID: "2", AutoDraft: false})
	q.Push(launch.Message{ID: "3", AutoDraft: true})

	if len(got) != 2 {
		t.Fatalf("expected onAutoDraft called 2 times, got %d", len(got))
	}
	if got[0] != "1" || got[1] != "3" {
		t.Errorf("expected IDs [1, 3], got %v", got)
	}
}

func TestNotifyQueue_AutoDraftNilCallback(t *testing.T) {
	q := launch.NewNotifyQueue(10, nil)
	// Should not panic when onAutoDraft is nil.
	q.Push(launch.Message{ID: "1", AutoDraft: true})
}

func TestNotifyQueue_AutoDraftRoundtrip(t *testing.T) {
	q := launch.NewNotifyQueue(10, nil)
	q.Push(launch.Message{ID: "1", AutoDraft: true})
	q.Push(launch.Message{ID: "2", AutoDraft: false})

	msgs := q.Messages()
	if !msgs[0].AutoDraft {
		t.Error("expected AutoDraft=true on first message")
	}
	if msgs[1].AutoDraft {
		t.Error("expected AutoDraft=false on second message")
	}
}

func TestNotifyQueue_FindByID(t *testing.T) {
	q := launch.NewNotifyQueue(10, nil)
	q.Push(launch.Message{ID: "1", Author: "Alice"})
	q.Push(launch.Message{ID: "2", Author: "Bob"})

	msg, found := q.FindByID("2")
	if !found {
		t.Fatal("expected to find message with ID '2'")
	}
	if msg.Author != "Bob" {
		t.Errorf("Author = %q, want Bob", msg.Author)
	}

	_, found = q.FindByID("nonexistent")
	if found {
		t.Error("expected false for nonexistent ID")
	}
}

func TestNotifyQueue_SetDraft(t *testing.T) {
	q := launch.NewNotifyQueue(10, nil)
	q.Push(launch.Message{ID: "1", Author: "Alice", Read: true})

	ok := q.SetDraft("1", "Here is my draft reply")
	if !ok {
		t.Fatal("SetDraft should return true for existing message")
	}

	msg, _ := q.FindByID("1")
	if msg.Draft != "Here is my draft reply" {
		t.Errorf("Draft = %q, want 'Here is my draft reply'", msg.Draft)
	}
	if msg.DraftFor != "1" {
		t.Errorf("DraftFor = %q, want '1'", msg.DraftFor)
	}
	if msg.Read {
		t.Error("message should be marked unread after SetDraft")
	}
}

func TestNotifyQueue_SetDraft_Nonexistent(t *testing.T) {
	q := launch.NewNotifyQueue(10, nil)
	ok := q.SetDraft("nonexistent", "draft")
	if ok {
		t.Error("SetDraft should return false for nonexistent message")
	}
}

func TestNotifyQueue_SetDraft_OnChangeCallback(t *testing.T) {
	var called int
	q := launch.NewNotifyQueue(10, func() { called++ })
	q.Push(launch.Message{ID: "1"})
	called = 0 // reset after Push

	q.SetDraft("1", "draft")
	if called != 1 {
		t.Errorf("onChange called %d times, want 1", called)
	}

	// SetDraft on nonexistent should not call onChange.
	called = 0
	q.SetDraft("nonexistent", "draft")
	if called != 0 {
		t.Errorf("onChange called %d times for nonexistent, want 0", called)
	}
}

func TestNotifyQueue_ApproveDraft(t *testing.T) {
	q := launch.NewNotifyQueue(10, nil)
	q.Push(launch.Message{ID: "1"})
	q.SetDraft("1", "my draft")

	q.ApproveDraft("1")

	msg, _ := q.FindByID("1")
	if msg.Draft != "\x00approved" {
		t.Errorf("Draft = %q, want approved sentinel", msg.Draft)
	}
}

func TestNotifyQueue_ClearDraft(t *testing.T) {
	q := launch.NewNotifyQueue(10, nil)
	q.Push(launch.Message{ID: "1"})
	q.SetDraft("1", "my draft")

	q.ClearDraft("1")

	msg, _ := q.FindByID("1")
	if msg.Draft != "" {
		t.Errorf("Draft = %q, want empty", msg.Draft)
	}
	if msg.DraftFor != "" {
		t.Errorf("DraftFor = %q, want empty", msg.DraftFor)
	}
}

func TestNotifyQueue_RecentByChannel(t *testing.T) {
	q := launch.NewNotifyQueue(10, nil)
	q.Push(launch.Message{ID: "1", Source: "slack", Channel: "#backend", AutoDraft: true, Timestamp: time.Now()})
	q.Push(launch.Message{ID: "2", Source: "slack", Channel: "#frontend", AutoDraft: true, Timestamp: time.Now()})
	q.Push(launch.Message{ID: "3", Source: "discord", Channel: "#backend", AutoDraft: true, Timestamp: time.Now()})

	msg, found := q.RecentByChannel("slack", "#backend", 5*time.Minute)
	if !found {
		t.Fatal("expected to find recent message")
	}
	if msg.ID != "1" {
		t.Errorf("ID = %q, want '1'", msg.ID)
	}

	// Non-auto-draft message should not match.
	q2 := launch.NewNotifyQueue(10, nil)
	q2.Push(launch.Message{ID: "4", Source: "slack", Channel: "#backend", AutoDraft: false, Timestamp: time.Now()})
	_, found = q2.RecentByChannel("slack", "#backend", 5*time.Minute)
	if found {
		t.Error("should not match non-auto-draft message")
	}
}

func TestNotifyQueue_RecentByChannel_Expired(t *testing.T) {
	q := launch.NewNotifyQueue(10, nil)
	q.Push(launch.Message{
		ID: "1", Source: "slack", Channel: "#backend",
		AutoDraft: true, Timestamp: time.Now().Add(-10 * time.Minute),
	})

	_, found := q.RecentByChannel("slack", "#backend", 5*time.Minute)
	if found {
		t.Error("should not match expired message")
	}
}

func TestNotifyQueue_RecentByChannel_ReturnsNewest(t *testing.T) {
	q := launch.NewNotifyQueue(10, nil)
	q.Push(launch.Message{ID: "old", Source: "slack", Channel: "#backend", AutoDraft: true, Timestamp: time.Now().Add(-1 * time.Minute)})
	q.Push(launch.Message{ID: "new", Source: "slack", Channel: "#backend", AutoDraft: true, Timestamp: time.Now()})

	msg, found := q.RecentByChannel("slack", "#backend", 5*time.Minute)
	if !found {
		t.Fatal("expected to find message")
	}
	if msg.ID != "new" {
		t.Errorf("expected newest message, got ID %q", msg.ID)
	}
}

func TestNotifyQueue_DraftFieldsRoundtrip(t *testing.T) {
	q := launch.NewNotifyQueue(10, nil)
	q.Push(launch.Message{ID: "1", Draft: "pre-set draft", DraftFor: "orig-1"})

	msgs := q.Messages()
	if msgs[0].Draft != "pre-set draft" {
		t.Errorf("Draft = %q, want 'pre-set draft'", msgs[0].Draft)
	}
	if msgs[0].DraftFor != "orig-1" {
		t.Errorf("DraftFor = %q, want 'orig-1'", msgs[0].DraftFor)
	}
}

func TestNotifyQueue_ApproveDraft_Nonexistent(t *testing.T) {
	var called int
	q := launch.NewNotifyQueue(10, func() { called++ })
	q.Push(launch.Message{ID: "1"})
	called = 0

	// ApproveDraft on a nonexistent ID should still call onChange
	// (it iterates but finds nothing, then calls onChange).
	q.ApproveDraft("nonexistent")
	if called != 1 {
		t.Errorf("onChange called %d times, want 1", called)
	}

	// Verify no message was modified.
	msg, _ := q.FindByID("1")
	if msg.Draft != "" {
		t.Errorf("Draft = %q, want empty", msg.Draft)
	}
}

func TestNotifyQueue_ClearDraft_Nonexistent(t *testing.T) {
	var called int
	q := launch.NewNotifyQueue(10, func() { called++ })
	q.Push(launch.Message{ID: "1"})
	q.SetDraft("1", "my draft")
	called = 0

	// ClearDraft on a nonexistent ID should still call onChange.
	q.ClearDraft("nonexistent")
	if called != 1 {
		t.Errorf("onChange called %d times, want 1", called)
	}

	// Verify the existing draft was not affected.
	msg, _ := q.FindByID("1")
	if msg.Draft != "my draft" {
		t.Errorf("Draft = %q, want 'my draft'", msg.Draft)
	}
}

func TestNotifyQueue_ApproveDraft_NilOnChange(t *testing.T) {
	q := launch.NewNotifyQueue(10, nil)
	q.Push(launch.Message{ID: "1"})
	q.SetDraft("1", "draft")

	// Should not panic with nil onChange.
	q.ApproveDraft("1")
	msg, _ := q.FindByID("1")
	if msg.Draft != "\x00approved" {
		t.Errorf("Draft = %q, want approved sentinel", msg.Draft)
	}
}

func TestNotifyQueue_ClearDraft_NilOnChange(t *testing.T) {
	q := launch.NewNotifyQueue(10, nil)
	q.Push(launch.Message{ID: "1"})
	q.SetDraft("1", "draft")

	// Should not panic with nil onChange.
	q.ClearDraft("1")
	msg, _ := q.FindByID("1")
	if msg.Draft != "" {
		t.Errorf("Draft = %q, want empty", msg.Draft)
	}
}

func TestNotifyQueue_QuietHoursSuppressesOnChange(t *testing.T) {
	var count atomic.Int32
	q := launch.NewNotifyQueue(10, func() {
		count.Add(1)
	})
	q.SetQuietHours(&launchpolicy.QuietHoursConfig{
		Start: "20:00",
		End:   "08:00",
	})
	// Simulate 23:00 — inside quiet hours.
	q.SetNowFunc(func() time.Time {
		return time.Date(2026, 1, 15, 23, 0, 0, 0, time.Local)
	})

	q.Push(launch.Message{ID: "1", Priority: "normal"})

	if count.Load() != 0 {
		t.Errorf("onChange should be suppressed during quiet hours, called %d times", count.Load())
	}
	// Message should still be queued.
	if q.Len() != 1 {
		t.Errorf("expected 1 message in queue, got %d", q.Len())
	}
}

func TestNotifyQueue_QuietHoursHighPriorityBypasses(t *testing.T) {
	var count atomic.Int32
	q := launch.NewNotifyQueue(10, func() {
		count.Add(1)
	})
	q.SetQuietHours(&launchpolicy.QuietHoursConfig{
		Start: "20:00",
		End:   "08:00",
	})
	q.SetNowFunc(func() time.Time {
		return time.Date(2026, 1, 15, 23, 0, 0, 0, time.Local)
	})

	q.Push(launch.Message{ID: "1", Priority: "high"})

	if count.Load() != 1 {
		t.Errorf("onChange should fire for high-priority during quiet hours, called %d times", count.Load())
	}
}

func TestNotifyQueue_OutsideQuietHoursNotSuppressed(t *testing.T) {
	var count atomic.Int32
	q := launch.NewNotifyQueue(10, func() {
		count.Add(1)
	})
	q.SetQuietHours(&launchpolicy.QuietHoursConfig{
		Start: "22:00",
		End:   "06:00",
	})
	// Simulate 12:00 — outside quiet hours.
	q.SetNowFunc(func() time.Time {
		return time.Date(2026, 1, 15, 12, 0, 0, 0, time.Local)
	})

	q.Push(launch.Message{ID: "1", Priority: "normal"})

	if count.Load() != 1 {
		t.Errorf("onChange should fire outside quiet hours, called %d times", count.Load())
	}
}

func TestNotifyQueue_NoQuietHoursNotSuppressed(t *testing.T) {
	var count atomic.Int32
	q := launch.NewNotifyQueue(10, func() {
		count.Add(1)
	})
	// No quiet hours configured.
	q.Push(launch.Message{ID: "1", Priority: "normal"})

	if count.Load() != 1 {
		t.Errorf("onChange should fire when no quiet hours configured, called %d times", count.Load())
	}
}

func TestNotifyQueue_IsQuietHours(t *testing.T) {
	q := launch.NewNotifyQueue(10, nil)
	q.SetQuietHours(&launchpolicy.QuietHoursConfig{
		Start: "22:00",
		End:   "06:00",
	})

	// Inside quiet hours (23:00).
	q.SetNowFunc(func() time.Time {
		return time.Date(2026, 1, 15, 23, 0, 0, 0, time.Local)
	})
	if !q.IsQuietHours() {
		t.Error("expected IsQuietHours=true at 23:00")
	}

	// Outside quiet hours (12:00).
	q.SetNowFunc(func() time.Time {
		return time.Date(2026, 1, 15, 12, 0, 0, 0, time.Local)
	})
	if q.IsQuietHours() {
		t.Error("expected IsQuietHours=false at 12:00")
	}
}

func TestNotifyQueue_HighPriorityUnreadCount(t *testing.T) {
	q := launch.NewNotifyQueue(10, nil)
	q.Push(launch.Message{ID: "1", Priority: "normal"})
	q.Push(launch.Message{ID: "2", Priority: "high"})
	q.Push(launch.Message{ID: "3", Priority: "high", Read: true})
	q.Push(launch.Message{ID: "4", Priority: "high"})

	if got := q.HighPriorityUnreadCount(); got != 2 {
		t.Errorf("HighPriorityUnreadCount = %d, want 2", got)
	}
}

func TestNotifyQueue_ConcurrentAccess(t *testing.T) {
	q := launch.NewNotifyQueue(100, nil)
	var wg sync.WaitGroup

	// 10 writers, 10 readers.
	for i := 0; i < 10; i++ {
		wg.Add(2)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				q.Push(launch.Message{
					ID:        string(rune(n*100 + j)),
					Timestamp: time.Now(),
				})
			}
		}(i)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_ = q.UnreadCount()
				_ = q.Messages()
				q.Latest()
			}
		}()
	}
	wg.Wait()

	// Should not have panicked and should have at most maxSize messages.
	if q.Len() > 100 {
		t.Errorf("Len = %d, should be <= 100", q.Len())
	}
}

func TestNotifyQueue_DraftPendingCount(t *testing.T) {
	q := launch.NewNotifyQueue(10, nil)

	// Auto-draft without a draft yet — counts as pending.
	q.Push(launch.Message{ID: "1", AutoDraft: true})
	// Non-auto-draft — not counted.
	q.Push(launch.Message{ID: "2", AutoDraft: false})
	// Auto-draft with a draft already — not pending.
	q.Push(launch.Message{ID: "3", AutoDraft: true, Draft: "reply", DraftFor: "3"})

	if got := q.DraftPendingCount(); got != 1 {
		t.Errorf("DraftPendingCount = %d, want 1", got)
	}

	// Mark the pending message as read — no longer counted.
	q.MarkRead("1")
	if got := q.DraftPendingCount(); got != 0 {
		t.Errorf("DraftPendingCount after MarkRead = %d, want 0", got)
	}
}
