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

	srv := &apiServer{
		log: slog.Default(),
		installer: &cstore.Installer{
			Resolver: resolver,
			Fetcher:  &fakeFetcher{bytesAt: map[string][]byte{url: buf.Bytes()}},
			Verifier: keyring,
			Store:    store,
		},
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
