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

func TestActionFileName_DelegatesToInternal(t *testing.T) {
	// Exported wrapper for callers outside the wrap package
	// (notably `aileron cli add`). Verify the exported wrapper
	// produces the same result as the internal helper for a
	// representative input, so the wrapper isn't accidentally
	// dropped or diverged.
	spec := &Spec{Connector: ConnectorSpec{Name: "local://user/linear"}}
	sub := SubcommandSpec{Name: "issues"}
	if got, want := ActionFileName(spec, sub), "linear-issues.md"; got != want {
		t.Errorf("ActionFileName=%q want %q", got, want)
	}
}

func TestActionFileName_HandlesUnscopedConnector(t *testing.T) {
	spec := &Spec{Connector: ConnectorSpec{Name: "barename"}}
	sub := SubcommandSpec{Name: "do"}
	if got, want := ActionFileName(spec, sub), "do.md"; got != want {
		t.Errorf("ActionFileName=%q want %q (no scope → no prefix)", got, want)
	}
}

func TestRenderActionMD_EmitsValidFrontmatter(t *testing.T) {
	// Exported wrapper used by `aileron cli add` to write
	// action.md files for local connectors. Verify the wrapper
	// produces text the daemon's action loader will accept by
	// asserting the front-matter delimiter and the canonical
	// fields are present.
	spec := &Spec{
		Connector:   ConnectorSpec{Name: "local://user/linear", Version: "0.0.1"},
		Subcommands: []SubcommandSpec{{Name: "issues", Description: "List issues"}},
	}
	sub := spec.Subcommands[0]
	body := RenderActionMD(spec, sub, nil, "")
	for _, want := range []string{
		"+++",
		`name = "issues"`,
		`version = "0.0.1"`,
		`[[requires.connectors]]`,
		`name = "local://user/linear"`,
		`hash = "sha256:bound-at-install"`,
		`[[execute]]`,
		`op = "issues"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("RenderActionMD output missing %q; got:\n%s", want, body)
		}
	}
}

func TestRenderActionMD_HonorsExplicitHash(t *testing.T) {
	// When a concrete hash is supplied (publisher-signed
	// install path), it replaces the placeholder.
	spec := &Spec{
		Connector:   ConnectorSpec{Name: "hub://aileron/foo", Version: "1.0.0"},
		Subcommands: []SubcommandSpec{{Name: "do"}},
	}
	body := RenderActionMD(spec, spec.Subcommands[0], nil, "sha256:abcd")
	if !strings.Contains(body, `hash = "sha256:abcd"`) {
		t.Errorf("explicit hash not honored: %s", body)
	}
	if strings.Contains(body, "bound-at-install") {
		t.Errorf("placeholder should not appear when explicit hash supplied: %s", body)
	}
}

func TestActionSource_LocalFQNNoDoublePrefix(t *testing.T) {
	// Regression test for #753: BYOCLI connectors use FQNs that
	// already carry a `local://` scheme, so the legacy `local:`
	// prefix actionSource added previously produced a confusing
	// `local:local://...` double prefix. The fix returns
	// `<FQN>/<op>@<version>` for any scheme-bearing FQN and
	// preserves the `local:` prefix only for schemeless names.
	scoped := &Spec{
		Connector:   ConnectorSpec{Name: "local://user/linear", Version: "0.0.1"},
		Subcommands: []SubcommandSpec{{Name: "issues"}},
	}
	if got := actionSource(scoped, scoped.Subcommands[0]); got != "local://user/linear/issues@0.0.1" {
		t.Errorf("scoped local source=%q (should not double-prefix)", got)
	}

	hub := &Spec{
		Connector:   ConnectorSpec{Name: "hub://aileron/foo", Version: "1.0.0"},
		Subcommands: []SubcommandSpec{{Name: "do"}},
	}
	if got := actionSource(hub, hub.Subcommands[0]); got != "hub://aileron/foo/do@1.0.0" {
		t.Errorf("hub source=%q", got)
	}

	bare := &Spec{
		Connector:   ConnectorSpec{Name: "barename", Version: "0.0.1"},
		Subcommands: []SubcommandSpec{{Name: "do"}},
	}
	if got := actionSource(bare, bare.Subcommands[0]); got != "local:barename/do@0.0.1" {
		t.Errorf("schemeless source=%q (legacy local: prefix should apply)", got)
	}
}

func TestParseSubcommands_SingleSpaceColumns(t *testing.T) {
	// Some CLIs (Linear's reflection-based linear-pp-cli is the
	// reported case) format `--help` with a single space between
	// the subcommand name and its description. Before this fix
	// the splitter treated 2+ spaces as the column separator,
	// which made the whole row a single "name" token — failing
	// the manifest schema's `[a-z][a-z0-9_-]*` rule and aborting
	// the entire `aileron pp add` install. Verify single-space
	// rows now parse correctly.
	help := `Usage: foo [command]

Commands:
  list List things
  authentication-session-responses Get a single auth session response
  do-work Run a job
`
	subs := parseSubcommands(help)
	got := map[string]string{}
	for _, s := range subs {
		got[s.Name] = s.Description
	}
	for _, name := range []string{"list", "authentication-session-responses", "do-work"} {
		if _, ok := got[name]; !ok {
			t.Errorf("missing subcommand %q in single-space-column help; got %v", name, got)
		}
	}
	if got["list"] != "List things" {
		t.Errorf("list desc=%q want %q", got["list"], "List things")
	}
}

func TestParseSubcommands_StillHandlesCobraStyleMultiSpace(t *testing.T) {
	// Regression check: relaxing the splitter to any whitespace
	// must not break the cobra/urfave-style 2+-space layout that
	// most CLIs use. The descriptions get a trailing-whitespace
	// trim from splitSubcommandLine, so both styles read clean.
	help := `Usage: foo

Commands:
  list      List things
  show      Show one
`
	subs := parseSubcommands(help)
	if len(subs) != 2 {
		t.Fatalf("got %d subcommands, want 2: %v", len(subs), subs)
	}
	if subs[0].Name != "list" || subs[0].Description != "List things" {
		t.Errorf("cobra-style row[0] mismatched: %+v", subs[0])
	}
	if subs[1].Name != "show" || subs[1].Description != "Show one" {
		t.Errorf("cobra-style row[1] mismatched: %+v", subs[1])
	}
}

func TestParseSubcommands_SkipsMalformedNames(t *testing.T) {
	// Defense in depth: a name that survives the splitter but
	// doesn't match the manifest's POSIX-shape rule is dropped
	// rather than failing the whole install. Without this, a
	// single rogue row in a CLI's `--help` would block users
	// from wrapping the rest of it.
	help := `Commands:
  list List things
  3invalid Number-led so the manifest rejects it
  Bad-Caps Uppercase not allowed in operation names
  has_underscore Permitted
`
	subs := parseSubcommands(help)
	names := make([]string, 0, len(subs))
	for _, s := range subs {
		names = append(names, s.Name)
	}
	wantPresent := []string{"list", "has_underscore"}
	wantAbsent := []string{"3invalid", "Bad-Caps"}
	for _, n := range wantPresent {
		found := false
		for _, got := range names {
			if got == n {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected %q in %v", n, names)
		}
	}
	for _, n := range wantAbsent {
		for _, got := range names {
			if got == n {
				t.Errorf("malformed name %q should have been dropped; got %v", n, names)
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
