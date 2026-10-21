package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/owncloud/ocis-agent/backend/internal/audit"
	"github.com/owncloud/ocis-agent/backend/internal/secrets"
	"github.com/owncloud/ocis-agent/backend/internal/store"
	"github.com/owncloud/ocis-agent/backend/internal/store/sqlite"
)

var rawKeyA = []byte("0123456789abcdef0123456789abcdef")

func appendAudit(st store.Store, user string) error {
	return st.InTx(context.Background(), func(tx store.Tx) error {
		_, err := tx.AppendAudit(audit.Event{UserID: user, Kind: audit.KindToolCall, Tool: "files.list", Risk: "read", Cred: "app", Outcome: "ok"})
		return err
	})
}

// restore follows the documented procedure with the sidecar stopped: copy
// the backup to <data dir>/agent.db and remove the -wal and -shm files.
func restore(t *testing.T, backup, dataDir string) {
	t.Helper()
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	in, err := os.Open(backup)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = in.Close() }()
	db := filepath.Join(dataDir, "agent.db")
	out, err := os.OpenFile(db, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := os.Remove(db + suffix); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
}

// A backup taken while another process writes restores into a working data
// directory: the audit chains verify, secrets unseal with the same master
// key, and a wrong key fails at start-up. The command needs neither the
// master key nor the oCIS URL and never writes the key anywhere.
func TestBackupAndRestore(t *testing.T) {
	dir, out, errOut := setup(t)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	st, err := sqlite.Open(filepath.Join(dir, "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	sec, err := secrets.New(st, rawKeyA)
	if err != nil {
		t.Fatal(err)
	}
	if err := sec.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	aad := []byte("app_token:u1")
	sealed, err := sec.Seal(context.Background(), []byte("the app token"), aad)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		if err := appendAudit(st, "u1"); err != nil {
			t.Fatal(err)
		}
	}

	// the sidecar keeps writing during the backup
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	for _, u := range []string{"u1", "u2"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				_ = appendAudit(st, u)
			}
		}()
	}
	t.Setenv("AGENT_MASTER_KEY", "")
	t.Setenv("AGENT_OCIS_URL", "")
	backup := filepath.Join(t.TempDir(), "agent-backup.db")
	err = run([]string{"backup", backup})
	cancel()
	wg.Wait()
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	if !strings.Contains(out.String(), backup) {
		t.Errorf("backup printed %q, want the destination", out.String())
	}

	data, err := os.ReadFile(backup)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range [][]byte{rawKeyA, []byte(keyA), []byte("the app token")} {
		if bytes.Contains(data, secret) || bytes.Contains(out.Bytes(), secret) || bytes.Contains(errOut.Bytes(), secret) {
			t.Errorf("the backup or the output contains secret material")
		}
	}

	restored := filepath.Join(t.TempDir(), "restored")
	restore(t, backup, restored)
	rst, err := sqlite.Open(filepath.Join(restored, "agent.db"))
	if err != nil {
		t.Fatalf("open the restored database: %v", err)
	}
	for _, u := range []string{"u1", "u2"} {
		if err := rst.View(context.Background(), func(tx store.Tx) error {
			n, err := tx.VerifyAudit(u)
			if err == nil && u == "u1" && n < 20 {
				t.Errorf("u1 has %d events after restore, want >= 20", n)
			}
			return err
		}); err != nil {
			t.Errorf("VerifyAudit(%s) after restore: %v", u, err)
		}
	}
	rsec, err := secrets.New(rst, rawKeyA)
	if err != nil {
		t.Fatal(err)
	}
	if err := rsec.Init(context.Background()); err != nil {
		t.Fatalf("Init with the same key after restore: %v", err)
	}
	plain, err := rsec.Open(context.Background(), sealed, aad)
	if err != nil || string(plain) != "the app token" {
		t.Errorf("unseal after restore = %q, %v", plain, err)
	}
	if err := rst.Close(); err != nil {
		t.Fatal(err)
	}

	// start-up with another key fails fast
	t.Setenv("AGENT_OCIS_URL", "https://cloud.example.com")
	t.Setenv("AGENT_MASTER_KEY", keyB)
	t.Setenv("AGENT_DATA_DIR", restored)
	t.Setenv("AGENT_ROLES", "api")
	if err := run(nil); err == nil || !strings.Contains(err.Error(), "AGENT_MASTER_KEY") {
		t.Errorf("start-up with the wrong key after restore: %v", err)
	}
}

func TestBackupCommandRefusals(t *testing.T) {
	dir, out, _ := setup(t)
	for _, args := range [][]string{{"backup"}, {"backup", ""}, {"backup", "a", "b"}} {
		if err := run(args); err == nil {
			t.Errorf("%q: no error", args)
		}
	}

	// no database: refused, nothing created
	dest := filepath.Join(t.TempDir(), "backup.db")
	err := run([]string{"backup", dest})
	if err == nil || !strings.Contains(err.Error(), filepath.Join(dir, "agent.db")) {
		t.Errorf("missing database: err = %v, want one naming the database path", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("backup created the data directory: %v", err)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Errorf("backup created the destination: %v", err)
	}

	// an existing destination is never overwritten
	if err := run([]string{"migrate"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dest, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"backup", dest}); err == nil {
		t.Error("existing destination: no error")
	}
	if got, _ := os.ReadFile(dest); string(got) != "keep" {
		t.Errorf("the existing destination was changed")
	}
	if out.Len() != 0 {
		t.Errorf("refused backups printed %q", out.String())
	}
}
