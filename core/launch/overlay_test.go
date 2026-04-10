package launch_test

import (
	"fmt"
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

func TestOverlay_DraftRequestKey(t *testing.T) {
	q := launch.NewNotifyQueue(10, nil)
	q.Push(launch.Message{ID: "1", Source: "slack", Channel: "#backend", Author: "Sarah", Body: "Does the auth change JWT claims?"})

	var w testWriter
	// Use a real OutputCopier so the copier branch in draftSelected is exercised.
	copier := launch.NewOutputCopier(strings.NewReader(""), &w, nil)
	dismissed := false
	o := launch.NewOverlay(q, copier, &w, 24, 80, func() {
		dismissed = true
	})

	var draftMsg launch.Message
	draftCalled := false
	o.OnDraftRequest = func(msg launch.Message) {
		draftCalled = true
		draftMsg = msg
	}

	o.Show()
	o.HandleKey('a')

	if !draftCalled {
		t.Fatal("expected onDraftRequest to be called")
	}
	if draftMsg.ID != "1" {
		t.Errorf("expected message ID '1', got %q", draftMsg.ID)
	}
	if draftMsg.Author != "Sarah" {
		t.Errorf("expected author 'Sarah', got %q", draftMsg.Author)
	}
	if o.IsActive() {
		t.Error("overlay should be dismissed after draft request")
	}
	if !dismissed {
		t.Error("onDismiss should have been called")
	}
	// Message should be marked read.
	if q.UnreadCount() != 0 {
		t.Errorf("expected 0 unread, got %d", q.UnreadCount())
	}
}

func TestOverlay_DraftRequestEmptyQueue(t *testing.T) {
	q := launch.NewNotifyQueue(10, nil)
	var w testWriter
	o := launch.NewOverlay(q, nil, &w, 24, 80, nil)

	draftCalled := false
	o.OnDraftRequest = func(msg launch.Message) {
		draftCalled = true
	}

	o.Show()
	o.HandleKey('a') // should not panic

	if draftCalled {
		t.Error("onDraftRequest should not be called on empty queue")
	}
}

func TestOverlay_ExpandedBodyRendered(t *testing.T) {
	q := launch.NewNotifyQueue(10, nil)
	q.Push(launch.Message{
		ID:      "1",
		Source:  "slack",
		Channel: "#backend",
		Author:  "Sarah",
		Preview: "Does the auth...",
		Body:    "Does the new auth middleware change the JWT claims? I need to know before updating the mobile client.",
	})

	var w testWriter
	o := launch.NewOverlay(q, nil, &w, 30, 80, nil)
	o.Show()
	out := w.String()

	// Should show the full body text (not just the preview).
	if !strings.Contains(out, "JWT claims") {
		t.Error("expected full message body in detail pane")
	}
	if !strings.Contains(out, "#backend") {
		t.Error("expected channel in detail pane header")
	}
}

func TestOverlay_ExpandedBodyLongTruncated(t *testing.T) {
	q := launch.NewNotifyQueue(10, nil)
	// Create a message with a very long body that exceeds 5 lines at 80 cols.
	longBody := strings.Repeat("This is a very long sentence that should wrap across multiple lines. ", 10)
	q.Push(launch.Message{
		ID:      "1",
		Source:  "slack",
		Channel: "#backend",
		Author:  "Sarah",
		Preview: "Long msg...",
		Body:    longBody,
	})

	var w testWriter
	o := launch.NewOverlay(q, nil, &w, 30, 80, nil)
	o.Show()
	out := w.String()

	// Body is long enough to exceed 5 lines, so truncation indicator should appear.
	if !strings.Contains(out, "...") {
		t.Error("expected '...' truncation indicator for long body")
	}
}

func TestOverlay_FooterShowsDraftAction(t *testing.T) {
	q := launch.NewNotifyQueue(10, nil)
	var w testWriter
	o := launch.NewOverlay(q, nil, &w, 24, 80, nil)
	o.Show()
	out := w.String()

	if !strings.Contains(out, "a draft reply") {
		t.Error("expected 'a draft reply' in footer")
	}
}

func TestOverlay_ReplyKey_EntersReplyMode(t *testing.T) {
	q := launch.NewNotifyQueue(10, nil)
	q.Push(launch.Message{ID: "1", Source: "slack", Channel: "#backend", Author: "Sarah", Body: "question?"})

	var w testWriter
	o := launch.NewOverlay(q, nil, &w, 30, 80, nil)
	o.Show()
	w.Reset()

	o.HandleKey('r')
	out := w.String()

	// Should show reply input prompt.
	if !strings.Contains(out, ">") {
		t.Error("expected reply input prompt")
	}
	// Footer should show Enter/Esc.
	if !strings.Contains(out, "Enter send") {
		t.Error("expected 'Enter send' in footer")
	}
	if !strings.Contains(out, "Esc cancel") {
		t.Error("expected 'Esc cancel' in footer")
	}
	// Should still be active (not dismissed).
	if !o.IsActive() {
		t.Error("overlay should still be active in reply mode")
	}
}

func TestOverlay_ReplyKey_TypeAndSubmit(t *testing.T) {
	q := launch.NewNotifyQueue(10, nil)
	q.Push(launch.Message{ID: "1", Source: "slack", Channel: "#backend", Author: "Sarah", Body: "question?"})

	var w testWriter
	copier := launch.NewOutputCopier(strings.NewReader(""), &w, nil)
	dismissed := false
	o := launch.NewOverlay(q, copier, &w, 30, 80, func() {
		dismissed = true
	})

	var repliedMsg launch.Message
	var repliedText string
	o.OnReply = func(msg launch.Message, reply string) {
		repliedMsg = msg
		repliedText = reply
	}

	o.Show()
	o.HandleKey('r')

	// Type "hello"
	for _, c := range "hello" {
		o.HandleKey(byte(c))
	}
	// Submit with Enter
	o.HandleKey('\r')

	if repliedText != "hello" {
		t.Errorf("expected reply 'hello', got %q", repliedText)
	}
	if repliedMsg.ID != "1" {
		t.Errorf("expected message ID '1', got %q", repliedMsg.ID)
	}
	if repliedMsg.Author != "Sarah" {
		t.Errorf("expected author 'Sarah', got %q", repliedMsg.Author)
	}
	if o.IsActive() {
		t.Error("overlay should be dismissed after reply submit")
	}
	if !dismissed {
		t.Error("onDismiss should have been called")
	}
	if q.UnreadCount() != 0 {
		t.Errorf("expected 0 unread, got %d", q.UnreadCount())
	}
}

func TestOverlay_ReplyKey_EscCancels(t *testing.T) {
	q := launch.NewNotifyQueue(10, nil)
	q.Push(launch.Message{ID: "1", Source: "slack", Channel: "#backend", Author: "Sarah", Body: "question?"})

	var w testWriter
	o := launch.NewOverlay(q, nil, &w, 30, 80, nil)

	replyCalled := false
	o.OnReply = func(msg launch.Message, reply string) {
		replyCalled = true
	}

	o.Show()
	o.HandleKey('r')

	// Type some text
	o.HandleKey('h')
	o.HandleKey('i')

	// Cancel with standalone ESC
	o.HandleKey(0x1B)
	time.Sleep(200 * time.Millisecond)

	if replyCalled {
		t.Error("OnReply should not be called on cancel")
	}
	if !o.IsActive() {
		t.Error("overlay should still be active after cancel (back to normal mode)")
	}
	// Should be back to normal footer.
	w.Reset()
	o.HandleKey('j') // trigger re-render via navigation
	out := w.String()
	if !strings.Contains(out, "r reply") {
		t.Error("expected normal footer after cancel")
	}
}

func TestOverlay_ReplyKey_Backspace(t *testing.T) {
	q := launch.NewNotifyQueue(10, nil)
	q.Push(launch.Message{ID: "1", Source: "slack", Channel: "#backend", Author: "Sarah", Body: "q?"})

	var w testWriter
	o := launch.NewOverlay(q, nil, &w, 30, 80, nil)

	var repliedText string
	o.OnReply = func(msg launch.Message, reply string) {
		repliedText = reply
	}

	o.Show()
	o.HandleKey('r')
	o.HandleKey('a')
	o.HandleKey('b')
	o.HandleKey('c')
	o.HandleKey(0x7f) // backspace — delete 'c'
	o.HandleKey('\r')

	if repliedText != "ab" {
		t.Errorf("expected 'ab' after backspace, got %q", repliedText)
	}
}

func TestOverlay_ReplyKey_EmptySubmitNoCallback(t *testing.T) {
	q := launch.NewNotifyQueue(10, nil)
	q.Push(launch.Message{ID: "1", Source: "slack", Channel: "#backend", Author: "Sarah", Body: "q?"})

	var w testWriter
	o := launch.NewOverlay(q, nil, &w, 30, 80, nil)

	replyCalled := false
	o.OnReply = func(msg launch.Message, reply string) {
		replyCalled = true
	}

	o.Show()
	o.HandleKey('r')
	o.HandleKey('\r') // submit with empty text

	if replyCalled {
		t.Error("OnReply should not be called for empty reply")
	}
}

func TestOverlay_ReplyKey_BackspaceCtrlH(t *testing.T) {
	q := launch.NewNotifyQueue(10, nil)
	q.Push(launch.Message{ID: "1", Source: "slack", Channel: "#backend", Author: "Sarah", Body: "q?"})

	var w testWriter
	o := launch.NewOverlay(q, nil, &w, 30, 80, nil)

	var repliedText string
	o.OnReply = func(msg launch.Message, reply string) {
		repliedText = reply
	}

	o.Show()
	o.HandleKey('r')
	o.HandleKey('x')
	o.HandleKey('y')
	o.HandleKey(0x08) // Ctrl-H backspace
	o.HandleKey('\r')

	if repliedText != "x" {
		t.Errorf("expected 'x' after Ctrl-H backspace, got %q", repliedText)
	}
}

func TestOverlay_ReplyKey_BackspaceOnEmpty(t *testing.T) {
	q := launch.NewNotifyQueue(10, nil)
	q.Push(launch.Message{ID: "1", Source: "slack", Channel: "#backend", Author: "Sarah", Body: "q?"})

	var w testWriter
	o := launch.NewOverlay(q, nil, &w, 30, 80, nil)

	o.Show()
	o.HandleKey('r')
	o.HandleKey(0x7f) // backspace on empty — should not panic
}

func TestOverlay_ReplyKey_ArrowKeysIgnored(t *testing.T) {
	q := launch.NewNotifyQueue(10, nil)
	q.Push(launch.Message{ID: "1", Source: "slack", Channel: "#backend", Author: "Sarah", Body: "q?"})

	var w testWriter
	o := launch.NewOverlay(q, nil, &w, 30, 80, nil)

	var repliedText string
	o.OnReply = func(msg launch.Message, reply string) {
		repliedText = reply
	}

	o.Show()
	o.HandleKey('r')
	o.HandleKey('h')
	o.HandleKey('i')
	// Arrow down (ESC [ B) — should be ignored in reply mode
	o.HandleKey(0x1B)
	o.HandleKey('[')
	o.HandleKey('B')
	o.HandleKey('\r')

	if repliedText != "hi" {
		t.Errorf("expected 'hi' (arrow keys ignored), got %q", repliedText)
	}
}

func TestOverlay_ReplyKey_EscBracketCancels(t *testing.T) {
	q := launch.NewNotifyQueue(10, nil)
	q.Push(launch.Message{ID: "1", Source: "slack", Channel: "#backend", Author: "Sarah", Body: "q?"})

	var w testWriter
	o := launch.NewOverlay(q, nil, &w, 30, 80, nil)

	replyCalled := false
	o.OnReply = func(msg launch.Message, reply string) {
		replyCalled = true
	}

	o.Show()
	o.HandleKey('r')
	o.HandleKey('x')
	// ESC followed by non-'[' — should cancel reply
	o.HandleKey(0x1B)
	o.HandleKey('x')

	if replyCalled {
		t.Error("OnReply should not be called after ESC cancel")
	}
	if !o.IsActive() {
		t.Error("overlay should still be active after cancel")
	}
}

func TestOverlay_ReplyKey_NonPrintableIgnored(t *testing.T) {
	q := launch.NewNotifyQueue(10, nil)
	q.Push(launch.Message{ID: "1", Source: "slack", Channel: "#backend", Author: "Sarah", Body: "q?"})

	var w testWriter
	o := launch.NewOverlay(q, nil, &w, 30, 80, nil)

	var repliedText string
	o.OnReply = func(msg launch.Message, reply string) {
		repliedText = reply
	}

	o.Show()
	o.HandleKey('r')
	o.HandleKey('a')
	o.HandleKey(0x01) // Ctrl-A — non-printable, should be ignored
	o.HandleKey('b')
	o.HandleKey('\r')

	if repliedText != "ab" {
		t.Errorf("expected 'ab' (control chars ignored), got %q", repliedText)
	}
}

func TestOverlay_ReplyKey_NewlineSubmits(t *testing.T) {
	q := launch.NewNotifyQueue(10, nil)
	q.Push(launch.Message{ID: "1", Source: "slack", Channel: "#backend", Author: "Sarah", Body: "q?"})

	var w testWriter
	o := launch.NewOverlay(q, nil, &w, 30, 80, nil)

	var repliedText string
	o.OnReply = func(msg launch.Message, reply string) {
		repliedText = reply
	}

	o.Show()
	o.HandleKey('r')
	o.HandleKey('o')
	o.HandleKey('k')
	o.HandleKey('\n') // newline also submits

	if repliedText != "ok" {
		t.Errorf("expected 'ok', got %q", repliedText)
	}
}

func TestOverlay_ReplyKey_EmptyQueue(t *testing.T) {
	q := launch.NewNotifyQueue(10, nil)
	var w testWriter
	o := launch.NewOverlay(q, nil, &w, 30, 80, nil)

	replyCalled := false
	o.OnReply = func(msg launch.Message, reply string) {
		replyCalled = true
	}

	o.Show()
	o.HandleKey('r') // should not enter reply mode on empty queue

	if replyCalled {
		t.Error("OnReply should not be called on empty queue")
	}
}

func TestOverlay_FooterShowsReplyAction(t *testing.T) {
	q := launch.NewNotifyQueue(10, nil)
	var w testWriter
	o := launch.NewOverlay(q, nil, &w, 24, 80, nil)
	o.Show()
	out := w.String()

	if !strings.Contains(out, "r reply") {
		t.Error("expected 'r reply' in footer")
	}
}

func TestOverlay_DraftMessage_RendersTag(t *testing.T) {
	q := launch.NewNotifyQueue(10, nil)
	q.Push(launch.Message{
		ID: "1", Source: "slack", Channel: "#backend", Author: "Alice",
		Preview: "original question", Body: "What about the deploy?",
		Draft: "Looks good to me!", DraftFor: "1",
	})

	var w testWriter
	o := launch.NewOverlay(q, nil, &w, 30, 80, nil)
	o.Show()
	out := w.String()

	if !strings.Contains(out, "[draft]") {
		t.Error("expected [draft] tag in message list")
	}
	if !strings.Contains(out, "Draft Reply") {
		t.Error("expected 'Draft Reply' section in detail pane")
	}
	if !strings.Contains(out, "Looks good to me!") {
		t.Error("expected draft text in detail pane")
	}
}

func TestOverlay_DraftMessage_FooterShowsYEN(t *testing.T) {
	q := launch.NewNotifyQueue(10, nil)
	q.Push(launch.Message{
		ID: "1", Source: "slack", Channel: "#backend", Author: "Alice",
		Preview: "question", Body: "question?",
		Draft: "my draft", DraftFor: "1",
	})

	var w testWriter
	o := launch.NewOverlay(q, nil, &w, 30, 80, nil)
	o.Show()
	out := w.String()

	if !strings.Contains(out, "y send") {
		t.Error("expected 'y send' in footer for draft")
	}
	if !strings.Contains(out, "e edit") {
		t.Error("expected 'e edit' in footer for draft")
	}
	if !strings.Contains(out, "n discard") {
		t.Error("expected 'n discard' in footer for draft")
	}
}

func TestOverlay_DraftMessage_NonDraftShowsNormalFooter(t *testing.T) {
	q := launch.NewNotifyQueue(10, nil)
	q.Push(launch.Message{ID: "1", Source: "slack", Author: "Alice", Preview: "normal msg", Body: "normal"})

	var w testWriter
	o := launch.NewOverlay(q, nil, &w, 30, 80, nil)
	o.Show()
	out := w.String()

	if !strings.Contains(out, "r reply") {
		t.Error("expected normal footer for non-draft message")
	}
	if strings.Contains(out, "y send") {
		t.Error("should not show draft footer for non-draft message")
	}
}

func TestOverlay_DraftApprove_YKey(t *testing.T) {
	q := launch.NewNotifyQueue(10, nil)
	q.Push(launch.Message{
		ID: "1", Source: "slack", Channel: "#backend", Author: "Alice",
		Preview: "question", Body: "question?",
		Draft: "my draft", DraftFor: "1",
	})

	var w testWriter
	o := launch.NewOverlay(q, nil, &w, 30, 80, nil)

	var approvedMsg launch.Message
	approveCalled := false
	o.OnDraftApprove = func(msg launch.Message) {
		approveCalled = true
		approvedMsg = msg
	}

	o.Show()
	o.HandleKey('y')

	if !approveCalled {
		t.Fatal("expected OnDraftApprove to be called")
	}
	if approvedMsg.ID != "1" {
		t.Errorf("expected message ID '1', got %q", approvedMsg.ID)
	}
	if approvedMsg.Draft != "my draft" {
		t.Errorf("expected draft text 'my draft', got %q", approvedMsg.Draft)
	}
	// Message should be marked read.
	if q.UnreadCount() != 0 {
		t.Errorf("expected 0 unread, got %d", q.UnreadCount())
	}
}

func TestOverlay_DraftApprove_YKey_NonDraftNoOp(t *testing.T) {
	q := launch.NewNotifyQueue(10, nil)
	q.Push(launch.Message{ID: "1", Source: "slack", Author: "Alice", Preview: "normal", Body: "normal"})

	var w testWriter
	o := launch.NewOverlay(q, nil, &w, 30, 80, nil)

	approveCalled := false
	o.OnDraftApprove = func(msg launch.Message) {
		approveCalled = true
	}

	o.Show()
	o.HandleKey('y')

	if approveCalled {
		t.Error("OnDraftApprove should not be called for non-draft message")
	}
}

func TestOverlay_DraftDiscard_NKey(t *testing.T) {
	q := launch.NewNotifyQueue(10, nil)
	q.Push(launch.Message{
		ID: "1", Source: "slack", Channel: "#backend", Author: "Alice",
		Preview: "question", Body: "question?",
		Draft: "my draft", DraftFor: "1",
	})

	var w testWriter
	o := launch.NewOverlay(q, nil, &w, 30, 80, nil)

	var discardedMsg launch.Message
	discardCalled := false
	o.OnDraftDiscard = func(msg launch.Message) {
		discardCalled = true
		discardedMsg = msg
	}

	o.Show()
	o.HandleKey('n')

	if !discardCalled {
		t.Fatal("expected OnDraftDiscard to be called")
	}
	if discardedMsg.ID != "1" {
		t.Errorf("expected message ID '1', got %q", discardedMsg.ID)
	}
	if q.UnreadCount() != 0 {
		t.Errorf("expected 0 unread, got %d", q.UnreadCount())
	}
}

func TestOverlay_DraftDiscard_NKey_NonDraftNoOp(t *testing.T) {
	q := launch.NewNotifyQueue(10, nil)
	q.Push(launch.Message{ID: "1", Source: "slack", Author: "Alice", Preview: "normal", Body: "normal"})

	var w testWriter
	o := launch.NewOverlay(q, nil, &w, 30, 80, nil)

	discardCalled := false
	o.OnDraftDiscard = func(msg launch.Message) {
		discardCalled = true
	}

	o.Show()
	o.HandleKey('n')

	if discardCalled {
		t.Error("OnDraftDiscard should not be called for non-draft message")
	}
}

func TestOverlay_DraftEdit_EKey(t *testing.T) {
	q := launch.NewNotifyQueue(10, nil)
	q.Push(launch.Message{
		ID: "1", Source: "slack", Channel: "#backend", Author: "Alice",
		Preview: "question", Body: "question?",
		Draft: "my draft", DraftFor: "1",
	})

	var w testWriter
	o := launch.NewOverlay(q, nil, &w, 30, 80, nil)

	o.Show()
	w.Reset()
	o.HandleKey('e')
	out := w.String()

	// Should enter reply mode pre-filled with draft text.
	if !strings.Contains(out, "my draft") {
		t.Error("expected draft text pre-filled in reply input")
	}
	if !strings.Contains(out, "Enter send") {
		t.Error("expected reply mode footer")
	}
	if !o.IsActive() {
		t.Error("overlay should still be active in edit mode")
	}
}

func TestOverlay_DraftEdit_EKey_NonDraftNoOp(t *testing.T) {
	q := launch.NewNotifyQueue(10, nil)
	q.Push(launch.Message{ID: "1", Source: "slack", Author: "Alice", Preview: "normal", Body: "normal"})

	var w testWriter
	o := launch.NewOverlay(q, nil, &w, 30, 80, nil)

	o.Show()
	w.Reset()
	o.HandleKey('e')
	out := w.String()

	// Should not enter reply mode for non-draft.
	if strings.Contains(out, "Enter send") {
		t.Error("should not enter reply mode for non-draft message")
	}
}

func TestOverlay_DraftEdit_SubmitCallsDraftEdit(t *testing.T) {
	q := launch.NewNotifyQueue(10, nil)
	q.Push(launch.Message{
		ID: "1", Source: "slack", Channel: "#backend", Author: "Alice",
		Preview: "question", Body: "question?",
		Draft: "my draft", DraftFor: "1",
	})

	var w testWriter
	copier := launch.NewOutputCopier(strings.NewReader(""), &w, nil)
	dismissed := false
	o := launch.NewOverlay(q, copier, &w, 30, 80, func() {
		dismissed = true
	})

	var editedMsg launch.Message
	var editedText string
	o.OnDraftEdit = func(msg launch.Message, edited string) {
		editedMsg = msg
		editedText = edited
	}

	// Ensure OnReply is NOT called for draft edits.
	replyCalled := false
	o.OnReply = func(msg launch.Message, reply string) {
		replyCalled = true
	}

	o.Show()
	o.HandleKey('e')

	// Append text to the pre-filled draft.
	o.HandleKey('!')
	o.HandleKey('\r')

	if editedText != "my draft!" {
		t.Errorf("expected edited text 'my draft!', got %q", editedText)
	}
	if editedMsg.ID != "1" {
		t.Errorf("expected message ID '1', got %q", editedMsg.ID)
	}
	if replyCalled {
		t.Error("OnReply should not be called for draft edit submission")
	}
	if !dismissed {
		t.Error("onDismiss should have been called")
	}
	if o.IsActive() {
		t.Error("overlay should be dismissed after submitting edit")
	}
}

func TestOverlay_DraftApprove_EmptyQueue(t *testing.T) {
	q := launch.NewNotifyQueue(10, nil)
	var w testWriter
	o := launch.NewOverlay(q, nil, &w, 30, 80, nil)

	approveCalled := false
	o.OnDraftApprove = func(msg launch.Message) {
		approveCalled = true
	}

	o.Show()
	o.HandleKey('y') // should not panic

	if approveCalled {
		t.Error("OnDraftApprove should not be called on empty queue")
	}
}

func TestOverlay_DraftDiscard_EmptyQueue(t *testing.T) {
	q := launch.NewNotifyQueue(10, nil)
	var w testWriter
	o := launch.NewOverlay(q, nil, &w, 30, 80, nil)

	discardCalled := false
	o.OnDraftDiscard = func(msg launch.Message) {
		discardCalled = true
	}

	o.Show()
	o.HandleKey('n') // should not panic

	if discardCalled {
		t.Error("OnDraftDiscard should not be called on empty queue")
	}
}

func TestOverlay_DraftEdit_EmptyQueue(t *testing.T) {
	q := launch.NewNotifyQueue(10, nil)
	var w testWriter
	o := launch.NewOverlay(q, nil, &w, 30, 80, nil)

	o.Show()
	o.HandleKey('e') // should not panic
}

func TestOverlay_ScrollAdjustment(t *testing.T) {
	q := launch.NewNotifyQueue(20, nil)
	// Push enough messages to require scrolling with a small terminal.
	for i := 0; i < 15; i++ {
		q.Push(launch.Message{
			ID:      fmt.Sprintf("%d", i),
			Source:  "slack",
			Author:  fmt.Sprintf("User%d", i),
			Preview: fmt.Sprintf("message %d", i),
			Body:    fmt.Sprintf("body %d", i),
		})
	}

	var w testWriter
	// Use a very small terminal (10 rows) so the visible area is tiny.
	o := launch.NewOverlay(q, nil, &w, 10, 80, nil)
	o.Show()

	// Navigate all the way down to force scroll adjustment.
	for i := 0; i < 14; i++ {
		o.HandleKey('j')
	}

	// Navigate back up to the top.
	for i := 0; i < 14; i++ {
		o.HandleKey('k')
	}

	// The overlay should still be active and not panicked.
	if !o.IsActive() {
		t.Error("overlay should still be active after scrolling")
	}
}

func TestOverlay_EscNonBracket_Dismisses(t *testing.T) {
	q := launch.NewNotifyQueue(10, nil)
	q.Push(launch.Message{ID: "1", Preview: "msg"})
	var w testWriter
	dismissed := false
	o := launch.NewOverlay(q, nil, &w, 24, 80, func() {
		dismissed = true
	})

	o.Show()
	// Send ESC followed by a non-'[' byte — should dismiss.
	o.HandleKey(0x1B)
	o.HandleKey('x')

	if o.IsActive() {
		t.Error("overlay should be dismissed after ESC + non-bracket")
	}
	if !dismissed {
		t.Error("onDismiss should have been called")
	}
}

func TestOverlay_HideWithCopier(t *testing.T) {
	q := launch.NewNotifyQueue(10, nil)
	var w testWriter
	copier := launch.NewOutputCopier(strings.NewReader(""), &w, nil)
	o := launch.NewOverlay(q, copier, &w, 24, 80, nil)

	o.Show()
	o.Hide()

	if o.IsActive() {
		t.Error("should not be active after Hide")
	}
	out := w.String()
	if !strings.Contains(out, "\033[?1049l") {
		t.Error("expected screen restore sequence")
	}
}

func TestOverlay_DismissWithQAndCopier(t *testing.T) {
	q := launch.NewNotifyQueue(10, nil)
	q.Push(launch.Message{ID: "1", Preview: "msg"})
	var w testWriter
	copier := launch.NewOutputCopier(strings.NewReader(""), &w, nil)
	dismissed := false
	o := launch.NewOverlay(q, copier, &w, 24, 80, func() {
		dismissed = true
	})

	o.Show()
	o.HandleKey('q')

	if o.IsActive() {
		t.Error("overlay should be dismissed")
	}
	if !dismissed {
		t.Error("onDismiss should have been called")
	}
}

func TestOverlay_ResizeInactive(t *testing.T) {
	q := launch.NewNotifyQueue(10, nil)
	q.Push(launch.Message{ID: "1", Preview: "msg"})

	var w testWriter
	o := launch.NewOverlay(q, nil, &w, 24, 80, nil)

	// Resize without showing — should not render.
	w.Reset()
	o.Resize(30, 120)
	out := w.String()
	if strings.Contains(out, "aileron notifications") {
		t.Error("should not render when inactive")
	}
}

func TestOverlay_MoveCursorEmptyQueue(t *testing.T) {
	q := launch.NewNotifyQueue(10, nil)
	var w testWriter
	o := launch.NewOverlay(q, nil, &w, 24, 80, nil)
	o.Show()

	// Navigate on empty queue — should not panic.
	o.HandleKey('j')
	o.HandleKey('k')
}

func TestOverlay_DismissSelectedEmptyQueue(t *testing.T) {
	q := launch.NewNotifyQueue(10, nil)
	var w testWriter
	o := launch.NewOverlay(q, nil, &w, 24, 80, nil)
	o.Show()

	// Dismiss on empty queue — should not panic.
	o.HandleKey('d')
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
