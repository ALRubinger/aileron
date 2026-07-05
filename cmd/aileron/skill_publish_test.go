package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/flightplan/freeze"
	"github.com/ALRubinger/aileron/internal/flightplan/publish"
	"github.com/ALRubinger/aileron/internal/flightplan/store"
)

func writeFrozenFixture(t *testing.T, dir, name, id string, lock freeze.Lockfile) {
	t.Helper()
	lb, err := freeze.MarshalLockfile(lock)
	if err != nil {
		t.Fatal(err)
	}
	s := store.New(dir)
	if err := s.WriteFrozen(name, store.FrozenVersion{
		ID:        id,
		SkillMD:   []byte("# frozen\n"),
		Lockfile:  lb,
		Signature: []byte("sig"),
		PublicKey: []byte("pub"),
	}); err != nil {
		t.Fatalf("write frozen fixture: %v", err)
	}
}

func withStubPublish(t *testing.T, fn func(context.Context, publish.Options) (publish.Result, error)) {
	t.Helper()
	orig := publishRun
	publishRun = fn
	t.Cleanup(func() { publishRun = orig })
}

func TestRunSkillPublishRequiresRegistry(t *testing.T) {
	var out, errBuf bytes.Buffer
	code := runSkillPublish([]string{"demo"}, &out, &errBuf)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errBuf.String(), "--registry") {
		t.Errorf("stderr = %q, want mention of --registry", errBuf.String())
	}
}

func TestRunSkillPublishHappyPath(t *testing.T) {
	dir := t.TempDir()
	skillStoreDir = dir
	t.Cleanup(func() { skillStoreDir = "" })
	pin := freeze.ImagePin{Ref: "docker.io/library/python", Digest: "sha256:abc"}
	writeFrozenFixture(t, dir, "demo", "v1", freeze.Lockfile{ResolvedImages: []freeze.ImagePin{pin}})

	var got publish.Options
	withStubPublish(t, func(_ context.Context, o publish.Options) (publish.Result, error) {
		got = o
		return publish.Result{ArtifactRef: o.Registry + ":" + o.VersionID}, nil
	})

	var out, errBuf bytes.Buffer
	code := runSkillPublish([]string{"demo", "--registry", "ghcr.io/acme/demo"}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr=%s)", code, errBuf.String())
	}
	if got.Name != "demo" || got.VersionID != "v1" || got.Registry != "ghcr.io/acme/demo" {
		t.Errorf("options = %+v, want name=demo version=v1 registry=ghcr.io/acme/demo", got)
	}
	if len(got.Lock.ResolvedImages) != 1 || got.Lock.ResolvedImages[0] != pin {
		t.Errorf("lock pin = %+v, want %+v", got.Lock.ResolvedImages, pin)
	}
}

func TestRunSkillPublishUnfrozen(t *testing.T) {
	dir := t.TempDir()
	skillStoreDir = dir
	t.Cleanup(func() { skillStoreDir = "" })
	var out, errBuf bytes.Buffer
	code := runSkillPublish([]string{"missing", "--registry", "ghcr.io/acme/x"}, &out, &errBuf)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errBuf.String(), "no frozen versions") {
		t.Errorf("stderr = %q, want 'no frozen versions'", errBuf.String())
	}
}

func TestRunSkillPublishMismatchMapped(t *testing.T) {
	dir := t.TempDir()
	skillStoreDir = dir
	t.Cleanup(func() { skillStoreDir = "" })
	writeFrozenFixture(t, dir, "demo", "v1", freeze.Lockfile{ResolvedImages: []freeze.ImagePin{{Ref: "r", Digest: "sha256:abc", LocalTag: "t"}}})
	withStubPublish(t, func(context.Context, publish.Options) (publish.Result, error) {
		return publish.Result{}, publish.ErrConfigDigestMismatch
	})
	var out, errBuf bytes.Buffer
	code := runSkillPublish([]string{"demo", "--registry", "ghcr.io/acme/demo"}, &out, &errBuf)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errBuf.String(), "config digest") {
		t.Errorf("stderr = %q, want config-digest message", errBuf.String())
	}
}

