package launch

import (
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

// Overlay renders a full-screen notification viewer when activated.
// It uses the terminal's alternate screen buffer so the agent's output
// is preserved and restored automatically on dismiss.
type Overlay struct {
	mu        sync.Mutex
	queue     *NotifyQueue
	copier    *OutputCopier
	stdout    io.Writer
	rows      int
	cols      int
	scrollPos int // index of top visible message
	cursorPos int // index of selected message
	active         bool
	onDismiss      func()
	OnDraftRequest func(msg Message)
	OnReply        func(msg Message, reply string)
	OnDraftApprove func(msg Message)         // called when user presses 'y' on a draft
	OnDraftDiscard func(msg Message)         // called when user presses 'n' on a draft
	OnDraftEdit    func(msg Message, edited string) // called when user edits and sends a draft
	OnDraftConverse func(msg Message, feedback string) // called when user provides revision feedback

	// Reply mode state.
	replyMode    bool
	converseMode bool // true when reply mode is collecting revision feedback
	replyBuf     strings.Builder
	replyMsg     Message

	// Escape sequence state machine for arrow keys.
	escState int // 0=normal, 1=got ESC, 2=got ESC+[
	escTimer *time.Timer
}

// NewOverlay creates an overlay that renders notifications from the
// given queue. The onDismiss callback is called when the user presses
// Escape to return to the agent session.
func NewOverlay(queue *NotifyQueue, copier *OutputCopier, stdout io.Writer, rows, cols int, onDismiss func()) *Overlay {
	return &Overlay{
		queue:     queue,
		copier:    copier,
		stdout:    stdout,
		rows:      rows,
		cols:      cols,
		onDismiss: onDismiss,
	}
}

// Show activates the overlay, switching to the alternate screen buffer
// and rendering the notification list.
func (o *Overlay) Show() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.active = true
	o.scrollPos = 0
	o.cursorPos = 0
	// Switch to alternate screen buffer and hide cursor.
	fmt.Fprint(o.stdout, "\033[?1049h\033[?25l")
	o.render()
}

// Hide dismisses the overlay, restoring the alternate screen buffer
// and flushing any buffered agent output.
func (o *Overlay) Hide() {
	o.mu.Lock()
	o.active = false
	// Restore original screen buffer and show cursor.
	fmt.Fprint(o.stdout, "\033[?25h\033[?1049l")
	o.mu.Unlock()

	if o.copier != nil {
		o.copier.Flush()
	}
}

// IsActive returns whether the overlay is currently shown.
func (o *Overlay) IsActive() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.active
}

// HandleKey processes a keystroke while the overlay is active.
// Arrow keys arrive as 3-byte escape sequences (\033[A, \033[B).
// A standalone Escape dismisses the overlay.
func (o *Overlay) HandleKey(b byte) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if o.replyMode {
		o.handleReplyKey(b)
		return
	}

	switch o.escState {
	case 1: // got ESC
		if b == '[' {
			o.escState = 2
			return
		}
		// ESC followed by non-'[' — treat the ESC as dismiss.
		o.escState = 0
		o.dismiss()
		return
	case 2: // got ESC+[
		o.escState = 0
		switch b {
		case 'A': // up
			o.moveCursor(-1)
		case 'B': // down
			o.moveCursor(1)
		}
		return
	}

	// Normal state.
	switch b {
	case 0x1B: // ESC
		o.escState = 1
		// Set a timer to handle standalone ESC (no follow-up byte).
		if o.escTimer != nil {
			o.escTimer.Stop()
		}
		o.escTimer = time.AfterFunc(100*time.Millisecond, func() {
			o.mu.Lock()
			if o.escState == 1 {
				o.escState = 0
				o.dismiss()
			}
			o.mu.Unlock()
		})
	case 'q', 'Q':
		o.dismiss()
	case 'k':
		o.moveCursor(-1)
	case 'j':
		o.moveCursor(1)
	case 'd':
		o.dismissSelected()
	case 'a':
		o.draftSelected()
	case 'r':
		o.startReply()
	case 'y':
		o.approveDraft()
	case 'e':
		o.editDraft()
	case 'c':
		o.converseDraft()
	case 'n':
		o.discardDraft()
	}
}

// Resize updates the overlay dimensions and re-renders if active.
func (o *Overlay) Resize(rows, cols int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.rows = rows
	o.cols = cols
	if o.active {
		o.render()
	}
}

func (o *Overlay) dismiss() {
	o.active = false
	fmt.Fprint(o.stdout, "\033[?25h\033[?1049l")
	if o.copier != nil {
		// Unlock before Flush to avoid holding the overlay lock during I/O.
		o.mu.Unlock()
		o.copier.Flush()
		o.mu.Lock()
	}
	if o.onDismiss != nil {
		cb := o.onDismiss
		o.mu.Unlock()
		cb()
		o.mu.Lock()
	}
}

