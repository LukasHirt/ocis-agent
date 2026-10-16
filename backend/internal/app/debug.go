package app

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/store"
)

// readyTimeout bounds the store check of /readyz.
const readyTimeout = 2 * time.Second

// debugHandler serves exactly GET /healthz and GET /readyz, without
// authentication: they reveal nothing but up or down. Every other path is a
// 404, so the debug port never exposes the API or the extension's routes.
func (a *App) debugHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeStatus(w, http.StatusOK, `{"status":"ok"}`)
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if a.ready(r.Context()) {
			writeStatus(w, http.StatusOK, `{"status":"ready"}`)
			return
		}
		writeStatus(w, http.StatusServiceUnavailable, `{"status":"not ready"}`)
	})
	return mux
}

func writeStatus(w http.ResponseWriter, code int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_, _ = w.Write([]byte(body + "\n"))
}

// ready reports whether the process can do its work: the shutdown has not
// begun, the secrets service is initialised, the store answers a read and, with the worker role, at least
// one worker goroutine runs.
func (a *App) ready(ctx context.Context) bool {
	if a.stopping.Load() || !a.secretsReady.Load() {
		return false
	}
	if a.cfg.Process.Has(RoleWorker) && a.alive.Load() < 1 {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, readyTimeout)
	defer cancel()
	err := a.st.View(ctx, func(tx store.Tx) error {
		_, err := tx.GetSystem("instance_id")
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	})
	return err == nil
}
