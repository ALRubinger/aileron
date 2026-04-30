package cstore

import (
	"errors"
	"testing"
)

// The expected URLs in this file are taken directly from ADR-0004's
// "Resolution of a github FQN with a subpath" example and §"github:// and
// gitlab://" rules. They are the documented contract.

func TestDefaultResolver_GitHubSingleArtifactTagIsVPrefixedSemver(t *testing.T) {
	r := DefaultResolver()
	ref := mustRef(t, "github://aileron/slack@1.2.0")
	url, err := r.ResolveTarball(ref)
	if err != nil {
		t.Fatalf("ResolveTarball: %v", err)
	}
	want := "https://github.com/aileron/slack/releases/download/v1.2.0/aileron.tar.gz"
	if url != want {
		t.Errorf("ResolveTarball =\n%q\nwant\n%q", url, want)
	}
}

func TestDefaultResolver_GitHubMonorepoTagIsSubpathSlashV(t *testing.T) {
	// ADR-0004 example: github://aileron/integrations/connectors/slack@1.2.0
	// Tag is connectors/slack/v1.2.0, URL-escaped to connectors%2Fslack%2Fv1.2.0.
	r := DefaultResolver()
	ref := mustRef(t, "github://aileron/integrations/connectors/slack@1.2.0")
	url, err := r.ResolveTarball(ref)
	if err != nil {
		t.Fatalf("ResolveTarball: %v", err)
	}
	want := "https://github.com/aileron/integrations/releases/download/connectors%2Fslack%2Fv1.2.0/aileron.tar.gz"
	if url != want {
		t.Errorf("ResolveTarball =\n%q\nwant\n%q", url, want)
	}
}

func TestDefaultResolver_GitLabSingleArtifact(t *testing.T) {
	r := DefaultResolver()
	ref := mustRef(t, "gitlab://team/linear@2.0.0")
	url, err := r.ResolveTarball(ref)
	if err != nil {
		t.Fatalf("ResolveTarball: %v", err)
	}
	want := "https://gitlab.com/team/linear/-/releases/v2.0.0/downloads/aileron.tar.gz"
	if url != want {
		t.Errorf("ResolveTarball =\n%q\nwant\n%q", url, want)
	}
}

func TestDefaultResolver_HubReturnsHTTPAPIPath(t *testing.T) {
	// ADR-0004: GET https://hub.aileron.dev/api/v1/<owner>/<...path>/<artifact>/<version>/aileron.tar.gz
	r := DefaultResolver()
	ref := mustRef(t, "hub://aileron/integrations/slack@3.0.1")
	url, err := r.ResolveTarball(ref)
	if err != nil {
		t.Fatalf("ResolveTarball: %v", err)
	}
	want := "https://hub.aileron.dev/api/v1/aileron/integrations/slack/3.0.1/aileron.tar.gz"
	if url != want {
		t.Errorf("ResolveTarball =\n%q\nwant\n%q", url, want)
	}
	manifestURL, err := r.ResolveManifest(ref)
	if err != nil {
		t.Fatalf("ResolveManifest: %v", err)
	}
	if manifestURL != "https://hub.aileron.dev/api/v1/aileron/integrations/slack/3.0.1/manifest" {
		t.Errorf("hub manifest URL = %q", manifestURL)
	}
}

func TestDefaultResolver_GitHostManifestURLEqualsTarballURL(t *testing.T) {
	// Per ADR-0004, git-host schemes ship the manifest *inside* the
	// tarball, so the manifest URL is the same as the tarball URL.
	r := DefaultResolver()
	ref := mustRef(t, "github://aileron/slack@1.2.0")
	tar, _ := r.ResolveTarball(ref)
	man, _ := r.ResolveManifest(ref)
	if tar != man {
		t.Errorf("manifest URL %q != tarball URL %q for git-host scheme", man, tar)
	}
}

func TestDefaultResolver_UnknownSchemeIsHardFail(t *testing.T) {
	// Strictly speaking the FQN parser rejects unknown schemes before
	// we get this far, but the resolver layer must also fail closed in
	// case a custom Resolver is built without registering a scheme.
	r := MuxResolver{} // no schemes registered
	_, err := r.ResolveTarball(Ref{FQN: FQN{Scheme: "ftp", Owner: "x", Repo: "y", Raw: "ftp://x/y"}, Version: "1.0.0"})
	if err == nil {
		t.Fatal("ResolveTarball accepted unknown scheme; want error")
	}
	var aerr *Error
	if !errors.As(err, &aerr) || aerr.Class != ClassUnknownScheme {
		t.Errorf("err class = %v, want ClassUnknownScheme", aerr)
	}
}

// mustRef parses a ref string or fails the test. Saves boilerplate.
func mustRef(t *testing.T, s string) Ref {
	t.Helper()
	r, err := ParseRef(s)
	if err != nil {
		t.Fatalf("ParseRef(%q): %v", s, err)
	}
	return r
}
