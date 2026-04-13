package launch

import (
	"regexp"
	"strings"
)

// ansiRE matches ANSI escape sequences (CSI and OSC).
var ansiRE = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]|\x1b\][^\x1b]*\x1b\\`)

// PanelConfig configures the visual chrome of a panel.
type PanelConfig struct {
	Title       string   // e.g. "✈️ Aileron" or "✈️ Aileron ─ Send Message"
	InnerWidth  int      // 0 = auto (termCols - 4, capped at 80)
	Centered    bool     // vertically and horizontally center the box
	FooterHints []string // e.g. ["y send", "n discard"] rendered in └─ ... ─┘
}

// Panel renders a bordered box with caller-supplied content lines.
type Panel struct {
	config   PanelConfig
	termRows int
	termCols int
	inner    int // effective inner width
}

// NewPanel creates a panel with the given config and terminal dimensions.
func NewPanel(cfg PanelConfig, termRows, termCols int) *Panel {
	inner := cfg.InnerWidth
	if inner <= 0 {
		inner = termCols - 4
		if inner > 80 {
			inner = 80
		}
	}
	if inner < 10 {
		inner = 10
	}
	return &Panel{config: cfg, termRows: termRows, termCols: termCols, inner: inner}
}

// InnerWidth returns the effective inner width of the box content area.
func (p *Panel) InnerWidth() int {
	return p.inner
}

// borderColor wraps s in the yellow border color.
func borderColor(s string) string {
	return "\033[33m" + s + "\033[0m"
}

// Render builds the complete bordered box as a string. contentLines are
// pre-formatted lines to display inside the box. Each line is padded to
// InnerWidth visible characters. Lines longer than InnerWidth are truncated.
func (p *Panel) Render(contentLines []string) string {
	w := p.inner
	var buf strings.Builder

	// Top border: ┌─ Title ─────┐
	titleStr := " " + p.config.Title + " "
	titleWidth := visibleWidth(titleStr)
	dashesAfterTitle := w - titleWidth - 1 // -1 for the leading ─
	if dashesAfterTitle < 1 {
		dashesAfterTitle = 1
	}
	buf.WriteString(borderColor("┌─"+titleStr+strings.Repeat("─", dashesAfterTitle)+"┐") + "\r\n")

	// Content lines: │ content padded │
	for _, line := range contentLines {
		vw := visibleWidth(line)
		pad := w - vw
		if pad < 0 {
			pad = 0
		}
		buf.WriteString(borderColor("│") + line + strings.Repeat(" ", pad) + borderColor("│") + "\r\n")
	}

	// Bottom border: └─ footer ─────┘  or  └─────────┘
	if len(p.config.FooterHints) > 0 {
		footer := " " + strings.Join(p.config.FooterHints, "  ") + " "
		footerWidth := len(footer)
		dashesAfterFooter := w - footerWidth - 1
		if dashesAfterFooter < 1 {
			dashesAfterFooter = 1
		}
		buf.WriteString(borderColor("└─"+footer+strings.Repeat("─", dashesAfterFooter)+"┘") + "\r\n")
	} else {
		buf.WriteString(borderColor("└" + strings.Repeat("─", w) + "┘") + "\r\n")
	}

	return buf.String()
}

// RenderToAltScreen wraps Render() output with alternate screen buffer
// enter, clear screen, and centering. Does NOT include alt-screen exit.
func (p *Panel) RenderToAltScreen(contentLines []string) string {
	rendered := p.Render(contentLines)

	if !p.config.Centered {
		return "\033[?1049h\033[2J\033[1;1H" + rendered
	}

	// Count rendered lines for vertical centering.
	lines := strings.Split(rendered, "\r\n")
	// Remove trailing empty element from split.
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	boxHeight := len(lines)
	boxWidth := p.inner + 2 // inner + left/right border chars

	topPad := (p.termRows - boxHeight) / 2
	if topPad < 0 {
		topPad = 0
	}
	leftPad := (p.termCols - boxWidth) / 2
	if leftPad < 0 {
		leftPad = 0
	}
	margin := strings.Repeat(" ", leftPad)

	var buf strings.Builder
	buf.WriteString("\033[?1049h\033[2J\033[1;1H")
	for i := 0; i < topPad; i++ {
		buf.WriteString("\r\n")
	}
	for _, line := range lines {
		buf.WriteString(margin)
		buf.WriteString(line)
		buf.WriteString("\r\n")
	}
	return buf.String()
}

// SeparatorLine returns a horizontal separator that spans the inner width
// of the panel, using ├ and ┤ side connectors.
func (p *Panel) SeparatorLine() string {
	return borderColor("├") + borderColor(strings.Repeat("─", p.inner)) + borderColor("┤")
}

// PadLine returns a line padded with spaces to the panel's inner width.
// Useful for building content lines with left-aligned text.
func (p *Panel) PadLine(text string) string {
	vw := visibleWidth(text)
	pad := p.inner - vw
	if pad < 0 {
		pad = 0
	}
	return text + strings.Repeat(" ", pad)
}

// BlankLine returns an empty content line (all spaces, inner width).
func (p *Panel) BlankLine() string {
	return strings.Repeat(" ", p.inner)
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
