package cstore

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Fetcher downloads bytes from a URL the resolver computed. The interface
// is narrow so tests can substitute an in-memory fetcher without HTTP.
type Fetcher interface {
	Fetch(ctx context.Context, url string) (io.ReadCloser, error)
}

// HTTPFetcher is the default Fetcher: a thin wrapper around an http.Client
// that surfaces non-2xx responses as ClassFetchFailed.
type HTTPFetcher struct {
	Client *http.Client
}

// Fetch implements Fetcher.
func (h *HTTPFetcher) Fetch(ctx context.Context, url string) (io.ReadCloser, error) {
	client := h.Client
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, newError(ClassFetchFailed, BoundaryRuntime, false, "build request: %s", err.Error())
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, newError(ClassFetchFailed, BoundaryExternal, true, "fetch %s: %s", url, err.Error())
	}
	if resp.StatusCode == http.StatusNotFound {
		resp.Body.Close()
		return nil, newError(ClassFetchFailed, BoundaryExternal, false, "fetch %s: 404 (tag or release not found)", url)
	}
	if resp.StatusCode/100 != 2 {
		resp.Body.Close()
		retriable := resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests
		return nil, newError(ClassFetchFailed, BoundaryExternal, retriable,
			"fetch %s: status %d", url, resp.StatusCode)
	}
	return resp.Body, nil
}

// Installer orchestrates the install pipeline per ADR-0004 §"Install
// pipeline":
//
//   1. Parse FQN; identify scheme; reject unknown.
//   2. Resolve fetch URL via the scheme's resolver.
//   3. Download tarball.
//   4. Extract tarball.
//   5. Verify signature against keys associated with the FQN's authority.
//   6. Compute SHA-256 over the canonical hash input.
//   7. Compare computed hash:
//        - First install (no expected hash): record the computed hash.
//        - Subsequent install (caller declares an expected hash): MUST match.
//   8. Atomic rename to sha256/<hash>/.
//   9. Update the index cache.
//
// Steps 3–7 leave the store untouched on failure; only a successful step 8
// publishes anything to the content-addressed tree.
type Installer struct {
	Resolver Resolver
	Fetcher  Fetcher
	Verifier Verifier
	Store    *Store
}

// InstallRequest is the input to Install. ExpectedHash is optional: when
// supplied (typical for installs driven by an action file's declared
// `[[requires.connectors]] hash`), the pipeline rejects any tarball whose
// canonical hash does not match. When empty (typical for first install of
// a connector), the pipeline records whatever hash the bytes produce.
type InstallRequest struct {
	Ref          Ref
	ExpectedHash string
}

// InstallResult is the outcome of a successful install.
type InstallResult struct {
	// Hash is the canonical `sha256:<hex>` hash of the installed bytes.
	Hash string

	// EntryDir is the absolute path to the store directory holding the
	// installed connector (`<root>/connectors/sha256/<hex>/`).
	EntryDir string

	// AlreadyInstalled is true when the install was a no-op because an
	// entry with the matching hash already existed in the store
	// (concurrent install, repeat install, or shared connector across
	// actions). The pipeline still verifies the signature/hash even in
	// this case unless the entry was present *before* fetch — see
	// "Reinstall of an already-stored hash is offline" in ADR-0004.
	AlreadyInstalled bool
}

// Install runs the pipeline. Returns a structured *Error on any failure
// per ADR-0010.
func (i *Installer) Install(ctx context.Context, req InstallRequest) (*InstallResult, error) {
	if i.Store == nil {
		return nil, fmt.Errorf("Installer.Store is nil")
	}
	if i.Resolver == nil {
		return nil, fmt.Errorf("Installer.Resolver is nil")
	}
	if i.Fetcher == nil {
		return nil, fmt.Errorf("Installer.Fetcher is nil")
	}
	if i.Verifier == nil {
		return nil, fmt.Errorf("Installer.Verifier is nil")
	}

	// Short-circuit when the caller supplied an ExpectedHash and the store
	// already has it. ADR-0004: "Reinstall of an already-stored hash is
	// offline."
	if req.ExpectedHash != "" {
		exists, err := i.Store.HasHash(req.ExpectedHash)
		if err != nil {
			return nil, wrapStoreErr(err)
		}
		if exists {
			dir, _ := i.Store.EntryDir(req.ExpectedHash)
			// Update index in case the entry exists but isn't yet recorded
			// (e.g., index file was wiped).
			if err := i.Store.recordIndex(req.Ref, req.ExpectedHash); err != nil {
				return nil, err
			}
			return &InstallResult{
				Hash:             req.ExpectedHash,
				EntryDir:         dir,
				AlreadyInstalled: true,
			}, nil
		}
	}

	url, err := i.Resolver.ResolveTarball(req.Ref)
	if err != nil {
		return nil, err
	}

	body, err := i.Fetcher.Fetch(ctx, url)
	if err != nil {
		return nil, err
	}
	defer body.Close()

	tarball, err := ExtractTarball(body)
	if err != nil {
		return nil, err
	}

	// Manifest sanity checks before we write anything: the manifest's
	// declared FQN must match the FQN being installed (per ADR-0002), and
	// its declared version must match the requested version.
	parsedManifest, err := ParseManifest("", tarball.Manifest)
	if err != nil {
		return nil, err
	}
	if vErr := ValidateManifest(parsedManifest, ""); vErr != nil {
		return nil, vErr
	}
	if parsedManifest.Connector.Name != req.Ref.FQN.String() {
		return nil, &Error{
			Class:    ClassFQNMismatch,
			Boundary: BoundaryRuntime,
			Message:  "manifest FQN does not match requested FQN",
			Details: map[string]any{
				"requested_fqn": req.Ref.FQN.String(),
				"manifest_fqn":  parsedManifest.Connector.Name,
			},
		}
	}
	if parsedManifest.Connector.Version != req.Ref.Version {
		return nil, &Error{
			Class:    ClassFQNMismatch,
			Boundary: BoundaryRuntime,
			Message:  "manifest version does not match requested version",
			Details: map[string]any{
				"requested_version": req.Ref.Version,
				"manifest_version":  parsedManifest.Connector.Version,
			},
		}
	}

	if err := i.Verifier.Verify(req.Ref.FQN.Authority(), tarball.Binary, tarball.Manifest, tarball.Signature); err != nil {
		return nil, err
	}

	computedHashHex := tarball.CanonicalHashHex()
	computed := "sha256:" + computedHashHex

	if req.ExpectedHash != "" && !strings.EqualFold(req.ExpectedHash, computed) {
		return nil, &Error{
			Class:    ClassHashMismatch,
			Boundary: BoundaryRuntime,
			Message:  "computed hash does not match declared hash",
			Details: map[string]any{
				"expected": req.ExpectedHash,
				"computed": computed,
				"ref":      req.Ref.String(),
			},
		}
	}

	if err := i.Store.commit(tarball, req.Ref, computedHashHex); err != nil {
		return nil, err
	}

	dir, _ := i.Store.EntryDir(computed)
	return &InstallResult{
		Hash:     computed,
		EntryDir: dir,
	}, nil
}
