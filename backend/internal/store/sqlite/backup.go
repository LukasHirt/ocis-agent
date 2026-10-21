package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

// ErrNoDatabase is returned by Backup and Verify when the path is not an
// existing regular file.
var ErrNoDatabase = errors.New("sqlite: no database file")

// BackupInfo describes a written backup.
type BackupInfo struct {
	// SchemaVersion is the migration version of the snapshot.
	SchemaVersion int
}

// Backup writes a consistent snapshot of the database at src to dest with
// VACUUM INTO, which reads one transaction, so it is safe while another
// process writes. The source is opened read-only: it is never created or
// migrated. dest must not exist (os.ErrExist otherwise); it is created with
// mode 0600, synced to disk, and verified (integrity check and schema
// version equal to the source's) before Backup returns. On any failure dest
// is removed again. The snapshot holds the sealed secrets as stored; the
// master key is not needed and never read.
func Backup(ctx context.Context, src, dest string) (BackupInfo, error) {
	srcVersion, err := checkDatabase(ctx, src)
	if err != nil {
		return BackupInfo{}, err
	}
	abs, err := filepath.Abs(dest)
	if err != nil {
		return BackupInfo{}, fmt.Errorf("sqlite: backup destination: %w", err)
	}
	// claim the name: O_EXCL refuses an existing file (or a symlink)
	fh, err := os.OpenFile(abs, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return BackupInfo{}, fmt.Errorf("sqlite: create the backup file: %w", err)
	}
	_ = fh.Close()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(abs)
			_ = os.Remove(abs + "-journal")
		}
	}()

	if err := vacuumInto(ctx, src, abs); err != nil {
		return BackupInfo{}, err
	}
	if err := syncFile(abs); err != nil {
		return BackupInfo{}, err
	}
	version, err := Verify(ctx, abs)
	if err != nil {
		return BackupInfo{}, err
	}
	if version != srcVersion {
		return BackupInfo{}, fmt.Errorf("sqlite: the backup has schema version %d, the source %d", version, srcVersion)
	}
	if err := syncFile(filepath.Dir(abs)); err != nil {
		return BackupInfo{}, err
	}
	ok = true
	return BackupInfo{SchemaVersion: version}, nil
}

// Verify opens the database at path read-only, runs PRAGMA integrity_check
// and checks that its schema version is one this binary knows and matches
// the version recorded in the system table. It returns the schema version.
func Verify(ctx context.Context, path string) (int, error) {
	return checkDatabase(ctx, path)
}

func checkDatabase(ctx context.Context, path string) (int, error) {
	fi, err := os.Lstat(path)
	if err != nil || !fi.Mode().IsRegular() {
		return 0, fmt.Errorf("%w at %s", ErrNoDatabase, path)
	}
	db, err := openReadOnly(path)
	if err != nil {
		return 0, err
	}
	defer func() { _ = db.Close() }()

	var version int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return 0, fmt.Errorf("sqlite: read the schema version of %s: %w", path, err)
	}
	supported, err := migrationCount()
	if err != nil {
		return 0, err
	}
	if version < 1 || version > supported {
		return 0, fmt.Errorf("sqlite: %s has schema version %d, this binary supports 1 to %d", path, version, supported)
	}
	var recorded string
	if err := db.QueryRowContext(ctx, `SELECT value FROM system WHERE key = 'schema_version'`).Scan(&recorded); err != nil {
		return 0, fmt.Errorf("sqlite: read the recorded schema version of %s: %w", path, err)
	}
	if recorded != strconv.Itoa(version) {
		return 0, fmt.Errorf("sqlite: %s has schema version %d but records %q", path, version, recorded)
	}

	var result string
	if err := db.QueryRowContext(ctx, "PRAGMA integrity_check(1)").Scan(&result); err != nil {
		return 0, fmt.Errorf("sqlite: integrity check of %s: %w", path, err)
	}
	if result != "ok" {
		return 0, fmt.Errorf("sqlite: integrity check of %s failed", path)
	}
	return version, nil
}

// openReadOnly opens path without creating it, read-only, on one connection.
func openReadOnly(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", "file:"+escapePath(path)+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("sqlite: open %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

func vacuumInto(ctx context.Context, src, dest string) error {
	db, err := openReadOnly(src)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	if _, err := db.ExecContext(ctx, "VACUUM INTO ?", dest); err != nil {
		return fmt.Errorf("sqlite: snapshot %s: %w", src, err)
	}
	return nil
}

func syncFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("sqlite: sync %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sqlite: sync %s: %w", path, err)
	}
	return nil
}

func migrationCount() (int, error) {
	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		return 0, fmt.Errorf("sqlite: read migrations: %w", err)
	}
	return len(entries), nil
}