func (o *Overlay) moveCursor(delta int) {
	msgs := o.queue.Messages()
	if len(msgs) == 0 {
		return
	}
	o.cursorPos += delta
	if o.cursorPos < 0 {
		o.cursorPos = 0
	}
	if o.cursorPos >= len(msgs) {
		o.cursorPos = len(msgs) - 1
	}
	// Adjust scroll to keep cursor visible.
	visible := o.rows - 4 // header + footer + borders
	if visible < 1 {
		visible = 1
	}
	if o.cursorPos < o.scrollPos {
		o.scrollPos = o.cursorPos
	}
	if o.cursorPos >= o.scrollPos+visible {
		o.scrollPos = o.cursorPos - visible + 1
	}
	o.render()
}

func (o *Overlay) dismissSelected() {
	msgs := o.queue.Messages()
	if o.cursorPos < len(msgs) {
		o.queue.MarkRead(msgs[o.cursorPos].ID)
		o.render()
	}
}

func (o *Overlay) draftSelected() {
	msgs := o.queue.Messages()
	if o.cursorPos >= len(msgs) {
		return
	}
	msg := msgs[o.cursorPos]
	o.queue.MarkRead(msg.ID)

	// Dismiss the overlay first, then fire the draft callback.
	o.active = false
	fmt.Fprint(o.stdout, "\033[?25h\033[?1049l")
	if o.copier != nil {
		o.mu.Unlock()
		o.copier.Flush()
		o.mu.Lock()
	}
	if o.onDismiss != nil {
		cb := o.onDismiss
		o.mu.Unlock()
		cb()
		o.mu.Lock()
	}
	if o.OnDraftRequest != nil {
		cb := o.OnDraftRequest
		o.mu.Unlock()
		cb(msg)
		o.mu.Lock()
	}
}

func (o *Overlay) selectedIsDraft() bool {
	msgs := o.queue.Messages()
	if o.cursorPos >= len(msgs) {
		return false
	}
	return msgs[o.cursorPos].Draft != ""
}

func (o *Overlay) approveDraft() {
	msgs := o.queue.Messages()
	if o.cursorPos >= len(msgs) {
		return
	}
	msg := msgs[o.cursorPos]
	if msg.Draft == "" {
		return
	}
	o.queue.MarkRead(msg.ID)

	if o.OnDraftApprove != nil {
		cb := o.OnDraftApprove
		o.mu.Unlock()
		cb(msg)
		o.mu.Lock()
	}
	o.render()
}

func (o *Overlay) discardDraft() {
	msgs := o.queue.Messages()
	if o.cursorPos >= len(msgs) {
		return
	}
	msg := msgs[o.cursorPos]
	if msg.Draft == "" {
		return
	}
	o.queue.MarkRead(msg.ID)

	if o.OnDraftDiscard != nil {
		cb := o.OnDraftDiscard
		o.mu.Unlock()
		cb(msg)
		o.mu.Lock()
	}
	o.render()
}

func (o *Overlay) editDraft() {
	msgs := o.queue.Messages()
	if o.cursorPos >= len(msgs) {
		return
	}
	msg := msgs[o.cursorPos]
	if msg.Draft == "" {
		return
	}
	// Enter reply mode pre-filled with the draft text.
	o.replyMode = true
	o.replyMsg = msg
	o.replyBuf.Reset()
	o.replyBuf.WriteString(msg.Draft)
	o.render()
}

func (o *Overlay) converseDraft() {
	msgs := o.queue.Messages()
	if o.cursorPos >= len(msgs) {
		return
	}
	msg := msgs[o.cursorPos]
	if msg.Draft == "" {
		return
	}
	// Enter reply mode for revision feedback (empty buffer).
	o.replyMode = true
	o.converseMode = true
	o.replyMsg = msg
	o.replyBuf.Reset()
	o.render()
}

func (o *Overlay) startReply() {
	msgs := o.queue.Messages()
	if o.cursorPos >= len(msgs) {
		return
	}
	o.replyMode = true
	o.replyMsg = msgs[o.cursorPos]
	o.replyBuf.Reset()
	o.render()
}

func (o *Overlay) handleReplyKey(b byte) {
	switch o.escState {
	case 1: // got ESC
		if b == '[' {
			o.escState = 2
			return
		}
		// Standalone ESC — cancel reply.
		o.escState = 0
		o.cancelReply()
		return
	case 2: // got ESC+[
		// Ignore arrow keys in reply mode.
		o.escState = 0
		return
	}

	switch b {
	case 0x1B: // ESC
		o.escState = 1
		if o.escTimer != nil {
			o.escTimer.Stop()
		}
		o.escTimer = time.AfterFunc(100*time.Millisecond, func() {
			o.mu.Lock()
			if o.escState == 1 {
				o.escState = 0
				o.cancelReply()
			}
			o.mu.Unlock()
		})
	case '\r', '\n': // Enter — submit
		o.submitReply()
	case 0x7f, 0x08: // Backspace / DEL
		s := o.replyBuf.String()
		if len(s) > 0 {
			o.replyBuf.Reset()
			o.replyBuf.WriteString(s[:len(s)-1])
		}
		o.render()
	default:
		if b >= 0x20 && b < 0x7f { // printable ASCII
			o.replyBuf.WriteByte(b)
			o.render()
		}
	}
}

