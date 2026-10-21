package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/owncloud/ocis-agent/backend/internal/audit"
	"github.com/owncloud/ocis-agent/backend/internal/store"
)

func appendEvents(t *testing.T, s *Store, user string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if err := s.InTx(context.Background(), func(tx store.Tx) error {
			_, err := tx.AppendAudit(audit.Event{UserID: user, Kind: audit.KindToolCall, Tool: "files.list", Risk: "read", Cred: "app", Outcome: "ok"})
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func countAudit(t *testing.T, s *Store, user string) int64 {
	t.Helper()
	var n int64
	if err := s.View(context.Background(), func(tx store.Tx) error {
		var err error
		n, err = tx.VerifyAudit(user)
		return err
	}); err != nil {
		t.Fatalf("VerifyAudit: %v", err)
	}
	return n
}

// The snapshot is consistent while writers keep committing: it opens, passes
// the integrity check, and every user's chain verifies.
func TestBackupUnderConcurrentWrites(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "agent.db")
	s, err := Open(src)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	appendEvents(t, s, "u1", 50)

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	var written atomic.Int64
	for _, u := range []string{"u1", "u2", "u3"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				if err := s.InTx(ctx, func(tx store.Tx) error {
					_, err := tx.AppendAudit(audit.Event{UserID: u, Kind: audit.KindToolCall, Tool: "files.list", Risk: "read", Cred: "app", Outcome: "ok"})
					return err
				}); err == nil {
					written.Add(1)
				}
			}
		}()
	}
	var dests []string
	for i := 0; i < 3; i++ {
		for written.Load() < int64(20*(i+1)) {
			runtime.Gosched()
		}
		dest := filepath.Join(t.TempDir(), "backup.db")
		info, err := Backup(context.Background(), src, dest)
		if err != nil {
			cancel()
			wg.Wait()
			t.Fatalf("Backup: %v", err)
		}
		if info.SchemaVersion != supportedVersion(t) {
			t.Errorf("SchemaVersion = %d, want %d", info.SchemaVersion, supportedVersion(t))
		}
		dests = append(dests, dest)
	}
	cancel()
	wg.Wait()

	for _, dest := range dests {
		b, err := Open(dest)
		if err != nil {
			t.Fatalf("open the backup: %v", err)
		}
		if n := countAudit(t, b, "u1"); n < 50 {
			t.Errorf("u1 chain in the backup has %d events, want >= 50", n)
		}
		countAudit(t, b, "u2")
		countAudit(t, b, "u3")
		_ = b.Close()
	}
}

func supportedVersion(t *testing.T) int {
	t.Helper()
	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	return len(entries)
}

func TestBackupRefusesAnExistingDestination(t *testing.T) {
	src := filepath.Join(t.TempDir(), "agent.db")
	s, err := Open(src)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	for _, content := range []string{"", "precious"} {
		dest := filepath.Join(t.TempDir(), "backup.db")
		if err := os.WriteFile(dest, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Backup(context.Background(), src, dest); !errors.Is(err, os.ErrExist) {
			t.Errorf("existing %q destination: err = %v, want os.ErrExist", content, err)
		}
		if got, _ := os.ReadFile(dest); string(got) != content {
			t.Errorf("the existing destination was changed to %q", got)
		}
	}
}

func TestBackupIsPrivate(t *testing.T) {
	src := filepath.Join(t.TempDir(), "agent.db")
	s, err := Open(src)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	appendEvents(t, s, "u1", 3)
	dir := t.TempDir()
	dest := filepath.Join(dir, "backup.db")
	if _, err := Backup(context.Background(), src, dest); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(dest)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("backup mode = %v, want 0600", fi.Mode().Perm())
	}
	// nothing but the backup is left behind (no journal, no temp file)
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		names := []string{}
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("destination directory holds %v, want only backup.db", names)
	}
}

func TestBackupRefusesAMissingDatabase(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "agent.db")
	dest := filepath.Join(dir, "backup.db")
	if _, err := Backup(context.Background(), src, dest); !errors.Is(err, ErrNoDatabase) {
		t.Errorf("missing source: err = %v, want ErrNoDatabase", err)
	}
	if err := os.Mkdir(src, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Backup(context.Background(), src, dest); !errors.Is(err, ErrNoDatabase) {
		t.Errorf("directory as source: err = %v, want ErrNoDatabase", err)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Errorf("a destination was created: %v", err)
	}
	entries, _ := os.ReadDir(src)
	if len(entries) != 0 {
		t.Errorf("files were created in the source path")
	}
}

