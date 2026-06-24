package resolver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHTTPFetcherDecodesActions(t *testing.T) {
	var sawAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization")
		if r.URL.Path != "/actions" {
			t.Errorf("path = %q, want /actions", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[{"name":"query-series","requires":{"connectors":[{"name":"github://aileron/metrics"}]}}]}`))
	}))
	defer srv.Close()

	f := HTTPFetcher{
		BaseURL: srv.URL,
		Authorize: func(req *http.Request) {
			req.Header.Set("Authorization", "Bearer test-token")
		},
	}
	actions, err := f.FetchActions(context.Background())
	if err != nil {
		t.Fatalf("FetchActions: %v", err)
	}
	if len(actions) != 1 || actions[0].Name != "query-series" {
		t.Fatalf("decoded actions = %+v", actions)
	}
	if actions[0].Requires.Connectors[0].Name != "github://aileron/metrics" {
		t.Errorf("connector not decoded: %+v", actions[0].Requires.Connectors)
	}
	if sawAuth != "Bearer test-token" {
		t.Errorf("Authorize hook not applied; saw %q", sawAuth)
	}
}

func TestHTTPFetcherNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer srv.Close()

	f := HTTPFetcher{BaseURL: srv.URL}
	if _, err := f.FetchActions(context.Background()); err == nil {
		t.Error("expected error on non-200 response")
	}
}
