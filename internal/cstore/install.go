package cstore

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
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

	// ScopeDriftHook, when non-nil, is invoked at the tail of every
	// successful Install with the installed connector's FQN and the
	// OAuth scope set its manifest declares (or nil when the connector
	// does not declare OAuth2). The hook lets a layer above cstore
	// (the daemon) iterate bindings against this connector and mark
	// the ones whose recorded grant no longer satisfies the manifest
	// as `stale`, surfacing a reauthorization prompt in the
	// webapp/CLI. The hook is non-blocking from cstore's perspective —
	// errors inside the hook are the daemon's responsibility, not the
	// installer's, so the install result is unaffected.
	//
	// Defined here rather than in package binding so cstore stays free
	// of binding/vault imports.
	ScopeDriftHook ScopeDriftHook
}

// ScopeDriftHook is invoked after a successful install with the
// installed connector's FQN and OAuth scope set. See
// [Installer.ScopeDriftHook] for the contract.
type ScopeDriftHook func(ctx context.Context, fqn string, requiredScopes []string)

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

// PreviewResult is what Preview returns: enough information for the
// CLI to render a consent prompt per ADR-0007 ("show the user what
// they're about to install"). Fetch + verify + parse have already
// run; the only thing Preview does NOT do is commit.
//
// The caller distinguishes "already installed at this hash" (no-op,
// no prompt needed) from "different hash for the same FQN+version"
// (hash-mismatch error) using AlreadyInstalled and the Manifest's
// content compared against what's on disk.
type PreviewResult struct {
	// Manifest is the parsed connector manifest from the fetched
	// tarball. Used to render capabilities, description, publisher,
	// etc. in the consent prompt.
	Manifest *Manifest

	// Hash is the canonical `sha256:<hex>` of the canonical hash
	// input. Same shape as InstallResult.Hash.
	Hash string

	// SignatureVerified is true when the verifier accepted the
	// tarball under one of the FQN authority's keys. Per ADR-0007
	// signature failure is a hard fail and surfaces as an error
	// (not as `false` here) — this field exists for the rare case
	// where Verify is structurally a no-op (test verifier, etc.).
	SignatureVerified bool

	// AlreadyInstalled is true when an entry with the matching hash
	// already exists in the cstore. Per ADR-0007 the CLI should not
	// re-prompt in this case — install is a no-op.
	AlreadyInstalled bool
}

// Preview runs the install pipeline up through verification and hash
// computation but does NOT commit to the store. Used by the CLI's
// consent flow per ADR-0007: fetch + verify + parse → show the user
// what they're about to install → commit on confirmation.
//
// Failure modes match Install: signature failure, hash mismatch (when
// ExpectedHash is supplied), FQN/version mismatch, fetch failure
// surface as structured *cstore.Error and the CLI maps them to user-
// visible messages. ADR-0007: "signature failure is a hard fail;
// `--yes` does not bypass."
//
// Per ADR-0004, when an entry with the matching expected_hash is
// already in the store this short-circuits to AlreadyInstalled=true
// without re-fetching — same offline-reinstall path Install honors.
func (i *Installer) Preview(ctx context.Context, req InstallRequest) (*PreviewResult, error) {
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

	// Offline reinstall: caller declared the expected hash and we
	// already have it. We still need the manifest for the consent
	// prompt, so read it back from the on-disk entry rather than
	// re-fetching.
	if req.ExpectedHash != "" {
		exists, err := i.Store.HasHash(req.ExpectedHash)
		if err != nil {
			return nil, wrapStoreErr(err)
		}
		if exists {
			dir, _ := i.Store.EntryDir(req.ExpectedHash)
			manifestBytes, mErr := readInstalledManifest(dir)
			if mErr != nil {
				return nil, wrapStoreErr(mErr)
			}
			parsed, pErr := ParseManifest("", manifestBytes)
			if pErr != nil {
				return nil, pErr
			}
			return &PreviewResult{
				Manifest:          parsed,
				Hash:              req.ExpectedHash,
				SignatureVerified: true,
				AlreadyInstalled:  true,
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

	computed := "sha256:" + tarball.CanonicalHashHex()
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

	// Already-installed-at-this-hash detection without ExpectedHash:
	// the caller didn't declare a hash but we just computed one and
	// the store already has it. CLI uses this to avoid re-prompting
	// when the operator runs `install` twice for the same artifact.
	already, _ := i.Store.HasHash(computed)

	return &PreviewResult{
		Manifest:          parsedManifest,
		Hash:              computed,
		SignatureVerified: true,
		AlreadyInstalled:  already,
	}, nil
}

// readInstalledManifest reads the manifest.toml under an installed
// entry's directory. Used by Preview's offline-reinstall path so the
// CLI consent prompt has the same metadata it would get from a fresh
// fetch.
func readInstalledManifest(entryDir string) ([]byte, error) {
	return os.ReadFile(filepath.Join(entryDir, tarManifestFile))
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
			i.runScopeDriftHook(ctx, req.Ref.FQN.String(), dir)
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
	i.runScopeDriftHookWithManifest(ctx, req.Ref.FQN.String(), parsedManifest)
	return &InstallResult{
		Hash:     computed,
		EntryDir: dir,
	}, nil
}

// runScopeDriftHook reads the installed manifest from disk and invokes
// the configured ScopeDriftHook. Used on the already-installed
// short-circuit path where the parsed manifest isn't in hand. A
// missing/unreadable manifest is treated as "no OAuth scopes" — the
// hook still fires with a nil scope list so the migration-mark case
// (binding has no recorded grant) can run.
func (i *Installer) runScopeDriftHook(ctx context.Context, fqn, entryDir string) {
	if i.ScopeDriftHook == nil {
		return
	}
	mfBytes, err := readInstalledManifest(entryDir)
	if err != nil {
		i.ScopeDriftHook(ctx, fqn, nil)
		return
	}
	parsed, err := ParseManifest("", mfBytes)
	if err != nil {
		i.ScopeDriftHook(ctx, fqn, nil)
		return
	}
	i.runScopeDriftHookWithManifest(ctx, fqn, parsed)
}

// runScopeDriftHookWithManifest invokes ScopeDriftHook with the OAuth
// scope set declared by parsed (or nil when the connector does not
// declare OAuth2). Split from runScopeDriftHook so the full install
// path (where the parsed manifest is already in hand) does not pay
// for a re-read + re-parse.
func (i *Installer) runScopeDriftHookWithManifest(ctx context.Context, fqn string, parsed *Manifest) {
	if i.ScopeDriftHook == nil {
		return
	}
	var scopes []string
	if parsed != nil && parsed.Capabilities.Credential != nil &&
		parsed.Capabilities.Credential.OAuth2 != nil {
		scopes = parsed.Capabilities.Credential.OAuth2.Scopes
	}
	i.ScopeDriftHook(ctx, fqn, scopes)
}
