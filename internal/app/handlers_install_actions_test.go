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
// supplied connector FQN at version 1.0.0 with the given pinned
// `sha256:<hex>` hash. Tests that exercise the install pipeline must
// pass the hash that the dependency connector actually produces (see
// fakeConnectorHash) so the cross-check passes.
func goodActionMD(connectorFQN, hash string) []byte {
	return []byte(`+++
name = "list-recent-prs"
version = "0.1.0"
source = "github://ALRubinger/aileron-connector-github/actions/list-recent-prs@0.1.0"

[[requires.connectors]]
name = "` + connectorFQN + `"
version = "1.0.0"
hash = "` + hash + `"
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
	md := goodActionMD(depFQN, fakeConnectorHash(depFQN, "1.0.0", "api_key"))
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
	md := goodActionMD(depFQN, fakeConnectorHash(depFQN, "1.0.0", "api_key"))
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
	md := goodActionMD(depFQN, fakeConnectorHash(depFQN, "1.0.0", "api_key"))
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
	// Action references a connector whose pinned hash isn't in the
	// store → 422 connectors_missing with structured details per
	// missing entry. Per ADR-0007 the install consent flow ratifies
	// "no surprise installs"; the structured details let the CLI
	// surface FQN, version, and hash to the user before retrying
	// with auto_install_connectors=true (issue #413).
	ref, _ := cstore.ParseRef("github://acme/aileron-connector-x/actions/run@0.1.0")
	md := goodActionMD("github://nobody/missing", "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
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
	if !strings.Contains(rec.Body.String(), "connectors_missing") {
		t.Errorf("expected error code connectors_missing in body: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "github://nobody/missing") {
		t.Errorf("body should name missing connector: %s", rec.Body.String())
	}
	// Structured details — name, version, hash for each missing entry.
	var env api.Error
	if err := json.NewDecoder(rec.Body).Decode(&env); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if env.Error.Details == nil || len(*env.Error.Details) != 1 {
		t.Fatalf("expected exactly one details entry, got %v", env.Error.Details)
	}
	d := (*env.Error.Details)[0]
	if d["name"] != "github://nobody/missing" {
		t.Errorf("details.name = %v", d["name"])
	}
	if d["version"] != "1.0.0" {
		t.Errorf("details.version = %v", d["version"])
	}
	if got, ok := d["hash"].(string); !ok || got == "" {
		t.Errorf("details.hash = %v (must be non-empty)", d["hash"])
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
	md := goodActionMD(depFQN, fakeConnectorHash(depFQN, "1.0.0", "api_key"))
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
	md1 := goodActionMD(depFQN, fakeConnectorHash(depFQN, "1.0.0", "api_key"))
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
	md1 := goodActionMD(depFQN, fakeConnectorHash(depFQN, "1.0.0", "api_key"))
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
	// ADR-0007 §"For audit and security": every consent decision is
	// logged with FQN, version, hash, signature status, decision, and
	// (for actions) the dependency list — so a reviewer can answer
	// "what did I install, and what did I agree it could do?" from the
	// audit log alone. The signed-tarball path here exercises the
	// happy case where signature_status = "verified".
	ref, _ := cstore.ParseRef("github://acme/aileron-connector-x/actions/run@0.1.0")
	depFQN := "github://acme/aileron-connector-x"
	depHash := fakeConnectorHash(depFQN, "1.0.0", "api_key")
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	pub := priv.Public().(ed25519.PublicKey)
	srv := installActionTestServer(t, ref, buildActionTarball(t, goodActionMD(depFQN, depHash), priv), pub, depFQN)
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
			for _, key := range []string{"name", "fqn", "version", "hash", "signature_status", "decision", "path", "dependencies"} {
				if _, ok := e.Payload[key]; !ok {
					t.Errorf("payload missing %q", key)
				}
			}
			if got, _ := e.Payload["signature_status"].(string); got != "verified" {
				t.Errorf("signature_status = %q, want \"verified\"", got)
			}
			if got, _ := e.Payload["decision"].(string); got != "approved" {
				t.Errorf("decision = %q, want \"approved\"", got)
			}
			hash, _ := e.Payload["hash"].(string)
			if !strings.HasPrefix(hash, "sha256:") || len(hash) != len("sha256:")+64 {
				t.Errorf("hash = %q, want sha256:<64 hex>", hash)
			}
			deps, ok := e.Payload["dependencies"].([]map[string]any)
			if !ok || len(deps) != 1 {
				t.Errorf("dependencies = %#v, want one entry", e.Payload["dependencies"])
			} else {
				if deps[0]["name"] != depFQN {
					t.Errorf("dependencies[0].name = %v, want %q", deps[0]["name"], depFQN)
				}
				if deps[0]["version"] != "1.0.0" {
					t.Errorf("dependencies[0].version = %v, want \"1.0.0\"", deps[0]["version"])
				}
				if deps[0]["hash"] != depHash {
					t.Errorf("dependencies[0].hash = %v, want %q", deps[0]["hash"], depHash)
				}
			}
		}
	}
	if !found {
		t.Error("expected an action.installed event")
	}
}

func TestInstallAction_AuditUnsignedTarball(t *testing.T) {
	// ADR-0007 keeps action signing optional in v1: an unsigned
	// tarball still installs but the audit event must record
	// signature_status = "unsigned" so reviewers can tell the two
	// trust postures apart.
	ref, _ := cstore.ParseRef("github://acme/aileron-connector-x/actions/run@0.1.0")
	depFQN := "github://acme/aileron-connector-x"
	srv := installActionTestServer(t, ref, buildActionTarball(t, goodActionMD(depFQN, fakeConnectorHash(depFQN, "1.0.0", "api_key")), nil), nil, depFQN)
	if rec := postInstallAction(srv, `{"fqn":"github://acme/aileron-connector-x/actions/run","version":"0.1.0"}`); rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	events := dumpEvents(t, srv.auditStore)
	var got *audit.Event
	for i := range events {
		if events[i].EventType == "action.installed" {
			got = &events[i]
			break
		}
	}
	if got == nil {
		t.Fatal("expected an action.installed event")
	}
	if status, _ := got.Payload["signature_status"].(string); status != "unsigned" {
		t.Errorf("signature_status = %q, want \"unsigned\"", status)
	}
}

// _ silences the io import used only in fakeFetcher (already in the
// package's other test files); the import here is for future test
// helpers that stream bodies.
var _ io.Reader = (*bytes.Reader)(nil)

// signedConnectorTarball builds a real, signed connector tarball that
// can be fed through the actual Installer.Install pipeline (signature
// verification, hash compute, store commit). Used by the
// auto-install-connectors tests for action add (issue #413).
//
// Returns the tarball bytes and the canonical `sha256:<hex>` hash they
// produce, which is what the action manifest must pin.
func signedConnectorTarball(t *testing.T, fqn, version string, priv ed25519.PrivateKey) (tarball []byte, hash string) {
	t.Helper()
	manifest := []byte(`[connector]
name = "` + fqn + `"
version = "` + version + `"
publisher = "test"

[capabilities.credential]
kind = "api_key"
scope = "issues:write"
`)
	binary := []byte("BIN-AUTO-INSTALL")
	payload := append(append([]byte{}, binary...), manifest...)
	sig := ed25519.Sign(priv, payload)

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range []struct {
		name string
		body []byte
	}{
		{"connector.wasm", binary},
		{"manifest.toml", manifest},
		{"signature.sig", sig},
	} {
		_ = tw.WriteHeader(&tar.Header{Name: e.name, Mode: 0o644, Size: int64(len(e.body)), ModTime: time.Unix(0, 0)})
		_, _ = tw.Write(e.body)
	}
	_ = tw.Close()
	_ = gz.Close()

	tb := &cstore.Tarball{Binary: binary, Manifest: manifest}
	return buf.Bytes(), "sha256:" + tb.CanonicalHashHex()
}

// buildAutoInstallServer assembles an apiServer where:
//   - the action tarball is served at the resolver URL for actionRef
//   - the connector tarball is served at the resolver URL for connectorRef
//   - the keyring trusts the action publisher and the connector publisher
//     against the same ed25519 key (test convenience)
//   - the connector store starts empty so the auto-install path is
//     exercised end-to-end by the action install pipeline
func buildAutoInstallServer(t *testing.T,
	actionRef cstore.Ref, actionTarball []byte,
	connectorRef cstore.Ref, connectorTarball []byte,
	pub ed25519.PublicKey,
) *apiServer {
	t.Helper()

	resolver := cstore.DefaultResolver()
	actionURL, err := resolver.ResolveTarball(actionRef)
	if err != nil {
		t.Fatalf("resolve action: %v", err)
	}
	connectorURL, err := resolver.ResolveTarball(connectorRef)
	if err != nil {
		t.Fatalf("resolve connector: %v", err)
	}

	keyring := cstore.NewEd25519Keyring()
	keyring.Add(actionRef.FQN.Authority(), pub)
	keyring.Add(connectorRef.FQN.Authority(), pub)

	auditStore := audit.NewMemStore()
	srv := &apiServer{
		log:     slog.Default(),
		actions: action.NewStore(t.TempDir()),
		installer: &cstore.Installer{
			Resolver: resolver,
			Fetcher: &fakeFetcher{bytesAt: map[string][]byte{
				actionURL:    actionTarball,
				connectorURL: connectorTarball,
			}},
			Verifier: keyring,
			Store:    cstore.NewStore(t.TempDir()),
		},
		auditStore:    auditStore,
		auditRecorder: audit.NewRecorder(auditStore, nil, nil),
	}
	if _, err := srv.actions.Load(); err != nil {
		t.Fatalf("actions.Load: %v", err)
	}
	return srv
}

func TestInstallAction_AutoInstallConnectorsSucceeds(t *testing.T) {
	// auto_install_connectors=true on the request makes the server
	// run the connector install pipeline for every missing
	// `[[requires.connectors]]` entry before completing the action
	// install. The connector tarball is fetched, signature-verified,
	// and committed to the store at the canonical hash; the action
	// install's HasHash check then passes and the action lands.
	depFQN := "github://acme/aileron-connector-x"
	depVersion := "1.0.0"
	connectorRef, _ := cstore.ParseRef(depFQN + "@" + depVersion)
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	connectorTarball, depHash := signedConnectorTarball(t, depFQN, depVersion, priv)

	actionRef, _ := cstore.ParseRef("github://acme/aileron-connector-x/actions/run@0.1.0")
	md := goodActionMD(depFQN, depHash)
	actionTarball := buildActionTarball(t, md, priv)

	srv := buildAutoInstallServer(t, actionRef, actionTarball, connectorRef, connectorTarball, pub)

	// Sanity: without the flag, the server returns connectors_missing.
	rec := postInstallAction(srv, `{
		"fqn": "github://acme/aileron-connector-x/actions/run",
		"version": "0.1.0"
	}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("preflight (no flag) status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "connectors_missing") {
		t.Fatalf("preflight body should contain connectors_missing: %s", rec.Body.String())
	}

	// With the flag set, the server fetches the connector and the
	// action install completes.
	rec = postInstallAction(srv, `{
		"fqn": "github://acme/aileron-connector-x/actions/run",
		"version": "0.1.0",
		"auto_install_connectors": true
	}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("auto-install status = %d, body = %s", rec.Code, rec.Body.String())
	}

	// The connector landed in the store at the canonical hash.
	has, err := srv.installer.Store.HasHash(depHash)
	if err != nil {
		t.Fatalf("HasHash: %v", err)
	}
	if !has {
		t.Errorf("connector at %s not in store after auto-install", depHash)
	}
}

func TestInstallAction_AutoInstallHashMismatchAborts(t *testing.T) {
	// The action manifest pins a hash that does NOT match what the
	// connector tarball produces. Auto-install runs the connector
	// pipeline with ExpectedHash set; ADR-0004 requires hash mismatch
	// to fail-closed. The action install must abort with the
	// connector's structured failure (`hash_mismatch`).
	depFQN := "github://acme/aileron-connector-x"
	depVersion := "1.0.0"
	connectorRef, _ := cstore.ParseRef(depFQN + "@" + depVersion)
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	connectorTarball, _ := signedConnectorTarball(t, depFQN, depVersion, priv)

	bogusHash := "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	actionRef, _ := cstore.ParseRef("github://acme/aileron-connector-x/actions/run@0.1.0")
	md := goodActionMD(depFQN, bogusHash)
	actionTarball := buildActionTarball(t, md, priv)

	srv := buildAutoInstallServer(t, actionRef, actionTarball, connectorRef, connectorTarball, pub)

	rec := postInstallAction(srv, `{
		"fqn": "github://acme/aileron-connector-x/actions/run",
		"version": "0.1.0",
		"auto_install_connectors": true
	}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), string(cstore.ClassHashMismatch)) {
		t.Errorf("body should surface hash_mismatch class: %s", rec.Body.String())
	}
	// Nothing was committed to the action store.
	if entries, _ := os.ReadDir(srv.actions.Dir()); len(entries) != 0 {
		t.Errorf("action dir should be empty after hash mismatch, got %d entries", len(entries))
	}
}

