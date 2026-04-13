package launch

import (
	"fmt"
	"io"
	"strings"
	"sync"
)

// StatusBar renders a persistent notification bar at the bottom of the
// terminal. It occupies 3 rows: a separator line, a content line, and
// a hint line showing keybindings. When a NotifyQueue is connected,
// unread messages are shown on the left with the branding text on the right.
type StatusBar struct {
	mu    sync.Mutex
	rows  int
	cols  int
	text  string
	queue *NotifyQueue
}

// NewStatusBar creates a status bar with the given terminal dimensions and
// right-aligned text.
func NewStatusBar(rows, cols int, text string) *StatusBar {
	return &StatusBar{rows: rows, cols: cols, text: text}
}

// Resize updates the bar dimensions and re-renders. It clears the old bar
// position first to avoid ghost lines when the terminal grows vertically.
func (b *StatusBar) Resize(w io.Writer, rows, cols int) {
	b.mu.Lock()
	defer b.mu.Unlock()

	// Clear old bar position before updating dimensions.
	if b.rows >= 4 {
		oldSep := b.rows - 2
		oldContent := b.rows - 1
		oldHint := b.rows
		fmt.Fprintf(w, "\0337\033[%d;1H\033[2K\033[%d;1H\033[2K\033[%d;1H\033[2K\0338", oldSep, oldContent, oldHint)
	}

	b.rows = rows
	b.cols = cols
	b.render(w)
}

// Render draws the status bar. It saves the cursor, draws the separator,
// content, and hint in the bottom 3 rows, then restores the cursor.
func (b *StatusBar) Render(w io.Writer) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.render(w)
}

// SetQueue connects a notification queue to the status bar. When
// connected, unread messages are shown on the left side of the bar.
func (b *StatusBar) SetQueue(q *NotifyQueue) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.queue = q
}

func (b *StatusBar) render(w io.Writer) {
	if b.rows < 4 || b.cols < 1 {
		return
	}

	sepRow := b.rows - 2
	contentRow := b.rows - 1
	hintRow := b.rows

	// Build the separator line: dim thin rule
	sep := strings.Repeat("─", b.cols)

	// Build the content line: notifications on the left, branding on the right.
	content := b.buildContent()

	// Build the hint line: keybinding instructions.
	hint := b.buildHintLine()

	var buf strings.Builder
	// Save cursor position
	buf.WriteString("\0337")
	// Move to separator row, clear line, draw dim separator
	fmt.Fprintf(&buf, "\033[%d;1H\033[2K\033[2m%s\033[0m", sepRow, sep)
	// Move to content row, clear line, draw content
	fmt.Fprintf(&buf, "\033[%d;1H\033[2K%s", contentRow, content)
	// Move to hint row, clear line, draw hint
	fmt.Fprintf(&buf, "\033[%d;1H\033[2K%s", hintRow, hint)
	// Restore cursor position
	buf.WriteString("\0338")

	io.WriteString(w, buf.String())
}

// buildContent composes the status bar text line. When a notification
// queue is connected and has unread messages, the format is:
//
//	[N unread] "preview..."                    branding text
//
// Otherwise, the branding text is right-aligned.
func (b *StatusBar) buildContent() string {
	brandWidth := displayWidth(b.text)

	// During quiet hours with no high-priority unread messages, show a
	// quiet indicator instead of the unread count.
	if b.queue != nil && b.queue.IsQuietHours() {
		highUnread := b.queue.HighPriorityUnreadCount()
		if highUnread == 0 {
			left := "\033[2m[quiet]\033[0m"
			leftDisplay := len("[quiet]")
			gap := b.cols - leftDisplay - brandWidth
			if gap < 1 {
				gap = 1
			}
			return left + strings.Repeat(" ", gap) + b.text
		}
	}

	// No queue or no unread messages — right-align branding only.
	if b.queue == nil || b.queue.UnreadCount() == 0 {
		padding := b.cols - brandWidth
		if padding < 0 {
			padding = 0
		}
		return strings.Repeat(" ", padding) + b.text
	}

	unread := b.queue.UnreadCount()
	latest, _ := b.queue.Latest()

	left := fmt.Sprintf("\033[33m[%d unread]\033[0m", unread)
	leftDisplay := len(fmt.Sprintf("[%d unread]", unread)) // display width without ANSI

	// Append draft-pending indicator if any auto-draft messages are waiting.
	if draftPending := b.queue.DraftPendingCount(); draftPending > 0 {
		tag := fmt.Sprintf(" [%d awaiting draft]", draftPending)
		left += fmt.Sprintf(" \033[36m[%d awaiting draft]\033[0m", draftPending)
		leftDisplay += len(tag)
	}

	// Add preview if there's room.
	previewSpace := b.cols - leftDisplay - brandWidth - 3 // 3 for spacing
	if previewSpace > 10 && latest.Preview != "" {
		preview := latest.Preview
		if len(preview) > previewSpace {
			preview = preview[:previewSpace-3] + "..."
		}
		left += " \033[2m" + preview + "\033[0m"
	}

	// Right-align branding.
	// We can't easily compute the exact display width of ANSI-containing
	// left string, so we pad based on the known display widths.
	usedDisplay := leftDisplay
	if previewSpace > 10 && latest.Preview != "" {
		p := latest.Preview
		if len(p) > previewSpace {
			p = p[:previewSpace-3] + "..."
		}
		usedDisplay += 1 + len(p) // space + preview
	}

	gap := b.cols - usedDisplay - brandWidth
	if gap < 1 {
		gap = 1
	}

	return left + strings.Repeat(" ", gap) + b.text
}

// Dims returns the current terminal dimensions (rows, cols).
func (b *StatusBar) Dims() (int, int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.rows, b.cols
}

// BarHeight returns the number of terminal rows the bar occupies.
func (b *StatusBar) BarHeight() int {
	return 3
}

// buildHintLine composes the keybinding hint shown at the bottom of the bar.
func (b *StatusBar) buildHintLine() string {
	return "\033[2mCtrl-] notifications\033[0m"
}

// SetScrollRegion writes the ANSI escape to confine scrolling to the top
// portion of the terminal (above the status bar).
func SetScrollRegion(w io.Writer, top, bottom int) {
	fmt.Fprintf(w, "\033[%d;%dr", top, bottom)
}

// ResetScrollRegion restores the scroll region to the full terminal.
func ResetScrollRegion(w io.Writer) {
	io.WriteString(w, "\033[r")
}

// displayWidth returns the display width of a string, counting emoji and
// multi-byte characters as 2 columns. This is a simple approximation.
func displayWidth(s string) int {
	n := 0
	for _, r := range s {
		if r > 0xFFFF {
			// Emoji and supplementary plane characters are typically 2 columns wide
			n += 2
		} else {
			n++
		}
	}
	return n
}
