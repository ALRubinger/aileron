package launch_test

import (
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/core/launch"
)

func TestPanel_Render_BasicBox(t *testing.T) {
	p := launch.NewPanel(launch.PanelConfig{
		Title:      "✈️ Aileron",
		InnerWidth: 40,
	}, 24, 80)

	out := p.Render([]string{
		p.PadLine("  Hello, world!"),
	})

	if !strings.Contains(out, "┌") {
		t.Error("expected top-left corner")
	}
	if !strings.Contains(out, "┘") {
		t.Error("expected bottom-right corner")
	}
	if !strings.Contains(out, "Aileron") {
		t.Error("expected title in top border")
	}
	if !strings.Contains(out, "Hello, world!") {
		t.Error("expected content")
	}
	if !strings.Contains(out, "│") {
		t.Error("expected side borders")
	}
}

func TestPanel_Render_FooterHints(t *testing.T) {
	p := launch.NewPanel(launch.PanelConfig{
		Title:       "✈️ Aileron",
		InnerWidth:  40,
		FooterHints: []string{"y send", "n discard"},
	}, 24, 80)

	out := p.Render([]string{p.BlankLine()})

	if !strings.Contains(out, "y send") {
		t.Error("expected 'y send' in footer")
	}
	if !strings.Contains(out, "n discard") {
		t.Error("expected 'n discard' in footer")
	}
	if !strings.Contains(out, "└") {
		t.Error("expected bottom-left corner")
	}
}

func TestPanel_Render_NoFooter(t *testing.T) {
	p := launch.NewPanel(launch.PanelConfig{
		Title:      "Test",
		InnerWidth: 30,
	}, 24, 80)

	out := p.Render([]string{p.BlankLine()})

	if !strings.Contains(out, "└") {
		t.Error("expected bottom border")
	}
	// Should not have stray text in footer area.
	lines := strings.Split(out, "\r\n")
	for _, line := range lines {
		if strings.Contains(line, "└") && strings.Contains(line, "y ") {
			t.Error("should not contain hint text when FooterHints is empty")
		}
	}
}

func TestPanel_InnerWidth_Auto(t *testing.T) {
	p := launch.NewPanel(launch.PanelConfig{
		Title: "Test",
	}, 24, 100)

	// Auto: termCols - 4, capped at 80.
	if p.InnerWidth() != 80 {
		t.Errorf("expected auto inner width 80 (capped), got %d", p.InnerWidth())
	}

	p2 := launch.NewPanel(launch.PanelConfig{Title: "Test"}, 24, 60)
	if p2.InnerWidth() != 56 {
		t.Errorf("expected auto inner width 56 (60-4), got %d", p2.InnerWidth())
	}
}

func TestPanel_InnerWidth_Explicit(t *testing.T) {
	p := launch.NewPanel(launch.PanelConfig{
		Title:      "Test",
		InnerWidth: 74,
	}, 24, 80)

	if p.InnerWidth() != 74 {
		t.Errorf("expected inner width 74, got %d", p.InnerWidth())
	}
}

func TestPanel_SeparatorLine(t *testing.T) {
	p := launch.NewPanel(launch.PanelConfig{
		Title:      "Test",
		InnerWidth: 30,
	}, 24, 80)

	sep := p.SeparatorLine()
	if !strings.Contains(sep, "├") {
		t.Error("expected ├ in separator")
	}
	if !strings.Contains(sep, "┤") {
		t.Error("expected ┤ in separator")
	}
	if !strings.Contains(sep, "─") {
		t.Error("expected ─ in separator")
	}
}

func TestPanel_PadLine(t *testing.T) {
	p := launch.NewPanel(launch.PanelConfig{
		Title:      "Test",
		InnerWidth: 20,
	}, 24, 80)

	padded := p.PadLine("hi")
	// "hi" is 2 chars, inner width 20, so 18 trailing spaces.
	if len(padded) != 20 {
		t.Errorf("expected padded length 20, got %d", len(padded))
	}
}

