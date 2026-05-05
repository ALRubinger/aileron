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

	// Already-installed detection for the action itself: same
	// (name, hash) lookup the CLI does post-install. Compute the
	// action hash as sha256 of the raw manifest bytes — that's the
	// thing on disk after install, and matches what an operator
	// would expect from `sha256sum ~/.aileron/actions/<name>.md`.
	actionHash := "sha256:" + hashHex(tb.Manifest)
	alreadyInstalled := false
	if existing, err := s.actions.Get(manifest.Name); err == nil {
		if existingBytes, rErr := readFileSafe(existing.Path); rErr == nil {
			if "sha256:"+hashHex(existingBytes) == actionHash {
				alreadyInstalled = true
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
