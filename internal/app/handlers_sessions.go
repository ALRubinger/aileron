package app

import (
	"errors"
	"net/http"
	"time"

	api "github.com/ALRubinger/aileron/internal/api/gen"
	"github.com/ALRubinger/aileron/internal/auth"
	"github.com/ALRubinger/aileron/internal/sessions"
)

// CreateSession registers a new launch session. The daemon mints a
// ULID-based id and stamps StartedAt; the response carries the full
// Session record. `aileron launch` posts here at agent startup.
func (s *apiServer) CreateSession(w http.ResponseWriter, r *http.Request) {
	if s.sessions == nil {
		writeError(w, http.StatusServiceUnavailable, "sessions_disabled",
			"daemon is not configured with a sessions store")
		return
	}

	var req api.CreateSessionRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if req.Agent == "" {
		writeError(w, http.StatusBadRequest, "missing_agent", "agent is required")
		return
	}

	sess := sessions.Session{
		ID:        sessions.NewID(),
		StartedAt: time.Now().UTC(),
		Agent:     req.Agent,
	}
	if req.WorkingDir != nil {
		sess.WorkingDir = *req.WorkingDir
	}

	if err := s.sessions.Put(r.Context(), sess); err != nil {
		writeError(w, http.StatusInternalServerError, "session_put_failed", err.Error())
		return
	}

	resp := api.CreateSessionResponse{
		Id:         sess.ID,
		StartedAt:  sess.StartedAt,
		EndedAt:    sess.EndedAt,
		Agent:      sess.Agent,
		WorkingDir: sess.WorkingDir,
		ExitCode:   sess.ExitCode,
	}
	// Mint a session-scoped caveat token (ADR-0024, #958) so the
	// in-container aileron-mcp reaches the daemon with only the
	// capabilities it needs, bound to this session — never the master
	// token. The issuer is nil on an unprotected daemon (no bearer
	// required), in which case caveat_token is omitted.
	if s.caveatIssuer != nil {
		caveat, err := s.caveatIssuer.Issue(sess.ID, auth.SandboxMCPCaps)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "caveat_mint_failed", err.Error())
			return
		}
		resp.CaveatToken = &caveat
	}
	writeJSON(w, http.StatusOK, resp)
}

// EndSession marks a session ended. Called by `aileron launch` on
// agent exit with the agent's exit code. Omitting exit_code stamps
// EndedAt without an exit code — the same shape the orphan-reaper
// produces on daemon restart.
func (s *apiServer) EndSession(w http.ResponseWriter, r *http.Request, sessionID string) {
	if s.sessions == nil {
		writeError(w, http.StatusServiceUnavailable, "sessions_disabled",
			"daemon is not configured with a sessions store")
		return
	}

	var req api.EndSessionRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	sess, err := s.sessions.Get(r.Context(), sessionID)
	if errors.Is(err, sessions.ErrNotFound) {
		writeError(w, http.StatusNotFound, "session_not_found",
			"no session with that id")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "session_get_failed", err.Error())
		return
	}

	now := time.Now().UTC()
	sess.EndedAt = &now
	sess.ExitCode = req.ExitCode

	if err := s.sessions.Put(r.Context(), sess); err != nil {
		writeError(w, http.StatusInternalServerError, "session_put_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, toAPISession(sess))
}

// GetSession returns one session by id, or 404 if not found.
func (s *apiServer) GetSession(w http.ResponseWriter, r *http.Request, sessionID string) {
	if s.sessions == nil {
		writeError(w, http.StatusServiceUnavailable, "sessions_disabled",
			"daemon is not configured with a sessions store")
		return
	}

	sess, err := s.sessions.Get(r.Context(), sessionID)
	if errors.Is(err, sessions.ErrNotFound) {
		writeError(w, http.StatusNotFound, "session_not_found",
			"no session with that id")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "session_get_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, toAPISession(sess))
}

// ListSessions returns sessions matching the supplied filters,
// newest-first by StartedAt.
func (s *apiServer) ListSessions(w http.ResponseWriter, r *http.Request, params api.ListSessionsParams) {
	if s.sessions == nil {
		writeError(w, http.StatusServiceUnavailable, "sessions_disabled",
			"daemon is not configured with a sessions store")
		return
	}

	filter := sessions.Filter{}
	if params.ActiveOnly != nil {
		filter.ActiveOnly = *params.ActiveOnly
	}
	if params.Agent != nil {
		filter.Agent = *params.Agent
	}
	if params.Since != nil {
		filter.Since = *params.Since
	}
	if params.Limit != nil {
		filter.Limit = *params.Limit
	}

	list, err := s.sessions.List(r.Context(), filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "session_list_failed", err.Error())
		return
	}
	items := make([]api.Session, 0, len(list))
	for _, sess := range list {
		items = append(items, toAPISession(sess))
	}
	writeJSON(w, http.StatusOK, api.SessionListResponse{Items: items})
}

// toAPISession projects a sessions.Session to the wire-format api.Session.
func toAPISession(s sessions.Session) api.Session {
	return api.Session{
		Id:         s.ID,
		StartedAt:  s.StartedAt,
		EndedAt:    s.EndedAt,
		Agent:      s.Agent,
		WorkingDir: s.WorkingDir,
		ExitCode:   s.ExitCode,
	}
}
