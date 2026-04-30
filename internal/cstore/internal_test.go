package cstore

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestStore_Root_ReturnsConfiguredPath(t *testing.T) {
	root := t.TempDir()
	s := NewStore(root)
	if got := s.Root(); got != root {
		t.Errorf("Root() = %q, want %q", got, root)
	}
}

func TestDefaultRoot_FallbackWhenHomeUnset(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("USERPROFILE", "")
	got := DefaultRoot()
	if got != filepath.Join(".aileron", "store") {
		t.Errorf("DefaultRoot() = %q, want fallback path", got)
	}
}

func TestStore_RebuildIndex_SkipsMalformedManifests(t *testing.T) {
	// ADR-0004: "Manifests that fail to parse are skipped" during rebuild —
	// their entries on disk remain present and addressable by hash, but the
	// index gap is papered over rather than blowing up the rebuild.
	s := NewStore(t.TempDir())

	// Drop a junk manifest under sha256/<garbage>/.
	garbageDir := filepath.Join(s.sha256Root(), "deadbeef")
	if err := os.MkdirAll(garbageDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(garbageDir, tarManifestFile), []byte("not toml"), 0o644); err != nil {
		t.Fatalf("write garbage manifest: %v", err)
	}

	// Drop a real entry alongside.
	manifest := []byte(canonicalManifestFor("github://aileron/slack", "1.2.0"))
	tb := &Tarball{BinaryName: tarBinaryWASM, Binary: []byte("BIN"), Manifest: manifest, Signature: []byte("SIG")}
	if err := s.commit(tb, mustRef(t, "github://aileron/slack@1.2.0"), tb.CanonicalHashHex()); err != nil {
		t.Fatalf("commit real entry: %v", err)
	}

	if err := s.RebuildIndex(); err != nil {
		t.Fatalf("RebuildIndex: %v", err)
	}
	if _, ok := s.Lookup(mustRef(t, "github://aileron/slack@1.2.0")); !ok {
		t.Errorf("Lookup after rebuild missing the valid entry")
	}
	// The garbage entry must NOT appear in the index.
	for k := range s.index {
		if k == "deadbeef" || k == "" {
			t.Errorf("garbage manifest leaked into index: %v", s.index)
		}
	}
}

func TestStore_RebuildIndex_HandlesMissingSha256Root(t *testing.T) {
	s := NewStore(t.TempDir())
	if err := s.RebuildIndex(); err != nil {
		t.Errorf("RebuildIndex on empty store err = %v; want nil", err)
	}
}

func TestHTTPFetcher_FetchesAndPropagatesStatus(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/ok", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("payload"))
	})
	mux.HandleFunc("/notfound", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("/down", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	f := &HTTPFetcher{Client: srv.Client()}

	// Happy path returns the body bytes.
	body, err := f.Fetch(context.Background(), srv.URL+"/ok")
	if err != nil {
		t.Fatalf("Fetch ok: %v", err)
	}
	defer body.Close()
	buf := make([]byte, 8)
	n, _ := body.Read(buf)
	if string(buf[:n]) != "payload" {
		t.Errorf("body = %q", string(buf[:n]))
	}

	// 404 surfaces as ClassFetchFailed, NOT retriable per ADR-0004
	// (tag/release not found is not a transient error).
	_, err = f.Fetch(context.Background(), srv.URL+"/notfound")
	if err == nil {
		t.Fatal("Fetch on 404 succeeded; want failure")
	}
	var aerr *Error
	if !errors.As(err, &aerr) || aerr.Class != ClassFetchFailed {
		t.Fatalf("err class = %v, want ClassFetchFailed", aerr)
	}
	if aerr.Retriable {
		t.Errorf("404 marked Retriable=true; want false (release not found is terminal)")
	}

	// 5xx is retriable per ADR-0010.
	_, err = f.Fetch(context.Background(), srv.URL+"/down")
	if err == nil {
		t.Fatal("Fetch on 500 succeeded; want failure")
	}
	if !errors.As(err, &aerr) || aerr.Class != ClassFetchFailed {
		t.Fatalf("err class = %v, want ClassFetchFailed", aerr)
	}
	if !aerr.Retriable {
		t.Errorf("5xx marked Retriable=false; want true")
	}
}

func TestHTTPFetcher_NetworkErrorIsRetriable(t *testing.T) {
	// A connection refused / closed connection should surface as
	// ClassFetchFailed with retriable=true (ADR-0010 network_error class).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close() // close immediately so the URL no longer accepts connections.

	f := &HTTPFetcher{Client: srv.Client()}
	_, err := f.Fetch(context.Background(), srv.URL+"/anywhere")
	if err == nil {
		t.Fatal("Fetch on closed server succeeded; want failure")
	}
	var aerr *Error
	if !errors.As(err, &aerr) || aerr.Class != ClassFetchFailed {
		t.Fatalf("err class = %v, want ClassFetchFailed", aerr)
	}
	if !aerr.Retriable {
		t.Errorf("network error marked Retriable=false; want true")
	}
}
