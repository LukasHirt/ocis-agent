package app

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/api"
)

const (
	// readHeaderTimeout bounds the request headers on both listeners.
	readHeaderTimeout = 10 * time.Second
	// apiShutdownTimeout bounds the graceful stop of the API listener.
	apiShutdownTimeout = 10 * time.Second
	// finalWait bounds the wait for the workers (and the scheduler) after
	// their jobs were cancelled.
	finalWait = 30 * time.Second
	// debugShutdownTimeout bounds the stop of the debug listener.
	debugShutdownTimeout = 5 * time.Second
)

var errWorkersStuck = errors.New("app: the workers did not stop in time")

// Run starts the selected roles and serves until ctx ends or a listener
// fails, then shuts down in three phases:
//
//  1. stop claiming jobs, stop the scheduler and stop the API listener;
//  2. wait up to Process.ShutdownDrain for the running jobs to finish;
//  3. cancel the jobs still running (the workers requeue them with the
//     shutdown delay) and wait for the workers to return.
//
// The debug listener keeps answering until the end. Run returns the listener
// error that ended it, if any; it never closes the store. It may be called
// once.
func (a *App) Run(ctx context.Context) error {
	if !a.ran.CompareAndSwap(false, true) {
		return errors.New("app: Run may be called only once")
	}
	defer a.stopJobs()
	defer a.hc.CloseIdleConnections()

	base := context.WithoutCancel(ctx)
	claimCtx, stopClaims := context.WithCancel(base)
	defer stopClaims()
	schedCtx, stopScheduler := context.WithCancel(base)
	defer stopScheduler()

	fatal := make(chan error, 2)
	var servers sync.WaitGroup
	defer servers.Wait()

	debugSrv, err := a.listen(a.cfg.Process.DebugAddr, a.debugHandler(), &a.debugAddr, &servers, fatal)
	if err != nil {
		a.log.Error("app: debug listener failed", "category", "listen")
		return err
	}
	defer a.shutdownDebug(debugSrv)
	var apiSrv *http.Server
	if a.cfg.Process.Has(RoleAPI) {
		if apiSrv, err = a.listen(a.cfg.HTTPAddr, api.NewRouter(), &a.apiAddr, &servers, fatal); err != nil {
			a.log.Error("app: api listener failed", "category", "listen")
			return err
		}
		a.log.Info("app: api listening", "addr", a.APIAddr())
	}
	if a.beforeWorkers != nil {
		a.beforeWorkers()
	}

	workersDone := a.startWorkers(claimCtx)
	schedDone := make(chan struct{})
	if a.scheduler != nil {
		go func() {
			defer close(schedDone)
			a.scheduler.Run(schedCtx)
		}()
	} else {
		close(schedDone)
	}
	close(a.started)
	a.log.Info("app: started", "roles", len(a.cfg.Process.Roles), "workers", len(a.workers))

	var runErr error
	select {
	case <-ctx.Done():
	case runErr = <-fatal:
		a.log.Error("app: listener stopped", "category", "serve")
	}

	// phase 1: no new work
	stopClaims()
	stopScheduler()
	if apiSrv != nil {
		sctx, cancel := context.WithTimeout(base, apiShutdownTimeout)
		if err := apiSrv.Shutdown(sctx); err != nil {
			_ = apiSrv.Close()
		}
		cancel()
		a.setAddr(&a.apiAddr, "")
	}
	// phase 2: let the running jobs finish
	if !waitFor(workersDone, a.cfg.Process.ShutdownDrain) {
		// phase 3: cancel them; the workers requeue them
		a.log.Info("app: drain over, cancelling running jobs")
		a.stopJobs()
		if !waitFor(workersDone, finalWait) {
			a.log.Error("app: workers did not stop", "category", "timeout")
			runErr = errors.Join(runErr, errWorkersStuck)
		}
	}
	if !waitFor(schedDone, finalWait) {
		a.log.Error("app: scheduler did not stop", "category", "timeout")
		runErr = errors.Join(runErr, errors.New("app: the scheduler did not stop in time"))
	}
	a.log.Info("app: stopped")
	return runErr
}

// listen binds addr and serves h on it; addr "" means disabled (nil server).
// A serve error other than a regular close is sent to fatal.
func (a *App) listen(addr string, h http.Handler, bound *string, wg *sync.WaitGroup, fatal chan<- error) (*http.Server, error) {
	if addr == "" {
		return nil, nil
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: readHeaderTimeout}
	a.setAddr(bound, ln.Addr().String())
	wg.Go(func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fatal <- err
		}
	})
	return srv, nil
}

func (a *App) shutdownDebug(srv *http.Server) {
	if srv == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), debugShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		_ = srv.Close()
	}
	a.setAddr(&a.debugAddr, "")
}

func (a *App) setAddr(dst *string, v string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	*dst = v
}

// startWorkers runs every worker with ctx as its claim context (their jobs
// run under a.jobCtx) and returns a channel closed when all have returned.
func (a *App) startWorkers(ctx context.Context) <-chan struct{} {
	done := make(chan struct{})
	var wg sync.WaitGroup
	for _, w := range a.workers {
		a.alive.Add(1)
		wg.Go(func() {
			defer a.alive.Add(-1)
			w.Run(ctx)
		})
	}
	go func() {
		wg.Wait()
		close(done)
	}()
	return done
}

// waitFor waits up to d for done; it reports whether done was closed.
func waitFor(done <-chan struct{}, d time.Duration) bool {
	select {
	case <-done:
		return true
	default:
	}
	if d <= 0 {
		return false
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-done:
		return true
	case <-t.C:
		return false
	}
}
