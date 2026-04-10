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

	// Reply mode state.
	replyMode bool
	replyBuf  strings.Builder
	replyMsg  Message

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
	o.replyMode = false
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
	if reply != "" && o.OnReply != nil {
		cb := o.OnReply
		o.mu.Unlock()
		cb(msg, reply)
		o.mu.Lock()
	}
}

func (o *Overlay) cancelReply() {
	o.replyMode = false
	o.replyBuf.Reset()
	o.render()
}

func (o *Overlay) render() {
	msgs := o.queue.Messages()

	var buf strings.Builder
	// Clear screen and move to top.
	buf.WriteString("\033[2J\033[1;1H")

	// Header.
	header := fmt.Sprintf(" aileron notifications (%d messages) — Esc/q to return", len(msgs))
	if len(header) > o.cols {
		header = header[:o.cols]
	}
	fmt.Fprintf(&buf, "\033[1m%s\033[0m\n", header)
	buf.WriteString(strings.Repeat("─", o.cols) + "\n")

	// Reserve rows for the detail pane (separator + channel/author + up to 5 body lines).
	detailRows := 8
	// Message list.
	visible := o.rows - 4 - detailRows
	if visible < 1 {
		visible = 1
	}

	if len(msgs) == 0 {
		buf.WriteString("\n  No notifications.\n")
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

			line := fmt.Sprintf("%s%s %s · %s: %s",
				cursor, readMark, m.Source, m.Author, m.Preview)

			// Truncate to terminal width.
			if displayWidth(line) > o.cols {
				line = line[:o.cols]
			}
			buf.WriteString(line + "\n")
		}

		// Detail pane for selected message.
		if o.cursorPos < len(msgs) {
			selected := msgs[o.cursorPos]
			buf.WriteString("\n" + strings.Repeat("─", o.cols) + "\n")
			buf.WriteString(fmt.Sprintf(" \033[1m%s · %s\033[0m\n", selected.Channel, selected.Author))
			bodyLines := wrapText(selected.Body, o.cols-2)
			maxBodyLines := 5
			for i, line := range bodyLines {
				if i >= maxBodyLines {
					buf.WriteString("  ...\n")
					break
				}
				buf.WriteString(" " + line + "\n")
			}

			// Reply input box.
			if o.replyMode {
				buf.WriteString("\n")
				buf.WriteString(fmt.Sprintf(" \033[33m>\033[0m %s\033[7m \033[0m\n", o.replyBuf.String()))
			}
		}
	}

	// Footer.
	var footer string
	if o.replyMode {
		footer = "\n" + strings.Repeat("─", o.cols) + "\n"
		footer += " Enter send  Esc cancel"
	} else {
		footer = "\n" + strings.Repeat("─", o.cols) + "\n"
		footer += " j/k or ↑/↓ navigate  r reply  a draft reply  d dismiss  q return"
	}
	buf.WriteString(footer)

	fmt.Fprint(o.stdout, buf.String())
}
