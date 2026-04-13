package launch_test

import (
	"log/slog"
	"testing"

	"github.com/ALRubinger/aileron/core/launch"
)

func TestParseLogLevel(t *testing.T) {
	tests := []struct {
		input string
		want  slog.Level
	}{
		{"trace", launch.LevelTrace},
		{"TRACE", launch.LevelTrace},
		{"debug", slog.LevelDebug},
		{"DEBUG", slog.LevelDebug},
		{"info", slog.LevelInfo},
		{"Info", slog.LevelInfo},
		{"warn", slog.LevelWarn},
		{"WARN", slog.LevelWarn},
		{"error", slog.LevelError},
		{"Error", slog.LevelError},
		{"", slog.LevelWarn},
		{"bogus", slog.LevelWarn},
	}
	for _, tt := range tests {
		got := launch.ParseLogLevel(tt.input)
		if got != tt.want {
			t.Errorf("ParseLogLevel(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestLevelTrace_BelowDebug(t *testing.T) {
	if launch.LevelTrace >= slog.LevelDebug {
		t.Errorf("LevelTrace (%d) should be below LevelDebug (%d)", launch.LevelTrace, slog.LevelDebug)
	}
}
