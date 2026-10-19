// Package api is the sidecar's HTTP surface. All routes live under /agent-api/.
package api

import (
	"net/http"
)

// NewRouter returns a handler that serves only GET /agent-api/healthz. It is
// what the api role serves until the process wires NewServer.
func NewRouter() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /agent-api/healthz", healthz)
	return mux
}
