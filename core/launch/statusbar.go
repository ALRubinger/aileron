package launch

import (
	"fmt"
	"io"
	"strings"
	"sync"
)

// StatusBar renders a persistent notification bar at the bottom of the
// terminal. It occupies 2 rows: a separator line and a content line.
type StatusBar struct {
	mu   sync.Mutex
	rows int
	cols int
	text string
}

// NewStatusBar creates a status bar with the given terminal dimensions and
// right-aligned text.
func NewStatusBar(rows, cols int, text string) *StatusBar {
	return &StatusBar{rows: rows, cols: cols, text: text}
}

// Resize updates the bar dimensions and re-renders.
func (b *StatusBar) Resize(w io.Writer, rows, cols int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rows = rows
	b.cols = cols
	b.render(w)
}

// Render draws the status bar. It saves the cursor, draws the separator
// and content in the bottom 2 rows, then restores the cursor.
func (b *StatusBar) Render(w io.Writer) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.render(w)
}

func (b *StatusBar) render(w io.Writer) {
	if b.rows < 3 || b.cols < 1 {
		return
	}

	sepRow := b.rows - 1
	textRow := b.rows

	// Build the separator line: dim thin rule
	sep := strings.Repeat("─", b.cols)

	// Right-align the text
	content := b.text
	displayLen := displayWidth(content)
	if displayLen >= b.cols {
		content = content[:b.cols]
	}
	padding := b.cols - displayLen
	if padding < 0 {
		padding = 0
	}

	var buf strings.Builder
	// Save cursor position
	buf.WriteString("\0337")
	// Move to separator row, clear line, draw dim separator
	fmt.Fprintf(&buf, "\033[%d;1H\033[2K\033[2m%s\033[0m", sepRow, sep)
	// Move to text row, clear line, draw right-aligned text
	fmt.Fprintf(&buf, "\033[%d;1H\033[2K%s%s", textRow, strings.Repeat(" ", padding), content)
	// Restore cursor position
	buf.WriteString("\0338")

	io.WriteString(w, buf.String())
}

// BarHeight returns the number of terminal rows the bar occupies.
func (b *StatusBar) BarHeight() int {
	return 2
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
