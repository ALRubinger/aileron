package toolchain

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ALRubinger/aileron/internal/cstore"
	"github.com/ALRubinger/aileron/internal/sandbox/container"
	"github.com/ALRubinger/aileron/internal/sandbox/nodedist"
)

// pinnedVersion mirrors the committed tools.lock.json pin via its public
// accessor, so the test fetches the version the checksum guard expects.
var pinnedVersion = container.PinnedNodeVersion()

// fakeFetcher is an in-memory cstore.Fetcher serving canned bodies keyed by URL.
// Any URL not registered returns an error, so a test asserts exactly which URLs
// are fetched.
type fakeFetcher struct {
	bodies map[string][]byte
}

func (f *fakeFetcher) Fetch(_ context.Context, url string) (io.ReadCloser, error) {
	body, ok := f.bodies[url]
	if !ok {
		return nil, errors.New("fakeFetcher: no body for " + url)
	}
	return io.NopCloser(bytes.NewReader(body)), nil
}

// fakeCLIInstaller records its inputs and returns a canned entrypoint.
type fakeCLIInstaller struct {
	entrypoint string
	fromCache  bool
	err        error
	calls      int
	gotNode    string
	gotVersion string
}

func (f *fakeCLIInstaller) Install(_ context.Context, nodeBinary, cliVersion, _ string) (string, bool, error) {
	f.calls++
	f.gotNode = nodeBinary
	f.gotVersion = cliVersion
	if f.err != nil {
		return "", false, f.err
	}
	return f.entrypoint, f.fromCache, nil
}

// stagedNode pre-populates the toolchain's Node cache with a synthetic Node tree
// committed at hash, so Fetcher.Fetch hits the cache short-circuit (no archive
// download) and returns a NodeDist whose SHA256 is hash. The committed tree has
// a bin/node file so NodeBinary points at a real path. Returns the node-cache
// root the test passes as Options.CacheRoot's <root>/node.
func stagedNode(t *testing.T, cacheRoot, hash string) {
	t.Helper()
	nodeRoot := filepath.Join(cacheRoot, "node")
	_, _, err := cstore.CommitDir(nodeRoot, hash, func(dir string) error {
		bin := filepath.Join(dir, "bin")
		if err := os.MkdirAll(bin, 0o755); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(bin, "node"), []byte("#!/bin/sh\n"), 0o755)
	})
	if err != nil {
		t.Fatalf("stage node cache: %v", err)
	}
}

func TestProvisionComposesFetchVerifyAndCLIInstall(t *testing.T) {
	cacheRoot := t.TempDir()
	target := nodedist.Target{GOOS: "linux", GOARCH: "amd64"}

	// Use the real pinned checksum for linux-x64 as the cache hash so
	// VerifyNodeChecksum passes at the boundary.
	hash := pinnedChecksumForLinuxX64(t)
	stagedNode(t, cacheRoot, hash)
	body, sig, keyringFn, _ := signedChecksumsWithHash(t, hash, target)

	urls, err := nodedist.ResolveURLs(pinnedVersion, target)
	if err != nil {
		t.Fatalf("ResolveURLs: %v", err)
	}
	fetcher := &fakeFetcher{bodies: map[string][]byte{
		urls.Checksums: body,
		urls.Signature: sig,
		// Archive URL deliberately omitted: the cache hit must short-circuit
		// before any archive download.
	}}
	installer := &fakeCLIInstaller{entrypoint: "/cache/devcontainer-cli/x/devcontainer.js"}

	managed, err := Provision(context.Background(), Options{
		CacheRoot:    cacheRoot,
		HTTP:         fetcher,
		Keyring:      keyringFn,
		GOOS:         "linux",
		GOARCH:       "amd64",
		CLIInstaller: installer,
	})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if installer.calls != 1 {
		t.Fatalf("CLI installer calls = %d, want 1", installer.calls)
	}
	if installer.gotVersion != container.DevcontainerCLIVersion {
		t.Fatalf("CLI installer version = %q, want pinned %q", installer.gotVersion, container.DevcontainerCLIVersion)
	}
	wantNode := filepath.Join(cacheRoot, "node", "sha256", hash, "bin", "node")
	if managed.NodeBinary != wantNode {
		t.Fatalf("NodeBinary = %q, want %q", managed.NodeBinary, wantNode)
	}
	if installer.gotNode != wantNode {
		t.Fatalf("installer node = %q, want %q", installer.gotNode, wantNode)
	}
	if managed.CLIEntrypoint != installer.entrypoint {
		t.Fatalf("CLIEntrypoint = %q, want %q", managed.CLIEntrypoint, installer.entrypoint)
	}
}

