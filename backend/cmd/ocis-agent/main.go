// Command ocis-agent is the always-on agent sidecar.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/api"
	"github.com/owncloud/ocis-agent/backend/internal/config"
	"github.com/owncloud/ocis-agent/backend/internal/secrets"
	"github.com/owncloud/ocis-agent/backend/internal/store/sqlite"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "ocis-agent:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	migrateOnly := false
	switch {
	case len(args) == 0:
	case len(args) == 1 && args[0] == "migrate":
		migrateOnly = true
	case args[0] == "migrate":
		return errors.New("migrate takes no arguments")
	default:
		return fmt.Errorf("unknown command %q (known commands: migrate)", strings.Join(args, " "))
	}

	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}

	// The database holds app tokens (encrypted) and the audit log: keep the
	// directory private. Opening applies any pending migrations.
	if err := os.MkdirAll(filepath.Dir(cfg.SQLitePath), 0o700); err != nil {
		return fmt.Errorf("create the data directory: %w", err)
	}
	st, err := sqlite.Open(cfg.SQLitePath)
	if err != nil {
		return err
	}
	defer func() {
		if err := st.Close(); err != nil {
			slog.Error("close the store", "error", err)
		}
	}()
	if migrateOnly {
		slog.Info("migrations applied", "path", cfg.SQLitePath)
		return nil
	}

	// Create the first data-encryption key and check the master key against the
	// stored one, so a wrong AGENT_MASTER_KEY fails fast.
	sec, err := secrets.New(st, cfg.MasterKey)
	if err != nil {
		return err
	}
	if err := sec.Init(context.Background()); err != nil {
		return err
	}

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           api.NewRouter(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	slog.Info("ocis-agent listening", "addr", cfg.HTTPAddr)

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
