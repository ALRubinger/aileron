package draft

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// TimeRange represents a date window extracted from a message.
type TimeRange struct {
	Start time.Time
	End   time.Time
	Label string // human-readable label, e.g. "this week (Mon Apr 14 – Fri Apr 18)"
}

// Days returns each date in the range as a slice, inclusive of Start and End.
func (tr TimeRange) Days() []time.Time {
	var days []time.Time
	for d := tr.Start; !d.After(tr.End); d = d.AddDate(0, 0, 1) {
		days = append(days, d)
	}
	return days
}

// DetectTimeRange parses a message for time-range signals and returns the
// expected date window relative to the given reference time (usually now).
// Returns nil if no time range is detected.
func DetectTimeRange(message string, now time.Time) *TimeRange {
	lower := strings.ToLower(message)

	// Normalize today to midnight for clean date arithmetic.
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	weekday := today.Weekday()

	// "this week" / "the week" — Monday through today (or Friday if weekend).
	if matchesAny(lower, `this week`, `the week`, `this past week`) {
		monday := today.AddDate(0, 0, -int(weekday-time.Monday))
		if weekday == time.Sunday {
			monday = today.AddDate(0, 0, -6)
		}
		end := today
		if weekday == time.Saturday || weekday == time.Sunday {
			end = lastWeekday(today, time.Friday)
		}
		return &TimeRange{
			Start: monday,
			End:   end,
			Label: fmt.Sprintf("this week (%s – %s)", fmtDate(monday), fmtDate(end)),
		}
	}

	// "last week" — previous Monday through Friday.
	if matchesAny(lower, `last week`, `previous week`, `the past week`) {
		thisMonday := today.AddDate(0, 0, -int(weekday-time.Monday))
		if weekday == time.Sunday {
			thisMonday = today.AddDate(0, 0, -6)
		}
		lastMonday := thisMonday.AddDate(0, 0, -7)
		lastFriday := lastMonday.AddDate(0, 0, 4)
		return &TimeRange{
			Start: lastMonday,
			End:   lastFriday,
			Label: fmt.Sprintf("last week (%s – %s)", fmtDate(lastMonday), fmtDate(lastFriday)),
		}
	}

	// "since <day>" — e.g., "since Monday", "since Tuesday".
	if m := sinceDayPattern.FindStringSubmatch(lower); m != nil {
		targetDay := parseDayName(m[1])
		if targetDay >= 0 {
			start := lastWeekday(today, targetDay)
			return &TimeRange{
				Start: start,
				End:   today,
				Label: fmt.Sprintf("since %s (%s – %s)", m[1], fmtDate(start), fmtDate(today)),
			}
		}
	}

	return nil
}

var sinceDayPattern = regexp.MustCompile(`since\s+(monday|tuesday|wednesday|thursday|friday|saturday|sunday)`)

// FindUncoveredDays checks which days in the range have no mention in the
// gathered context. It looks for YYYY-MM-DD, "Mon Jan 2", day names (Monday,
// Tuesday, etc.), and short dates (Apr 14, April 14).
func FindUncoveredDays(gathered string, tr TimeRange) []time.Time {
	lower := strings.ToLower(gathered)

	var uncovered []time.Time
	for _, day := range tr.Days() {
		if !dayMentioned(lower, day) {
			uncovered = append(uncovered, day)
		}
	}
	return uncovered
}

// dayMentioned checks if a specific date is mentioned in the text using
// multiple common formats.
func dayMentioned(lowerText string, day time.Time) bool {
	patterns := []string{
		day.Format("2006-01-02"),                          // 2026-04-14
		strings.ToLower(day.Format("Monday")),             // monday
		strings.ToLower(day.Format("Mon")),                // mon
		strings.ToLower(day.Format("Jan 2")),              // apr 14
		strings.ToLower(day.Format("January 2")),          // april 14
		strings.ToLower(day.Format("Jan 02")),             // apr 14 (zero-padded)
		strings.ToLower(day.Format("1/2")),                // 4/14
		strings.ToLower(day.Format("01/02")),              // 04/14
	}
	for _, p := range patterns {
		if strings.Contains(lowerText, p) {
			return true
		}
	}
	return false
}

// matchesAny returns true if text contains any of the given substrings.
func matchesAny(text string, subs ...string) bool {
	for _, s := range subs {
		if strings.Contains(text, s) {
			return true
		}
	}
	return false
}

// lastWeekday returns the most recent date with the given weekday,
// on or before ref. If ref is that weekday, it returns ref.
func lastWeekday(ref time.Time, target time.Weekday) time.Time {
	diff := int(ref.Weekday()) - int(target)
	if diff < 0 {
		diff += 7
	}
	return ref.AddDate(0, 0, -diff)
}

func parseDayName(name string) time.Weekday {
	switch strings.ToLower(name) {
	case "monday":
		return time.Monday
	case "tuesday":
		return time.Tuesday
	case "wednesday":
		return time.Wednesday
	case "thursday":
		return time.Thursday
	case "friday":
		return time.Friday
	case "saturday":
		return time.Saturday
	case "sunday":
		return time.Sunday
	}
	return -1
}

func fmtDate(t time.Time) string {
	return t.Format("Mon Jan 2")
}
