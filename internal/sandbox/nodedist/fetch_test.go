package nodedist_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ALRubinger/aileron/internal/sandbox/nodedist"
)

// fakeFetcher is an in-memory cstore.Fetcher: it serves pre-registered URL
// bodies and records every URL requested so tests can assert the cache
// short-circuit skipped the archive download.
type fakeFetcher struct {
	mu      sync.Mutex
	bodies  map[string][]byte
	calls   []string
	failURL string // when set, a request for this URL returns an error
}

func newFakeFetcher() *fakeFetcher {
	return &fakeFetcher{bodies: map[string][]byte{}}
}

func (f *fakeFetcher) Fetch(_ context.Context, url string) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, url)
	if url == f.failURL {
		return nil, fmt.Errorf("simulated fetch failure for %s", url)
	}
	body, ok := f.bodies[url]
	if !ok {
		return nil, fmt.Errorf("no body registered for %s", url)
	}
	return io.NopCloser(strings.NewReader(string(body))), nil
}

func (f *fakeFetcher) requested(url string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c == url {
			return true
		}
	}
	return false
}

// linuxTarget is used across the fetch tests; it produces a tar.gz archive.
var linuxTarget = nodedist.Target{GOOS: "linux", GOARCH: "amd64"}

const fetchVersion = "24.2.0"

// fixture builds a complete, internally-consistent fixture: a synthetic
// tar.gz Node archive, a SHASUMS256.txt listing its real sha256, and a valid
// detached signature, all registered on the returned fetcher. The signed
// archive hash is returned for assertions.
func fixture(t *testing.T, signerKeyring func(t *testing.T) (*signer, keyring)) (*fakeFetcher, keyring, string, []byte) {
	t.Helper()
	distro, err := nodedist.DistroName(fetchVersion, linuxTarget)
	if err != nil {
		t.Fatalf("DistroName: %v", err)
	}
	archive := buildTarGz(t, distro, []archiveEntry{
		{name: distro + "/bin/node", body: []byte("#!/bin/node\n"), mode: 0o755},
		{name: distro + "/README.md", body: []byte("node readme"), mode: 0o644},
	})
	sum := sha256.Sum256(archive)
	archiveHash := hex.EncodeToString(sum[:])

	archiveName, _ := nodedist.ArchiveName(fetchVersion, linuxTarget)
	checksums := fmt.Sprintf("%s  %s\n", archiveHash, archiveName)

	sk, kr := signerKeyring(t)
	sig := sk.armoredDetachedSign(t, []byte(checksums))

	urls, _ := nodedist.ResolveURLs(fetchVersion, linuxTarget)
	ff := newFakeFetcher()
	ff.bodies[urls.Archive] = archive
	ff.bodies[urls.Checksums] = []byte(checksums)
	ff.bodies[urls.Signature] = sig
	return ff, kr, archiveHash, archive
}

func TestFetch_HappyPath(t *testing.T) {
	ff, kr, archiveHash, _ := fixture(t, matchedSigner)
	root := filepath.Join(t.TempDir(), "node")

	f := &nodedist.Fetcher{HTTP: ff, Keyring: kr, Root: root}
	dist, err := f.Fetch(context.Background(), nodedist.Request{Version: fetchVersion, Target: linuxTarget})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	if dist.SHA256 != archiveHash {
		t.Errorf("SHA256 = %q, want %q", dist.SHA256, archiveHash)
	}
	if dist.Version != fetchVersion {
		t.Errorf("Version = %q, want %q", dist.Version, fetchVersion)
	}
	if dist.FromCache {
		t.Errorf("FromCache = true on first fetch")
	}
	// The unpacked tree has a runnable bin/node layout (prefix stripped).
	wantBin := filepath.Join(dist.EntryDir, "bin", "node")
	if dist.NodeBinary != wantBin {
		t.Errorf("NodeBinary = %q, want %q", dist.NodeBinary, wantBin)
	}
	body, err := os.ReadFile(wantBin)
	if err != nil {
		t.Fatalf("read bin/node: %v", err)
	}
	if string(body) != "#!/bin/node\n" {
		t.Errorf("bin/node body = %q", body)
	}
	// Cache key directory is the verified hash.
	if filepath.Base(dist.EntryDir) != archiveHash {
		t.Errorf("entry dir base = %q, want verified hash %q", filepath.Base(dist.EntryDir), archiveHash)
	}
}

