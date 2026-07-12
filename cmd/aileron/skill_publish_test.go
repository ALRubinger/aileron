package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"reflect"
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
	if len(got.Lock.ResolvedImages) != 1 || !reflect.DeepEqual(got.Lock.ResolvedImages[0], pin) {
		t.Errorf("lock pin = %+v, want %+v", got.Lock.ResolvedImages, pin)
	}
}

// TestRunSkillPublishQuietPlumbed proves the --quiet flag threads through into
// publish.Options.Quiet, and that its absence leaves Quiet false.
func TestRunSkillPublishQuietPlumbed(t *testing.T) {
	dir := t.TempDir()
	skillStoreDir = dir
	t.Cleanup(func() { skillStoreDir = "" })
	pin := freeze.ImagePin{Ref: "docker.io/library/python", Digest: "sha256:abc"}
	writeFrozenFixture(t, dir, "demo", "v1", freeze.Lockfile{ResolvedImages: []freeze.ImagePin{pin}})

	var got publish.Options
	withStubPublish(t, func(_ context.Context, o publish.Options) (publish.Result, error) {
		got = o
		return publish.Result{}, nil
	})

	var out, errBuf bytes.Buffer
	if code := runSkillPublish([]string{"demo", "--registry", "ghcr.io/acme/demo", "--quiet"}, &out, &errBuf); code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr=%s)", code, errBuf.String())
	}
	if !got.Quiet {
		t.Errorf("Quiet = false, want true when --quiet is passed")
	}

	got = publish.Options{}
	out.Reset()
	errBuf.Reset()
	if code := runSkillPublish([]string{"demo", "--registry", "ghcr.io/acme/demo"}, &out, &errBuf); code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr=%s)", code, errBuf.String())
	}
	if got.Quiet {
		t.Errorf("Quiet = true, want false when --quiet is omitted")
	}
}

// TestRunSkillPublishRecordsOrigin is the #2127 regression: a successful publish
// MUST write the install-origin sidecar so a later `skill launch` of the
// just-published version takes the registry pull-and-verify boot path instead of
// the mutable, shared local composed tag (which a later freeze of any plan with
// the same environment repoints, stranding this plan's signed lock on a stale
// digest and tripping the #1863 config-content-digest guard). Before the fix
// publish never wrote the sidecar, so ReadOrigin returned ok=false and launch
// stayed on the local-tag dead-end. Registry+VersionTag must be exactly what
// launch feeds pull.PullImage (origin.Registry, ComposedImageTag(origin.VersionTag)).
func TestRunSkillPublishRecordsOrigin(t *testing.T) {
	dir := t.TempDir()
	skillStoreDir = dir
	t.Cleanup(func() { skillStoreDir = "" })
	pin := freeze.ImagePin{Ref: "docker.io/library/python", Digest: "sha256:abc"}
	writeFrozenFixture(t, dir, "demo", "v1", freeze.Lockfile{ResolvedImages: []freeze.ImagePin{pin}})

	withStubPublish(t, func(_ context.Context, o publish.Options) (publish.Result, error) {
		return publish.Result{ArtifactRef: o.Registry + ":" + o.VersionID}, nil
	})

	var out, errBuf bytes.Buffer
	if code := runSkillPublish([]string{"demo", "--registry", "ghcr.io/acme/demo"}, &out, &errBuf); code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr=%s)", code, errBuf.String())
	}

	s := store.New(dir)
	origin, ok, err := s.ReadOrigin("demo", "v1")
	if err != nil {
		t.Fatalf("ReadOrigin: %v", err)
	}
	if !ok {
		t.Fatal("origin sidecar absent after publish; launch would stay on the mutable local-tag boot path (#2127)")
	}
	if origin.Registry != "ghcr.io/acme/demo" {
		t.Errorf("origin.Registry = %q, want ghcr.io/acme/demo (the --registry value launch pulls from)", origin.Registry)
	}
	if origin.VersionTag != "v1" {
		t.Errorf("origin.VersionTag = %q, want v1 (the resolved version id launch derives ComposedImageTag from)", origin.VersionTag)
	}
}

// TestRunSkillPublishPrintsSelfTrustCommand is the #2136 item-4 regression: a
// successful publish of a plan that declares a publisher must print the exact
// working `keyring trust --plan <name>` command that unblocks self-launch, so
// the author is not stranded by the no-op `keyring trust <publisher>` path. It
// must NOT silently auto-register the key (that would undermine the opt-in trust
// model); printing the command satisfies the ask without changing trust posture.
func TestRunSkillPublishPrintsSelfTrustCommand(t *testing.T) {
	dir := t.TempDir()
	skillStoreDir = dir
	t.Cleanup(func() { skillStoreDir = "" })
	pin := freeze.ImagePin{Ref: "docker.io/library/python", Digest: "sha256:abc"}
	writeFrozenFixture(t, dir, "demo", "v1", freeze.Lockfile{
		ResolvedImages: []freeze.ImagePin{pin},
		Publisher:      "github://acme/plans",
	})
	withStubPublish(t, func(_ context.Context, o publish.Options) (publish.Result, error) {
		return publish.Result{ArtifactRef: o.Registry + ":" + o.VersionID}, nil
	})

	var out, errBuf bytes.Buffer
	if code := runSkillPublish([]string{"demo", "--registry", "ghcr.io/acme/demo"}, &out, &errBuf); code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr=%s)", code, errBuf.String())
	}
	if !strings.Contains(out.String(), "aileron keyring trust --plan demo") {
		t.Errorf("stdout = %q, want the self-trust command `aileron keyring trust --plan demo`", out.String())
	}
}