func TestProvisionWarmCacheIsIdempotent(t *testing.T) {
	cacheRoot := t.TempDir()
	target := nodedist.Target{GOOS: "linux", GOARCH: "amd64"}
	hash := pinnedChecksumForLinuxX64(t)
	stagedNode(t, cacheRoot, hash)
	body, sig, keyringFn, _ := signedChecksumsWithHash(t, hash, target)
	urls, _ := nodedist.ResolveURLs(pinnedVersion, target)
	fetcher := &fakeFetcher{bodies: map[string][]byte{urls.Checksums: body, urls.Signature: sig}}
	installer := &fakeCLIInstaller{entrypoint: "/c/devcontainer.js", fromCache: true}

	for i := 0; i < 2; i++ {
		if _, err := Provision(context.Background(), Options{
			CacheRoot: cacheRoot, HTTP: fetcher, Keyring: keyringFn,
			GOOS: "linux", GOARCH: "amd64", CLIInstaller: installer,
		}); err != nil {
			t.Fatalf("Provision #%d: %v", i, err)
		}
	}
	// The Node fetch is cache-served both times (no archive URL registered); the
	// CLI installer is invoked each call but reports fromCache, modelling the warm
	// path. The key property is that two calls succeed without re-download.
}

func TestProvisionChecksumMismatchAborts(t *testing.T) {
	cacheRoot := t.TempDir()
	target := nodedist.Target{GOOS: "linux", GOARCH: "amd64"}
	// Stage a cache entry under a hash that is NOT the pinned checksum, and sign
	// checksums for that same wrong hash so nodedist returns it; VerifyNodeChecksum
	// must then reject it at the boundary.
	wrongHash := strings.Repeat("a", 64)
	stagedNode(t, cacheRoot, wrongHash)
	body, sig, keyringFn, _ := signedChecksumsWithHash(t, wrongHash, target)
	urls, _ := nodedist.ResolveURLs(pinnedVersion, target)
	fetcher := &fakeFetcher{bodies: map[string][]byte{urls.Checksums: body, urls.Signature: sig}}
	installer := &fakeCLIInstaller{entrypoint: "/c/devcontainer.js"}

	_, err := Provision(context.Background(), Options{
		CacheRoot: cacheRoot, HTTP: fetcher, Keyring: keyringFn,
		GOOS: "linux", GOARCH: "amd64", CLIInstaller: installer,
	})
	if err == nil {
		t.Fatal("Provision: want checksum-mismatch error, got nil")
	}
	if installer.calls != 0 {
		t.Fatalf("CLI installer ran (%d calls) despite a checksum mismatch; want 0", installer.calls)
	}
}

func TestProvisionUnsupportedPlatformFailsFast(t *testing.T) {
	cacheRoot := t.TempDir()
	_, err := Provision(context.Background(), Options{
		CacheRoot:    cacheRoot,
		HTTP:         &fakeFetcher{bodies: map[string][]byte{}},
		Keyring:      func() (keyringList, error) { return nil, nil },
		GOOS:         "plan9",
		GOARCH:       "mips",
		CLIInstaller: &fakeCLIInstaller{entrypoint: "/x"},
	})
	if err == nil {
		t.Fatal("Provision: want unsupported-platform error, got nil")
	}
}

