package nodedist

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"path/filepath"
	"runtime"

	"github.com/ALRubinger/aileron/internal/cstore"
	"golang.org/x/crypto/openpgp" //nolint:staticcheck // openpgp is the GPG verification primitive; see verify.go.
)

// Fetcher provisions a verified Node distribution and caches its unpacked
// tree content-addressed. It ties together the package's units: URL
// resolution, signature-verified checksums, SHA-256 archive matching,
// archive unpack, and the shared atomic-rename content-addressed store
// (cstore.CommitDir).
//
// Fetcher reuses cstore.Fetcher for all three downloads so tests can
// substitute an in-memory fetcher with no network, exactly as the connector
// install pipeline does.
type Fetcher struct {
	// HTTP downloads the archive and the clearsigned SHASUMS256.txt.asc.
	// Required.
	HTTP cstore.Fetcher

	// Keyring is the set of trusted Node release public keys the checksums
	// signature is verified against. Required and must be non-empty;
	// verification fails closed otherwise.
	Keyring openpgp.EntityList

	// Root is the cache subtree root for unpacked Node trees. The store lays
	// out entries as `<Root>/sha256/<verified-hash>/`. Required. Callers
	// typically pass `filepath.Join(cstore.DefaultRoot(), "node")` so the
	// Node cache never collides with the connector store's `connectors/`
	// subtree.
	Root string
}

// Request selects which Node distribution to provision.
type Request struct {
	// Version is the Node version, with or without a leading "v" (e.g.
	// "24.2.0" or "v24.2.0"). Required.
	Version string

	// Target is the platform to fetch for. When zero-valued, the running
	// host's GOOS/GOARCH is used.
	Target Target
}

// NodeDist describes a provisioned (cached) Node distribution.
type NodeDist struct {
	// Version is the resolved version (no leading "v").
	Version string

	// EntryDir is the absolute path to the cached unpacked tree
	// (`<Root>/sha256/<hash>/`). On Unix the layout has `bin/node`; on
	// Windows `node.exe` at the root.
	EntryDir string

	// SHA256 is the verified hex SHA-256 of the source archive — the cache
	// key. Equal to the entry's SHASUMS256.txt value.
	SHA256 string

	// NodeBinary is the absolute path to the node executable inside
	// EntryDir.
	NodeBinary string

	// FromCache is true when the request was served from the
	// content-addressed cache without re-downloading the archive.
	FromCache bool
}

