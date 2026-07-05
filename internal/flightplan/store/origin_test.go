package store

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteReadOrigin_RoundTrip(t *testing.T) {
	s := New(t.TempDir())
	if err := s.WriteFrozen("demo", sampleVersion("v1abc")); err != nil {
		t.Fatalf("WriteFrozen: %v", err)
	}
	want := Origin{Registry: "ghcr.io/acme/plan", VersionTag: "v1abc"}
	if err := s.WriteOrigin("demo", "v1abc", want); err != nil {
		t.Fatalf("WriteOrigin: %v", err)
	}
	got, ok, err := s.ReadOrigin("demo", "v1abc")
	if err != nil {
		t.Fatalf("ReadOrigin: %v", err)
	}
	if !ok {
		t.Fatal("ReadOrigin ok = false, want a recorded origin")
	}
	if got != want {
		t.Errorf("origin = %+v, want %+v", got, want)
	}
}

func TestReadOrigin_AbsentSidecarIsNotOK(t *testing.T) {
	// A locally-frozen version has no sidecar: ReadOrigin returns ok=false with
	// no error, the signal launch uses to stay on the local-tag boot path.
	s := New(t.TempDir())
	if err := s.WriteFrozen("demo", sampleVersion("v1abc")); err != nil {
		t.Fatalf("WriteFrozen: %v", err)
	}
	_, ok, err := s.ReadOrigin("demo", "v1abc")
	if err != nil {
		t.Fatalf("ReadOrigin: %v", err)
	}
	if ok {
		t.Fatal("ReadOrigin ok = true for a version with no sidecar; want false")
	}
}

func TestWriteOrigin_IdenticalRewriteIsNoOp(t *testing.T) {
	s := New(t.TempDir())
	if err := s.WriteFrozen("demo", sampleVersion("v1abc")); err != nil {
		t.Fatalf("WriteFrozen: %v", err)
	}
	o := Origin{Registry: "ghcr.io/acme/plan", VersionTag: "v1abc"}
	if err := s.WriteOrigin("demo", "v1abc", o); err != nil {
		t.Fatalf("WriteOrigin (first): %v", err)
	}
	path := filepath.Join(s.FrozenDir("demo", "v1abc"), originFile)
	fi1, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat origin: %v", err)
	}
	if err := s.WriteOrigin("demo", "v1abc", o); err != nil {
		t.Fatalf("WriteOrigin (identical rewrite): %v", err)
	}
	fi2, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat origin after rewrite: %v", err)
	}
	if !fi1.ModTime().Equal(fi2.ModTime()) {
		t.Errorf("identical rewrite changed mtime %v -> %v; want a no-op", fi1.ModTime(), fi2.ModTime())
	}
}

func TestWriteOrigin_DifferingRewriteUpdatesSource(t *testing.T) {
	// The origin is non-signed provenance, not immutable content: re-installing
	// the same version from a different registry updates where launch pulls from.
	s := New(t.TempDir())
	if err := s.WriteFrozen("demo", sampleVersion("v1abc")); err != nil {
		t.Fatalf("WriteFrozen: %v", err)
	}
	if err := s.WriteOrigin("demo", "v1abc", Origin{Registry: "ghcr.io/acme/plan", VersionTag: "v1abc"}); err != nil {
		t.Fatalf("WriteOrigin (first): %v", err)
	}
	updated := Origin{Registry: "registry.internal/mirror/plan", VersionTag: "v1abc"}
	if err := s.WriteOrigin("demo", "v1abc", updated); err != nil {
		t.Fatalf("WriteOrigin (update): %v", err)
	}
	got, ok, err := s.ReadOrigin("demo", "v1abc")
	if err != nil {
		t.Fatalf("ReadOrigin: %v", err)
	}
	if !ok || got != updated {
		t.Errorf("origin = %+v ok=%v, want %+v", got, ok, updated)
	}
}

func TestWriteOrigin_MissingVersionErrors(t *testing.T) {
	s := New(t.TempDir())
	err := s.WriteOrigin("demo", "v1abc", Origin{Registry: "ghcr.io/acme/plan", VersionTag: "v1abc"})
	if err == nil {
		t.Fatal("WriteOrigin on a missing frozen version must error")
	}
}

func TestReadOrigin_MalformedSidecarErrors(t *testing.T) {
	// A malformed sidecar is a hard error, not a silent local-path fallback: a
	// version that recorded an origin but cannot report it must not be mistaken
	// for locally frozen.
	s := New(t.TempDir())
	if err := s.WriteFrozen("demo", sampleVersion("v1abc")); err != nil {
		t.Fatalf("WriteFrozen: %v", err)
	}
	path := filepath.Join(s.FrozenDir("demo", "v1abc"), originFile)
	if err := os.WriteFile(path, []byte("{ not json"), 0o644); err != nil {
		t.Fatalf("write malformed sidecar: %v", err)
	}
	_, ok, err := s.ReadOrigin("demo", "v1abc")
	if err == nil {
		t.Fatal("ReadOrigin on a malformed sidecar must error")
	}
	if ok {
		t.Error("ReadOrigin ok = true on a malformed sidecar; want false")
	}
}

func TestWriteOrigin_InvalidVersionIDRejected(t *testing.T) {
	s := New(t.TempDir())
	if err := s.WriteOrigin("demo", "../escape", Origin{Registry: "r", VersionTag: "t"}); err == nil {
		t.Fatal("WriteOrigin must reject a version id that is not a single path segment")
	}
}
