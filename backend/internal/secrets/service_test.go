package secrets

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/owncloud/ocis-agent/backend/internal/store"
	"github.com/owncloud/ocis-agent/backend/internal/store/sqlite"
)

type env struct {
	st     *sqlite.Store
	path   string
	master []byte
	svc    *Service
}

func newEnv(t *testing.T) *env {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agent.db")
	st, err := sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	master := make([]byte, 32)
	if _, err := rand.Read(master); err != nil {
		t.Fatal(err)
	}
	svc, err := New(st, master)
	if err != nil {
		t.Fatal(err)
	}
	return &env{st: st, path: path, master: master, svc: svc}
}

var bg = context.Background

func TestNewRejectsAWrongSizedMasterKey(t *testing.T) {
	e := newEnv(t)
	for _, n := range []int{0, 16, 31, 33, 64} {
		if _, err := New(e.st, make([]byte, n)); err == nil {
			t.Errorf("New accepted a %d-byte master key", n)
		}
	}
}

func TestRoundTrip(t *testing.T) {
	e := newEnv(t)
	sealed, err := e.svc.Seal(bg(), []byte("token-value"), []byte("user-1"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := e.svc.Open(bg(), sealed, []byte("user-1"))
	if err != nil || string(got) != "token-value" {
		t.Fatalf("Open = %q, %v", got, err)
	}
}

func TestSealingTheSameValueTwiceGivesDifferentCiphertext(t *testing.T) {
	e := newEnv(t)
	a, _ := e.svc.Seal(bg(), []byte("same"), nil)
	b, _ := e.svc.Seal(bg(), []byte("same"), nil)
	if bytes.Equal(a.Nonce, b.Nonce) || bytes.Equal(a.Ciphertext, b.Ciphertext) {
		t.Fatal("nonce or ciphertext was reused")
	}
}

func TestOpenFailsOnWrongAADOrTamperedData(t *testing.T) {
	e := newEnv(t)
	sealed, _ := e.svc.Seal(bg(), []byte("secret"), []byte("user-1"))

	if _, err := e.svc.Open(bg(), sealed, []byte("user-2")); !errors.Is(err, ErrDecrypt) {
		t.Errorf("wrong aad: err = %v, want ErrDecrypt", err)
	}
	flipped := sealed
	flipped.Ciphertext = append([]byte(nil), sealed.Ciphertext...)
	flipped.Ciphertext[0] ^= 0xff
	if _, err := e.svc.Open(bg(), flipped, []byte("user-1")); !errors.Is(err, ErrDecrypt) {
		t.Errorf("tampered ciphertext: err = %v, want ErrDecrypt", err)
	}
	truncated := sealed
	truncated.Ciphertext = sealed.Ciphertext[:3]
	if _, err := e.svc.Open(bg(), truncated, []byte("user-1")); !errors.Is(err, ErrDecrypt) {
		t.Errorf("truncated ciphertext: err = %v, want ErrDecrypt", err)
	}
}

func TestOpenNeverPanicsOnCorruptInput(t *testing.T) {
	e := newEnv(t)
	sealed, _ := e.svc.Seal(bg(), []byte("secret"), nil)

	shortNonce := sealed
	shortNonce.Nonce = sealed.Nonce[:5]
	emptyNonce := sealed
	emptyNonce.Nonce = nil
	unknownKey := sealed
	unknownKey.DEKID = "does-not-exist"
	for name, s := range map[string]store.Sealed{"short nonce": shortNonce, "empty nonce": emptyNonce, "unknown key": unknownKey} {
		if _, err := e.svc.Open(bg(), s, nil); !errors.Is(err, ErrDecrypt) {
			t.Errorf("%s: err = %v, want ErrDecrypt", name, err)
		}
	}
}

func TestAnotherMasterKeyCannotOpenOrSeal(t *testing.T) {
	e := newEnv(t)
	sealed, _ := e.svc.Seal(bg(), []byte("secret"), nil)

	other := make([]byte, 32)
	if _, err := rand.Read(other); err != nil {
		t.Fatal(err)
	}
	svc2, err := New(e.st, other)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc2.Open(bg(), sealed, nil); !errors.Is(err, ErrDecrypt) {
		t.Errorf("Open with another master key: err = %v, want ErrDecrypt", err)
	}
	if _, err := svc2.Seal(bg(), []byte("x"), nil); !errors.Is(err, ErrDecrypt) {
		t.Errorf("Seal with another master key: err = %v, want ErrDecrypt (the active DEK cannot be unwrapped)", err)
	}
}

func TestRotateKeepsOldCiphertextReadable(t *testing.T) {
	e := newEnv(t)
	before, _ := e.svc.Seal(bg(), []byte("old"), nil)
	if err := e.svc.Rotate(bg()); err != nil {
		t.Fatal(err)
	}
	after, err := e.svc.Seal(bg(), []byte("new"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if before.DEKID == after.DEKID {
		t.Fatal("Rotate did not switch to a new DEK")
	}
	for _, c := range []struct {
		s    store.Sealed
		want string
	}{{before, "old"}, {after, "new"}} {
		got, err := e.svc.Open(bg(), c.s, nil)
		if err != nil || string(got) != c.want {
			t.Errorf("Open = %q, %v; want %q", got, err, c.want)
		}
	}
	// a fresh service (empty key cache) must read both too
	svc2, _ := New(e.st, e.master)
	if got, err := svc2.Open(bg(), before, nil); err != nil || string(got) != "old" {
		t.Errorf("fresh service: Open(old) = %q, %v", got, err)
	}
}

func TestConcurrentFirstUseCreatesOneDEK(t *testing.T) {
	e := newEnv(t)
	const n = 10
	ids := make([]string, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s, err := e.svc.Seal(bg(), []byte("x"), nil)
			if err != nil {
				t.Errorf("Seal: %v", err)
				return
			}
			ids[i] = s.DEKID
		}(i)
	}
	wg.Wait()
	for _, id := range ids {
		if id != ids[0] {
			t.Fatalf("concurrent first use created several DEKs: %v", ids)
		}
	}
}

func TestNoSecretMaterialInTheDatabaseFiles(t *testing.T) {
	e := newEnv(t)
	marker := []byte("super-secret-token-value-0123456789")
	persist := func(name string, s store.Sealed) {
		t.Helper()
		err := e.st.InTx(bg(), func(tx store.Tx) error {
			if err := tx.SetSystem(name+".ct", hex.EncodeToString(s.Ciphertext)); err != nil {
				return err
			}
			return tx.SetSystem(name+".nonce", hex.EncodeToString(s.Nonce))
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	first, err := e.svc.Seal(bg(), marker, []byte("user-1"))
	if err != nil {
		t.Fatal(err)
	}
	persist("first", first)
	if err := e.svc.Rotate(bg()); err != nil {
		t.Fatal(err)
	}
	second, err := e.svc.Seal(bg(), marker, []byte("user-1"))
	if err != nil {
		t.Fatal(err)
	}
	persist("second", second)
	if first.DEKID == second.DEKID {
		t.Fatal("expected two DEKs")
	}

	var deks []store.DEK
	for _, id := range []string{first.DEKID, second.DEKID} {
		var d store.DEK
		err := e.st.View(bg(), func(tx store.Tx) error {
			var err error
			d, err = tx.GetDEK(id)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		deks = append(deks, d)
	}

	db, err := os.ReadFile(e.path)
	if err != nil {
		t.Fatalf("reading the database file: %v", err)
	}
	all := db
	if wal, err := os.ReadFile(e.path + "-wal"); err == nil {
		all = append(append([]byte(nil), db...), wal...)
	}

	if !bytes.Contains(all, deks[0].WrappedKey) && !bytes.Contains(all, deks[1].WrappedKey) {
		t.Fatal("positive control failed: no wrapped DEK found in the database files")
	}
	if !bytes.Contains(all, []byte(hex.EncodeToString(first.Ciphertext))) {
		t.Fatal("positive control failed: sealed value not found in the database files")
	}
	if bytes.Contains(all, e.master) {
		t.Error("master key found in the database files")
	}
	if bytes.Contains(all, marker) {
		t.Error("plaintext found in the database files")
	}
	for _, d := range deks {
		raw, err := e.svc.master.Open(nil, d.Nonce, d.WrappedKey, []byte("dek:"+d.ID))
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(all, raw) {
			t.Errorf("plaintext DEK %s found in the database files", d.ID)
		}
	}
}

func TestAWrappedKeyCannotBeMovedToAnotherRow(t *testing.T) {
	e := newEnv(t)
	sealed, err := e.svc.Seal(bg(), []byte("secret"), nil)
	if err != nil {
		t.Fatal(err)
	}
	var a store.DEK
	if err := e.st.View(bg(), func(tx store.Tx) error {
		var err error
		a, err = tx.GetDEK(sealed.DEKID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	err = e.st.InTx(bg(), func(tx store.Tx) error {
		if err := tx.PutDEK(store.DEK{ID: "forged", WrappedKey: a.WrappedKey, Nonce: a.Nonce}); err != nil {
			return err
		}
		return tx.PutDEK(store.DEK{ID: "badnonce", WrappedKey: a.WrappedKey, Nonce: []byte{1, 2, 3}})
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"forged", "badnonce"} {
		moved := sealed
		moved.DEKID = id
		if _, err := e.svc.Open(bg(), moved, nil); !errors.Is(err, ErrDecrypt) {
			t.Errorf("%s: err = %v, want ErrDecrypt", id, err)
		}
	}
}