// Fetch provisions the requested Node distribution and returns the cached
// entry. The pipeline:
//
//  1. Resolve archive/checksums/signature URLs for version+target.
//  2. Download the clearsigned SHASUMS256.txt.asc.
//  3. Verify the clearsigned message against the keyring, then parse the
//     embedded verified checksums. A bad signature aborts here — no archive is
//     fetched and nothing is committed.
//  4. Look up the selected archive's verified SHA-256.
//  5. Cache short-circuit: if a tree for that hash is already stored, return
//     it WITHOUT downloading the archive.
//  6. Otherwise download the archive and SHA-256 it; reject on a mismatch
//     against the signed value (nothing is committed).
//  7. Unpack into a temp tree (zip-slip guarded) and atomically commit it
//     under the verified hash via cstore.CommitDir.
func (f *Fetcher) Fetch(ctx context.Context, req Request) (NodeDist, error) {
	if f.HTTP == nil {
		return NodeDist{}, wrapErr(ErrConfig, nil, "Fetcher.HTTP is nil")
	}
	if len(f.Keyring) == 0 {
		return NodeDist{}, wrapErr(ErrConfig, nil, "Fetcher.Keyring is empty")
	}
	if f.Root == "" {
		return NodeDist{}, wrapErr(ErrConfig, nil, "Fetcher.Root is empty")
	}

	target := req.Target
	if target == (Target{}) {
		target = Target{GOOS: runtime.GOOS, GOARCH: runtime.GOARCH}
	}

	version := normalizeVersion(req.Version)
	urls, err := ResolveURLs(version, target)
	if err != nil {
		return NodeDist{}, err
	}
	platform, err := target.resolve()
	if err != nil {
		return NodeDist{}, err
	}
	archiveName, err := ArchiveName(version, target)
	if err != nil {
		return NodeDist{}, err
	}
	distroName, err := DistroName(version, target)
	if err != nil {
		return NodeDist{}, err
	}

	// Fetch + signature-verify + parse the checksums. Node's
	// SHASUMS256.txt.asc is clearsigned (it embeds the checksum lines inside
	// the signed message), so it is the single source of truth — there is no
	// separate unsigned SHASUMS256.txt to download.
	ascBytes, err := f.download(ctx, urls.Signature)
	if err != nil {
		return NodeDist{}, err
	}
	sums, err := ChecksumVerifier{Keyring: f.Keyring}.VerifyAndParse(ascBytes)
	if err != nil {
		return NodeDist{}, err
	}

	expectedHash, ok := sums[archiveName]
	if !ok {
		return NodeDist{}, wrapErr(ErrChecksumParse, nil,
			"SHASUMS256.txt has no entry for %s", archiveName)
	}

	// Cache short-circuit: already provisioned at this verified hash.
	present, err := cstore.HasDirHash(f.Root, expectedHash)
	if err != nil {
		return NodeDist{}, wrapErr(ErrStore, err, "probe cache for %s", expectedHash)
	}
	if present {
		entryDir, _ := cstore.DirEntryDir(f.Root, expectedHash)
		return NodeDist{
			Version:    version,
			EntryDir:   entryDir,
			SHA256:     expectedHash,
			NodeBinary: nodeBinaryPath(entryDir, target),
			FromCache:  true,
		}, nil
	}

	// Download the archive and SHA-256 it against the signed checksum.
	archiveBytes, err := f.download(ctx, urls.Archive)
	if err != nil {
		return NodeDist{}, err
	}
	gotHash := sha256Hex(archiveBytes)
	if gotHash != expectedHash {
		return NodeDist{}, wrapErr(ErrChecksumMismatch, nil,
			"archive %s sha256 %s does not match signed checksum %s",
			archiveName, gotHash, expectedHash)
	}

	// Unpack into a temp tree and atomically commit under the verified hash.
	entryDir, _, cErr := cstore.CommitDir(f.Root, expectedHash, func(dir string) error {
		return unpackArchive(archiveBytes, platform.ext, distroName, dir)
	})
	if cErr != nil {
		// Surface unpack failures with their own kind; wrap store failures.
		var ndErr *Error
		if errors.As(cErr, &ndErr) {
			return NodeDist{}, cErr
		}
		return NodeDist{}, wrapErr(ErrStore, cErr, "commit unpacked Node tree")
	}

	return NodeDist{
		Version:    version,
		EntryDir:   entryDir,
		SHA256:     expectedHash,
		NodeBinary: nodeBinaryPath(entryDir, target),
		FromCache:  false,
	}, nil
}

// download fetches url via the configured Fetcher and reads the full body.
func (f *Fetcher) download(ctx context.Context, url string) ([]byte, error) {
	body, err := f.HTTP.Fetch(ctx, url)
	if err != nil {
		return nil, wrapErr(ErrFetchFailed, err, "fetch %s", url)
	}
	defer body.Close()
	data, err := io.ReadAll(body)
	if err != nil {
		return nil, wrapErr(ErrFetchFailed, err, "read %s", url)
	}
	return data, nil
}

// nodeBinaryPath returns the absolute path to the node executable inside an
// unpacked entry for the given target.
func nodeBinaryPath(entryDir string, target Target) string {
	if target.GOOS == "windows" {
		return filepath.Join(entryDir, "node.exe")
	}
	return filepath.Join(entryDir, "bin", "node")
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
