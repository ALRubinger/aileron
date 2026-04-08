package launch_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/core/launch"
)

func TestStatusBar_Render(t *testing.T) {
	bar := launch.NewStatusBar(24, 80, "Flying ✈️ withaileron.ai")
	var buf bytes.Buffer
	bar.Render(&buf)
	out := buf.String()

	// Should contain cursor save/restore
	if !strings.Contains(out, "\0337") {
		t.Error("expected cursor save escape")
	}
	if !strings.Contains(out, "\0338") {
		t.Error("expected cursor restore escape")
	}
	// Should contain the text
	if !strings.Contains(out, "withaileron.ai") {
		t.Error("expected status bar text in output")
	}
	// Should contain separator character
	if !strings.Contains(out, "─") {
		t.Error("expected separator line")
	}
	// Should position at row 23 (rows-1) for separator
	if !strings.Contains(out, "\033[23;1H") {
		t.Errorf("expected cursor move to row 23, got %q", out)
	}
	// Should position at row 24 (rows) for text
	if !strings.Contains(out, "\033[24;1H") {
		t.Errorf("expected cursor move to row 24, got %q", out)
	}
}

func TestStatusBar_Resize(t *testing.T) {
	bar := launch.NewStatusBar(24, 80, "test")
	var buf bytes.Buffer

	bar.Resize(&buf, 30, 120)
	out := buf.String()

	// After resize to 30 rows, separator should be at row 29
	if !strings.Contains(out, "\033[29;1H") {
		t.Errorf("expected cursor move to row 29 after resize, got %q", out)
	}
}

func TestStatusBar_TooSmallTerminal(t *testing.T) {
	bar := launch.NewStatusBar(2, 80, "test")
	var buf bytes.Buffer
	bar.Render(&buf)
	// With only 2 rows, bar should not render (needs at least 3)
	if buf.Len() != 0 {
		t.Errorf("expected no output for 2-row terminal, got %q", buf.String())
	}
}

func TestStatusBar_BarHeight(t *testing.T) {
	bar := launch.NewStatusBar(24, 80, "test")
	if bar.BarHeight() != 2 {
		t.Errorf("expected bar height 2, got %d", bar.BarHeight())
	}
}

func TestSetScrollRegion(t *testing.T) {
	var buf bytes.Buffer
	launch.SetScrollRegion(&buf, 1, 22)
	if buf.String() != "\033[1;22r" {
		t.Errorf("expected scroll region escape, got %q", buf.String())
	}
}

func TestResetScrollRegion(t *testing.T) {
	var buf bytes.Buffer
	launch.ResetScrollRegion(&buf)
	if buf.String() != "\033[r" {
		t.Errorf("expected reset scroll region escape, got %q", buf.String())
	}
}
