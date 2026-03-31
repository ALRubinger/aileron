//go:build integration

package integration

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/gorillamux"
)

var (
	specRouter routers.Router
	specOnce   sync.Once
	specErr    error
)

func loadRouter() (routers.Router, error) {
	specOnce.Do(func() {
		specPath := specFilePath()
		loader := openapi3.NewLoader()
		doc, err := loader.LoadFromFile(specPath)
		if err != nil {
			specErr = err
			return
		}
		// Clear servers so validation doesn't check host/port.
		doc.Servers = nil
		// Skip doc.Validate() — kin-openapi's validator does not fully
		// support OpenAPI 3.1 and rejects valid 3.1 keywords like
		// schema-level "examples". Response validation still works.
		specRouter, specErr = gorillamux.NewRouter(doc)
	})
	return specRouter, specErr
}

// specFilePath returns the absolute path to the OpenAPI spec.
func specFilePath() string {
	if p := os.Getenv("AILERON_OPENAPI_SPEC"); p != "" {
		return p
	}
	// Default: relative to this source file -> ../../core/api/openapi.yaml
	_, thisFile, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(thisFile), "..", "..", "core", "api", "openapi.yaml")
}

// validateResponse checks that an HTTP response conforms to the OpenAPI spec.
func validateResponse(t *testing.T, resp *http.Response) {
	t.Helper()

	router, err := loadRouter()
	if err != nil {
		t.Fatalf("failed to load OpenAPI spec: %v", err)
	}

	route, pathParams, err := router.FindRoute(resp.Request)
	if err != nil {
		t.Fatalf("no matching route in spec for %s %s: %v", resp.Request.Method, resp.Request.URL.Path, err)
	}

	input := &openapi3filter.ResponseValidationInput{
		RequestValidationInput: &openapi3filter.RequestValidationInput{
			Request:    resp.Request,
			PathParams: pathParams,
			Route:      route,
		},
		Status: resp.StatusCode,
		Header: resp.Header,
		Body:   resp.Body,
	}

	if err := openapi3filter.ValidateResponse(context.Background(), input); err != nil {
		t.Errorf("response does not match OpenAPI spec: %v", err)
	}
}
