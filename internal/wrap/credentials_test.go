package wrap

import (
	"strings"
	"testing"
)

func TestDetectCredentialEnvKeys_FindsCommonShapes(t *testing.T) {
	help := `linear - manage Linear issues

Environment:
  LINEAR_API_TOKEN     Personal access token (required)
  LINEAR_TEAM_ID       Default team id

Commands:
  list   List issues
  create Create an issue
`
	got := DetectCredentialEnvKeys(help)
	wantContains(t, got, "LINEAR_API_TOKEN")
	wantNotContains(t, got, "LINEAR_TEAM_ID")
}

func TestDetectCredentialEnvKeys_PrintingPressTriad(t *testing.T) {
	// The three validation targets named in the #750 acceptance.
	// Each CLI's --help in PrintingPress's catalog documents the
	// credential env var in slightly different prose; the heuristic
	// must catch all three.
	cases := []struct {
		name string
		help string
		want string
	}{
		{
			name: "linear",
			help: "Set LINEAR_API_TOKEN in your environment before running.",
			want: "LINEAR_API_TOKEN",
		},
		{
			name: "sentry",
			help: "Authentication is via SENTRY_AUTH_TOKEN (https://sentry.io/...)",
			want: "SENTRY_AUTH_TOKEN",
		},
		{
			name: "coingecko",
			help: "Pass your CoinGecko Pro API key via the COINGECKO_API_KEY env var.",
			want: "COINGECKO_API_KEY",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := DetectCredentialEnvKeys(c.help)
			wantContains(t, got, c.want)
		})
	}
}

func TestDetectCredentialEnvKeys_AllSuffixVariants(t *testing.T) {
	help := `
Usage variables:
  ACME_TOKEN          API token
  ACME_API_TOKEN      Equivalent alias
  ACME_AUTH_TOKEN     OAuth bearer
  ACME_ACCESS_TOKEN   GitHub PAT-style
  ACME_API_KEY        Key auth
  ACME_KEY            Bare key auth
  ACME_SECRET         Shared secret
  ACME_CLIENT_SECRET  OAuth client secret
  ACME_PASSWORD       Basic auth
  ACME_PASS           Same thing
  ACME_PAT            Personal access token
  ACME_AUTH           Generic auth
  ACME_HOME           Not a credential
  ACME_BASE_URL       Not a credential
`
	got := DetectCredentialEnvKeys(help)
	for _, want := range []string{
		"ACME_TOKEN", "ACME_API_TOKEN", "ACME_AUTH_TOKEN",
		"ACME_ACCESS_TOKEN", "ACME_API_KEY", "ACME_KEY",
		"ACME_SECRET", "ACME_CLIENT_SECRET", "ACME_PASSWORD",
		"ACME_PASS", "ACME_PAT", "ACME_AUTH",
	} {
		wantContains(t, got, want)
	}
	wantNotContains(t, got, "ACME_HOME")
	wantNotContains(t, got, "ACME_BASE_URL")
}

func TestDetectCredentialEnvKeys_BareNames(t *testing.T) {
	help := `Usage:
  Set TOKEN to your access token.
  API_KEY is required for write operations.
  SECRET enables signing.
`
	got := DetectCredentialEnvKeys(help)
	for _, want := range []string{"TOKEN", "API_KEY", "SECRET"} {
		wantContains(t, got, want)
	}
}

func TestDetectCredentialEnvKeys_Deduplicates(t *testing.T) {
	// A key mentioned twice (e.g. once in Environment block and
	// once in inline prose) appears once in the output.
	help := "LINEAR_API_TOKEN is required. Set LINEAR_API_TOKEN."
	got := DetectCredentialEnvKeys(help)
	count := 0
	for _, g := range got {
		if g == "LINEAR_API_TOKEN" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("LINEAR_API_TOKEN appeared %d times, want 1; got %v", count, got)
	}
}

func TestDetectCredentialEnvKeys_Sorted(t *testing.T) {
	help := "ZULU_TOKEN ALPHA_TOKEN MIKE_TOKEN"
	got := DetectCredentialEnvKeys(help)
	want := []string{"ALPHA_TOKEN", "MIKE_TOKEN", "ZULU_TOKEN"}
	for i, w := range want {
		if i >= len(got) || got[i] != w {
			t.Errorf("got %v, want %v", got, want)
			break
		}
	}
}

func TestDetectCredentialEnvKeys_EmptyInput(t *testing.T) {
	if got := DetectCredentialEnvKeys(""); got != nil {
		t.Errorf("expected nil for empty input, got %v", got)
	}
}

func TestDetectCredentialEnvKeys_NoCandidatesReturnsNil(t *testing.T) {
	help := "Usage: foo [command]\n  list   List things\n"
	if got := DetectCredentialEnvKeys(help); got != nil {
		t.Errorf("expected nil when no credentials present, got %v", got)
	}
}

func TestLooksLikeCredentialEnvName_RejectsShortTokens(t *testing.T) {
	// Tokens shorter than 3 chars never make it through
	// envTokenRe; verify the predicate is also conservative.
	for _, bad := range []string{"KEY", "AUTH", "AP"} {
		if looksLikeCredentialEnvName(bad) {
			// "KEY" / "AUTH" — must NOT match as bare names; only
			// the listed bare names (TOKEN, API_KEY, API_TOKEN,
			// SECRET, PASSWORD) get through.
			if _, ok := credentialBareNames[bad]; !ok {
				t.Errorf("looksLikeCredentialEnvName(%q) accepted; not in bare list", bad)
			}
		}
	}
}

// wantContains asserts the slice contains all expected values.
func wantContains(t *testing.T, got []string, want string) {
	t.Helper()
	for _, g := range got {
		if g == want {
			return
		}
	}
	t.Errorf("missing %q in %v", want, got)
}

// wantNotContains asserts the slice does NOT contain the value.
func wantNotContains(t *testing.T, got []string, unwanted string) {
	t.Helper()
	for _, g := range got {
		if g == unwanted {
			t.Errorf("unexpectedly found %q in %v", unwanted, got)
			return
		}
	}
	_ = strings.Join // keep strings imported in case of future helper growth
}
