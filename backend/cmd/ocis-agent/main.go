// Command ocis-agent is the always-on agent sidecar.
//
//	ocis-agent                          run the roles named by AGENT_ROLES
//	ocis-agent migrate                  apply the database migrations and exit
//	ocis-agent killswitch on|off|status set or show the kill switch
//	ocis-agent backup <dest-file>       write a verified snapshot of the database
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
	cmdBackup     = "backup"
)

func run(args []string) error {
	cmd, arg, err := parseArgs(args)
	if err != nil {
		return err
	}
	if cmd == cmdBackup {
		// reads only the database location: no master key, no oCIS URL
		return backup(config.DatabasePath(os.Getenv), arg)
	}

	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}
	log := newLogger(cfg.Process.LogLevel, stderr)
	slog.SetDefault(log)

	if cmd == cmdKillSwitch {
		// never create a database here: a switch set in a database nobody
		// reads (a wrong AGENT_DATA_DIR) would be reported as success
		if fi, err := os.Stat(cfg.SQLitePath); err != nil || !fi.Mode().IsRegular() {
			return fmt.Errorf("no database at %s: check AGENT_DATA_DIR / AGENT_SQLITE_PATH (run migrate to create one)", cfg.SQLitePath)
		}
	} else {
		// The database holds app tokens (encrypted) and the audit log: keep
		// the directory private. Opening applies any pending migrations.
		if err := os.MkdirAll(filepath.Dir(cfg.SQLitePath), 0o700); err != nil {
			return fmt.Errorf("create the data directory: %w", err)
		}
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
	// after the first signal the default behaviour returns, so a second
	// SIGINT or SIGTERM ends the process at once instead of waiting for the
	// drain
	context.AfterFunc(ctx, stop)
	a, err := app.Build(ctx, cfg, st, app.Options{Log: log})
	if err != nil {
		return err
	}
	return a.Run(ctx)
}

// backup writes a snapshot of the database at src to dest. It never creates
// or migrates the database and never opens the secrets: the snapshot holds
// them sealed, and restoring it needs the same AGENT_MASTER_KEY.
func backup(src, dest string) error {
	info, err := sqlite.Backup(context.Background(), src, dest)
	if errors.Is(err, sqlite.ErrNoDatabase) {
		return fmt.Errorf("no database at %s: check AGENT_DATA_DIR / AGENT_SQLITE_PATH", src)
	}
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "backup written to %s (schema version %d)\n", dest, info.SchemaVersion)
	return err
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
	case args[0] == cmdBackup:
		if len(args) != 2 || args[1] == "" {
			return "", "", errors.New("backup takes one argument: the file to write (it must not exist)")
		}
		return cmdBackup, args[1], nil
	}
	return "", "", fmt.Errorf("unknown command %q (known commands: migrate, killswitch, backup; settings come from the environment)", strings.Join(args, " "))
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
