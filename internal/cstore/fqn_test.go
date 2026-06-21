package cstore

import (
	"strings"
	"testing"
)

func TestParseFQN_AcceptsADRExamples(t *testing.T) {
	// The ADR-0002 examples table is the contract for what valid FQNs
	// look like. Each row here is taken directly from that table.
	cases := []struct {
		raw     string
		owner   string
		repo    string
		subpath string
	}{
		{"github://aileron/slack", "aileron", "slack", ""},
		{"github://aileron/integrations/connectors/slack", "aileron", "integrations", "connectors/slack"},
		{"github://aileron/integrations/connectors/discord", "aileron", "integrations", "connectors/discord"},
		{"gitlab://team/linear", "team", "linear", ""},
		{"hub://aileron/slack", "aileron", "slack", ""},
		{"hub://aileron/integrations/slack", "aileron", "integrations", "slack"},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			f, err := ParseFQN(tc.raw)
			if err != nil {
				t.Fatalf("ParseFQN(%q) err = %v", tc.raw, err)
			}
			if f.Owner != tc.owner {
				t.Errorf("Owner = %q, want %q", f.Owner, tc.owner)
			}
			if f.Repo != tc.repo {
				t.Errorf("Repo = %q, want %q", f.Repo, tc.repo)
			}
			if f.Subpath != tc.subpath {
				t.Errorf("Subpath = %q, want %q", f.Subpath, tc.subpath)
			}
			if f.Raw != tc.raw {
				t.Errorf("Raw = %q, want %q", f.Raw, tc.raw)
			}
			if got := f.String(); got != tc.raw {
				t.Errorf("String() = %q, want %q", got, tc.raw)
			}
		})
	}
}

func TestParseFQN_RejectsUnknownScheme(t *testing.T) {
	_, err := ParseFQN("ftp://owner/repo")
	if err == nil {
		t.Fatal("ParseFQN accepted ftp:// scheme; want error")
	}
	if !strings.Contains(err.Error(), "scheme") {
		t.Errorf("err = %q; want it to mention scheme", err.Error())
	}
}

func TestParseFQN_RejectsMissingRepoOrOwner(t *testing.T) {
	for _, in := range []string{
		"github://owner",       // missing repo
		"github://",            // missing both
		"",                     // empty
		"github://owner@1.0.0", // version suffix forbidden
	} {
		t.Run(in, func(t *testing.T) {
			if _, err := ParseFQN(in); err == nil {
				t.Errorf("ParseFQN(%q) succeeded; want error", in)
			}
		})
	}
}

func TestFQN_Authority_ReturnsSchemeOwnerRepo(t *testing.T) {
	// ADR-0002: signing keys are associated with the FQN's *authority*
	// (`<scheme>://<owner>/<repo>`). The Verifier uses Authority() to
	// look up keys; subpaths beneath the repo do not affect signing
	// authority.
	f, err := ParseFQN("github://aileron/integrations/connectors/slack")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got, want := f.Authority(), "github://aileron/integrations"; got != want {
		t.Errorf("Authority() = %q, want %q", got, want)
	}
}

func TestFQN_OwnerAuthority_ReturnsSchemeOwner(t *testing.T) {
	// ADR-0013: owner-level (per-publisher) trust keys on
	// `<scheme>://<owner>`, dropping the repo segment Authority()
	// includes. The two accessors differ exactly by the repo segment.
	cases := []struct {
		fqn           string
		wantOwner     string
		wantAuthority string
	}{
		{"github://acme/connector/sub", "github://acme", "github://acme/connector"},
		{"gitlab://acme/connector", "gitlab://acme", "gitlab://acme/connector"},
		{"hub://acme/namespace/nested", "hub://acme", "hub://acme/namespace"},
	}
	for _, tc := range cases {
		t.Run(tc.fqn, func(t *testing.T) {
			f, err := ParseFQN(tc.fqn)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if got := f.OwnerAuthority(); got != tc.wantOwner {
				t.Errorf("OwnerAuthority() = %q, want %q", got, tc.wantOwner)
			}
			if got := f.Authority(); got != tc.wantAuthority {
				t.Errorf("Authority() = %q, want %q", got, tc.wantAuthority)
			}
			// The owner authority is the per-repo authority minus the
			// `/<repo>` segment.
			if want := f.OwnerAuthority() + "/" + f.Repo; want != f.Authority() {
				t.Errorf("OwnerAuthority()+/+Repo = %q, want Authority() = %q", want, f.Authority())
			}
		})
	}
}

func TestParseRef_AcceptsCanonicalCompactForm(t *testing.T) {
	r, err := ParseRef("github://aileron/slack@1.2.0")
	if err != nil {
		t.Fatalf("ParseRef err = %v", err)
	}
	if r.FQN.Raw != "github://aileron/slack" {
		t.Errorf("FQN.Raw = %q", r.FQN.Raw)
	}
	if r.Version != "1.2.0" {
		t.Errorf("Version = %q", r.Version)
	}
	if got := r.String(); got != "github://aileron/slack@1.2.0" {
		t.Errorf("String() = %q", got)
	}
}

func TestParseRef_AcceptsPreReleaseVersion(t *testing.T) {
	r, err := ParseRef("github://acme/stripe@1.1.0-rc.1")
	if err != nil {
		t.Fatalf("ParseRef err = %v", err)
	}
	if r.Version != "1.1.0-rc.1" {
		t.Errorf("Version = %q", r.Version)
	}
}

func TestParseRef_Rejects(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"github://owner/repo", "missing @"},
		{"github://owner/repo@", "empty version"},
		{"github://owner/repo@latest", "strict SemVer"},
		{"github://owner/repo@^1.2.0", "strict SemVer"},
		{"github://owner/repo@1.2", "strict SemVer"},
		{"ftp://owner/repo@1.2.0", "scheme"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			_, err := ParseRef(tc.in)
			if err == nil {
				t.Fatalf("ParseRef(%q) succeeded; want error containing %q", tc.in, tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %q; want substring %q", err.Error(), tc.want)
			}
		})
	}
}
