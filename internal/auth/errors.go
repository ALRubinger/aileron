package auth

import (
	"encoding/json"
	"net/http"
)

// writeError emits an api.Error-compatible structured envelope:
//
//	{"error": {"code": "<code>", "message": "<prose>"}}
//
// Auth handlers historically used `http.Error(w, "{\"error\": ...}",
// status)` with a string-valued `error` key — incompatible with the
// rest of the CRUD API. ADR-0010 (#365) ratified the envelope shape
// for action/gateway responses; auth stays on the existing CRUD
// envelope but is normalized here so all v1 endpoints emit the same
// nested-object shape.
func writeError(w http.ResponseWriter, status int, code, message string) {
	body, _ := json.Marshal(struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}{Error: struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}{Code: code, Message: message}})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
