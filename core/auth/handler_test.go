package auth

import "testing"

func TestIsPersonalEmail(t *testing.T) {
	tests := []struct {
		email string
		want  bool
	}{
		{"alice@gmail.com", true},
		{"alice@GMAIL.COM", true},
		{"alice@googlemail.com", true},
		{"alice@yahoo.com", true},
		{"alice@hotmail.com", true},
		{"alice@outlook.com", true},
		{"alice@proton.me", true},
		{"alice@icloud.com", true},
		{"alice@acme.com", false},
		{"alice@company.io", false},
		{"alice@startup.dev", false},
		{"invalid", false},
	}
	for _, tt := range tests {
		t.Run(tt.email, func(t *testing.T) {
			if got := isPersonalEmail(tt.email); got != tt.want {
				t.Errorf("isPersonalEmail(%q) = %v, want %v", tt.email, got, tt.want)
			}
		})
	}
}
