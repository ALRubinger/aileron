package launch_test

import (
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/core/launch"
)

func TestPanel_Render_LeftAccent(t *testing.T) {
	p := launch.NewPanel(launch.PanelConfig{
		Title: "✈️ Aileron",
	}, 24, 80)

	out := p.Render([]string{"  Hello, world!"})

	if !strings.Contains(out, "│") {
		t.Error("expected left accent bar")
	}
	if !strings.Contains(out, "Aileron") {
		t.Error("expected title in header")
	}
	if !strings.Contains(out, "Hello, world!") {
		t.Error("expected content")
	}
}

func TestPanel_Render_FooterHints(t *testing.T) {
	p := launch.NewPanel(launch.PanelConfig{
		Title:       "✈️ Aileron",
		FooterHints: []string{"y send", "n discard"},
	}, 24, 80)

	out := p.Render([]string{""})

	if !strings.Contains(out, "y send") {
		t.Error("expected 'y send' in footer")
	}
	if !strings.Contains(out, "n discard") {
		t.Error("expected 'n discard' in footer")
	}
}

func TestPanel_Render_NoFooter(t *testing.T) {
	p := launch.NewPanel(launch.PanelConfig{
		Title: "Test",
	}, 24, 80)

	out := p.Render([]string{""})

	// Footer separator should still appear.
	if !strings.Contains(out, "─") {
		t.Error("expected separator in output")
	}
}

func TestPanel_ContentWidth(t *testing.T) {
	p := launch.NewPanel(launch.PanelConfig{Title: "Test"}, 24, 80)
	if p.ContentWidth() != 78 {
		t.Errorf("expected content width 78 (80-2), got %d", p.ContentWidth())
	}

	p2 := launch.NewPanel(launch.PanelConfig{Title: "Test"}, 24, 5)
	if p2.ContentWidth() != 10 {
		t.Errorf("expected content width 10 (minimum), got %d", p2.ContentWidth())
	}
}

func TestPanel_SeparatorLine(t *testing.T) {
	p := launch.NewPanel(launch.PanelConfig{Title: "Test"}, 24, 80)

	sep := p.SeparatorLine()
	if !strings.Contains(sep, "─") {
		t.Error("expected ─ in separator")
	}
}

func TestPanel_RenderToAltScreen(t *testing.T) {
	p := launch.NewPanel(launch.PanelConfig{
		Title: "Test",
	}, 24, 80)

	out := p.RenderToAltScreen([]string{"content"})

	if !strings.HasPrefix(out, "\033[?1049h") {
		t.Error("expected alt-screen enter at start")
	}
	if !strings.Contains(out, "content") {
		t.Error("expected content in output")
	}
}

func TestPanel_Render_CRLFLineEndings(t *testing.T) {
	p := launch.NewPanel(launch.PanelConfig{Title: "Test"}, 24, 80)
	out := p.Render([]string{"line1", "line2"})

	// Every line should use \r\n for raw terminal mode.
	lines := strings.Split(out, "\r\n")
	if len(lines) < 4 {
		t.Errorf("expected at least 4 CRLF-delimited lines (header, sep, content, footer), got %d", len(lines))
	}
	// Should NOT contain bare \n (without preceding \r).
	cleaned := strings.ReplaceAll(out, "\r\n", "")
	if strings.Contains(cleaned, "\n") {
		t.Error("found bare \\n without \\r — will misalign in raw terminal mode")
	}
}

func TestVisibleWidth_PlainText(t *testing.T) {
	// "hello" is 5 visible chars.
	if launch.VisibleWidthForTest("hello") != 5 {
		t.Errorf("expected 5, got %d", launch.VisibleWidthForTest("hello"))
	}
}

func TestVisibleWidth_ANSIStripped(t *testing.T) {
	// Bold "hi" is 2 visible chars.
	if launch.VisibleWidthForTest("\033[1mhi\033[0m") != 2 {
		t.Errorf("expected 2, got %d", launch.VisibleWidthForTest("\033[1mhi\033[0m"))
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
