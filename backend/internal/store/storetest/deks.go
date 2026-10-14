package storetest

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/store"
)

func init() { register("DEKs", runDEKs) }

func newDEK(id string) store.DEK {
	return store.DEK{ID: id, WrappedKey: []byte("wrapped-" + id), Nonce: []byte("nonce-" + id)}
}

func mustDEK(t *testing.T, st store.Store) store.DEK {
	t.Helper()
	d := newDEK(store.NewID())
	mustTx(t, st, func(tx store.Tx) error { return tx.PutDEK(d) })
	return d
}

func runDEKs(t *testing.T, f Factory) {
	t.Run("put and get round trip", func(t *testing.T) {
		st, clk := open(t, f)
		d := newDEK("k1")
		mustTx(t, st, func(tx store.Tx) error { return tx.PutDEK(d) })
		var got store.DEK
		err := st.View(bg(), func(tx store.Tx) error {
			var err error
			got, err = tx.GetDEK("k1")
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got.WrappedKey, d.WrappedKey) || !bytes.Equal(got.Nonce, d.Nonce) {
			t.Errorf("key material changed: %+v", got)
		}
		if !got.CreatedAt.Equal(clk.Now()) || got.RetiredAt != nil {
			t.Errorf("CreatedAt = %v, RetiredAt = %v", got.CreatedAt, got.RetiredAt)
		}
	})

	t.Run("duplicate id conflicts", func(t *testing.T) {
		st, _ := open(t, f)
		d := newDEK("k1")
		mustTx(t, st, func(tx store.Tx) error { return tx.PutDEK(d) })
		err := st.InTx(bg(), func(tx store.Tx) error { return tx.PutDEK(d) })
		if !errors.Is(err, store.ErrConflict) {
			t.Fatalf("err = %v, want ErrConflict", err)
		}
	})

	t.Run("unknown id is not found", func(t *testing.T) {
		st, _ := open(t, f)
		err := st.View(bg(), func(tx store.Tx) error {
			_, err := tx.GetDEK("nope")
			return err
		})
		if !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
		err = st.InTx(bg(), func(tx store.Tx) error { return tx.RetireDEK("nope") })
		if !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("RetireDEK err = %v, want ErrNotFound", err)
		}
	})

	t.Run("active key is the newest non-retired one", func(t *testing.T) {
		st, clk := open(t, f)
		active := func() (store.DEK, error) {
			var d store.DEK
			err := st.View(bg(), func(tx store.Tx) error {
				var err error
				d, err = tx.ActiveDEK()
				return err
			})
			return d, err
		}
		if _, err := active(); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("empty store: err = %v, want ErrNotFound", err)
		}
		mustTx(t, st, func(tx store.Tx) error { return tx.PutDEK(newDEK("old")) })
		clk.Advance(time.Minute)
		mustTx(t, st, func(tx store.Tx) error { return tx.PutDEK(newDEK("new")) })
		if d, err := active(); err != nil || d.ID != "new" {
			t.Fatalf("active = %v, %v; want new", d.ID, err)
		}
		mustTx(t, st, func(tx store.Tx) error { return tx.RetireDEK("new") })
		mustTx(t, st, func(tx store.Tx) error { return tx.RetireDEK("new") }) // idempotent
		if d, err := active(); err != nil || d.ID != "old" {
			t.Fatalf("after retiring new: active = %v, %v; want old", d.ID, err)
		}
		var retired store.DEK
		_ = st.View(bg(), func(tx store.Tx) error {
			var err error
			retired, err = tx.GetDEK("new")
			return err
		})
		if retired.RetiredAt == nil {
			t.Error("a retired key must keep working for reads and carry RetiredAt")
		}
	})
}
