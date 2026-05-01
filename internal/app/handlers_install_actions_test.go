package app

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	api "github.com/ALRubinger/aileron/internal/api/gen"
	"github.com/ALRubinger/aileron/internal/action"
	"github.com/ALRubinger/aileron/internal/audit"
	"github.com/ALRubinger/aileron/internal/cstore"
)

// buildActionTarball builds a gzipped tar containing action.md (with
// the supplied bytes) and an optional signature.sig (signed with the
// supplied private key). When priv is nil, the signature.sig entry is
// omitted entirely so the test can exercise the unsigned path.
func buildActionTarball(t *testing.T, actionMD []byte, priv ed25519.PrivateKey) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	mustWrite := func(name string, body []byte) {
		t.Helper()
		_ = tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), ModTime: time.Unix(0, 0)})
		_, _ = tw.Write(body)
	}
	mustWrite("action.md", actionMD)
	if priv != nil {
		sig := ed25519.Sign(priv, actionMD)
		mustWrite("signature.sig", sig)
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

// installActionTestServer wires an apiServer with:
//   - a cstore that already has the connector that the action depends
//     on installed (so the cross-check passes)
//   - an action store rooted at a fresh temp dir
//   - a fake fetcher serving the supplied tarball at the resolved URL
//   - a keyring that trusts pub for the FQN's authority
func installActionTestServer(t *testing.T, ref cstore.Ref, tarball []byte, pub ed25519.PublicKey, depFQN string) *apiServer {
	t.Helper()

	resolver := cstore.DefaultResolver()
	url, err := resolver.ResolveTarball(ref)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	store := cstore.NewStore(t.TempDir())

	// Install the dependency connector at v1.0.0 so cross-check passes.
	if depFQN != "" {
		installFakeAPIKeyConnector(t, store, depFQN, "1.0.0", "api_key")
	}

	keyring := cstore.NewEd25519Keyring()
	if pub != nil {
		keyring.Add(ref.FQN.Authority(), pub)
	}

	auditStore := audit.NewMemStore()
	srv := &apiServer{
		log:     slog.Default(),
		actions: action.NewStore(t.TempDir()),
		installer: &cstore.Installer{
			Resolver: resolver,
			Fetcher:  &fakeFetcher{bytesAt: map[string][]byte{url: tarball}},
			Verifier: keyring,
			Store:    store,
		},
		auditStore:    auditStore,
		auditRecorder: audit.NewRecorder(auditStore, nil, nil),
	}
	if _, err := srv.actions.Load(); err != nil {
		t.Fatalf("actions.Load: %v", err)
	}
	return srv
}

// goodActionMD returns a valid action manifest referencing the
// supplied connector FQN at version 1.0.0. The hash field is a
// placeholder — Validate accepts any sha256: prefix string.
func goodActionMD(connectorFQN string) []byte {
	return []byte(`+++
name = "list-recent-prs"
version = "0.1.0"
source = "github://ALRubinger/aileron-connector-github/actions/list-recent-prs@0.1.0"

[[requires.connectors]]
name = "` + connectorFQN + `"
version = "1.0.0"
hash = "sha256:placeholder"
capabilities = ["list_prs"]

[match]
intent = "list pull requests"

[[execute]]
id = "list"
connector = "` + connectorFQN + `"
op = "list_prs"
+++

# List Recent PRs

Lists recent merged PRs from a GitHub repository.
`)
}

func postInstallAction(srv *apiServer, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/actions/install", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.InstallAction(rec, req)
	return rec
}

func TestInstallAction_HappyPathSignedTarball(t *testing.T) {
	ref, _ := cstore.ParseRef("github://ALRubinger/aileron-connector-github/actions/list-recent-prs@0.1.0")
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	depFQN := "github://ALRubinger/aileron-connector-github"
	md := goodActionMD(depFQN)
	tarball := buildActionTarball(t, md, priv)

	srv := installActionTestServer(t, ref, tarball, pub, depFQN)
	rec := postInstallAction(srv, `{
		"fqn": "github://ALRubinger/aileron-connector-github/actions/list-recent-prs",
		"version": "0.1.0"
	}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got api.InstalledAction
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Name != "list-recent-prs" {
		t.Errorf("Name = %q", got.Name)
	}
	if got.Path == "" {
		t.Errorf("Path is empty")
	}
	// File landed on disk.
	body, err := os.ReadFile(got.Path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(body, md) {
		t.Errorf("file bytes do not match tarball contents")
	}
	// Audit recorded the install with no credential bytes.
	events := dumpEvents(t, srv.auditStore)
	if !containsEvent(events, "action.installed") {
		t.Error("expected action.installed audit event")
	}
}

func TestInstallAction_UnsignedTarballAccepted(t *testing.T) {
	// v1 keeps action signing optional. An unsigned tarball installs
	// (with a debug log on the server side); mandatory signing lands
	// with #363 install consent.
	ref, _ := cstore.ParseRef("github://acme/aileron-connector-x/actions/run@0.1.0")
	depFQN := "github://acme/aileron-connector-x"
	md := goodActionMD(depFQN)
	tarball := buildActionTarball(t, md, nil) // no private key → no signature

	srv := installActionTestServer(t, ref, tarball, nil, depFQN)
	rec := postInstallAction(srv, `{
		"fqn": "github://acme/aileron-connector-x/actions/run",
		"version": "0.1.0"
	}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestInstallAction_BadSignatureRejected(t *testing.T) {
	// Tarball signed with one key, server's keyring registers a
	// different key for the authority → signature_failure.
	ref, _ := cstore.ParseRef("github://acme/aileron-connector-x/actions/run@0.1.0")
	depFQN := "github://acme/aileron-connector-x"
	md := goodActionMD(depFQN)
	_, signingPriv, _ := ed25519.GenerateKey(rand.Reader)
	tarball := buildActionTarball(t, md, signingPriv)

	wrongPub, _, _ := ed25519.GenerateKey(rand.Reader)
	srv := installActionTestServer(t, ref, tarball, wrongPub, depFQN)
	rec := postInstallAction(srv, `{
		"fqn": "github://acme/aileron-connector-x/actions/run",
		"version": "0.1.0"
	}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "signature_failure") {
		t.Errorf("body = %s", rec.Body.String())
	}
}

func TestInstallAction_MissingConnectorDepRejected(t *testing.T) {
	// Action references a connector that is not installed → 422
	// validation_error naming the missing connector. Per ADR-0007 the
	// install consent flow ratifies "no surprise installs"; this
	// pre-check enforces the structural part of that.
	ref, _ := cstore.ParseRef("github://acme/aileron-connector-x/actions/run@0.1.0")
	md := goodActionMD("github://nobody/missing")
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	pub := priv.Public().(ed25519.PublicKey)
	tarball := buildActionTarball(t, md, priv)

	srv := installActionTestServer(t, ref, tarball, pub, "") // depFQN="" → no deps installed
	rec := postInstallAction(srv, `{
		"fqn": "github://acme/aileron-connector-x/actions/run",
		"version": "0.1.0"
	}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "github://nobody/missing") {
		t.Errorf("body should name missing connector: %s", rec.Body.String())
	}
}

func TestInstallAction_MalformedActionRejected(t *testing.T) {
	ref, _ := cstore.ParseRef("github://acme/aileron-connector-x/actions/run@0.1.0")
	depFQN := "github://acme/aileron-connector-x"
	md := []byte(`+++
name = "broken
+++
`)
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	pub := priv.Public().(ed25519.PublicKey)
	tarball := buildActionTarball(t, md, priv)

	srv := installActionTestServer(t, ref, tarball, pub, depFQN)
	rec := postInstallAction(srv, `{
		"fqn": "github://acme/aileron-connector-x/actions/run",
		"version": "0.1.0"
	}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestInstallAction_AlreadyInstalledIs200WhenBytesMatch(t *testing.T) {
	ref, _ := cstore.ParseRef("github://acme/aileron-connector-x/actions/run@0.1.0")
	depFQN := "github://acme/aileron-connector-x"
	md := goodActionMD(depFQN)
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	pub := priv.Public().(ed25519.PublicKey)
	tarball := buildActionTarball(t, md, priv)

	srv := installActionTestServer(t, ref, tarball, pub, depFQN)
	body := `{"fqn":"github://acme/aileron-connector-x/actions/run","version":"0.1.0"}`

	rec := postInstallAction(srv, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("first install status = %d", rec.Code)
	}
	rec = postInstallAction(srv, body)
	if rec.Code != http.StatusOK {
		t.Errorf("second install status = %d, want 200", rec.Code)
	}
	var got api.InstalledAction
	_ = json.NewDecoder(rec.Body).Decode(&got)
	if got.AlreadyInstalled == nil || !*got.AlreadyInstalled {
		t.Errorf("AlreadyInstalled = %v, want true", got.AlreadyInstalled)
	}
}

func TestInstallAction_ConflictWhenContentDiffers(t *testing.T) {
	ref, _ := cstore.ParseRef("github://acme/aileron-connector-x/actions/run@0.1.0")
	depFQN := "github://acme/aileron-connector-x"
	md1 := goodActionMD(depFQN)
	md2 := append([]byte(nil), md1...)
	md2[len(md2)-1] = '?' // changed bytes → looks like a different version of the action
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	pub := priv.Public().(ed25519.PublicKey)

	srv := installActionTestServer(t, ref, buildActionTarball(t, md1, priv), pub, depFQN)
	if rec := postInstallAction(srv, `{"fqn":"github://acme/aileron-connector-x/actions/run","version":"0.1.0"}`); rec.Code != http.StatusCreated {
		t.Fatalf("first install: %d", rec.Code)
	}
	// Repoint fetcher at the new tarball without force=true.
	url, _ := srv.installer.Resolver.ResolveTarball(ref)
	srv.installer.Fetcher = &fakeFetcher{bytesAt: map[string][]byte{url: buildActionTarball(t, md2, priv)}}
	rec := postInstallAction(srv, `{"fqn":"github://acme/aileron-connector-x/actions/run","version":"0.1.0"}`)
	if rec.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409", rec.Code)
	}
}

func TestInstallAction_ForceOverridesConflict(t *testing.T) {
	ref, _ := cstore.ParseRef("github://acme/aileron-connector-x/actions/run@0.1.0")
	depFQN := "github://acme/aileron-connector-x"
	md1 := goodActionMD(depFQN)
	md2 := append([]byte(nil), md1...)
	md2[len(md2)-1] = '?'
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	pub := priv.Public().(ed25519.PublicKey)

	srv := installActionTestServer(t, ref, buildActionTarball(t, md1, priv), pub, depFQN)
	postInstallAction(srv, `{"fqn":"github://acme/aileron-connector-x/actions/run","version":"0.1.0"}`)

	url, _ := srv.installer.Resolver.ResolveTarball(ref)
	srv.installer.Fetcher = &fakeFetcher{bytesAt: map[string][]byte{url: buildActionTarball(t, md2, priv)}}
	rec := postInstallAction(srv, `{"fqn":"github://acme/aileron-connector-x/actions/run","version":"0.1.0","force":true}`)
	if rec.Code != http.StatusCreated {
		t.Errorf("force install status = %d, body = %s", rec.Code, rec.Body.String())
	}
	// File on disk reflects the new contents.
	dest := filepath.Join(srv.actions.Dir(), "list-recent-prs.md")
	got, _ := os.ReadFile(dest)
	if !bytes.Equal(got, md2) {
		t.Errorf("file contents not updated by force=true")
	}
}

func TestInstallAction_DisabledReturns503WhenInstallerNil(t *testing.T) {
	srv := &apiServer{log: slog.Default()}
	rec := postInstallAction(srv, `{"fqn":"x","version":"y"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
}

func TestInstallAction_BadFQNReturns400(t *testing.T) {
	srv := &apiServer{
		log: slog.Default(),
		installer: &cstore.Installer{
			Resolver: cstore.DefaultResolver(),
			Fetcher:  &fakeFetcher{},
			Verifier: cstore.NewEd25519Keyring(),
			Store:    cstore.NewStore(t.TempDir()),
		},
		actions: action.NewStore(t.TempDir()),
	}
	rec := postInstallAction(srv, `{"fqn":"not-an-fqn","version":"0.1.0"}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestInstallAction_MissingVersionReturns400(t *testing.T) {
	srv := &apiServer{
		log: slog.Default(),
		installer: &cstore.Installer{
			Resolver: cstore.DefaultResolver(),
			Fetcher:  &fakeFetcher{},
			Verifier: cstore.NewEd25519Keyring(),
			Store:    cstore.NewStore(t.TempDir()),
		},
		actions: action.NewStore(t.TempDir()),
	}
	rec := postInstallAction(srv, `{"fqn":"github://x/y/actions/z"}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestInstallAction_ContextCarriesAuditPayload(t *testing.T) {
	// Sanity: the audit event names the action and never the file body
	// (which is fine because the tarball only contains the manifest,
	// not credentials). The structured payload includes name + fqn +
	// version + source + path.
	ref, _ := cstore.ParseRef("github://acme/aileron-connector-x/actions/run@0.1.0")
	depFQN := "github://acme/aileron-connector-x"
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	pub := priv.Public().(ed25519.PublicKey)
	srv := installActionTestServer(t, ref, buildActionTarball(t, goodActionMD(depFQN), priv), pub, depFQN)
	postInstallAction(srv, `{"fqn":"github://acme/aileron-connector-x/actions/run","version":"0.1.0"}`)

	traces, _ := srv.auditStore.ListTraces(context.Background(), audit.Filter{})
	if len(traces) == 0 {
		t.Fatal("no traces recorded")
	}
	found := false
	for _, tr := range traces {
		for _, e := range tr.Events {
			if e.EventType != "action.installed" {
				continue
			}
			found = true
			for _, key := range []string{"name", "fqn", "version", "path"} {
				if _, ok := e.Payload[key]; !ok {
					t.Errorf("payload missing %q", key)
				}
			}
		}
	}
	if !found {
		t.Error("expected an action.installed event")
	}
}

// _ silences the io import used only in fakeFetcher (already in the
// package's other test files); the import here is for future test
// helpers that stream bodies.
var _ io.Reader = (*bytes.Reader)(nil)
