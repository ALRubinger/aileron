package model_test

import (
	"testing"

	"github.com/ALRubinger/aileron/internal/model"
)

func TestConnectedAccount_VaultPath(t *testing.T) {
	tests := []struct {
		name     string
		userID   string
		provider model.ConnectedAccountProvider
		want     string
	}{
		{
			name:     "gmail",
			userID:   "usr_abc",
			provider: model.ConnectedAccountProviderGmail,
			want:     "connected-accounts/usr_abc/gmail",
		},
		{
			name:     "google_calendar",
			userID:   "usr_xyz",
			provider: model.ConnectedAccountProviderGoogleCalendar,
			want:     "connected-accounts/usr_xyz/google_calendar",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			acc := model.ConnectedAccount{
				UserID:   tt.userID,
				Provider: tt.provider,
			}
			if got := acc.VaultPath(); got != tt.want {
				t.Errorf("VaultPath() = %q, want %q", got, tt.want)
			}
		})
	}
}
