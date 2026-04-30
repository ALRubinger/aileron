package app

import "net/http"

// PostChatCompletions handles POST /v1/chat/completions.
//
// Stage 1: scaffolding only. The endpoint is registered so an agent
// pointing its base URL at this server discovers it, but the gateway
// (proxy, tool augmentation, interception, bypass, streaming) is not
// yet implemented. Returns 501 Not Implemented per the OpenAPI spec.
func (s *apiServer) PostChatCompletions(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotImplemented, "not_implemented",
		"chat completions gateway is not yet implemented; see issue #356")
}

// PostMessages handles POST /v1/messages.
//
// Stage 1: scaffolding only. See PostChatCompletions for the staged
// rollout plan.
func (s *apiServer) PostMessages(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotImplemented, "not_implemented",
		"messages gateway is not yet implemented; see issue #356")
}
