package launch_test

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ALRubinger/aileron/core/launch"
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