// TestRunSkillPublishNoPublisherOmitsSelfTrustCommand proves a plan with no
// declared publisher has no launch-time trust gate, so publish does not print
// the (inapplicable) self-trust command.
func TestRunSkillPublishNoPublisherOmitsSelfTrustCommand(t *testing.T) {
	dir := t.TempDir()
	skillStoreDir = dir
	t.Cleanup(func() { skillStoreDir = "" })
	pin := freeze.ImagePin{Ref: "docker.io/library/python", Digest: "sha256:abc"}
	writeFrozenFixture(t, dir, "demo", "v1", freeze.Lockfile{ResolvedImages: []freeze.ImagePin{pin}})
	withStubPublish(t, func(_ context.Context, o publish.Options) (publish.Result, error) {
		return publish.Result{}, nil
	})

	var out, errBuf bytes.Buffer
	if code := runSkillPublish([]string{"demo", "--registry", "ghcr.io/acme/demo"}, &out, &errBuf); code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr=%s)", code, errBuf.String())
	}
	if strings.Contains(out.String(), "keyring trust --plan") {
		t.Errorf("stdout = %q, want no self-trust command for a publisher-less plan", out.String())
	}
}

// TestRunSkillPublishOriginRecordsResolvedNewest proves the sidecar records the
// RESOLVED newest version id (not a literal --version), so launching the newest
// after an unpinned publish takes the registry path against the version that was
// actually pushed.
func TestRunSkillPublishOriginRecordsResolvedNewest(t *testing.T) {
	dir := t.TempDir()
	skillStoreDir = dir
	t.Cleanup(func() { skillStoreDir = "" })
	pin := freeze.ImagePin{Ref: "r", Digest: "sha256:abc"}
	writeFrozenFixture(t, dir, "demo", "v1", freeze.Lockfile{ResolvedImages: []freeze.ImagePin{pin}})
	writeFrozenFixture(t, dir, "demo", "v2", freeze.Lockfile{ResolvedImages: []freeze.ImagePin{pin}})

	var published string
	withStubPublish(t, func(_ context.Context, o publish.Options) (publish.Result, error) {
		published = o.VersionID
		return publish.Result{}, nil
	})

	var out, errBuf bytes.Buffer
	if code := runSkillPublish([]string{"demo", "--registry", "ghcr.io/acme/demo"}, &out, &errBuf); code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr=%s)", code, errBuf.String())
	}
	if published != "v2" {
		t.Fatalf("published = %q, want v2 (the newest frozen version)", published)
	}
	origin, ok, err := store.New(dir).ReadOrigin("demo", published)
	if err != nil {
		t.Fatalf("ReadOrigin: %v", err)
	}
	if !ok {
		t.Fatalf("origin sidecar absent for the published version %q", published)
	}
	if origin.VersionTag != published {
		t.Errorf("origin.VersionTag = %q, want the resolved published version %q", origin.VersionTag, published)
	}
}

// TestRunSkillPublishOriginWriteFailureFailsPublish proves a sidecar write
// failure fails the publish (mirroring the OCI-install path), rather than
// reporting success for a version whose launch would silently stay on the
// local-tag dead-end. The frozen version dir is removed after publishRun so
// WriteOrigin's missing-version-dir guard fires.
func TestRunSkillPublishOriginWriteFailureFailsPublish(t *testing.T) {
	dir := t.TempDir()
	skillStoreDir = dir
	t.Cleanup(func() { skillStoreDir = "" })
	pin := freeze.ImagePin{Ref: "r", Digest: "sha256:abc"}
	writeFrozenFixture(t, dir, "demo", "v1", freeze.Lockfile{ResolvedImages: []freeze.ImagePin{pin}})

	s := store.New(dir)
	withStubPublish(t, func(_ context.Context, _ publish.Options) (publish.Result, error) {
		// Remove the version directory so the subsequent WriteOrigin fails its
		// "cannot write origin for missing frozen version" guard.
		if err := os.RemoveAll(s.FrozenDir("demo", "v1")); err != nil {
			t.Fatalf("remove frozen dir: %v", err)
		}
		return publish.Result{}, nil
	})

	var out, errBuf bytes.Buffer
	if code := runSkillPublish([]string{"demo", "--registry", "ghcr.io/acme/demo"}, &out, &errBuf); code != 1 {
		t.Fatalf("exit = %d, want 1 (a sidecar write failure must fail the publish)", code)
	}
	if !strings.Contains(errBuf.String(), "record publish origin") {
		t.Errorf("stderr = %q, want a 'record publish origin' failure", errBuf.String())
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
	writeFrozenFixture(t, dir, "demo", "v1", freeze.Lockfile{ResolvedImages: []freeze.ImagePin{{Ref: "r", ConfigDigests: []freeze.PlatformDigest{{OS: "linux", Arch: "amd64", Digest: "sha256:abc"}}, LocalTag: "t"}}})
	withStubPublish(t, func(context.Context, publish.Options) (publish.Result, error) {
		return publish.Result{}, publish.ErrConfigContentDigestMismatch
	})
	var out, errBuf bytes.Buffer
	code := runSkillPublish([]string{"demo", "--registry", "ghcr.io/acme/demo"}, &out, &errBuf)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errBuf.String(), "config content digest") {
		t.Errorf("stderr = %q, want config-content-digest message", errBuf.String())
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