func TestFetch_ChecksumMismatchRejected(t *testing.T) {
	ff, kr, archiveHash, _ := fixture(t, matchedSigner)
	root := filepath.Join(t.TempDir(), "node")

	// Corrupt the archive body so its sha256 no longer matches the signed
	// checksum, but keep the (validly signed) checksums file intact.
	urls, _ := nodedist.ResolveURLs(fetchVersion, linuxTarget)
	ff.bodies[urls.Archive] = append(ff.bodies[urls.Archive], 0xFF)

	f := &nodedist.Fetcher{HTTP: ff, Keyring: kr, Root: root}
	_, err := f.Fetch(context.Background(), nodedist.Request{Version: fetchVersion, Target: linuxTarget})
	if err == nil {
		t.Fatalf("Fetch accepted an archive whose sha256 != signed checksum")
	}
	var ndErr *nodedist.Error
	if !errors.As(err, &ndErr) || ndErr.Kind != nodedist.ErrChecksumMismatch {
		t.Fatalf("error = %v, want ErrChecksumMismatch", err)
	}
	// Nothing committed: the verified-hash entry must not exist.
	if _, statErr := os.Stat(filepath.Join(root, "sha256", archiveHash)); statErr == nil {
		t.Fatalf("a store entry was committed despite checksum mismatch")
	}
}

func TestFetch_BadSignatureRejectedBeforeArchiveFetch(t *testing.T) {
	// Sign the checksums with a key the fetcher does NOT trust.
	ff, kr, _, _ := fixture(t, mismatchedSigner)
	root := filepath.Join(t.TempDir(), "node")

	f := &nodedist.Fetcher{HTTP: ff, Keyring: kr, Root: root}
	_, err := f.Fetch(context.Background(), nodedist.Request{Version: fetchVersion, Target: linuxTarget})
	if err == nil {
		t.Fatalf("Fetch accepted a checksums file signed by an untrusted key")
	}
	var ndErr *nodedist.Error
	if !errors.As(err, &ndErr) || ndErr.Kind != nodedist.ErrBadSignature {
		t.Fatalf("error = %v, want ErrBadSignature", err)
	}
	// The archive must NOT have been fetched: signature is checked first.
	urls, _ := nodedist.ResolveURLs(fetchVersion, linuxTarget)
	if ff.requested(urls.Archive) {
		t.Fatalf("archive was fetched despite a bad signature")
	}
	// Nothing committed.
	if entries, _ := os.ReadDir(filepath.Join(root, "sha256")); len(entries) != 0 {
		t.Fatalf("store has entries despite bad signature")
	}
}

func TestFetch_CacheShortCircuitSkipsArchive(t *testing.T) {
	ff, kr, archiveHash, _ := fixture(t, matchedSigner)
	root := filepath.Join(t.TempDir(), "node")

	f := &nodedist.Fetcher{HTTP: ff, Keyring: kr, Root: root}
	ctx := context.Background()

	// First fetch populates the cache.
	if _, err := f.Fetch(ctx, nodedist.Request{Version: fetchVersion, Target: linuxTarget}); err != nil {
		t.Fatalf("first Fetch: %v", err)
	}

	// Second fetch against the SAME store and SAME (trusted) checksums/sig,
	// but with the archive body removed and the call log reset. A cache hit
	// must serve the entry without ever requesting the archive URL.
	urls, _ := nodedist.ResolveURLs(fetchVersion, linuxTarget)
	delete(ff.bodies, urls.Archive) // archive intentionally unavailable now
	ff.mu.Lock()
	ff.calls = nil
	ff.mu.Unlock()

	dist, err := f.Fetch(ctx, nodedist.Request{Version: fetchVersion, Target: linuxTarget})
	if err != nil {
		t.Fatalf("cached Fetch: %v", err)
	}
	if !dist.FromCache {
		t.Errorf("FromCache = false on a cached request")
	}
	if dist.SHA256 != archiveHash {
		t.Errorf("SHA256 = %q, want %q", dist.SHA256, archiveHash)
	}
	if ff.requested(urls.Archive) {
		t.Fatalf("archive URL was requested despite a cache hit")
	}
}

