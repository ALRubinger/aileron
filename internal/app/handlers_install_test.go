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
	"path/filepath"
	"strings"
	"testing"
	"time"

	api "github.com/ALRubinger/aileron/internal/api/gen"
	"github.com/ALRubinger/aileron/internal/audit"
	"github.com/ALRubinger/aileron/internal/cstore"
	"github.com/ALRubinger/aileron/internal/hub"
)

// fakeFetcher serves bytes from an in-memory map keyed by URL. This file
// avoids importing cstore_test.go's helper because cross-package test
// helpers aren't visible in Go.
type fakeFetcher struct {
	bytesAt map[string][]byte
}

func (f *fakeFetcher) Fetch(_ context.Context, url string) (io.ReadCloser, error) {
	if body, ok := f.bytesAt[url]; ok {
		return io.NopCloser(bytes.NewReader(body)), nil
	}
	return nil, &cstore.Error{Class: cstore.ClassFetchFailed, Message: "no fixture"}
}

// installTestServer builds an apiServer with an Installer pointing at an
// in-memory fetcher whose canonical tarball lives at the resolver-computed
// URL for ref. Returns the server, the canonical hash the tarball
// produces, and the install ref.
func installTestServer(t *testing.T, ref cstore.Ref) (*apiServer, string) {
	t.Helper()
	manifest := []byte(`[connector]
name = "` + ref.FQN.String() + `"
version = "` + ref.Version + `"
publisher = "test"
`)
	binary := []byte("BIN")
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	payload := append(append([]byte{}, binary...), manifest...)
	sig := ed25519.Sign(priv, payload)

	// Build a tarball.
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

	resolver := cstore.DefaultResolver()
	url, err := resolver.ResolveTarball(ref)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	keyring := cstore.NewEd25519Keyring()
	keyring.Add(ref.FQN.Authority(), pub)

	store := cstore.NewStore(t.TempDir())
	tb := &cstore.Tarball{Binary: binary, Manifest: manifest}

	auditStore := audit.NewMemStore()
	srv := &apiServer{
		log: slog.Default(),
		installer: &cstore.Installer{
			Resolver: resolver,
			Fetcher:  &fakeFetcher{bytesAt: map[string][]byte{url: buf.Bytes()}},
			Verifier: keyring,
			Store:    store,
		},
		auditStore:    auditStore,
		auditRecorder: audit.NewRecorder(auditStore, nil, nil),
	}
	return srv, "sha256:" + tb.CanonicalHashHex()
}

func postInstall(srv *apiServer, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/connectors/install", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.InstallConnector(rec, req)
	return rec
}

