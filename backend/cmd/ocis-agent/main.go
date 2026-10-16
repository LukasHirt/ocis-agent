// Command ocis-agent is the always-on agent sidecar.
//
//	ocis-agent                          run the roles named by AGENT_ROLES
//	ocis-agent migrate                  apply the database migrations and exit
//	ocis-agent killswitch on|off|status set or show the kill switch
//
// Every setting comes from the environment (see internal/config).
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/owncloud/ocis-agent/backend/internal/app"
	"github.com/owncloud/ocis-agent/backend/internal/config"
	"github.com/owncloud/ocis-agent/backend/internal/store/sqlite"
)

// stdout and stderr are replaced in tests.
var stdout, stderr io.Writer = os.Stdout, os.Stderr

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "ocis-agent:", err)
		os.Exit(1)
	}
}

const (
	cmdRun        = "run"
	cmdMigrate    = "migrate"
	cmdKillSwitch = "killswitch"
)

func run(args []string) error {
	cmd, arg, err := parseArgs(args)
	if err != nil {
		return err
	}

	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}
	log := newLogger(cfg.Process.LogLevel, stderr)
	slog.SetDefault(log)

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
			log.Error("close the store", "category", "store")
		}
	}()

	switch cmd {
	case cmdMigrate:
		log.Info("migrations applied", "path", cfg.SQLitePath)
		return nil
	case cmdKillSwitch:
		// needs no usable master key: the secrets are never opened
		return app.KillSwitch(context.Background(), st, cfg.Agent.KillSwitch, arg, stdout)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	a, err := app.Build(ctx, cfg, st, app.Options{Log: log})
	if err != nil {
		return err
	}
	return a.Run(ctx)
}

// parseArgs returns the command and its argument.
func parseArgs(args []string) (cmd, arg string, err error) {
	switch {
	case len(args) == 0:
		return cmdRun, "", nil
	case args[0] == cmdMigrate:
		if len(args) != 1 {
			return "", "", errors.New("migrate takes no arguments")
		}
		return cmdMigrate, "", nil
	case args[0] == cmdKillSwitch:
		if len(args) != 2 || (args[1] != "on" && args[1] != "off" && args[1] != "status") {
			return "", "", errors.New("killswitch takes one argument: on, off or status")
		}
		return cmdKillSwitch, args[1], nil
	}
	return "", "", fmt.Errorf("unknown command %q (known commands: migrate, killswitch; settings come from the environment)", strings.Join(args, " "))
}

// newLogger logs JSON lines at level (debug, info, warn or error; validated
// by config.Load).
func newLogger(level string, w io.Writer) *slog.Logger {
	var l slog.Level
	switch level {
	case "debug":
		l = slog.LevelDebug
	case "warn":
		l = slog.LevelWarn
	case "error":
		l = slog.LevelError
	default:
		l = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: l}))
}
