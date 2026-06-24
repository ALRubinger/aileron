// Contract tests for the systestlib assert helpers, ported one-to-one from the
// POSIX-shell assert_test.sh. Each case states the expected behavior from the
// helper's contract (nil error on the satisfied condition, a non-nil error
// carrying a diagnostic on violation) and exercises both the happy path and the
// documented failure path. The shell test inspected return codes and stderr;
// here we inspect the returned error and its message.
package systestlib_test

import (
	"strings"
	"testing"

	systestlib "github.com/ALRubinger/aileron/test/system/lib"
)

func TestAssertEq(t *testing.T) {
	tests := []struct {
		name             string
		expected, actual string
		wantErr          bool
		// wantInMsg are substrings the error message must contain on the
		// failure path (mirroring assert_eq's stderr diagnostic).
		wantInMsg []string
	}{
		{name: "equal returns nil", expected: "a", actual: "a", wantErr: false},
		{name: "unequal returns error", expected: "a", actual: "b", wantErr: true, wantInMsg: []string{"eq-fail", "a", "b"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := systestlib.AssertEq(tc.expected, tc.actual, "eq-fail")
			assertErr(t, err, tc.wantErr, tc.wantInMsg)
		})
	}
}

func TestAssertContains(t *testing.T) {
	// The multi-line haystack is the load-bearing case from the brief: env
	// dumps / mount lists contain newlines and assert_contains must still match.
	multiLine := "a\nbind:/home/agent/.codex\nc"
	tests := []struct {
		name             string
		haystack, needle string
		wantErr          bool
		wantInMsg        []string
	}{
		{name: "substring present returns nil", haystack: "hello world", needle: "lo wo", wantErr: false},
		{name: "substring absent returns error", haystack: "hello world", needle: "xyz", wantErr: true, wantInMsg: []string{"xyz", "hello world"}},
		{name: "multi-line haystack matches", haystack: multiLine, needle: "bind:/home/agent/.codex", wantErr: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := systestlib.AssertContains(tc.haystack, tc.needle, "contains-what")
			assertErr(t, err, tc.wantErr, tc.wantInMsg)
		})
	}
}

func TestAssertNotEmpty(t *testing.T) {
	tests := []struct {
		name      string
		value     string
		wantErr   bool
		wantInMsg []string
	}{
		{name: "non-empty returns nil", value: "x", wantErr: false},
		{name: "empty returns error", value: "", wantErr: true, wantInMsg: []string{"notempty-fail", "value was empty"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := systestlib.AssertNotEmpty(tc.value, "notempty-fail")
			assertErr(t, err, tc.wantErr, tc.wantInMsg)
		})
	}
}

func TestFail(t *testing.T) {
	err := systestlib.Fail("boom")
	if err == nil {
		t.Fatal("Fail returned nil; want non-nil error")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("Fail error %q does not contain %q", err.Error(), "boom")
	}
}

// assertErr checks an Assert* return against its contract: nil when wantErr is
// false, otherwise a non-nil error whose message contains every wantInMsg
// substring.
func assertErr(t *testing.T, err error, wantErr bool, wantInMsg []string) {
	t.Helper()
	if wantErr {
		if err == nil {
			t.Fatal("got nil error; want non-nil")
		}
		for _, sub := range wantInMsg {
			if !strings.Contains(err.Error(), sub) {
				t.Errorf("error %q does not contain %q", err.Error(), sub)
			}
		}
		return
	}
	if err != nil {
		t.Fatalf("got error %q; want nil", err.Error())
	}
}
