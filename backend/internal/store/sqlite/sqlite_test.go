package sqlite

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/store"
	"github.com/owncloud/ocis-agent/backend/internal/store/storetest"
)

func newStore(t *testing.T, now func() time.Time) store.Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "agent.db"), WithClock(now))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func openSQLite(t *testing.T) *Store {
	t.Helper()
	return newStore(t, storetest.NewClock().Now).(*Store)
}

func TestConformance(t *testing.T) {
	storetest.Run(t, newStore)
}

func TestOpenAppliesPragmas(t *testing.T) {
	s := openSQLite(t)
	cases := []struct {
		db   *sql.DB
		name string
		want string
	}{
		{s.w, "journal_mode", "wal"},
		{s.w, "foreign_keys", "1"},
		{s.w, "busy_timeout", "5000"},
		{s.w, "synchronous", "2"},
		{s.r, "foreign_keys", "1"},
		{s.r, "query_only", "1"},
	}
	for _, c := range cases {
		var got string
		if err := c.db.QueryRow("PRAGMA " + c.name).Scan(&got); err != nil {
			t.Fatalf("PRAGMA %s: %v", c.name, err)
		}
		if got != c.want {
			t.Errorf("PRAGMA %s = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestViewRejectsWrites(t *testing.T) {
	s := openSQLite(t)
	err := s.View(context.Background(), func(tx store.Tx) error {
		return tx.SetSystem("k", "v")
	})
	if err == nil {
		t.Fatal("a write inside View succeeded")
	}
}

func TestSchemaVersionMatchesUserVersion(t *testing.T) {
	s := openSQLite(t)
	var v int
	if err := s.w.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v < 1 {
		t.Fatalf("user_version = %d, want >= 1", v)
	}
	var got string
	err := s.View(context.Background(), func(tx store.Tx) error {
		var err error
		got, err = tx.GetSystem("schema_version")
		return err
	})
	if err != nil || got != strconv.Itoa(v) {
		t.Fatalf("system schema_version = %q (%v), want %d", got, err, v)
	}
}

func TestMigrationsAreIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	var v1 int
	_ = s.w.QueryRow("PRAGMA user_version").Scan(&v1)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer s.Close()
	var v2 int
	_ = s.w.QueryRow("PRAGMA user_version").Scan(&v2)
	if v1 != v2 {
		t.Fatalf("user_version changed on reopen: %d -> %d", v1, v2)
	}
}

func TestOpenRejectsNewerSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()

	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec("PRAGMA user_version = 9999"); err != nil {
		t.Fatal(err)
	}
	_ = raw.Close()

	if _, err := Open(path); err == nil || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("Open of a newer schema: err = %v, want a 'newer' error", err)
	}
}

func TestOpenPathWithSpecialCharacters(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "dir with space", "q?#%x")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "agent.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open(%q): %v", path, err)
	}
	defer s.Close()
	if err := s.InTx(context.Background(), func(tx store.Tx) error { return tx.SetSystem("k", "v") }); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("database file is not at the requested path: %v", err)
	}
}

func TestPanicInTxRollsBackAndPoolStaysUsable(t *testing.T) {
	s := openSQLite(t)
	func() {
		defer func() {
			if recover() == nil {
				t.Error("the panic was swallowed")
			}
		}()
		_ = s.InTx(context.Background(), func(tx store.Tx) error {
			_ = tx.SetSystem("k", "v")
			panic("boom")
		})
	}()
	err := s.InTx(context.Background(), func(tx store.Tx) error {
		if _, err := tx.GetSystem("k"); err == nil {
			t.Error("write from the panicked transaction is visible")
		}
		return tx.SetSystem("k2", "v2")
	})
	if err != nil {
		t.Fatalf("store unusable after panic: %v", err)
	}
}

func TestDatabaseFilesArePrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.InTx(context.Background(), func(tx store.Tx) error { return tx.SetSystem("k", "v") }); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{path, path + "-wal"} {
		info, err := os.Stat(p)
		if err != nil {
			if p != path && os.IsNotExist(err) {
				continue
			}
			t.Fatal(err)
		}
		if info.Mode().Perm()&0o077 != 0 {
			t.Errorf("%s has mode %v, want no group/other access", filepath.Base(p), info.Mode().Perm())
		}
	}
}