func TestFetch_UnpackFailurePropagates(t *testing.T) {
	// A "valid" download: the bytes are not a real archive, but the signed
	// checksum matches their sha256, so the flow reaches unpack and fails
	// there. The error must surface as ErrUnpack (not reclassified as a
	// store error) and nothing is committed.
	sk, kr := matchedSigner(t)
	bogus := []byte("this is not a gzip archive")
	sum := sha256.Sum256(bogus)
	bogusHash := hex.EncodeToString(sum[:])
	archiveName, _ := nodedist.ArchiveName(fetchVersion, linuxTarget)
	checksums := fmt.Sprintf("%s  %s\n", bogusHash, archiveName)
	sig := sk.armoredDetachedSign(t, []byte(checksums))

	urls, _ := nodedist.ResolveURLs(fetchVersion, linuxTarget)
	ff := newFakeFetcher()
	ff.bodies[urls.Archive] = bogus
	ff.bodies[urls.Checksums] = []byte(checksums)
	ff.bodies[urls.Signature] = sig

	root := filepath.Join(t.TempDir(), "node")
	f := &nodedist.Fetcher{HTTP: ff, Keyring: kr, Root: root}
	_, err := f.Fetch(context.Background(), nodedist.Request{Version: fetchVersion, Target: linuxTarget})
	var ndErr *nodedist.Error
	if !errors.As(err, &ndErr) || ndErr.Kind != nodedist.ErrUnpack {
		t.Fatalf("error = %v, want ErrUnpack", err)
	}
	if _, statErr := os.Stat(filepath.Join(root, "sha256", bogusHash)); statErr == nil {
		t.Fatalf("an entry was committed despite an unpack failure")
	}
}

func TestFetch_UnsupportedTarget(t *testing.T) {
	ff, kr, _, _ := fixture(t, matchedSigner)
	f := &nodedist.Fetcher{HTTP: ff, Keyring: kr, Root: t.TempDir()}
	_, err := f.Fetch(context.Background(), nodedist.Request{
		Version: fetchVersion,
		Target:  nodedist.Target{GOOS: "plan9", GOARCH: "mips"},
	})
	var ndErr *nodedist.Error
	if !errors.As(err, &ndErr) || ndErr.Kind != nodedist.ErrUnsupportedTarget {
		t.Fatalf("error = %v, want ErrUnsupportedTarget", err)
	}
}

func TestFetch_ConfigValidation(t *testing.T) {
	ff := newFakeFetcher()
	_, kr := matchedSigner(t)
	cases := []struct {
		name string
		f    *nodedist.Fetcher
	}{
		{"nil HTTP", &nodedist.Fetcher{Keyring: kr, Root: "/tmp/x"}},
		{"empty keyring", &nodedist.Fetcher{HTTP: ff, Root: "/tmp/x"}},
		{"empty root", &nodedist.Fetcher{HTTP: ff, Keyring: kr}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.f.Fetch(context.Background(), nodedist.Request{Version: fetchVersion, Target: linuxTarget})
			var ndErr *nodedist.Error
			if !errors.As(err, &ndErr) || ndErr.Kind != nodedist.ErrConfig {
				t.Fatalf("error = %v, want ErrConfig", err)
			}
		})
	}
}

func TestFetch_MissingChecksumEntry(t *testing.T) {
	// A validly signed checksums file that omits the requested archive.
	sk, kr := matchedSigner(t)
	urls, _ := nodedist.ResolveURLs(fetchVersion, linuxTarget)
	checksums := "1111111111111111111111111111111111111111111111111111111111111111  some-other-file.tar.gz\n"
	sig := sk.armoredDetachedSign(t, []byte(checksums))

	ff := newFakeFetcher()
	ff.bodies[urls.Checksums] = []byte(checksums)
	ff.bodies[urls.Signature] = sig

	f := &nodedist.Fetcher{HTTP: ff, Keyring: kr, Root: filepath.Join(t.TempDir(), "node")}
	_, err := f.Fetch(context.Background(), nodedist.Request{Version: fetchVersion, Target: linuxTarget})
	var ndErr *nodedist.Error
	if !errors.As(err, &ndErr) || ndErr.Kind != nodedist.ErrChecksumParse {
		t.Fatalf("error = %v, want ErrChecksumParse", err)
	}
	if ff.requested(urls.Archive) {
		t.Fatalf("archive fetched despite no checksum entry for it")
	}
}

func TestFetch_FetchFailurePropagates(t *testing.T) {
	ff, kr, _, _ := fixture(t, matchedSigner)
	urls, _ := nodedist.ResolveURLs(fetchVersion, linuxTarget)
	ff.failURL = urls.Checksums

	f := &nodedist.Fetcher{HTTP: ff, Keyring: kr, Root: filepath.Join(t.TempDir(), "node")}
	_, err := f.Fetch(context.Background(), nodedist.Request{Version: fetchVersion, Target: linuxTarget})
	var ndErr *nodedist.Error
	if !errors.As(err, &ndErr) || ndErr.Kind != nodedist.ErrFetchFailed {
		t.Fatalf("error = %v, want ErrFetchFailed", err)
	}
}