// The source is never migrated or created: a database with an older schema
// stays at its version, and an empty file is not a database.
func TestBackupDoesNotMigrateTheSource(t *testing.T) {
	src := filepath.Join(t.TempDir(), "agent.db")
	s, err := Open(src)
	if err != nil {
		t.Fatal(err)
	}
	old := supportedVersion(t) - 1
	// undo the latest migration (0016)
	for _, q := range []string{
		`DROP TABLE job_partials`,
		`PRAGMA user_version = 15`,
		`UPDATE system SET value = '15' WHERE key = 'schema_version'`,
	} {
		if _, err := s.w.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	_ = s.Close()

	dest := filepath.Join(t.TempDir(), "backup.db")
	info, err := Backup(context.Background(), src, dest)
	if err != nil {
		t.Fatal(err)
	}
	if info.SchemaVersion != old {
		t.Errorf("SchemaVersion = %d, want %d", info.SchemaVersion, old)
	}
	if v := userVersion(t, src); v != old {
		t.Errorf("source user_version = %d after backup, want %d", v, old)
	}

	empty := filepath.Join(t.TempDir(), "agent.db")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	dest2 := filepath.Join(t.TempDir(), "backup.db")
	if _, err := Backup(context.Background(), empty, dest2); err == nil {
		t.Error("an empty file was backed up")
	}
	if _, err := os.Stat(dest2); !os.IsNotExist(err) {
		t.Errorf("a failed backup left its destination: %v", err)
	}
}

func userVersion(t *testing.T, path string) int {
	t.Helper()
	db, err := openReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var v int
	if err := db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// A corrupt source is detected and leaves no destination behind.
func TestBackupDetectsACorruptSource(t *testing.T) {
	src := filepath.Join(t.TempDir(), "agent.db")
	s, err := Open(src)
	if err != nil {
		t.Fatal(err)
	}
	appendEvents(t, s, "u1", 400)
	if _, err := s.w.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	corrupt(t, src)

	dest := filepath.Join(t.TempDir(), "backup.db")
	if _, err := Backup(context.Background(), src, dest); err == nil {
		t.Fatal("a corrupt database was backed up")
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Errorf("a failed backup left its destination: %v", err)
	}
}

// Verify rejects a damaged backup file and a newer or foreign schema.
func TestVerifyDetectsADamagedBackup(t *testing.T) {
	src := filepath.Join(t.TempDir(), "agent.db")
	s, err := Open(src)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	appendEvents(t, s, "u1", 400)
	dest := filepath.Join(t.TempDir(), "backup.db")
	if _, err := Backup(context.Background(), src, dest); err != nil {
		t.Fatal(err)
	}
	if v, err := Verify(context.Background(), dest); err != nil || v != supportedVersion(t) {
		t.Fatalf("Verify of a good backup = %d, %v", v, err)
	}
	corrupt(t, dest)
	if _, err := Verify(context.Background(), dest); err == nil {
		t.Error("Verify accepted a corrupt backup")
	}

	// a newer schema than this binary knows
	newer := filepath.Join(t.TempDir(), "backup.db")
	if _, err := Backup(context.Background(), src, newer); err != nil {
		t.Fatal(err)
	}
	setVersion(t, newer, supportedVersion(t)+1)
	if _, err := Verify(context.Background(), newer); err == nil {
		t.Error("Verify accepted a newer schema")
	}
	// user_version and the system table disagree
	split := filepath.Join(t.TempDir(), "backup.db")
	if _, err := Backup(context.Background(), src, split); err != nil {
		t.Fatal(err)
	}
	setVersion(t, split, supportedVersion(t)-1)
	if _, err := Verify(context.Background(), split); err == nil {
		t.Error("Verify accepted a schema version mismatch")
	}
}

func setVersion(t *testing.T, path string, v int) {
	t.Helper()
	db, err := openReadWrite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec("PRAGMA user_version = " + strconv.Itoa(v)); err != nil {
		t.Fatal(err)
	}
}

// corrupt overwrites the middle pages of a database file with garbage.
func corrupt(t *testing.T, path string) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() < 8*4096 {
		t.Fatalf("database too small to corrupt: %d bytes", fi.Size())
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	garbage := make([]byte, 4096*2)
	for i := range garbage {
		garbage[i] = byte(i*7 + 3)
	}
	if _, err := f.WriteAt(garbage, fi.Size()/2/4096*4096); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func openReadWrite(path string) (*sql.DB, error) {
	return sql.Open("sqlite", "file:"+escapePath(path)+"?mode=rw")
}