func TestPanel_PadLine_WithANSI(t *testing.T) {
	p := launch.NewPanel(launch.PanelConfig{
		Title:      "Test",
		InnerWidth: 20,
	}, 24, 80)

	// ANSI codes should not count toward visible width.
	padded := p.PadLine("\033[1mhi\033[0m")
	// Visible "hi" = 2, so 18 spaces of padding, but total bytes > 20 due to ANSI.
	if !strings.HasSuffix(padded, strings.Repeat(" ", 18)) {
		t.Error("expected 18 trailing spaces for ANSI-styled 2-char text")
	}
}

func TestPanel_BlankLine(t *testing.T) {
	p := launch.NewPanel(launch.PanelConfig{
		Title:      "Test",
		InnerWidth: 15,
	}, 24, 80)

	blank := p.BlankLine()
	if len(blank) != 15 {
		t.Errorf("expected blank line length 15, got %d", len(blank))
	}
}

func TestPanel_RenderToAltScreen_Centered(t *testing.T) {
	p := launch.NewPanel(launch.PanelConfig{
		Title:      "Test",
		InnerWidth: 40,
		Centered:   true,
	}, 24, 80)

	out := p.RenderToAltScreen([]string{p.PadLine("content")})

	if !strings.HasPrefix(out, "\033[?1049h") {
		t.Error("expected alt-screen enter at start")
	}
	if !strings.Contains(out, "content") {
		t.Error("expected content in output")
	}
	// Should have leading spaces for horizontal centering.
	// Box width = 42 (40 inner + 2 borders), terminal = 80, margin = 19.
	if !strings.Contains(out, strings.Repeat(" ", 19)) {
		t.Error("expected horizontal centering margin")
	}
}

func TestPanel_RenderToAltScreen_NotCentered(t *testing.T) {
	p := launch.NewPanel(launch.PanelConfig{
		Title:      "Test",
		InnerWidth: 40,
		Centered:   false,
	}, 24, 80)

	out := p.RenderToAltScreen([]string{p.PadLine("content")})

	if !strings.HasPrefix(out, "\033[?1049h") {
		t.Error("expected alt-screen enter")
	}
	// Non-centered should not have leading margin spaces before the box.
	afterClear := strings.TrimPrefix(out, "\033[?1049h\033[2J\033[1;1H")
	if strings.HasPrefix(afterClear, "   ") {
		t.Error("non-centered output should not have large left margin")
	}
}

func TestVisibleWidth_PlainText(t *testing.T) {
	p := launch.NewPanel(launch.PanelConfig{Title: "T", InnerWidth: 20}, 24, 80)
	line := p.PadLine("hello")
	// "hello" = 5 visible chars, padded to 20.
	if len(line) != 20 {
		t.Errorf("expected length 20 for 'hello' padded, got %d", len(line))
	}
}

func TestVisibleWidth_ANSIStripped(t *testing.T) {
	p := launch.NewPanel(launch.PanelConfig{Title: "T", InnerWidth: 20}, 24, 80)
	// Bold "hi" is 2 visible chars.
	line := p.PadLine("\033[1mhi\033[0m")
	// Should end with 18 spaces.
	if !strings.HasSuffix(line, strings.Repeat(" ", 18)) {
		t.Errorf("expected 18 trailing spaces, got line=%q", line)
	}
}

func TestWrapText_ShortLine(t *testing.T) {
	lines := launch.WrapTextForTest("short", 20)
	if len(lines) != 1 || lines[0] != "short" {
		t.Errorf("expected single line 'short', got %v", lines)
	}
}

func TestWrapText_LongLine(t *testing.T) {
	text := "the quick brown fox jumps over the lazy dog"
	lines := launch.WrapTextForTest(text, 20)
	if len(lines) < 2 {
		t.Errorf("expected multiple lines, got %d", len(lines))
	}
	for _, line := range lines {
		if len(line) > 20 {
			t.Errorf("line exceeds maxWidth: %q", line)
		}
	}
}
