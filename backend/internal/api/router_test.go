package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHealthz(t *testing.T) {
	rec := httptest.NewRecorder()
	NewRouter().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/agent-api/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != `{"status":"ok"}`+"\n" {
		t.Errorf("body = %q", got)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
}

// The extension's UI routes live under /agent/ and must reach oCIS Web, so the
// sidecar must never answer there.
func TestSidecarDoesNotAnswerUnderAgentUIPrefix(t *testing.T) {
	for _, path := range []string{"/agent/", "/agent/chat", "/agent/approvals", "/agent/healthz", "/healthz"} {
		rec := httptest.NewRecorder()
		NewRouter().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, rec.Code)
		}
	}
}

func TestHealthzRejectsOtherMethods(t *testing.T) {
	rec := httptest.NewRecorder()
	NewRouter().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/agent-api/healthz", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST status = %d, want 405", rec.Code)
	}
}
