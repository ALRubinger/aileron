package launch

import "testing"

// resolveHostLogin contract (#1268): flag > env > default(on). Only an
// explicit "off" (on the flag, or via env when the flag is auto/empty)
// disables host-side credential acquisition. "auto"/"on"/empty/unknown
// all resolve to enabled.
func TestResolveHostLogin(t *testing.T) {
	cases := []struct {
		name string
		flag string
		env  string
		want bool
	}{
		{"default empty enables", "", "", true},
		{"auto enables", "auto", "", true},
		{"on enables", "on", "", true},
		{"off disables", "off", "", false},
		{"flag off beats env on", "off", "on", false},
		{"flag on beats env off", "on", "off", true},
		{"env off applies when flag auto", "auto", "off", false},
		{"env on applies when flag empty", "", "on", true},
		{"unknown flag falls through to env off", "bogus", "off", false},
		{"unknown flag and unknown env default enabled", "bogus", "nonsense", true},
		{"true alias enables", "true", "", true},
		{"false alias disables", "false", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveHostLogin(tc.flag, tc.env); got != tc.want {
				t.Errorf("resolveHostLogin(%q, %q) = %v, want %v", tc.flag, tc.env, got, tc.want)
			}
		})
	}
}
