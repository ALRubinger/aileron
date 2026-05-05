package app

import (
	"net/http"

	api "github.com/ALRubinger/aileron/internal/api/gen"
	"github.com/ALRubinger/aileron/internal/audit"
)

// defaultAuditListLimit mirrors the OpenAPI spec's `limit.default`.
const defaultAuditListLimit = 100

// ListAudit returns events from the audit recorder, newest-first,
// optionally filtered by `since`, `audit_id`, `connector_fqn`, and
// `class`. Per ADR-0010 the recorder is in-memory in v1; events are
// scoped to the running daemon process.
func (s *apiServer) ListAudit(w http.ResponseWriter, r *http.Request, params api.ListAuditParams) {
	if s.auditStore == nil {
		writeJSON(w, http.StatusOK, api.AuditListResponse{Events: []api.AuditEvent{}})
		return
	}

	filter := audit.EventFilter{Limit: defaultAuditListLimit}
	if params.Since != nil {
		filter.Since = *params.Since
	}
	if params.AuditId != nil {
		filter.EventID = *params.AuditId
	}
	if params.ConnectorFqn != nil {
		filter.ConnectorFQN = *params.ConnectorFqn
	}
	if params.Class != nil {
		filter.Class = *params.Class
	}
	if params.Limit != nil && *params.Limit > 0 {
		filter.Limit = *params.Limit
	}

	events, err := s.auditStore.ListEvents(r.Context(), filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "audit_list_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, api.AuditListResponse{Events: toAPIAuditEvents(events)})
}

// GetAudit looks up a single audit event by its id, returning 404
// when no event with that id is in the store.
func (s *apiServer) GetAudit(w http.ResponseWriter, _ *http.Request, auditID string) {
	if s.auditStore == nil {
		writeError(w, http.StatusNotFound, "not_found", "audit event not found")
		return
	}
	ev, ok := s.auditStore.GetByEventID(auditID)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "audit event not found")
		return
	}
	writeJSON(w, http.StatusOK, toAPIAuditEvent(ev))
}

func toAPIAuditEvents(events []audit.Event) []api.AuditEvent {
	out := make([]api.AuditEvent, 0, len(events))
	for _, e := range events {
		out = append(out, toAPIAuditEvent(e))
	}
	return out
}

func toAPIAuditEvent(e audit.Event) api.AuditEvent {
	payload := e.Payload
	if payload == nil {
		payload = map[string]any{}
	}
	out := api.AuditEvent{
		AuditId:   e.EventID,
		EventType: string(e.EventType),
		Timestamp: e.Timestamp,
		Payload:   payload,
	}
	out.Actor.Type = string(e.Actor.Type)
	out.Actor.Id = e.Actor.ID
	return out
}
