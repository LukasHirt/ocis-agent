package memory

import (
	"errors"
	"testing"

	"github.com/owncloud/ocis-agent/backend/internal/ocisclient"
	"github.com/owncloud/ocis-agent/backend/internal/store"
)

func TestPurgeRemovesTheFolderAndTheCachedETags(t *testing.T) {
	e := ready(t, Limits{})
	if _, err := e.svc.Write(bg, e.sess, "profile.md", "x\n", nil); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Purge(bg, e.sess); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := e.fake.File("drive1", "/.ocis-agent/README.md"); ok {
		t.Fatal("the folder must be gone")
	}
	if _, err := e.svc.files.Stat(bg, e.sess.Creds, "drive1", Root); !errors.Is(err, ocisclient.ErrNotFound) {
		t.Fatalf("stat after purge = %v", err)
	}
	var c store.Cursor
	_ = e.st.View(bg, func(tx store.Tx) error { c, _ = tx.GetCursor("u1"); return nil })
	if len(c.MemoryETags) != 0 {
		t.Fatalf("cached ETags survived: %v", c.MemoryETags)
	}
	// purging twice is fine, and a fresh Ensure starts clean
	if err := e.svc.Purge(bg, e.sess); err != nil {
		t.Fatalf("second purge: %v", err)
	}
	if err := e.svc.Ensure(bg, e.sess); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Write(bg, e.sess, "profile.md", "again\n", nil); err != nil {
		t.Fatalf("write after purge and ensure: %v", err)
	}
}

func TestPurgePassesCredentialFailuresThrough(t *testing.T) {
	e := ready(t, Limits{})
	bad := e.sess
	bad.Creds = ocisclient.AppToken("einstein", "wrong")
	if err := e.svc.Purge(bg, bad); !errors.Is(err, ocisclient.ErrUnauthorized) {
		t.Fatalf("err = %v", err)
	}
	if _, _, ok := e.fake.File("drive1", "/.ocis-agent/README.md"); !ok {
		t.Fatal("a refused purge must not delete anything")
	}
}
