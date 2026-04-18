package draft

import (
	"strings"
	"testing"
	"time"
)

// wednesday 2026-04-15 at noon — a known midweek anchor.
var testNow = time.Date(2026, 4, 15, 12, 0, 0, 0, time.UTC)

func TestDetectTimeRange_ThisWeek(t *testing.T) {
	for _, msg := range []string{
		"What happened this week?",
		"Give me a summary of the week",
		"this past week has been wild",
	} {
		tr := DetectTimeRange(msg, testNow)
		if tr == nil {
			t.Fatalf("expected time range for %q", msg)
		}
		// Wednesday Apr 15 → this week starts Monday Apr 13.
		if tr.Start.Weekday() != time.Monday {
			t.Errorf("%q: start = %s, want Monday", msg, tr.Start.Format("Monday 2006-01-02"))
		}
		if !tr.End.Equal(time.Date(2026, 4, 15, 0, 0, 0, 0, time.UTC)) {
			t.Errorf("%q: end = %s, want 2026-04-15", msg, tr.End.Format("2006-01-02"))
		}
	}
}

func TestDetectTimeRange_ThisWeek_Weekend(t *testing.T) {
	// Saturday Apr 18 — "this week" should end on Friday.
	saturday := time.Date(2026, 4, 18, 12, 0, 0, 0, time.UTC)
	tr := DetectTimeRange("what happened this week", saturday)
	if tr == nil {
		t.Fatal("expected time range")
	}
	if tr.Start.Weekday() != time.Monday {
		t.Errorf("start = %s, want Monday", tr.Start.Format("Monday"))
	}
	if tr.End.Weekday() != time.Friday {
		t.Errorf("end = %s, want Friday", tr.End.Format("Monday"))
	}
}

func TestDetectTimeRange_LastWeek(t *testing.T) {
	tr := DetectTimeRange("summary of last week", testNow)
	if tr == nil {
		t.Fatal("expected time range")
	}
	// testNow is Wed Apr 15. Last week: Mon Apr 6 – Fri Apr 10.
	if tr.Start.Weekday() != time.Monday {
		t.Errorf("start = %s, want Monday", tr.Start.Format("Monday 2006-01-02"))
	}
	if tr.End.Weekday() != time.Friday {
		t.Errorf("end = %s, want Friday", tr.End.Format("Monday 2006-01-02"))
	}
	if tr.Start.Day() != 6 {
		t.Errorf("start = Apr %d, want Apr 6", tr.Start.Day())
	}
	if tr.End.Day() != 10 {
		t.Errorf("end = Apr %d, want Apr 10", tr.End.Day())
	}
}

func TestDetectTimeRange_SinceDay(t *testing.T) {
	tr := DetectTimeRange("what's changed since monday?", testNow)
	if tr == nil {
		t.Fatal("expected time range")
	}
	if tr.Start.Weekday() != time.Monday {
		t.Errorf("start = %s, want Monday", tr.Start.Format("Monday"))
	}
	if !tr.End.Equal(time.Date(2026, 4, 15, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("end = %s, want today", tr.End.Format("2006-01-02"))
	}
}

func TestDetectTimeRange_NoRange(t *testing.T) {
	for _, msg := range []string{
		"What's the status of the auth refactor?",
		"Can you help with this PR?",
		"Hey, quick question about the deploy",
	} {
		tr := DetectTimeRange(msg, testNow)
		if tr != nil {
			t.Errorf("expected no time range for %q, got %+v", msg, tr)
		}
	}
}

func TestTimeRange_Days(t *testing.T) {
	tr := TimeRange{
		Start: time.Date(2026, 4, 13, 0, 0, 0, 0, time.UTC), // Monday
		End:   time.Date(2026, 4, 15, 0, 0, 0, 0, time.UTC), // Wednesday
	}
	days := tr.Days()
	if len(days) != 3 {
		t.Fatalf("expected 3 days, got %d", len(days))
	}
	if days[0].Day() != 13 || days[1].Day() != 14 || days[2].Day() != 15 {
		t.Errorf("days = %v", days)
	}
}

func TestFindUncoveredDays_AllCovered(t *testing.T) {
	tr := TimeRange{
		Start: time.Date(2026, 4, 13, 0, 0, 0, 0, time.UTC),
		End:   time.Date(2026, 4, 15, 0, 0, 0, 0, time.UTC),
	}
	gathered := "On Monday 2026-04-13 we merged PR #1. On Tuesday apr 14 there was a deploy. Wednesday morning standup covered the release."
	uncovered := FindUncoveredDays(gathered, tr)
	if len(uncovered) != 0 {
		t.Errorf("expected 0 uncovered, got %d: %v", len(uncovered), uncovered)
	}
}

func TestFindUncoveredDays_GapDetected(t *testing.T) {
	tr := TimeRange{
		Start: time.Date(2026, 4, 13, 0, 0, 0, 0, time.UTC), // Mon
		End:   time.Date(2026, 4, 17, 0, 0, 0, 0, time.UTC), // Fri
	}
	// Only mentions Monday and Friday — Tue, Wed, Thu are gaps.
	gathered := "On Monday we merged PR #1. On Friday we deployed."
	uncovered := FindUncoveredDays(gathered, tr)
	if len(uncovered) != 3 {
		t.Fatalf("expected 3 uncovered days, got %d", len(uncovered))
	}
	// Tue, Wed, Thu
	if uncovered[0].Day() != 14 || uncovered[1].Day() != 15 || uncovered[2].Day() != 16 {
		t.Errorf("uncovered = %v", uncovered)
	}
}

func TestFindUncoveredDays_DateFormats(t *testing.T) {
	day := time.Date(2026, 4, 14, 0, 0, 0, 0, time.UTC)
	tr := TimeRange{Start: day, End: day}

	tests := []struct {
		name    string
		text    string
		covered bool
	}{
		{"ISO date", "activity on 2026-04-14", true},
		{"day name", "Tuesday was busy", true},
		{"short day", "met on tue", true},
		{"month day", "apr 14 deploy", true},
		{"full month day", "april 14 release", true},
		{"slash date", "deployed 4/14", true},
		{"zero-padded slash", "04/14 standup", true},
		{"no mention", "nothing relevant here", false},
	}
	for _, tt := range tests {
		uncovered := FindUncoveredDays(tt.text, tr)
		if tt.covered && len(uncovered) != 0 {
			t.Errorf("%s: expected covered but got uncovered", tt.name)
		}
		if !tt.covered && len(uncovered) != 1 {
			t.Errorf("%s: expected uncovered but got covered", tt.name)
		}
	}
}

func TestFormatGapPrompt(t *testing.T) {
	uncovered := []time.Time{
		time.Date(2026, 4, 14, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 4, 15, 0, 0, 0, 0, time.UTC),
	}
	prompt := FormatGapPrompt(uncovered, "What happened this week?")

	if !strings.Contains(prompt, "Tuesday 2026-04-14") {
		t.Error("expected Tuesday date in prompt")
	}
	if !strings.Contains(prompt, "Wednesday 2026-04-15") {
		t.Error("expected Wednesday date in prompt")
	}
	if !strings.Contains(prompt, "What happened this week?") {
		t.Error("expected original message in prompt")
	}
	if !strings.Contains(prompt, "NO activity") {
		t.Error("expected gap framing in prompt")
	}
}
