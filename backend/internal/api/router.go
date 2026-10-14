// Package api is the sidecar's HTTP surface. All routes live under /agent-api/.
package api

import (
	"net/http"
)

// NewRouter returns the sidecar's HTTP handler.
func NewRouter() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /agent-api/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}` + "\n"))
	})
	return mux
}
