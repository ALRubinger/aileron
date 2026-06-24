// Contract tests for the daemon-CLI output parsers used by the scenario driver.
package systestlib_test

import (
	"testing"

	systestlib "github.com/ALRubinger/aileron/test/system/lib"
)

func TestParseDaemonStartURL(t *testing.T) {
	cases := []struct {
		name string
		out  string
		want string
	}{
		{"start line", "Aileron daemon running at http://127.0.0.1:60036\n", "http://127.0.0.1:60036"},
		{"trailing whitespace", "Aileron daemon running at http://127.0.0.1:60036   \n", "http://127.0.0.1:60036"},
		{"amid other lines", "starting...\nAileron daemon running at http://127.0.0.1:7777\nready\n", "http://127.0.0.1:7777"},
		{"no url line", "some unrelated output\n", ""},
		{"empty", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := systestlib.ParseDaemonStartURL(c.out); got != c.want {
				t.Errorf("ParseDaemonStartURL(%q) = %q; want %q", c.out, got, c.want)
			}
		})
	}
}

func TestDaemonStatusRunning(t *testing.T) {
	if !systestlib.DaemonStatusRunning("Aileron daemon is running.\n  URL: http://x\n") {
		t.Error("running status not detected as running")
	}
	if systestlib.DaemonStatusRunning("Aileron daemon is not running.\nHint: ...\n") {
		t.Error("not-running status wrongly detected as running")
	}
	if systestlib.DaemonStatusRunning("") {
		t.Error("empty output wrongly detected as running")
	}
}
