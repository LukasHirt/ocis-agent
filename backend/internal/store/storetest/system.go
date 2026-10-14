package storetest

import (
	"errors"
	"testing"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/store"
)

func init() { register("System", runSystem) }

func runSystem(t *testing.T, f Factory) {
	t.Run("missing key is not found", func(t *testing.T) {
		st, _ := open(t, f)
		err := st.View(bg(), func(tx store.Tx) error {
			_, err := tx.GetSystem("nope")
			return err
		})
		if !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
	})

	t.Run("set then get and overwrite", func(t *testing.T) {
		st, _ := open(t, f)
		mustTx(t, st, func(tx store.Tx) error { return tx.SetSystem("killswitch", "0") })
		mustTx(t, st, func(tx store.Tx) error { return tx.SetSystem("killswitch", "1") })
		var got string
		err := st.View(bg(), func(tx store.Tx) error {
			var err error
			got, err = tx.GetSystem("killswitch")
			return err
		})
		if err != nil || got != "1" {
			t.Fatalf("got %q, %v", got, err)
		}
	})

	t.Run("an error rolls the transaction back", func(t *testing.T) {
		st, _ := open(t, f)
		boom := errors.New("boom")
		err := st.InTx(bg(), func(tx store.Tx) error {
			if err := tx.SetSystem("k", "v"); err != nil {
				return err
			}
			return boom
		})
		if !errors.Is(err, boom) {
			t.Fatalf("err = %v, want boom", err)
		}
		err = st.View(bg(), func(tx store.Tx) error {
			_, err := tx.GetSystem("k")
			return err
		})
		if !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("rolled-back write is visible: %v", err)
		}
	})

	t.Run("Now reads the injected clock", func(t *testing.T) {
		st, clk := open(t, f)
		want := clk.Now().Add(90 * time.Second)
		clk.Advance(90 * time.Second)
		mustTx(t, st, func(tx store.Tx) error {
			if got := tx.Now(); !got.Equal(want) {
				t.Errorf("Now = %v, want %v", got, want)
			}
			return nil
		})
	})
}