func TestInstallAction_AutoInstallConnectorFetchFailureAborts(t *testing.T) {
	// auto_install_connectors=true with no connector tarball wired in
	// the fetcher → fetch_failed bubbles up. Confirms the autoinstall
	// path doesn't silently swallow connector pipeline failures.
	depFQN := "github://acme/aileron-connector-x"
	depVersion := "1.0.0"
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	depHash := "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

	actionRef, _ := cstore.ParseRef("github://acme/aileron-connector-x/actions/run@0.1.0")
	md := goodActionMD(depFQN, depHash)
	actionTarball := buildActionTarball(t, md, priv)

	resolver := cstore.DefaultResolver()
	actionURL, _ := resolver.ResolveTarball(actionRef)
	keyring := cstore.NewEd25519Keyring()
	keyring.Add(actionRef.FQN.Authority(), pub)
	srv := &apiServer{
		log:     slog.Default(),
		actions: action.NewStore(t.TempDir()),
		installer: &cstore.Installer{
			Resolver: resolver,
			// Connector URL is intentionally absent → fetch_failed.
			Fetcher:  &fakeFetcher{bytesAt: map[string][]byte{actionURL: actionTarball}},
			Verifier: keyring,
			Store:    cstore.NewStore(t.TempDir()),
		},
	}
	if _, err := srv.actions.Load(); err != nil {
		t.Fatalf("actions.Load: %v", err)
	}
	_ = depVersion

	rec := postInstallAction(srv, `{
		"fqn": "github://acme/aileron-connector-x/actions/run",
		"version": "0.1.0",
		"auto_install_connectors": true
	}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), string(cstore.ClassFetchFailed)) {
		t.Errorf("body should surface fetch_failed: %s", rec.Body.String())
	}
}
