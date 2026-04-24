package launch_test

import (
	"testing"
	"time"

	"github.com/ALRubinger/aileron/internal/launch"
	launchpolicy "github.com/ALRubinger/aileron/internal/policy/launch"
)

func TestIsQuietHours_NilConfig(t *testing.T) {
	if launch.IsQuietHours(nil, nil) {
		t.Error("expected false for nil config")
	}
}

func TestIsQuietHours_OvernightWindow(t *testing.T) {
	cfg := &launchpolicy.QuietHoursConfig{
		Start: "22:00",
		End:   "06:00",
	}

	tests := []struct {
		name string
		hour int
		min  int
		want bool
	}{
		{"before start", 21, 0, false},
		{"at start", 22, 0, true},
		{"late night", 23, 30, true},
		{"midnight", 0, 0, true},
		{"early morning", 5, 59, true},
		{"at end", 6, 0, false},
		{"afternoon", 14, 0, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			now := func() time.Time {
				return time.Date(2026, 1, 15, tt.hour, tt.min, 0, 0, time.Local)
			}
			if got := launch.IsQuietHours(cfg, now); got != tt.want {
				t.Errorf("IsQuietHours at %02d:%02d = %v, want %v", tt.hour, tt.min, got, tt.want)
			}
		})
	}
}

func TestIsQuietHours_SameDayWindow(t *testing.T) {
	cfg := &launchpolicy.QuietHoursConfig{
		Start: "09:00",
		End:   "17:00",
	}

	tests := []struct {
		name string
		hour int
		min  int
		want bool
	}{
		{"before start", 8, 30, false},
		{"at start", 9, 0, true},
		{"midday", 12, 0, true},
		{"before end", 16, 59, true},
		{"at end", 17, 0, false},
		{"evening", 20, 0, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			now := func() time.Time {
				return time.Date(2026, 1, 15, tt.hour, tt.min, 0, 0, time.Local)
			}
			if got := launch.IsQuietHours(cfg, now); got != tt.want {
				t.Errorf("IsQuietHours at %02d:%02d = %v, want %v", tt.hour, tt.min, got, tt.want)
			}
		})
	}
}

func TestIsQuietHours_WithTimezone(t *testing.T) {
	cfg := &launchpolicy.QuietHoursConfig{
		Start:    "22:00",
		End:      "06:00",
		Timezone: "America/New_York",
	}

	// 23:00 in New York — should be quiet.
	ny, _ := time.LoadLocation("America/New_York")
	now := func() time.Time {
		return time.Date(2026, 1, 15, 23, 0, 0, 0, ny)
	}
	if !launch.IsQuietHours(cfg, now) {
		t.Error("expected quiet at 23:00 America/New_York")
	}

	// 12:00 in New York — should not be quiet.
	now = func() time.Time {
		return time.Date(2026, 1, 15, 12, 0, 0, 0, ny)
	}
	if launch.IsQuietHours(cfg, now) {
		t.Error("expected not quiet at 12:00 America/New_York")
	}
}

func TestIsQuietHours_InvalidTimezone(t *testing.T) {
	cfg := &launchpolicy.QuietHoursConfig{
		Start:    "22:00",
		End:      "06:00",
		Timezone: "Invalid/Zone",
	}
	now := func() time.Time {
		return time.Date(2026, 1, 15, 23, 0, 0, 0, time.UTC)
	}
	if launch.IsQuietHours(cfg, now) {
		t.Error("expected false for invalid timezone")
	}
}

func TestIsQuietHours_NilNowFunc(t *testing.T) {
	// When nowFunc is nil, IsQuietHours should use time.Now and not panic.
	// We use a wide window (00:00–23:59) to guarantee the result is true
	// regardless of when the test runs.
	cfg := &launchpolicy.QuietHoursConfig{
		Start: "00:00",
		End:   "23:59",
	}
	if !launch.IsQuietHours(cfg, nil) {
		t.Error("expected true for all-day window with nil nowFunc")
	}
}

func TestIsQuietHours_InvalidTimeFormat(t *testing.T) {
	tests := []struct {
		name  string
		start string
		end   string
	}{
		{"bad start", "25:00", "06:00"},
		{"bad end", "22:00", "6:00"},
		{"empty start", "", "06:00"},
		{"not HH:MM", "abcde", "06:00"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &launchpolicy.QuietHoursConfig{
				Start: tt.start,
				End:   tt.end,
			}
			now := func() time.Time {
				return time.Date(2026, 1, 15, 23, 0, 0, 0, time.Local)
			}
			if launch.IsQuietHours(cfg, now) {
				t.Error("expected false for invalid time format")
			}
		})
	}
}
