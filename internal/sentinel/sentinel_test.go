package sentinel_test

import (
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/sentinel"
)

// The sentinel contract (#1196):
//
//   - The GitHub sentinel mimics gh's classic PAT shape so gh's own
//     local validation accepts it and does not short-circuit: the
//     `ghp_` prefix, a 40-char total length, and an alphanumeric body.
//   - It is non-secret and self-identifying: it carries a human-readable
//     marker so it is recognizable as the deliberate Aileron placeholder.
//   - IsGitHubTokenSentinel matches only the exact reserved value and
//     rejects every foreign token (real-looking ghp_…, empty, padded).

func TestGitHubTokenSentinel_FormatMimicsGitHubPAT(t *testing.T) {
	s := sentinel.GitHubTokenSentinel

	if !strings.HasPrefix(s, "ghp_") {
		t.Errorf("sentinel %q lacks the ghp_ prefix gh validates against", s)
	}
	// gh's classic PAT is 40 chars (ghp_ + 36-char body). The sentinel
	// must hit that exact length so gh's local format check accepts it.
	if len(s) != 40 {
		t.Errorf("sentinel length = %d, want 40 (ghp_ + 36-char body)", len(s))
	}
	body := strings.TrimPrefix(s, "ghp_")
	if len(body) != 36 {
		t.Errorf("sentinel body length = %d, want 36", len(body))
	}
	for i, r := range body {
		isAllowed := (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if !isAllowed {
			t.Errorf("sentinel body has non-alphanumeric rune %q at %d; gh accepts only [A-Za-z0-9]", r, i)
		}
	}
}

func TestGitHubTokenSentinel_IsSelfIdentifyingAndNonSecret(t *testing.T) {
	// The sentinel must announce itself so anyone who sees it in a log or
	// a process listing knows it is the Aileron placeholder, not a leaked
	// secret. The marker is the non-secret signal.
	if !strings.Contains(sentinel.GitHubTokenSentinel, "AILERONSENTINEL") {
		t.Errorf("sentinel %q lacks the self-identifying AILERONSENTINEL marker", sentinel.GitHubTokenSentinel)
	}
}

func TestIsGitHubTokenSentinel_MatchesExactReservedValue(t *testing.T) {
	if !sentinel.IsGitHubTokenSentinel(sentinel.GitHubTokenSentinel) {
		t.Error("IsGitHubTokenSentinel returned false for the exact reserved value")
	}
}

func TestIsGitHubTokenSentinel_RejectsForeignTokens(t *testing.T) {
	foreign := []struct {
		name  string
		token string
	}{
		{"real-looking ghp_ token", "ghp_1234567890abcdefghijklmnopqrstuvwx"},
		{"empty", ""},
		{"whitespace only", "   "},
		{"leading whitespace padding", " " + sentinel.GitHubTokenSentinel},
		{"trailing whitespace padding", sentinel.GitHubTokenSentinel + " "},
		{"newline padding", sentinel.GitHubTokenSentinel + "\n"},
		{"truncated sentinel", strings.TrimSuffix(sentinel.GitHubTokenSentinel, "A")},
		{"different prefix", strings.Replace(sentinel.GitHubTokenSentinel, "ghp_", "ghs_", 1)},
		{"lowercased", strings.ToLower(sentinel.GitHubTokenSentinel)},
	}
	for _, tc := range foreign {
		if sentinel.IsGitHubTokenSentinel(tc.token) {
			t.Errorf("%s: IsGitHubTokenSentinel(%q) = true, want false (foreign tokens must not be recognized)", tc.name, tc.token)
		}
	}
}
