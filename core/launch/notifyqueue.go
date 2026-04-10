package launch

import (
	"sync"
	"time"
)

// Message represents an incoming notification from a comms channel.
type Message struct {
	ID        string
	Source    string // "slack", "discord", etc.
	Channel   string
	Author    string
	Preview   string // short text for status bar
	Body      string // full text for overlay
	Timestamp time.Time
	Read      bool
	AutoDraft bool // channel is configured for automatic draft replies
}

// NotifyQueue is a bounded, thread-safe FIFO of incoming messages.
// The onChange callback fires (outside the lock) whenever the queue
// state changes, so the status bar can re-render. The onAutoDraft
// callback fires for messages with AutoDraft set.
type NotifyQueue struct {
	mu          sync.Mutex
	messages    []Message
	maxSize     int
	onChange    func()
	onAutoDraft func(Message)
}

// NewNotifyQueue creates a queue with the given capacity. The onChange
// callback is optional — pass nil if not needed.
func NewNotifyQueue(maxSize int, onChange func()) *NotifyQueue {
	return &NotifyQueue{
		maxSize:  maxSize,
		onChange: onChange,
	}
}

// Push appends a message to the queue. If the queue exceeds maxSize,
// the oldest message is dropped.
func (q *NotifyQueue) Push(msg Message) {
	q.mu.Lock()
	q.messages = append(q.messages, msg)
	if len(q.messages) > q.maxSize {
		q.messages = q.messages[len(q.messages)-q.maxSize:]
	}
	q.mu.Unlock()

	if q.onChange != nil {
		q.onChange()
	}
	if msg.AutoDraft && q.onAutoDraft != nil {
		q.onAutoDraft(msg)
	}
}

// SetOnAutoDraft sets a callback that fires when an AutoDraft message
// is pushed. The callback runs outside the lock.
func (q *NotifyQueue) SetOnAutoDraft(fn func(Message)) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.onAutoDraft = fn
}

// Messages returns a snapshot (copy) of all messages.
func (q *NotifyQueue) Messages() []Message {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]Message, len(q.messages))
	copy(out, q.messages)
	return out
}

// Len returns the number of messages in the queue.
func (q *NotifyQueue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.messages)
}

// UnreadCount returns the number of unread messages.
func (q *NotifyQueue) UnreadCount() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	count := 0
	for _, m := range q.messages {
		if !m.Read {
			count++
		}
	}
	return count
}

// Latest returns the most recent message, or false if empty.
func (q *NotifyQueue) Latest() (Message, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.messages) == 0 {
		return Message{}, false
	}
	return q.messages[len(q.messages)-1], true
}

// MarkRead marks a single message as read by ID.
func (q *NotifyQueue) MarkRead(id string) {
	q.mu.Lock()
	for i := range q.messages {
		if q.messages[i].ID == id {
			q.messages[i].Read = true
			break
		}
	}
	q.mu.Unlock()

	if q.onChange != nil {
		q.onChange()
	}
}

// MarkAllRead marks all messages as read.
func (q *NotifyQueue) MarkAllRead() {
	q.mu.Lock()
	for i := range q.messages {
		q.messages[i].Read = true
	}
	q.mu.Unlock()

	if q.onChange != nil {
		q.onChange()
	}
}
