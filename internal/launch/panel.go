package launch

import (
	"fmt"
	"regexp"
	"strings"
)

// ansiRE matches ANSI escape sequences (CSI and OSC).
var ansiRE = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]|\x1b\][^\x1b]*\x1b\\`)

// PanelConfig configures the visual chrome of a panel.
type PanelConfig struct {
	Title       string   // e.g. "✈️ Aileron" or "✈️ Aileron ─ Vault"
	FooterHints []string // e.g. ["y send", "n discard"] rendered in footer
}

// Panel renders content with a yellow left accent bar. All Aileron UI
// uses this style for a consistent look: a colored │ prefix on every
// line, with a title header and optional footer hints.
type Panel struct {
	config   PanelConfig
	termRows int
	termCols int
}

// NewPanel creates a panel with the given config and terminal dimensions.
func NewPanel(cfg PanelConfig, termRows, termCols int) *Panel {
	return &Panel{config: cfg, termRows: termRows, termCols: termCols}
}

// ContentWidth returns the usable width for content (terminal width
// minus the 2-char accent prefix "│ ").
func (p *Panel) ContentWidth() int {
	w := p.termCols - 2
	if w < 10 {
		w = 10
	}
	return w
}

// accent returns the yellow left-border prefix.
func accent() string {
	return "\033[33m│\033[0m "
}

// Render builds the complete panel as a string. contentLines are
// pre-formatted lines displayed between the header and footer.
func (p *Panel) Render(contentLines []string) string {
	w := p.ContentWidth()
	a := accent()

	var buf strings.Builder

	// Header: accent + bold title.
	buf.WriteString(a)
	fmt.Fprintf(&buf, "\033[1m%s\033[0m\r\n", p.config.Title)
	// Header separator.
	buf.WriteString(a)
	buf.WriteString("\033[2m" + strings.Repeat("─", w) + "\033[0m\r\n")

	// Content lines.
	for _, line := range contentLines {
		buf.WriteString(a)
		buf.WriteString(line)
		buf.WriteString("\r\n")
	}

	// Footer.
	buf.WriteString(a + "\r\n")
	buf.WriteString(a)
	buf.WriteString("\033[2m" + strings.Repeat("─", w) + "\033[0m\r\n")
	if len(p.config.FooterHints) > 0 {
		buf.WriteString(a)
		buf.WriteString("\033[2m" + strings.Join(p.config.FooterHints, "  ") + "\033[0m")
	}

	return buf.String()
}

// RenderToAltScreen wraps Render() output with alternate screen buffer
// enter and clear screen. Does NOT include alt-screen exit.
func (p *Panel) RenderToAltScreen(contentLines []string) string {
	return "\033[?1049h\033[2J\033[1;1H" + p.Render(contentLines)
}

// SeparatorLine returns a dim horizontal rule for in-panel dividers.
func (p *Panel) SeparatorLine() string {
	return "\033[2m" + strings.Repeat("─", p.ContentWidth()) + "\033[0m"
}

// visibleWidth returns the display width of a string, excluding ANSI
// escape sequences. Emoji and supplementary-plane characters count as 2.
func visibleWidth(s string) int {
	stripped := ansiRE.ReplaceAllString(s, "")
	return displayWidth(stripped)
}

// wrapText splits text into lines of at most maxWidth characters,
// breaking at word boundaries where possible.
func wrapText(text string, maxWidth int) []string {
	if len(text) <= maxWidth {
		return []string{text}
	}
	var lines []string
	for len(text) > maxWidth {
		idx := strings.LastIndex(text[:maxWidth], " ")
		if idx < 0 {
			idx = maxWidth
		}
		lines = append(lines, text[:idx])
		text = strings.TrimLeft(text[idx:], " ")
	}
	if len(text) > 0 {
		lines = append(lines, text)
	}
	return lines
}
