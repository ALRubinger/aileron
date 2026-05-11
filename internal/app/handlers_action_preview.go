package app

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"os"
	"path/filepath"

	api "github.com/ALRubinger/aileron/internal/api/gen"
	"github.com/ALRubinger/aileron/internal/action"
	"github.com/ALRubinger/aileron/internal/cstore"
)

// PreviewAction implements POST /v1/actions/preview — the
// action-side consent-flow primitive per ADR-0007. Runs the install
// pipeline up through fetch + parse + validate but does NOT write
// the action file to `~/.aileron/actions/`. The CLI uses this to
// render the consent prompt before asking the operator to confirm.
//
// Same failure semantics as InstallAction: parse, validate, fetch,
// and signature failures surface as 422 with structured errors.
// ADR-0007 makes signature failure a hard fail; that surfaces here
// before the consent prompt fires.
//
// Connector dependencies declared in `[[requires.connectors]]` are
// enumerated with their already-installed status — the CLI uses
// this to render which connectors will be installed alongside the
// action vs. which already exist in the cstore.
func (s *apiServer) PreviewAction(w http.ResponseWriter, r *http.Request) {
	if s.installer == nil {
		writeError(w, http.StatusServiceUnavailable, "installer_disabled",
			"connector install pipeline is not configured")
		return
	}
	if s.actions == nil {
		writeError(w, http.StatusServiceUnavailable, "actions_disabled",
			"actions store is not configured")
		return
	}
	var req api.InstallActionRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if req.Version == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "version is required")
		return
	}
	ref, err := cstore.ParseRef(req.Fqn + "@" + req.Version)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_fqn", err.Error())
		return
	}

	// Fetch + parse, no commit. Mirrors the first half of
	// runInstallAction. Errors classify identically.
	url, err := s.installer.Resolver.ResolveTarball(ref)
	if err != nil {
		writeInstallActionErr(w, classifyInstallActionErr(err))
		return
	}
	body, err := s.installer.Fetcher.Fetch(r.Context(), url)
	if err != nil {
		writeInstallActionErr(w, classifyInstallActionErr(err))
		return
	}
	defer body.Close()

	tb, err := cstore.ExtractActionTarball(body)
	if err != nil {
		writeInstallActionErr(w, classifyInstallActionErr(err))
		return
	}

	// Optional signature verification. v1 makes action signing
	// optional; an unsigned tarball previews with signature_status =
	// "unsigned" and the operator decides whether to proceed.
	signatureStatus := api.ActionPreviewSignatureStatusUnsigned
	if tb.SignaturePresent() {
		if err := s.installer.Verifier.Verify(ref.FQN.Authority(), nil, tb.Manifest, tb.Signature); err != nil {
			writeInstallActionErr(w, classifyInstallActionErr(err))
			return
		}
		signatureStatus = api.ActionPreviewSignatureStatusVerified
	}

	tmpPath := filepath.Join(s.actions.Dir(), ".aileron-preview-tmp")
	manifest, err := action.Parse(tmpPath, tb.Manifest)
	if err != nil {
		writeInstallActionErr(w, classifyInstallActionErr(err))
		return
	}
	if err := action.Validate(manifest, tmpPath); err != nil {
		writeInstallActionErr(w, classifyInstallActionErr(err))
		return
	}

	// Enumerate connector deps. For each, compute already_installed
	// against the cstore so the CLI knows which deps are new.
	deps := make([]api.ActionConnectorDep, 0, len(manifest.Requires.Connectors))
	for _, c := range manifest.Requires.Connectors {
		dep := api.ActionConnectorDep{
			Fqn:     c.Name,
			Version: c.Version,
			Hash:    c.Hash,
		}
		if len(c.Capabilities) > 0 {
			caps := append([]string(nil), c.Capabilities...)
			dep.Capabilities = &caps
		}
		// Hash-pinned: lookup by content address. Unpinned: fall
		// back to "any version of the FQN is installed". Mirrors
		// the same logic in runInstallAction's connector cross-check.
		if c.Hash != "" {
			has, _ := s.installer.Store.HasHash(c.Hash)
			dep.AlreadyInstalled = has
		} else {
			fqn, perr := cstore.ParseFQN(c.Name)
			if perr == nil {
				_, _, ok := s.installer.Store.LookupAnyVersion(fqn)
				dep.AlreadyInstalled = ok
			}
		}
		deps = append(deps, dep)
	}

	// Already-installed detection for the action itself: read the
	// canonical install path directly rather than going through the
	// in-memory action store. The install handler in
	// handlers_install_actions.go writes to (and conflicts on) the
	// same `<actions-dir>/<name>.md` path — if preview consults a
	// stale or partially-loaded in-memory index, it can miss an
	// existing file and falsely render a fresh-install prompt, only
	// to have the install POST then surface a 409. The filesystem is
	// the source of truth on both sides.
	//
	// Three states the CLI cares about:
	//   1. No action with this name on disk → fresh install.
	//   2. Same name, identical bytes → no-op (`already_installed`).
	//   3. Same name, different bytes → upgrade candidate
	//      (`existing` populated). The CLI prompts before forcing
	//      an overwrite so the operator confirms the version
	//      change rather than silently clobbering pinned bytes.
	actionHash := "sha256:" + hashHex(tb.Manifest)
	alreadyInstalled := false
	var existingRef *api.InstalledActionRef
	dest := filepath.Join(s.actions.Dir(), manifest.Name+".md")
	if existingBytes, rErr := readFileSafe(dest); rErr == nil {
		existingHash := "sha256:" + hashHex(existingBytes)
		if existingHash == actionHash {
			alreadyInstalled = true
		} else {
			// Try the in-memory store first for the parsed version/
			// source so the upgrade banner can render them. Fall back
			// to parsing the on-disk bytes when the store hasn't
			// loaded this file (the common cause of this branch).
			existingVersion, existingSource := "", ""
			if existing, err := s.actions.Get(manifest.Name); err == nil && existing.Path == dest {
				existingVersion = existing.Manifest.Version
				existingSource = existing.Manifest.Source
			} else if existingManifest, perr := action.Parse(dest, existingBytes); perr == nil {
				existingVersion = existingManifest.Version
				existingSource = existingManifest.Source
			}
			existingRef = &api.InstalledActionRef{
				Version: existingVersion,
				Hash:    existingHash,
				Source:  existingSource,
				Path:    dest,
			}
		}
	}

	out := api.ActionPreview{
		Fqn:              ref.FQN.String(),
		Version:          ref.Version,
		Hash:             actionHash,
		Name:             manifest.Name,
		SignatureStatus:  &signatureStatus,
		AlreadyInstalled: &alreadyInstalled,
		Existing:         existingRef,
		ConnectorDeps:    deps,
	}
	if intent := manifest.Match.Intent; intent != "" {
		out.Intent = &intent
	}
	writeJSON(w, http.StatusOK, out)
}

// hashHex returns the hex-encoded sha256 of b. Helper for the
// preview's action-hash computation.
func hashHex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// readFileSafe reads a file via os.ReadFile. Aliased through a var so
// tests can stub it without reaching into the os package.
var readFileSafe = os.ReadFile
