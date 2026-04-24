package launch

import (
	"log/slog"
	"strings"
)

// LevelTrace is a custom slog level below Debug, reserved for
// high-volume tracing output.
const LevelTrace = slog.Level(-8)

// ParseLogLevel converts a string level name to an slog.Level.
// Accepted values: "trace", "debug", "info", "warn", "error".
// Returns slog.LevelWarn for unrecognized input.
func ParseLogLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "trace":
		return LevelTrace
	case "debug":
		return slog.LevelDebug
	case "info":
		return slog.LevelInfo
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelWarn
	}
}
