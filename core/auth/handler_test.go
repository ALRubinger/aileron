package auth

import (
	"net/http/httptest"
	"testing"
)

func TestCallbackURL_Dynamic(t *testing.T) {
	h := &Handler{} // no override set

	tests := []struct {
		name     string
		host     string
		fwdProto string
		fwdHost  string
		provider string
		want     string
	}{
		{
			name:     "http request derives http callback",
			host:     "localhost:8080",
			provider: "google",
			want:     "http://localhost:8080/auth/google/callback",
		},
		{
			name:     "X-Forwarded-Proto https upgrades scheme",
			host:     "localhost:8080",
			fwdProto: "https",
			provider: "google",
			want:     "https://localhost:8080/auth/google/callback",
		},
		{
			name:     "X-Forwarded-Host overrides host",
			host:     "internal:8080",
			fwdProto: "https",
			fwdHost:  "feat-foo-up.railway.app",
			provider: "google",
			want:     "https://feat-foo-up.railway.app/auth/google/callback",
		},
		{
			name:     "provider name included in path",
			host:     "api.example.com",
			fwdProto: "https",
			provider: "okta",
			want:     "https://api.example.com/auth/okta/callback",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/auth/"+tt.provider+"/login", nil)
			r.Host = tt.host
			if tt.fwdProto != "" {
				r.Header.Set("X-Forwarded-Proto", tt.fwdProto)
			}
			if tt.fwdHost != "" {
				r.Header.Set("X-Forwarded-Host", tt.fwdHost)
			}
			got := h.callbackURL(r, tt.provider)
			if got != tt.want {
				t.Errorf("callbackURL = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCallbackURL_Override(t *testing.T) {
	override := "https://api.example.com/auth/google/callback"
	h := &Handler{oauthRedirectURL: override}

	r := httptest.NewRequest("GET", "/auth/google/login", nil)
	r.Host = "some-branch.railway.app"
	r.Header.Set("X-Forwarded-Proto", "https")

	got := h.callbackURL(r, "google")
	if got != override {
		t.Errorf("callbackURL = %q, want override %q", got, override)
	}
}

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
