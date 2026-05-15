package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	api "github.com/ALRubinger/aileron/internal/api/gen"
	"github.com/ALRubinger/aileron/internal/action"
	"github.com/ALRubinger/aileron/internal/cstore"
	"github.com/ALRubinger/aileron/internal/model"
)

// --- Action install pipeline (ADR-0003 + #366) ---

// InstallAction runs the action install pipeline for the supplied
// FQN+version. Returns 201 with the InstalledAction envelope (or 200
// when the action was already installed and the bytes match). On any
// pipeline failure returns a structured error per ADR-0010.
//
// The pipeline parallels connector install:
//
//  1. Resolve fetch URL via the configured cstore Resolver.
//  2. Fetch the tarball via the cstore Fetcher.
//  3. Extract `action.md` and optional `signature.sig`.
//  4. If a signature is present, verify against the publisher's keys
//     in the configured Verifier (fail-closed when no key is
//     registered for the authority). v1 makes signing optional —
//     unsigned tarballs install with a debug log; mandatory signing
//     lands with #363 install consent.
//  5. Parse + validate the action manifest.
//  6. Cross-check that every `[[requires.connectors]]` entry is
//     present in the cstore. Hash-pinned dependencies are checked by
//     content address (`HasHash`); unpinned dependencies fall back to
//     "any version of the FQN is installed" (`LookupAnyVersion`).
//     When some hashes are missing and the request set
//     `auto_install_connectors: true`, the server runs the connector
//     install pipeline for each missing entry (using the version+hash
//     declared in the action manifest) before continuing. Otherwise
//     the install aborts with `connectors_missing` and structured
//     details so the CLI can prompt the user.
//  7. Write to `~/.aileron/actions/<name>.md`, refusing to clobber
//     unless `force=true` was set.
//
// Successful installs reload the in-memory action store so subsequent
// `GET /v1/actions` and intent matches see the new action without
// requiring a server restart.
func (s *apiServer) InstallAction(w http.ResponseWriter, r *http.Request) {
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
	force := req.Force != nil && *req.Force
	autoInstall := req.AutoInstallConnectors != nil && *req.AutoInstallConnectors

	res, herr := s.runInstallAction(r.Context(), ref, force, autoInstall)
	if herr != nil {
		writeInstallActionErr(w, herr)
		return
	}
	out := api.InstalledAction{
		Name:    res.Name,
		Fqn:     ref.FQN.String(),
		Version: ref.Version,
		Source:  res.Source,
		Path:    res.Path,
	}
	if unbound := s.unboundCapabilitiesFor(r.Context(), res.ConnectorFQNs); len(unbound) > 0 {
		out.UnboundCapabilities = &unbound
	}
	if stale := newlyStaleBindings(res.NewlyStale); len(stale) > 0 {
		out.NewlyStaleBindings = &stale
	}
	if res.AlreadyInstalled {
		already := true
		out.AlreadyInstalled = &already
		writeJSON(w, http.StatusOK, out)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

// unboundCapabilitiesFor scans the installed connectors the action
// references, looks up their declared credential capabilities, and
// returns one entry per capability that does not yet have a binding
// in the user's vault. The CLI uses this list to prompt the user to
// drop into `aileron binding setup` immediately, avoiding the
// hit-binding_required-later UX.
func (s *apiServer) unboundCapabilitiesFor(ctx context.Context, connectorFQNs []string) []api.UnboundCapability {
	var out []api.UnboundCapability
	for _, fqnStr := range connectorFQNs {
		canonical, manifest, err := s.lookupConnector(fqnStr)
		if err != nil {
			continue
		}
		cred := manifest.Capabilities.Credential
		if cred == nil || cred.Kind == "" {
			continue
		}
		if s.bindings == nil {
			// No binding store wired (dev edge case) — every capability
			// is "unbound" by definition; surface them so the user
			// knows.
			out = append(out, api.UnboundCapability{
				ConnectorFqn: canonical, Kind: cred.Kind, Scope: scopePtr(cred.Scope),
			})
			continue
		}
		// Existing binding for this (connector, kind)? If yes, no
		// prompt needed.
		if _, err := s.bindings.Resolve(ctx, canonical, cred.Kind); err == nil {
			continue
		}
		out = append(out, api.UnboundCapability{
			ConnectorFqn: canonical,
			Kind:         cred.Kind,
			Scope:        scopePtr(cred.Scope),
		})
	}
	return out
}

// scopePtr returns a *string for a possibly-empty scope; the API
// type uses an optional pointer for omittable strings.
func scopePtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// installActionResult is the success payload of runInstallAction.
type installActionResult struct {
	Name             string
	Path             string
	Source           string
	AlreadyInstalled bool
	// ConnectorFQNs is the canonical list of connectors the action
	// declares as dependencies, in declaration order. The handler
	// uses these to scan for unbound credential capabilities.
	ConnectorFQNs []string
	// NewlyStale aggregates the stale-binding transitions emitted by
	// the scope-drift hook across every implicit connector install
	// auto_install_connectors triggered. The handler exposes this on
	// the install response so the CLI can prompt the operator to
	// reauthorize inline (#741); without aggregation a stale
	// transition fired by an auto-installed connector dep would be
	// silently lost.
	NewlyStale []cstore.StaleBindingTransition
}

// installActionError carries an HTTP-renderable failure. When details
// is non-nil the error envelope includes structured per-entry detail
// objects (used by the `connectors_missing` failure to enumerate the
// missing connector deps so the CLI can render and prompt).
type installActionError struct {
	status  int
	code    string
	message string
	details []map[string]any
}

// writeInstallActionErr renders an installActionError as the standard
// api.Error envelope, optionally including the structured details
// array.
func writeInstallActionErr(w http.ResponseWriter, e *installActionError) {
	if len(e.details) == 0 {
		writeError(w, e.status, e.code, e.message)
		return
	}
	details := e.details
	writeJSON(w, e.status, api.Error{
		Error: struct {
			Code      string                    `json:"code"`
			Details   *[]map[string]interface{} `json:"details,omitempty"`
			Message   string                    `json:"message"`
			RequestId *string                   `json:"request_id,omitempty"`
		}{
			Code:    e.code,
			Message: e.message,
			Details: &details,
		},
	})
}

// runInstallAction is the pipeline. Split out so it can return a
// structured error type the handler maps onto an HTTP envelope.
func (s *apiServer) runInstallAction(ctx context.Context, ref cstore.Ref, force, autoInstall bool) (*installActionResult, *installActionError) {
	// Step 1 + 2: resolve + fetch.
	url, err := s.installer.Resolver.ResolveTarball(ref)
	if err != nil {
		return nil, classifyInstallActionErr(err)
	}
	body, err := s.installer.Fetcher.Fetch(ctx, url)
	if err != nil {
		return nil, classifyInstallActionErr(err)
	}
	defer body.Close()

	// Step 3: extract.
	tb, err := cstore.ExtractActionTarball(body)
	if err != nil {
		return nil, classifyInstallActionErr(err)
	}

	// Step 4: optional signature verification.
	signatureStatus := "unsigned"
	if tb.SignaturePresent() {
		if err := s.installer.Verifier.Verify(ref.FQN.Authority(), nil, tb.Manifest, tb.Signature); err != nil {
			return nil, classifyInstallActionErr(err)
		}
		signatureStatus = "verified"
	} else if s.log != nil {
		s.log.Debug("installing unsigned action tarball",
			"fqn", ref.FQN.String(), "version", ref.Version)
	}

	// Step 5: parse + validate.
	tmpPath := filepath.Join(s.actions.Dir(), ".aileron-install-tmp")
	manifest, err := action.Parse(tmpPath, tb.Manifest)
	if err != nil {
		return nil, classifyInstallActionErr(err)
	}
	if err := action.Validate(manifest, tmpPath); err != nil {
		return nil, classifyInstallActionErr(err)
	}

	// Step 6: cross-check connector deps.
	//
	// A `[[requires.connectors]]` entry that pins a `hash` is checked
	// by content address (HasHash): the action installs only when the
	// exact connector bytes the publisher tested against are in the
	// store. An entry that omits `hash` (legacy/unpinned) falls back
	// to "any version of the FQN is installed" via LookupAnyVersion.
	//
	// When some hash-pinned dependencies are missing, the response
	// shape depends on autoInstall:
	//   - false → return `connectors_missing` with structured details
	//             (name, version, hash) per entry. The CLI uses this
	//             to prompt the user before retrying with the flag.
	//   - true  → run the connector install pipeline for each missing
	//             entry (using the version+hash from the manifest)
	//             and continue. Any failure aborts with the
	//             connector's structured class (`hash_mismatch`,
	//             `signature_failure`, `fetch_failed`, …).
	type missingConnector struct {
		name    string
		version string
		hash    string
	}
	var newlyStale []cstore.StaleBindingTransition
	var missing []missingConnector
	var missingNames []string
	for _, c := range manifest.Requires.Connectors {
		fqn, perr := cstore.ParseFQN(c.Name)
		if perr != nil {
			return nil, &installActionError{
				status:  http.StatusBadRequest,
				code:    string(cstore.ClassValidationError),
				message: fmt.Sprintf("requires.connectors[%s]: %s", c.Name, perr.Error()),
			}
		}
		if c.Hash != "" {
			has, hErr := s.installer.Store.HasHash(c.Hash)
			if hErr != nil {
				return nil, &installActionError{
					status:  http.StatusInternalServerError,
					code:    string(cstore.ClassStoreUnwritable),
					message: hErr.Error(),
				}
			}
			if !has {
				missing = append(missing, missingConnector{name: c.Name, version: c.Version, hash: c.Hash})
				missingNames = append(missingNames, c.Name)
			}
			continue
		}
		// No hash pinned in the manifest — fall back to FQN presence.
		if _, _, ok := s.installer.Store.LookupAnyVersion(fqn); !ok {
			missing = append(missing, missingConnector{name: c.Name, version: c.Version})
			missingNames = append(missingNames, c.Name)
		}
	}
	if len(missing) > 0 {
		if !autoInstall {
			details := make([]map[string]any, 0, len(missing))
			for _, m := range missing {
				details = append(details, map[string]any{
					"name":    m.name,
					"version": m.version,
					"hash":    m.hash,
				})
			}
			return nil, &installActionError{
				status: http.StatusUnprocessableEntity,
				code:   "connectors_missing",
				message: fmt.Sprintf(
					"action requires connectors that are not installed: %v — install them first via `aileron connector install <FQN>@<version>` or retry with auto_install_connectors=true",
					missingNames),
				details: details,
			}
		}
		// Auto-install path: run the connector install pipeline for
		// each missing entry. The action manifest carries name,
		// version, and hash, so we never need release-listing or
		// hash→tag resolution — the connector pipeline is invoked
		// with ExpectedHash set, and a mismatch surfaces as
		// `hash_mismatch` per ADR-0004.
		for _, m := range missing {
			depFQN, perr := cstore.ParseFQN(m.name)
			if perr != nil {
				return nil, &installActionError{
					status:  http.StatusBadRequest,
					code:    string(cstore.ClassValidationError),
					message: fmt.Sprintf("requires.connectors[%s]: %s", m.name, perr.Error()),
				}
			}
			if m.version == "" {
				return nil, &installActionError{
					status: http.StatusUnprocessableEntity,
					code:   string(cstore.ClassValidationError),
					message: fmt.Sprintf(
						"requires.connectors[%s]: cannot auto-install without a pinned version",
						m.name),
				}
			}
			depRef := cstore.Ref{FQN: depFQN, Version: m.version}
			depRes, iErr := s.installer.Install(ctx, cstore.InstallRequest{
				Ref:          depRef,
				ExpectedHash: m.hash,
			})
			if iErr != nil {
				return nil, classifyInstallActionErr(iErr)
			}
			if depRes != nil && len(depRes.NewlyStale) > 0 {
				newlyStale = append(newlyStale, depRes.NewlyStale...)
			}
		}
	}

	// Step 7: write to disk.
	if err := os.MkdirAll(s.actions.Dir(), 0o755); err != nil {
		return nil, &installActionError{
			status:  http.StatusInternalServerError,
			code:    string(cstore.ClassStoreUnwritable),
			message: err.Error(),
		}
	}
	dest := filepath.Join(s.actions.Dir(), manifest.Name+".md")

	already := false
	if existing, err := os.ReadFile(dest); err == nil {
		if string(existing) == string(tb.Manifest) {
			already = true
		} else if !force {
			return nil, &installActionError{
				status:  http.StatusConflict,
				code:    "action_exists",
				message: fmt.Sprintf("action %q already installed at %s; pass force=true to overwrite", manifest.Name, dest),
			}
		}
	}

	if !already {
		if err := os.WriteFile(dest, tb.Manifest, 0o644); err != nil {
			return nil, &installActionError{
				status:  http.StatusInternalServerError,
				code:    string(cstore.ClassStoreUnwritable),
				message: err.Error(),
			}
		}
		// Reload the in-memory action store so subsequent GETs and
		// intent matches see the new action without restart.
		if _, lerr := s.actions.Load(); lerr != nil && s.log != nil {
			s.log.Warn("action store reload after install failed",
				"error", lerr, "dir", s.actions.Dir())
		}
	}

	connectorFQNs := make([]string, 0, len(manifest.Requires.Connectors))
	for _, c := range manifest.Requires.Connectors {
		connectorFQNs = append(connectorFQNs, c.Name)
	}

	if s.auditRecorder != nil {
		// Action manifests sign just the manifest bytes (no separate
		// binary), so the canonical hash is sha256 over those bytes —
		// matching what the verifier checked above and what
		// `[[requires.connectors]] hash` pins reference per ADR-0004.
		manifestHashHex := sha256.Sum256(tb.Manifest)
		deps := make([]map[string]any, 0, len(manifest.Requires.Connectors))
		for _, c := range manifest.Requires.Connectors {
			dep := map[string]any{"aileron.connector.fqn": c.Name}
			if c.Version != "" {
				dep["aileron.connector.version"] = c.Version
			}
			if c.Hash != "" {
				dep["aileron.connector.hash"] = c.Hash
			}
			deps = append(deps, dep)
		}
		s.auditRecorder.RecordSuccess(ctx, model.EventTypeActionInstalled,
			model.ActorRef{Type: model.ActorTypeHuman, ID: "user"},
			map[string]any{
				"aileron.action.name":          manifest.Name,
				"aileron.action.fqn":           ref.FQN.String(),
				"aileron.action.version":       ref.Version,
				"aileron.action.hash":          "sha256:" + hex.EncodeToString(manifestHashHex[:]),
				"aileron.signature.status":     signatureStatus,
				"aileron.consent.decision":     "approved",
				"aileron.action.source":        manifest.Source,
				"aileron.action.path":          dest,
				"aileron.action.dependencies":  deps,
			})
	}

	return &installActionResult{
		Name:             manifest.Name,
		Path:             dest,
		Source:           manifest.Source,
		AlreadyInstalled: already,
		ConnectorFQNs:    connectorFQNs,
		NewlyStale:       newlyStale,
	}, nil
}

// classifyInstallActionErr maps any error returned along the pipeline
// onto the right HTTP status + structured class. cstore errors carry
// their own class (matches connector install); action errors are
// remapped to 422 because the request was well-formed but the
// fetched bytes failed parsing/validation.
func classifyInstallActionErr(err error) *installActionError {
	var cerr *cstore.Error
	if errors.As(err, &cerr) {
		return &installActionError{
			status:  installHTTPStatus(cerr.Class),
			code:    string(cerr.Class),
			message: cerr.Message,
		}
	}
	var aerr *action.Error
	if errors.As(err, &aerr) {
		return &installActionError{
			status:  http.StatusUnprocessableEntity,
			code:    string(aerr.Class),
			message: aerr.Message,
		}
	}
	return &installActionError{
		status:  http.StatusInternalServerError,
		code:    "internal_error",
		message: err.Error(),
	}
}
