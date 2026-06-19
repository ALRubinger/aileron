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
//
// Recognition is no longer a function here (#1247): the proxy seam matches
// the inbound carrier against the matched binding's SentinelValue. The
// value-shape assertions below stay so a future change cannot silently
// alter the canonical GitHub sentinel out of gh's accepted format.

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
