package agents

import (
	"encoding/json"
	"testing"
)

// codexInterval contract: the usercode endpoint's interval field must
// parse from the string-encoded number the live Codex endpoint actually
// sends, while still accepting a bare JSON number, null, and absence.
// This is the regression for #1489, where modeling interval as a plain
// int failed the device-code parse on the real wire format and silently
// killed host credential capture.
func TestCodexInterval_UnmarshalJSON(t *testing.T) {
	cases := []struct {
		name    string
		json    string
		want    codexInterval
		wantErr bool
	}{
		{"string-encoded number (live wire format)", `"5"`, 5, false},
		{"bare JSON number", `7`, 7, false},
		{"string zero", `"0"`, 0, false},
		{"null is treated as absence", `null`, 0, false},
		{"empty string is treated as absence", `""`, 0, false},
		{"non-numeric string is a parse error", `"abc"`, 0, true},
		{"non-numeric bare token is a parse error", `true`, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var ci codexInterval
			err := json.Unmarshal([]byte(tc.json), &ci)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q, got nil (value %d)", tc.json, ci)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for %q: %v", tc.json, err)
			}
			if ci != tc.want {
				t.Errorf("interval = %d, want %d", ci, tc.want)
			}
		})
	}
}

// The full usercode response must decode when interval arrives as the
// string-encoded form, populating the device_auth_id/user_code fields the
// poll loop depends on (the exact failure path reported in #1489).
func TestCodexDeviceUserCodeResponse_StringInterval(t *testing.T) {
	const body = `{"device_auth_id":"dev-1","user_code":"WXYZ-1234","interval":"5"}`
	var dc codexDeviceUserCodeResponse
	if err := json.Unmarshal([]byte(body), &dc); err != nil {
		t.Fatalf("parse device-code response with string interval: %v", err)
	}
	if dc.DeviceAuthID != "dev-1" || dc.UserCode != "WXYZ-1234" {
		t.Errorf("got device_auth_id=%q user_code=%q, want dev-1 / WXYZ-1234",
			dc.DeviceAuthID, dc.UserCode)
	}
	if dc.Interval != 5 {
		t.Errorf("interval = %d, want 5", dc.Interval)
	}
}
