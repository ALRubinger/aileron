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
	"strings"
	"testing"
	"time"

	api "github.com/ALRubinger/aileron/internal/api/gen"
	"github.com/ALRubinger/aileron/internal/audit"
	"github.com/ALRubinger/aileron/internal/cstore"
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