func TestProvisionEmptyEntrypointErrors(t *testing.T) {
	cacheRoot := t.TempDir()
	target := nodedist.Target{GOOS: "linux", GOARCH: "amd64"}
	hash := pinnedChecksumForLinuxX64(t)
	stagedNode(t, cacheRoot, hash)
	body, sig, keyringFn, _ := signedChecksumsWithHash(t, hash, target)
	urls, _ := nodedist.ResolveURLs(pinnedVersion, target)
	// Installer returns no error but an empty entrypoint: Provision must reject it
	// rather than handing the build an empty CLI path.
	_, err := Provision(context.Background(), Options{
		CacheRoot:    cacheRoot,
		HTTP:         &fakeFetcher{bodies: map[string][]byte{urls.Checksums: body, urls.Signature: sig}},
		Keyring:      keyringFn,
		GOOS:         "linux",
		GOARCH:       "amd64",
		CLIInstaller: &fakeCLIInstaller{entrypoint: ""},
	})
	if err == nil {
		t.Fatal("Provision: want error for empty CLI entrypoint, got nil")
	}
}

func TestResolveAppliesDefaults(t *testing.T) {
	r, err := Options{}.resolve()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if r.CacheRoot == "" {
		t.Fatal("CacheRoot default empty")
	}
	if r.HTTP == nil {
		t.Fatal("HTTP default nil")
	}
	if r.keyring == nil {
		t.Fatal("keyring default nil")
	}
	if r.NodeVersion != container.PinnedNodeVersion() {
		t.Fatalf("NodeVersion default = %q, want pinned %q", r.NodeVersion, container.PinnedNodeVersion())
	}
	if r.CLIVersion != container.DevcontainerCLIVersion {
		t.Fatalf("CLIVersion default = %q, want pinned", r.CLIVersion)
	}
	if r.GOOS == "" || r.GOARCH == "" {
		t.Fatalf("platform defaults empty: %q/%q", r.GOOS, r.GOARCH)
	}
	if r.CLIInstaller == nil {
		t.Fatal("CLIInstaller default nil")
	}
}

func TestDefaultKeyringIsEmbeddedNodeKeyring(t *testing.T) {
	kr, err := defaultKeyring()
	if err != nil {
		t.Fatalf("defaultKeyring: %v", err)
	}
	if len(kr) == 0 {
		t.Fatal("defaultKeyring returned an empty keyring")
	}
}

func TestProvisionerAdapterDelegates(t *testing.T) {
	cacheRoot := t.TempDir()
	target := nodedist.Target{GOOS: "linux", GOARCH: "amd64"}
	hash := pinnedChecksumForLinuxX64(t)
	stagedNode(t, cacheRoot, hash)
	body, sig, keyringFn, _ := signedChecksumsWithHash(t, hash, target)
	urls, _ := nodedist.ResolveURLs(pinnedVersion, target)
	p := Provisioner{Options: Options{
		CacheRoot:    cacheRoot,
		HTTP:         &fakeFetcher{bodies: map[string][]byte{urls.Checksums: body, urls.Signature: sig}},
		Keyring:      keyringFn,
		GOOS:         "linux",
		GOARCH:       "amd64",
		CLIInstaller: &fakeCLIInstaller{entrypoint: "/c/devcontainer.js"},
	}}
	managed, err := p.Provision(context.Background())
	if err != nil {
		t.Fatalf("Provisioner.Provision: %v", err)
	}
	if managed.CLIEntrypoint != "/c/devcontainer.js" {
		t.Fatalf("CLIEntrypoint = %q", managed.CLIEntrypoint)
	}
}

func TestProvisionCLIInstallerErrorPropagates(t *testing.T) {
	cacheRoot := t.TempDir()
	target := nodedist.Target{GOOS: "linux", GOARCH: "amd64"}
	hash := pinnedChecksumForLinuxX64(t)
	stagedNode(t, cacheRoot, hash)
	body, sig, keyringFn, _ := signedChecksumsWithHash(t, hash, target)
	urls, _ := nodedist.ResolveURLs(pinnedVersion, target)
	fetcher := &fakeFetcher{bodies: map[string][]byte{urls.Checksums: body, urls.Signature: sig}}
	sentinel := errors.New("npm boom")
	installer := &fakeCLIInstaller{err: sentinel}

	_, err := Provision(context.Background(), Options{
		CacheRoot: cacheRoot, HTTP: fetcher, Keyring: keyringFn,
		GOOS: "linux", GOARCH: "amd64", CLIInstaller: installer,
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("Provision err = %v, want wrap of %v", err, sentinel)
	}
}