func TestRunSkillPublishNoImageMapped(t *testing.T) {
	dir := t.TempDir()
	skillStoreDir = dir
	t.Cleanup(func() { skillStoreDir = "" })
	writeFrozenFixture(t, dir, "demo", "v1", freeze.Lockfile{}) // instruction-only
	withStubPublish(t, func(context.Context, publish.Options) (publish.Result, error) {
		return publish.Result{}, publish.ErrNoImage
	})
	var out, errBuf bytes.Buffer
	if code := runSkillPublish([]string{"demo", "--registry", "ghcr.io/acme/demo"}, &out, &errBuf); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errBuf.String(), "instruction-only") {
		t.Errorf("stderr = %q, want instruction-only note", errBuf.String())
	}
}

func TestRunSkillPublishGenericErrorMapped(t *testing.T) {
	dir := t.TempDir()
	skillStoreDir = dir
	t.Cleanup(func() { skillStoreDir = "" })
	writeFrozenFixture(t, dir, "demo", "v1", freeze.Lockfile{ResolvedImages: []freeze.ImagePin{{Ref: "r", Digest: "sha256:abc"}}})
	withStubPublish(t, func(context.Context, publish.Options) (publish.Result, error) {
		return publish.Result{}, errors.New("registry unreachable")
	})
	var out, errBuf bytes.Buffer
	if code := runSkillPublish([]string{"demo", "--registry", "ghcr.io/acme/demo"}, &out, &errBuf); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errBuf.String(), "publish \"demo\"") || !strings.Contains(errBuf.String(), "registry unreachable") {
		t.Errorf("stderr = %q, want wrapped publish error", errBuf.String())
	}
}

func TestRunSkillPublishNewestBanner(t *testing.T) {
	dir := t.TempDir()
	skillStoreDir = dir
	t.Cleanup(func() { skillStoreDir = "" })
	pin := freeze.ImagePin{Ref: "r", Digest: "sha256:abc"}
	writeFrozenFixture(t, dir, "demo", "v1", freeze.Lockfile{ResolvedImages: []freeze.ImagePin{pin}})
	writeFrozenFixture(t, dir, "demo", "v2", freeze.Lockfile{ResolvedImages: []freeze.ImagePin{pin}})
	withStubPublish(t, func(context.Context, publish.Options) (publish.Result, error) {
		return publish.Result{}, nil
	})
	var out, errBuf bytes.Buffer
	if code := runSkillPublish([]string{"demo", "--registry", "ghcr.io/acme/demo"}, &out, &errBuf); code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr=%s)", code, errBuf.String())
	}
	if !strings.Contains(out.String(), "publishing") || !strings.Contains(out.String(), "newest of 2") {
		t.Errorf("stdout = %q, want a 'publishing ... newest of 2' banner", out.String())
	}
}

func TestRunSkillPublishCorruptLock(t *testing.T) {
	dir := t.TempDir()
	skillStoreDir = dir
	t.Cleanup(func() { skillStoreDir = "" })
	// Write a frozen version whose lock bytes are not valid YAML.
	s := store.New(dir)
	if err := s.WriteFrozen("demo", store.FrozenVersion{
		ID: "v1", SkillMD: []byte("# f\n"),
		Lockfile: []byte("\tnot: [valid yaml"), Signature: []byte("s"), PublicKey: []byte("p"),
	}); err != nil {
		t.Fatal(err)
	}
	var out, errBuf bytes.Buffer
	if code := runSkillPublish([]string{"demo", "--registry", "ghcr.io/acme/demo"}, &out, &errBuf); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errBuf.String(), "parse lockfile") {
		t.Errorf("stderr = %q, want a parse-lockfile error", errBuf.String())
	}
}

func TestRunSkillPublishBadArgs(t *testing.T) {
	var out, errBuf bytes.Buffer
	// Two positionals is invalid (want exactly one skill name).
	if code := runSkillPublish([]string{"a", "b", "--registry", "ghcr.io/x"}, &out, &errBuf); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
}