func (o *Overlay) submitReply() {
	reply := o.replyBuf.String()
	msg := o.replyMsg
	isConverse := o.converseMode
	isDraftEdit := !isConverse && msg.Draft != ""
	o.replyMode = false
	o.converseMode = false
	o.replyBuf.Reset()
	o.queue.MarkRead(msg.ID)

	// Dismiss the overlay, then fire the reply callback.
	o.active = false
	fmt.Fprint(o.stdout, "\033[?25h\033[?1049l")
	if o.copier != nil {
		o.mu.Unlock()
		o.copier.Flush()
		o.mu.Lock()
	}
	if o.onDismiss != nil {
		cb := o.onDismiss
		o.mu.Unlock()
		cb()
		o.mu.Lock()
	}
	if reply != "" {
		if isConverse && o.OnDraftConverse != nil {
			// Clear the current draft so the revision cycle can restart.
			o.queue.ClearDraft(msg.ID)
			cb := o.OnDraftConverse
			o.mu.Unlock()
			cb(msg, reply)
			o.mu.Lock()
		} else if isDraftEdit && o.OnDraftEdit != nil {
			cb := o.OnDraftEdit
			o.mu.Unlock()
			cb(msg, reply)
			o.mu.Lock()
		} else if o.OnReply != nil {
			cb := o.OnReply
			o.mu.Unlock()
			cb(msg, reply)
			o.mu.Lock()
		}
	}
}

func (o *Overlay) cancelReply() {
	o.replyMode = false
	o.converseMode = false
	o.replyBuf.Reset()
	o.render()
}

func (o *Overlay) render() {
	msgs := o.queue.Messages()

	// Determine footer hints based on current mode.
	var hints []string
	if o.replyMode {
		hints = []string{"Enter send", "Esc cancel"}
	} else if o.selectedIsDraft() {
		hints = []string{"j/k navigate", "y send", "e edit", "c revise", "n discard", "q return"}
	} else {
		hints = []string{"j/k navigate", "r reply", "a draft reply", "d dismiss", "q return"}
	}

	panel := NewPanel(PanelConfig{
		Title:       fmt.Sprintf("aileron notifications (%d messages)", len(msgs)),
		FooterHints: hints,
	}, o.rows, o.cols)

	w := panel.ContentWidth()

	// Reserve rows for detail pane.
	detailRows := 8
	visible := o.rows - 4 - detailRows
	if visible < 1 {
		visible = 1
	}

	var content []string

	if len(msgs) == 0 {
		content = append(content, "")
		content = append(content, "  No notifications.")
		content = append(content, "")
	} else {
		end := o.scrollPos + visible
		if end > len(msgs) {
			end = len(msgs)
		}
		for i := o.scrollPos; i < end; i++ {
			m := msgs[i]
			cursor := "  "
			if i == o.cursorPos {
				cursor = "\033[7m>\033[0m "
			}
			readMark := "\033[33m●\033[0m"
			if m.Read {
				readMark = " "
			}

			var line string
			if m.Draft != "" {
				line = fmt.Sprintf("%s%s \033[36m[draft]\033[0m %s · %s: %s",
					cursor, readMark, m.Source, m.Author, m.Preview)
			} else {
				line = fmt.Sprintf("%s%s %s · %s: %s",
					cursor, readMark, m.Source, m.Author, m.Preview)
			}
			content = append(content, line)
		}

		// Detail pane for selected message.
		if o.cursorPos < len(msgs) {
			selected := msgs[o.cursorPos]
			content = append(content, "")
			content = append(content, panel.SeparatorLine())
			content = append(content, fmt.Sprintf(" \033[1m%s · %s\033[0m", selected.Channel, selected.Author))
			bodyLines := wrapText(selected.Body, w-1)
			maxBodyLines := 5
			for i, line := range bodyLines {
				if i >= maxBodyLines {
					content = append(content, "  ...")
					break
				}
				content = append(content, " "+line)
			}

			// Draft reply section.
			if selected.Draft != "" {
				content = append(content, "")
				content = append(content, " \033[36m── Draft Reply ──\033[0m")
				draftLines := wrapText(selected.Draft, w-1)
				for i, line := range draftLines {
					if i >= maxBodyLines {
						content = append(content, "  ...")
						break
					}
					content = append(content, " "+line)
				}
			}

			// Reply input box.
			if o.replyMode {
				content = append(content, "")
				label := "\033[33m>\033[0m"
				if o.converseMode {
					label = "\033[33mfeedback>\033[0m"
				}
				content = append(content, fmt.Sprintf(" %s %s\033[7m \033[0m", label, o.replyBuf.String()))
			}
		}
	}

	// Clear screen and render.
	var buf strings.Builder
	buf.WriteString("\033[2J\033[1;1H")
	buf.WriteString(panel.Render(content))
	fmt.Fprint(o.stdout, buf.String())
}
