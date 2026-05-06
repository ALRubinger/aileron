package comms

import (
	"sync"
	"time"

	"github.com/ALRubinger/aileron/internal/config"
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
	AutoDraft bool   // channel is configured for automatic draft replies
	Priority  string // "normal" or "high"; high-priority messages bypass quiet hours

	// Draft fields: set when the agent drafts a reply to a message.
	Draft    string // agent's drafted reply text (empty for regular messages)
	DraftFor string // ID of the original message this draft replies to
}

// NotifyQueue is a bounded, thread-safe FIFO of incoming messages.
// The onChange callback fires (outside the lock) whenever the queue
// state changes, so the status bar can re-render. The onAutoDraft
// callback fires for messages with AutoDraft set.
//
// When QuietHours is configured, non-high-priority messages are still
// queued but the onChange callback is suppressed so the status bar does
// not surface them until quiet hours end.
type NotifyQueue struct {
	mu          sync.Mutex
	messages    []Message
	maxSize     int
	onChange    func()
	onAutoDraft func(Message)
	quietHours  *config.QuietHoursConfig
	nowFunc     func() time.Time // injectable clock for testing; defaults to time.Now
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
// the oldest message is dropped. During quiet hours, the onChange
// callback is suppressed for non-high-priority messages so the status
// bar stays quiet.
func (q *NotifyQueue) Push(msg Message) {
	q.mu.Lock()
	q.messages = append(q.messages, msg)
	if len(q.messages) > q.maxSize {
		q.messages = q.messages[len(q.messages)-q.maxSize:]
	}
	q.mu.Unlock()

	suppressed := q.isQuiet() && msg.Priority != "high"

	if q.onChange != nil && !suppressed {
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

// HighPriorityUnreadCount returns the number of unread high-priority messages.
func (q *NotifyQueue) HighPriorityUnreadCount() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	count := 0
	for _, m := range q.messages {
		if !m.Read && m.Priority == "high" {
			count++
		}
	}
	return count
}

// DraftPendingCount returns the number of unread auto-draft messages
// that do not yet have a draft reply. These are messages awaiting the
// user to open the overlay and request a draft.
func (q *NotifyQueue) DraftPendingCount() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	count := 0
	for _, m := range q.messages {
		if !m.Read && m.AutoDraft && m.Draft == "" {
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

// FindByID returns the message with the given ID, or false if not found.
func (q *NotifyQueue) FindByID(id string) (Message, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, m := range q.messages {
		if m.ID == id {
			return m, true
		}
	}
	return Message{}, false
}

// SetDraft attaches a draft reply to the message with the given ID.
// It sets the Draft and DraftFor fields and marks the message as unread
// so it reappears as a draft notification. Returns false if the target
// message was not found.
func (q *NotifyQueue) SetDraft(targetID, draftText string) bool {
	q.mu.Lock()
	found := false
	for i := range q.messages {
		if q.messages[i].ID == targetID {
			q.messages[i].Draft = draftText
			q.messages[i].DraftFor = targetID
			q.messages[i].Read = false
			found = true
			break
		}
	}
	q.mu.Unlock()

	if found && q.onChange != nil {
		q.onChange()
	}
	return found
}

// ApproveDraft sets the draft to the approved sentinel so the
// dispatcher knows to send the message as-is.
func (q *NotifyQueue) ApproveDraft(targetID string) {
	q.mu.Lock()
	for i := range q.messages {
		if q.messages[i].ID == targetID {
			q.messages[i].Draft = "\x00approved"
			break
		}
	}
	q.mu.Unlock()

	if q.onChange != nil {
		q.onChange()
	}
}

// ClearDraft removes the draft from the message with the given ID.
func (q *NotifyQueue) ClearDraft(targetID string) {
	q.mu.Lock()
	for i := range q.messages {
		if q.messages[i].ID == targetID {
			q.messages[i].Draft = ""
			q.messages[i].DraftFor = ""
			break
		}
	}
	q.mu.Unlock()

	if q.onChange != nil {
		q.onChange()
	}
}

// RecentByChannel returns the most recent message on the given service
// and channel that was auto-drafted (within the given duration). This is
// used to detect when a send_message call is replying to an auto-drafted
// message.
func (q *NotifyQueue) RecentByChannel(service, channel string, within time.Duration) (Message, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	cutoff := time.Now().Add(-within)
	// Search from newest to oldest.
	for i := len(q.messages) - 1; i >= 0; i-- {
		m := q.messages[i]
		if m.Source == service && m.Channel == channel && m.AutoDraft && m.Timestamp.After(cutoff) {
			return m, true
		}
	}
	return Message{}, false
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

// SetQuietHours configures the quiet hours window. Pass nil to disable.
func (q *NotifyQueue) SetQuietHours(cfg *config.QuietHoursConfig) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.quietHours = cfg
}

// SetNowFunc overrides the clock used for quiet-hours evaluation.
// Intended for testing.
func (q *NotifyQueue) SetNowFunc(fn func() time.Time) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.nowFunc = fn
}

// IsQuietHours reports whether the current time falls within the
// configured quiet hours window. Returns false if no quiet hours are
// configured or if the configuration is invalid.
func (q *NotifyQueue) IsQuietHours() bool {
	return q.isQuiet()
}

// isQuiet is the internal quiet-hours check that reads the config under
// the lock.
func (q *NotifyQueue) isQuiet() bool {
	q.mu.Lock()
	cfg := q.quietHours
	now := q.nowFunc
	q.mu.Unlock()

	return IsQuietHours(cfg, now)
}

// IsQuietHours determines whether the given time (from nowFunc, or
// time.Now if nil) falls within the quiet hours window defined by cfg.
// Returns false if cfg is nil or the start/end times are unparseable.
func IsQuietHours(cfg *config.QuietHoursConfig, nowFunc func() time.Time) bool {
	if cfg == nil {
		return false
	}
	if nowFunc == nil {
		nowFunc = time.Now
	}

	loc := time.Local
	if cfg.Timezone != "" {
		var err error
		loc, err = time.LoadLocation(cfg.Timezone)
		if err != nil {
			return false
		}
	}

	now := nowFunc().In(loc)
	startH, startM, ok := parseHHMM(cfg.Start)
	if !ok {
		return false
	}
	endH, endM, ok := parseHHMM(cfg.End)
	if !ok {
		return false
	}

	// Convert current time to minutes since midnight.
	nowMin := now.Hour()*60 + now.Minute()
	startMin := startH*60 + startM
	endMin := endH*60 + endM

	if startMin <= endMin {
		// Same-day window: e.g. 09:00–17:00
		return nowMin >= startMin && nowMin < endMin
	}
	// Overnight window: e.g. 22:00–08:00
	return nowMin >= startMin || nowMin < endMin
}

// parseHHMM parses a "HH:MM" string into hours and minutes.
func parseHHMM(s string) (int, int, bool) {
	if len(s) != 5 || s[2] != ':' {
		return 0, 0, false
	}
	h := int(s[0]-'0')*10 + int(s[1]-'0')
	m := int(s[3]-'0')*10 + int(s[4]-'0')
	if h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, 0, false
	}
	return h, m, true
}