func TestInstallConnector_HappyPathReturns201WithEntry(t *testing.T) {
	ref, _ := cstore.ParseRef("github://aileron/slack@1.2.0")
	srv, expectedHash := installTestServer(t, ref)

	rec := postInstall(srv, `{"fqn":"github://aileron/slack","version":"1.2.0"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	var got api.InstalledConnector
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Fqn != "github://aileron/slack" || got.Version != "1.2.0" {
		t.Errorf("envelope = %+v", got)
	}
	if got.Hash != expectedHash {
		t.Errorf("Hash = %q, want %q", got.Hash, expectedHash)
	}
	if got.AlreadyInstalled != nil && *got.AlreadyInstalled {
		t.Errorf("AlreadyInstalled = true on first install")
	}
}

func TestInstallConnector_OfflineReinstallReturns200WithAlreadyInstalled(t *testing.T) {
	// ADR-0004 §"Offline behavior": reinstall of an already-stored hash
	// is offline and must short-circuit. The handler reflects that with
	// status 200 (not 201) and AlreadyInstalled=true.
	ref, _ := cstore.ParseRef("github://aileron/slack@1.2.0")
	srv, expectedHash := installTestServer(t, ref)

	if rec := postInstall(srv, `{"fqn":"github://aileron/slack","version":"1.2.0"}`); rec.Code != http.StatusCreated {
		t.Fatalf("first install: status=%d body=%s", rec.Code, rec.Body.String())
	}

	rec := postInstall(srv, `{"fqn":"github://aileron/slack","version":"1.2.0","expected_hash":"`+expectedHash+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got api.InstalledConnector
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.AlreadyInstalled == nil || !*got.AlreadyInstalled {
		t.Errorf("AlreadyInstalled = %v, want true", got.AlreadyInstalled)
	}
}

func TestInstallConnector_AuditPayloadHasConsentFields(t *testing.T) {
	// ADR-0007 §"For audit and security": every consent decision is
	// logged with FQN, version, hash, signature status, and decision.
	// Reaching the success path implies the install pipeline ran
	// Verify, so signature_status records "verified".
	ref, _ := cstore.ParseRef("github://aileron/slack@1.2.0")
	srv, expectedHash := installTestServer(t, ref)

	if rec := postInstall(srv, `{"fqn":"github://aileron/slack","version":"1.2.0"}`); rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	events := dumpEvents(t, srv.auditStore)
	var got *audit.Event
	for i := range events {
		if events[i].EventType == "connector.installed" {
			got = &events[i]
			break
		}
	}
	if got == nil {
		t.Fatalf("expected a connector.installed event; got %d events", len(events))
	}
	if got.Payload["aileron.connector.fqn"] != "github://aileron/slack" {
		t.Errorf("aileron.connector.fqn = %v", got.Payload["aileron.connector.fqn"])
	}
	if got.Payload["aileron.connector.version"] != "1.2.0" {
		t.Errorf("aileron.connector.version = %v", got.Payload["aileron.connector.version"])
	}
	if got.Payload["aileron.connector.hash"] != expectedHash {
		t.Errorf("aileron.connector.hash = %v, want %q", got.Payload["aileron.connector.hash"], expectedHash)
	}
	if got.Payload["aileron.signature.status"] != "verified" {
		t.Errorf("aileron.signature.status = %v, want \"verified\"", got.Payload["aileron.signature.status"])
	}
	if got.Payload["aileron.consent.decision"] != "approved" {
		t.Errorf("aileron.consent.decision = %v, want \"approved\"", got.Payload["aileron.consent.decision"])
	}
	if got.Payload["aileron.install.already_installed"] != false {
		t.Errorf("aileron.install.already_installed = %v, want false", got.Payload["aileron.install.already_installed"])
	}
}

func TestInstallConnector_AuditOnOfflineReinstall(t *testing.T) {
	// Offline reinstall (already-stored hash) still represents a user
	// consent decision in the install flow, so it gets its own audit
	// event with already_installed = true. This lets a reviewer count
	// every install attempt — including the no-ops — when answering
	// "what did this user agree to?".
	ref, _ := cstore.ParseRef("github://aileron/slack@1.2.0")
	srv, expectedHash := installTestServer(t, ref)

	if rec := postInstall(srv, `{"fqn":"github://aileron/slack","version":"1.2.0"}`); rec.Code != http.StatusCreated {
		t.Fatalf("first install: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := postInstall(srv, `{"fqn":"github://aileron/slack","version":"1.2.0","expected_hash":"`+expectedHash+`"}`); rec.Code != http.StatusOK {
		t.Fatalf("reinstall: status=%d body=%s", rec.Code, rec.Body.String())
	}

	events := dumpEvents(t, srv.auditStore)
	count := 0
	var reinstall *audit.Event
	for i := range events {
		if events[i].EventType != "connector.installed" {
			continue
		}
		count++
		if v, _ := events[i].Payload["aileron.install.already_installed"].(bool); v {
			reinstall = &events[i]
		}
	}
	if count != 2 {
		t.Errorf("connector.installed count = %d, want 2", count)
	}
	if reinstall == nil {
		t.Errorf("expected an event with already_installed=true")
	}
}

func TestInstallConnector_HashMismatchReturns422(t *testing.T) {
	// ADR-0004's failure-modes table: hash mismatch is a hard fail. The
	// handler maps cstore.ClassHashMismatch to 422 because the request was
	// well-formed but the artifact didn't pass.
	ref, _ := cstore.ParseRef("github://aileron/slack@1.2.0")
	srv, _ := installTestServer(t, ref)

	rec := postInstall(srv, `{"fqn":"github://aileron/slack","version":"1.2.0","expected_hash":"sha256:00ff"}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), string(cstore.ClassHashMismatch)) {
		t.Errorf("body %q does not name the failure class", rec.Body.String())
	}
}

func TestInstallConnector_UnknownSchemeReturns400(t *testing.T) {
	ref, _ := cstore.ParseRef("github://aileron/slack@1.2.0")
	srv, _ := installTestServer(t, ref)

	rec := postInstall(srv, `{"fqn":"ftp://acme/slack","version":"1.0.0"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestInstallConnector_MalformedRequestReturns400(t *testing.T) {
	ref, _ := cstore.ParseRef("github://aileron/slack@1.2.0")
	srv, _ := installTestServer(t, ref)

	cases := []string{
		`{"fqn":"","version":"1.2.0"}`,
		`{"fqn":"github://aileron/slack","version":""}`,
		`{"fqn":"github://aileron/slack","version":"latest"}`,
		`not json at all`,
	}
	for _, body := range cases {
		t.Run(body, func(t *testing.T) {
			rec := postInstall(srv, body)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("body %q got status %d, want 400", body, rec.Code)
			}
		})
	}
}

func TestInstallConnector_DisabledServiceReturns503(t *testing.T) {
	srv := &apiServer{log: slog.Default()} // installer == nil
	rec := postInstall(srv, `{"fqn":"github://aileron/slack","version":"1.2.0"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
}

// installTestServerWithHub stands up an install pipeline whose
// publisher key is NOT pre-trusted in the keyring, plus a Hub fixture
// that lists the connector and a key server that serves the
// publisher's PEM. This is the shape of a "user is installing a
// community connector for the first time" scenario — the only path to
// trust is the Hub install-decision flow with a confirmed fingerprint.
//
// Returns (server, expected canonical hash, fingerprint string, keyring path).
// Caller asserts on keyring contents at `keyringPath` after the install.
func installTestServerWithHub(t *testing.T, ref cstore.Ref) (*apiServer, string, string, string) {
	t.Helper()
	manifest := []byte(`[connector]
name = "` + ref.FQN.String() + `"
version = "` + ref.Version + `"
publisher = "test"
`)
	binary := []byte("BIN")
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
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

	resolver := cstore.DefaultResolver()
	url, err := resolver.ResolveTarball(ref)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	// Empty keyring on disk. The Verifier reloads on each Install call,
	// so writing the publisher key to this file during install-time
	// trust confirmation will make the signature verify in the same
	// request.
	keyringPath := filepath.Join(t.TempDir(), "keyring.json")
	empty := cstore.NewEd25519Keyring()
	if err := empty.SaveKeyring(keyringPath); err != nil {
		t.Fatalf("SaveKeyring: %v", err)
	}

	// PEM key server stands in for the publisher's key_url.
	keySrv := httpKeyServer(t, pub)
	t.Cleanup(keySrv.Close)

	hubURL := makeHubFixture(t, map[string]string{
		"entry.yaml": entryYAML(ref.FQN.String(), "test connector", "test", keySrv.URL+"/publisher.pub"),
	})

	store := cstore.NewStore(t.TempDir())
	tb := &cstore.Tarball{Binary: binary, Manifest: manifest}

	auditStore := audit.NewMemStore()
	srv := &apiServer{
		log: slog.Default(),
		installer: &cstore.Installer{
			Resolver: resolver,
			Fetcher:  &fakeFetcher{bytesAt: map[string][]byte{url: buf.Bytes()}},
			Verifier: &cstore.ReloadingKeyring{Path: keyringPath},
			Store:    store,
		},
		auditStore:    auditStore,
		auditRecorder: audit.NewRecorder(auditStore, nil, nil),
		hub:           &hub.Client{URL: hubURL, HTTP: keySrv.Client()},
		keyringPath:   keyringPath,
	}
	return srv, "sha256:" + tb.CanonicalHashHex(), hub.Fingerprint(pub), keyringPath
}

func TestInstallConnector_ConfirmedFingerprintWritesTrustBeforeInstall(t *testing.T) {
	// ADR-0013 / #487 Q4: install accepts a confirmed_fingerprint, looks
	// up the FQN in the Hub, fetches the publisher key, verifies the
	// fingerprint matches, then writes the key to the keyring under the
	// FQN. The install pipeline's reloading verifier picks up the newly
	// trusted key and the install proceeds.
	ref, _ := cstore.ParseRef("github://aileron/slack@1.2.0")
	srv, _, fingerprint, keyringPath := installTestServerWithHub(t, ref)

	body := `{"fqn":"github://aileron/slack","version":"1.2.0","confirmed_fingerprint":"` + fingerprint + `"}`
	rec := postInstall(srv, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	// Trust must be persisted on the keyring file.
	kr, err := cstore.LoadKeyring(keyringPath)
	if err != nil {
		t.Fatalf("LoadKeyring: %v", err)
	}
	if got := kr.Keys(ref.FQN.String()); len(got) != 1 {
		t.Errorf("keyring keys for %s: got %d, want 1", ref.FQN, len(got))
	}
}

func TestInstallConnector_ConfirmedFingerprintMismatchReturns422(t *testing.T) {
	// Anti-tampering: the operator's confirmed fingerprint must match
	// the daemon's view of the publisher key. A mismatch means the key
	// at key_url changed between render-time and install-time (rotation,
	// MITM, or the operator confirmed a stale render); the install is
	// rejected and trust is NOT written.
	ref, _ := cstore.ParseRef("github://aileron/slack@1.2.0")
	srv, _, _, keyringPath := installTestServerWithHub(t, ref)

	body := `{"fqn":"github://aileron/slack","version":"1.2.0","confirmed_fingerprint":"sha256:DEADBEEFDEADBEEFDEADBE"}`
	rec := postInstall(srv, body)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "fingerprint_mismatch") {
		t.Errorf("expected fingerprint_mismatch code, got: %s", rec.Body.String())
	}

	// Trust must NOT be persisted.
	kr, _ := cstore.LoadKeyring(keyringPath)
	if got := kr.Keys(ref.FQN.String()); len(got) != 0 {
		t.Errorf("keyring keys for %s on mismatch: got %d, want 0", ref.FQN, len(got))
	}
}

func TestInstallConnector_ConfirmedFingerprintNotInHubReturns404(t *testing.T) {
	// The CLI only sends confirmed_fingerprint when it just rendered a
	// Hub install-decision for the FQN. If the daemon can't find that
	// entry, the client and daemon disagree on what's installable —
	// return 404 rather than silently trusting a key from an
	// unverifiable source.
	ref, _ := cstore.ParseRef("github://aileron/slack@1.2.0")
	srv, _, _, _ := installTestServerWithHub(t, ref)

	body := `{"fqn":"github://nobody/missing","version":"1.0.0","confirmed_fingerprint":"sha256:any22charsworthwhatever"}`
	rec := postInstall(srv, body)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404; body = %s", rec.Code, rec.Body.String())
	}
}

func TestInstallConnector_ConfirmedFingerprintHubDisabledReturns503(t *testing.T) {
	// Honoring a confirmed_fingerprint requires the Hub client to be
	// configured — there's no other way to resolve the publisher key
	// from the FQN alone. With hub == nil the request is rejected
	// rather than falling back to silent trust.
	ref, _ := cstore.ParseRef("github://aileron/slack@1.2.0")
	srv, _ := installTestServer(t, ref) // no hub wired
	body := `{"fqn":"github://aileron/slack","version":"1.2.0","confirmed_fingerprint":"sha256:irrelevantbutwellformed"}`
	rec := postInstall(srv, body)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503; body = %s", rec.Code, rec.Body.String())
	}
}

func TestInstallConnector_ConfirmedFingerprintTrustPersistsOnInstallFailure(t *testing.T) {
	// #487 Q4 resolution: trust persists even if the install pipeline
	// later fails. Re-running install after the operator fixes the
	// problem (e.g., a wrong --hash) must not require re-confirming the
	// publisher. We trigger a downstream failure with a deliberately
	// wrong expected_hash and confirm the keyring still carries the
	// new key.
	ref, _ := cstore.ParseRef("github://aileron/slack@1.2.0")
	srv, _, fingerprint, keyringPath := installTestServerWithHub(t, ref)

	body := `{"fqn":"github://aileron/slack","version":"1.2.0","confirmed_fingerprint":"` + fingerprint + `","expected_hash":"sha256:0000000000000000000000000000000000000000000000000000000000000000"}`
	rec := postInstall(srv, body)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (hash_mismatch); body = %s", rec.Code, rec.Body.String())
	}

	kr, err := cstore.LoadKeyring(keyringPath)
	if err != nil {
		t.Fatalf("LoadKeyring: %v", err)
	}
	if got := kr.Keys(ref.FQN.String()); len(got) != 1 {
		t.Errorf("keyring keys for %s after install failure: got %d, want 1 (trust must persist)", ref.FQN, len(got))
	}
}

func TestInstallHTTPStatus_MapsEveryDocumentedClass(t *testing.T) {
	// The handler maps cstore failure classes to HTTP statuses such that:
	//  - bad input → 400
	//  - artifact-rejected (request well-formed, but the artifact fails
	//    a check) → 422
	//  - unwritable store / unknown class → 500
	cases := []struct {
		class cstore.FailureClass
		want  int
	}{
		{cstore.ClassUnknownScheme, http.StatusBadRequest},
		{cstore.ClassParseError, http.StatusBadRequest},
		{cstore.ClassValidationError, http.StatusBadRequest},
		{cstore.ClassFetchFailed, http.StatusUnprocessableEntity},
		{cstore.ClassMalformedTarball, http.StatusUnprocessableEntity},
		{cstore.ClassSignatureFailure, http.StatusUnprocessableEntity},
		{cstore.ClassHashMismatch, http.StatusUnprocessableEntity},
		{cstore.ClassFQNMismatch, http.StatusUnprocessableEntity},
		{cstore.ClassStoreUnwritable, http.StatusInternalServerError},
		{cstore.FailureClass("brand_new_class"), http.StatusInternalServerError},
	}
	for _, tc := range cases {
		t.Run(string(tc.class), func(t *testing.T) {
			if got := installHTTPStatus(tc.class); got != tc.want {
				t.Errorf("installHTTPStatus(%q) = %d, want %d", tc.class, got, tc.want)
			}
		})
	}
}
