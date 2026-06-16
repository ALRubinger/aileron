package discovery

import "testing"

func TestSanitizeToolName(t *testing.T) {
	cases := map[string]string{
		"google":                 "google",
		"Google":                 "google",
		"  Google  ":             "google",
		"aileron connector":      "aileron-connector",
		"a--b":                   "a-b",
		"-leading-and-trailing-": "leading-and-trailing",
		"weird/name:with*chars":  "weird-name-with-chars",
		"keeps_dots.and_unders":  "keeps_dots.and_unders",
		"not a valid fqn":        "not-a-valid-fqn",
	}
	for in, want := range cases {
		if got := sanitizeToolName(in); got != want {
			t.Errorf("sanitizeToolName(%q) = %q, want %q", in, got, want)
		}
	}
}
