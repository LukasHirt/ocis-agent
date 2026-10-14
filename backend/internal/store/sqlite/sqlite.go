// Package sqlite implements store.Store on SQLite using the pure-Go
// modernc.org/sqlite driver (no cgo).
//
// Writes go through a single connection in immediate transactions, which
// serialises them; reads use a small pool of query_only connections.
package sqlite

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/store"

	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// Option configures Open.
type Option func(*Store)

// WithClock sets the clock the store reads time from (default: time.Now in UTC).
func WithClock(now func() time.Time) Option {
	return func(s *Store) { s.now = now }
}

// Store is a SQLite-backed store.Store.
type Store struct {
	w   *sql.DB
	r   *sql.DB
	now func() time.Time
}

var _ store.Store = (*Store)(nil)

// Open opens (creating if needed) the database at path and applies all
// pending migrations.
func Open(path string, opts ...Option) (*Store, error) {
	s := &Store{now: func() time.Time { return time.Now().UTC() }}
	for _, o := range opts {
		o(s)
	}

	base := "file:" + escapePath(path) +
		"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)"

	var err error
	if s.w, err = sql.Open("sqlite", base+"&_txlock=immediate"); err != nil {
		return nil, fmt.Errorf("sqlite: open writer: %w", err)
	}
	s.w.SetMaxOpenConns(1)
	if err = s.migrate(context.Background()); err != nil {
		_ = s.w.Close()
		return nil, err
	}

	if s.r, err = sql.Open("sqlite", base+"&_pragma=query_only(1)"); err != nil {
		_ = s.w.Close()
		return nil, fmt.Errorf("sqlite: open reader: %w", err)
	}
	s.r.SetMaxOpenConns(4)
	return s, nil
}

// escapePath makes a file path safe to embed in a SQLite URI.
func escapePath(p string) string {
	return strings.NewReplacer("%", "%25", "?", "%3f", "#", "%23", " ", "%20").Replace(p)
}

// Close closes all connections.
func (s *Store) Close() error {
	var errs []error
	if s.r != nil {
		errs = append(errs, s.r.Close())
	}
	if s.w != nil {
		errs = append(errs, s.w.Close())
	}
	return errors.Join(errs...)
}

// InTx implements store.Store.
func (s *Store) InTx(ctx context.Context, fn func(store.Tx) error) error {
	sqlTx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: begin: %w", err)
	}
	defer func() {
		if p := recover(); p != nil {
			_ = sqlTx.Rollback()
			panic(p)
		}
	}()
	if err := fn(&tx{ctx: ctx, q: sqlTx, now: s.now}); err != nil {
		_ = sqlTx.Rollback()
		return err
	}
	if err := sqlTx.Commit(); err != nil {
		return fmt.Errorf("sqlite: commit: %w", err)
	}
	return nil
}

// View implements store.Store. The reader connections are query_only, so any
// write inside fn fails.
func (s *Store) View(ctx context.Context, fn func(store.Tx) error) error {
	sqlTx, err := s.r.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: begin read: %w", err)
	}
	defer func() {
		_ = sqlTx.Rollback() // read-only: nothing to commit
	}()
	return fn(&tx{ctx: ctx, q: sqlTx, now: s.now})
}

// migrate applies the embedded migrations that the database has not seen yet.
// The schema version is tracked in PRAGMA user_version and mirrored into the
// system table.
func (s *Store) migrate(ctx context.Context) error {
	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		return fmt.Errorf("sqlite: read migrations: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)

	var current int
	if err := s.w.QueryRowContext(ctx, "PRAGMA user_version").Scan(&current); err != nil {
		return fmt.Errorf("sqlite: read schema version: %w", err)
	}
	if current > len(names) {
		return fmt.Errorf("sqlite: database schema version %d is newer than this binary supports (%d)", current, len(names))
	}

	for i, name := range names {
		version := i + 1
		num, err := strconv.Atoi(strings.SplitN(name, "_", 2)[0])
		if err != nil || num != version {
			return fmt.Errorf("sqlite: migration %q must be numbered %04d_*.sql without gaps", name, version)
		}
		if version <= current {
			continue
		}
		body, err := migrationFS.ReadFile("migrations/" + name)
		if err != nil {
			return fmt.Errorf("sqlite: read migration %s: %w", name, err)
		}
		if err := s.applyMigration(ctx, name, version, string(body)); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) applyMigration(ctx context.Context, name string, version int, body string) error {
	sqlTx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: begin migration %s: %w", name, err)
	}
	fail := func(err error) error {
		_ = sqlTx.Rollback()
		return fmt.Errorf("sqlite: migration %s: %w", name, err)
	}
	if _, err := sqlTx.ExecContext(ctx, body); err != nil {
		return fail(err)
	}
	if _, err := sqlTx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", version)); err != nil {
		return fail(err)
	}
	if _, err := sqlTx.ExecContext(ctx,
		`INSERT INTO system(key, value) VALUES('schema_version', ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`, strconv.Itoa(version)); err != nil {
		return fail(err)
	}
	if err := sqlTx.Commit(); err != nil {
		return fmt.Errorf("sqlite: commit migration %s: %w", name, err)
	}
	return nil
}
