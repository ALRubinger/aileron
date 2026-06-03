package app

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestStatusWriter_HijackUnsupported(t *testing.T) {
	w := &statusWriter{ResponseWriter: httptest.NewRecorder(), status: http.StatusOK}
	if _, _, err := w.Hijack(); err == nil {
		t.Fatal("expected unsupported hijack error")
	}
}
