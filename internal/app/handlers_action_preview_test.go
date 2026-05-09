package app

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	api "github.com/ALRubinger/aileron/internal/api/gen"
	"github.com/ALRubinger/aileron/internal/cstore"
)

// postPreviewAction issues the preview request and returns the
// recorder. Mirror of postInstallAction in handlers_install_actions_test.go.
func postPreviewAction(srv *apiServer, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/actions/preview", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.PreviewAction(rec, req)
	return rec
}

// TestPreviewAction_HappyPath asserts the canonical preview path: a
// well-formed action tarball with a verified signature returns the
// action metadata, the connector deps with their already-installed
// status, and signature_status=verified. The action file is NOT
// written to disk — the consent prompt fires next.
func TestPreviewAction_HappyPath(t *testing.T) {
	ref, _ := cstore.ParseRef("github://ALRubinger/aileron-connector-github/actions/list-recent-prs@0.1.0")
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	depFQN := "github://ALRubinger/aileron-connector-github"
	depHash := fakeConnectorHash(depFQN, "1.0.0", "api_key")
	md := goodActionMD(depFQN, depHash)
	tarball := buildActionTarball(t, md, priv)

	srv := installActionTestServer(t, ref, tarball, pub, depFQN)
	rec := postPreviewAction(srv, `{
		"fqn": "github://ALRubinger/aileron-connector-github/actions/list-recent-prs",
		"version": "0.1.0"
	}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var got api.ActionPreview
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Name != "list-recent-prs" {
		t.Errorf("Name = %q, want list-recent-prs", got.Name)
	}
	if got.Version != "0.1.0" {
		t.Errorf("Version = %q, want 0.1.0", got.Version)
	}
	if !strings.HasPrefix(got.Hash, "sha256:") {
		t.Errorf("Hash = %q, want sha256: prefix", got.Hash)
	}
	if got.SignatureStatus == nil || *got.SignatureStatus != api.ActionPreviewSignatureStatusVerified {
		t.Errorf("SignatureStatus = %v, want verified", got.SignatureStatus)
	}
	if got.Intent == nil || *got.Intent != "list pull requests" {
		t.Errorf("Intent = %v, want pointer to 'list pull requests'", got.Intent)
	}
	if len(got.ConnectorDeps) != 1 {
		t.Fatalf("ConnectorDeps = %v, want 1 entry", got.ConnectorDeps)
	}
	dep := got.ConnectorDeps[0]
	if dep.Fqn != depFQN || dep.Version != "1.0.0" || dep.Hash != depHash {
		t.Errorf("dep = %+v; want fqn=%s version=1.0.0 hash=%s", dep, depFQN, depHash)
	}
	if !dep.AlreadyInstalled {
		t.Error("dep.AlreadyInstalled = false; the test fixture pre-installed the dep")
	}
	if dep.Capabilities == nil || len(*dep.Capabilities) == 0 || (*dep.Capabilities)[0] != "list_prs" {
		t.Errorf("dep.Capabilities = %v, want [list_prs]", dep.Capabilities)
	}

	// Crucial: action file must not be on disk. The actions store
	// was empty before the preview; if it grew, we committed.
	if list := srv.actions.List(); len(list) != 0 {
		t.Errorf("actions store has %d entries after preview; want 0", len(list))
	}
}

// TestPreviewAction_DepNotInstalledFlag asserts the missing-dep path:
// when the action's connector dep isn't in the cstore, the preview
// reports already_installed=false. The CLI uses this to render
// "will be installed" alongside the dep entry.
func TestPreviewAction_DepNotInstalledFlag(t *testing.T) {
	ref, _ := cstore.ParseRef("github://ALRubinger/aileron-connector-github/actions/list-recent-prs@0.1.0")
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)

	// Action manifest declares a connector that we DON'T pre-install.
	depFQN := "github://acme/missing-connector"
	md := goodActionMD(depFQN, "sha256:notinstalled000000000000000000000000000000000000000000000000000")
	tarball := buildActionTarball(t, md, priv)

	srv := installActionTestServer(t, ref, tarball, pub, "") // empty depFQN → don't pre-install
	rec := postPreviewAction(srv, `{
		"fqn": "github://ALRubinger/aileron-connector-github/actions/list-recent-prs",
		"version": "0.1.0"
	}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var got api.ActionPreview
	_ = json.NewDecoder(rec.Body).Decode(&got)
	if len(got.ConnectorDeps) != 1 {
		t.Fatalf("ConnectorDeps = %v, want 1", got.ConnectorDeps)
	}
	if got.ConnectorDeps[0].AlreadyInstalled {
		t.Error("dep.AlreadyInstalled = true; want false (dep was not pre-installed)")
	}
}

// TestPreviewAction_UnsignedTarballRendersUnsigned asserts that v1's
// optional-signing posture flows through preview: an unsigned
// tarball returns signature_status=unsigned (no error), and the CLI
// surfaces this in the consent prompt so the operator decides.
func TestPreviewAction_UnsignedTarballRendersUnsigned(t *testing.T) {
	ref, _ := cstore.ParseRef("github://acme/aileron-connector-x/actions/run@0.1.0")
	depFQN := "github://acme/aileron-connector-x"
	md := goodActionMD(depFQN, fakeConnectorHash(depFQN, "1.0.0", "api_key"))
	tarball := buildActionTarball(t, md, nil) // no priv → no signature

	srv := installActionTestServer(t, ref, tarball, nil, depFQN)
	rec := postPreviewAction(srv, `{
		"fqn": "github://acme/aileron-connector-x/actions/run",
		"version": "0.1.0"
	}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var got api.ActionPreview
	_ = json.NewDecoder(rec.Body).Decode(&got)
	if got.SignatureStatus == nil || *got.SignatureStatus != api.ActionPreviewSignatureStatusUnsigned {
		t.Errorf("SignatureStatus = %v, want unsigned", got.SignatureStatus)
	}
}

// TestPreviewAction_RejectsMissingVersion asserts wire-level
// validation: a request with no version is a 400 rather than a
// server crash. Mirrors the same check in InstallAction.
func TestPreviewAction_RejectsMissingVersion(t *testing.T) {
	ref, _ := cstore.ParseRef("github://acme/x/actions/foo@0.1.0")
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	depFQN := "github://acme/x"
	md := goodActionMD(depFQN, fakeConnectorHash(depFQN, "1.0.0", "api_key"))
	srv := installActionTestServer(t, ref, buildActionTarball(t, md, priv), pub, depFQN)

	rec := postPreviewAction(srv, `{
		"fqn": "github://acme/x/actions/foo",
		"version": ""
	}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// TestPreviewAction_NoInstallerReturns503 covers the bare-server
// path: no installer wired, endpoint returns 503 not a panic. Same
// robustness contract as InstallAction.
func TestPreviewAction_NoInstallerReturns503(t *testing.T) {
	srv := &apiServer{}
	rec := httptest.NewRecorder()
	srv.PreviewAction(rec, httptest.NewRequest(http.MethodPost, "/v1/actions/preview",
		strings.NewReader(`{"fqn":"github://x/y/actions/z","version":"1.0.0"}`)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
}

// TestPreviewAction_AlreadyInstalledFlag asserts the action-already-
// installed short-circuit: install once, then preview the same FQN+
// version with the same bytes → preview reports already_installed=
// true. The CLI uses this to skip the prompt entirely.
func TestPreviewAction_AlreadyInstalledFlag(t *testing.T) {
	ref, _ := cstore.ParseRef("github://ALRubinger/aileron-connector-github/actions/list-recent-prs@0.1.0")
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	depFQN := "github://ALRubinger/aileron-connector-github"
	md := goodActionMD(depFQN, fakeConnectorHash(depFQN, "1.0.0", "api_key"))
	tarball := buildActionTarball(t, md, priv)

	srv := installActionTestServer(t, ref, tarball, pub, depFQN)

	// First install.
	if rec := postInstallAction(srv, `{
		"fqn": "github://ALRubinger/aileron-connector-github/actions/list-recent-prs",
		"version": "0.1.0"
	}`); rec.Code != http.StatusCreated {
		t.Fatalf("InstallAction setup failed: %d %s", rec.Code, rec.Body.String())
	}

	// Now preview — the action is on disk with the same bytes.
	rec := postPreviewAction(srv, `{
		"fqn": "github://ALRubinger/aileron-connector-github/actions/list-recent-prs",
		"version": "0.1.0"
	}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var got api.ActionPreview
	_ = json.NewDecoder(rec.Body).Decode(&got)
	if got.AlreadyInstalled == nil || !*got.AlreadyInstalled {
		t.Errorf("AlreadyInstalled = %v, want pointer to true", got.AlreadyInstalled)
	}
	if got.Existing != nil {
		t.Errorf("Existing = %+v, want nil when bytes match", got.Existing)
	}
}

// TestPreviewAction_ExistingPopulatedOnVersionMismatch asserts the
// upgrade-candidate path: an action with the same name is on disk
// with different bytes (e.g. an earlier version) → preview reports
// already_installed=false AND populates `existing` with the on-disk
// version + hash + source + path. The CLI uses this to render the
// upgrade prompt rather than treating the install as fresh.
func TestPreviewAction_ExistingPopulatedOnVersionMismatch(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	depFQN := "github://ALRubinger/aileron-connector-github"
	depHash := fakeConnectorHash(depFQN, "1.0.0", "api_key")

	// First install: version 0.1.0 of "list-recent-prs".
	v1Ref, _ := cstore.ParseRef("github://ALRubinger/aileron-connector-github/actions/list-recent-prs@0.1.0")
	v1MD := goodActionMD(depFQN, depHash)
	srv := installActionTestServer(t, v1Ref, buildActionTarball(t, v1MD, priv), pub, depFQN)
	if rec := postInstallAction(srv, `{
		"fqn": "github://ALRubinger/aileron-connector-github/actions/list-recent-prs",
		"version": "0.1.0"
	}`); rec.Code != http.StatusCreated {
		t.Fatalf("setup install failed: %d %s", rec.Code, rec.Body.String())
	}

	// Build a v0.2.0 manifest reusing the same name. The rest of the
	// bytes only need to differ; goodActionMDVersion swaps in the new
	// version + source so the file content really is different.
	v2MD := []byte(`+++
name = "list-recent-prs"
version = "0.2.0"
source = "github://ALRubinger/aileron-connector-github/actions/list-recent-prs@0.2.0"

[[requires.connectors]]
name = "` + depFQN + `"
version = "1.0.0"
hash = "` + depHash + `"
capabilities = ["list_prs"]

[match]
intent = "list pull requests"

[[execute]]
id = "list"
connector = "` + depFQN + `"
op = "list_prs"
+++

# List Recent PRs (v2)

Lists recent merged PRs from a GitHub repository.
`)
	v2Ref, _ := cstore.ParseRef("github://ALRubinger/aileron-connector-github/actions/list-recent-prs@0.2.0")
	v2URL, _ := srv.installer.Resolver.ResolveTarball(v2Ref)
	v2Tarball := buildActionTarball(t, v2MD, priv)
	srv.installer.Fetcher = &fakeFetcher{bytesAt: map[string][]byte{v2URL: v2Tarball}}

	rec := postPreviewAction(srv, `{
		"fqn": "github://ALRubinger/aileron-connector-github/actions/list-recent-prs",
		"version": "0.2.0"
	}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var got api.ActionPreview
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.AlreadyInstalled != nil && *got.AlreadyInstalled {
		t.Errorf("AlreadyInstalled = true; want false because bytes differ")
	}
	if got.Existing == nil {
		t.Fatal("Existing = nil; want populated because v0.1.0 is on disk with different bytes")
	}
	if got.Existing.Version != "0.1.0" {
		t.Errorf("Existing.Version = %q, want 0.1.0", got.Existing.Version)
	}
	if !strings.HasPrefix(got.Existing.Hash, "sha256:") {
		t.Errorf("Existing.Hash = %q, want sha256: prefix", got.Existing.Hash)
	}
	if got.Existing.Hash == got.Hash {
		t.Errorf("Existing.Hash == previewed Hash %q; want different (bytes diverged)", got.Hash)
	}
	if got.Existing.Path == "" {
		t.Errorf("Existing.Path is empty")
	}
	if got.Existing.Source != "github://ALRubinger/aileron-connector-github/actions/list-recent-prs@0.1.0" {
		t.Errorf("Existing.Source = %q, want v0.1.0 source", got.Existing.Source)
	}
}
