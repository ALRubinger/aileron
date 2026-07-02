package app

import (
	"net/http"

	api "github.com/ALRubinger/aileron/internal/api/gen"
	"github.com/ALRubinger/aileron/internal/audit"
	"github.com/ALRubinger/aileron/internal/model"
)

// defaultAuditListLimit mirrors the OpenAPI spec's `limit.default`.
const defaultAuditListLimit = 100

// ListAudit returns events from the audit recorder, newest-first,
// optionally filtered by `since`, `audit_id`, `connector_fqn`,
// `class`, `output_name`, and `content_hash`. Per ADR-0010 the
// recorder is in-memory in v1; events are scoped to the running
// daemon process.
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
	if params.OutputName != nil {
		filter.OutputName = *params.OutputName
	}
	if params.ContentHash != nil {
		filter.ContentHash = *params.ContentHash
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

// CreateAudit appends a single audit event via the recorder and returns
// the minted audit_id (201). The daemon is the single writer and single
// id-minter, so clients (the in-process Flight-Plan launch runtime) POST
// here rather than writing the store directly.
//
// The generated std-http server does no runtime schema validation, so the
// handler enforces the contract: an unknown `actor.type` is rejected with
// 400, and a missing audit subsystem returns 503 (a write that must
// persist cannot be silently acknowledged).
func (s *apiServer) CreateAudit(w http.ResponseWriter, r *http.Request) {
	if s.auditRecorder == nil || s.auditStore == nil {
		writeError(w, http.StatusServiceUnavailable, "audit_unavailable",
			"audit subsystem is not configured; cannot persist the event")
		return
	}

	var req api.AuditIngestRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "malformed request body: "+err.Error())
		return
	}
	if req.EventType == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "event_type is required")
		return
	}

	actorType, ok := normalizeActorType(req.Actor.Type)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_request",
			"actor.type must be one of agent, human, service, connector_runtime")
		return
	}

	payload := req.Payload
	if payload == nil {
		payload = map[string]any{}
	}
	actor := model.ActorRef{Type: actorType, ID: req.Actor.Id}
	id := s.auditRecorder.RecordSuccess(r.Context(), model.EventType(req.EventType), actor, payload)
	writeJSON(w, http.StatusCreated, api.AuditIngestResponse{AuditId: id})
}

// normalizeActorType maps a request-supplied actor type string onto the
// model.ActorType enum, reporting false for any value outside the four
// known kinds. An arbitrary string is never cast into ActorType.
func normalizeActorType(t api.ActorRefType) (model.ActorType, bool) {
	switch model.ActorType(t) {
	case model.ActorTypeAgent, model.ActorTypeHuman, model.ActorTypeService, model.ActorTypeConnectorRuntime:
		return model.ActorType(t), true
	default:
		return "", false
	}
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
