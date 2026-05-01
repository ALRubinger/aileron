package app

import (
	"context"
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
//     installed in the cstore. Refuse otherwise.
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

	res, herr := s.runInstallAction(r.Context(), ref, force)
	if herr != nil {
		writeError(w, herr.status, herr.code, herr.message)
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
}

// installActionError carries an HTTP-renderable failure.
type installActionError struct {
	status  int
	code    string
	message string
}

// runInstallAction is the pipeline. Split out so it can return a
// structured error type the handler maps onto an HTTP envelope.
func (s *apiServer) runInstallAction(ctx context.Context, ref cstore.Ref, force bool) (*installActionResult, *installActionError) {
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
	if tb.SignaturePresent() {
		if err := s.installer.Verifier.Verify(ref.FQN.Authority(), nil, tb.Manifest, tb.Signature); err != nil {
			return nil, classifyInstallActionErr(err)
		}
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
	missing := []string{}
	for _, c := range manifest.Requires.Connectors {
		fqn, perr := cstore.ParseFQN(c.Name)
		if perr != nil {
			return nil, &installActionError{
				status:  http.StatusBadRequest,
				code:    string(cstore.ClassValidationError),
				message: fmt.Sprintf("requires.connectors[%s]: %s", c.Name, perr.Error()),
			}
		}
		if _, _, ok := s.installer.Store.LookupAnyVersion(fqn); !ok {
			missing = append(missing, c.Name)
		}
	}
	if len(missing) > 0 {
		return nil, &installActionError{
			status: http.StatusUnprocessableEntity,
			code:   string(cstore.ClassValidationError),
			message: fmt.Sprintf(
				"action requires connectors that are not installed: %v — install them first via `aileron connector install <FQN>`",
				missing),
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

	if s.auditRecorder != nil {
		s.auditRecorder.RecordSuccess(ctx, model.EventTypeActionInstalled,
			model.ActorRef{Type: model.ActorTypeHuman, ID: "user"},
			map[string]any{
				"name":    manifest.Name,
				"fqn":     ref.FQN.String(),
				"version": ref.Version,
				"source":  manifest.Source,
				"path":    dest,
			})
	}

	connectorFQNs := make([]string, 0, len(manifest.Requires.Connectors))
	for _, c := range manifest.Requires.Connectors {
		connectorFQNs = append(connectorFQNs, c.Name)
	}

	return &installActionResult{
		Name:             manifest.Name,
		Path:             dest,
		Source:           manifest.Source,
		AlreadyInstalled: already,
		ConnectorFQNs:    connectorFQNs,
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
