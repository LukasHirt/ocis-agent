package storetest

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/audit"
	"github.com/owncloud/ocis-agent/backend/internal/store"
)

func ev(user, kind string) audit.Event {
	return audit.Event{UserID: user, Kind: kind, Tool: "files.list", Risk: "read", Cred: "app", Outcome: "ok"}
}

func appendN(t *testing.T, st store.Store, user string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		mustTx(t, st, func(tx store.Tx) error {
			_, err := tx.AppendAudit(ev(user, "tool_call"))
			return err
		})
	}
}

func readAll(t *testing.T, st store.Store, user string) []audit.Record {
	t.Helper()
	var recs []audit.Record
	err := st.View(bg(), func(tx store.Tx) error {
		var err error
		recs, err = tx.ReadAudit(user, 0, 1000)
		return err
	})
	if err != nil {
		t.Fatalf("ReadAudit: %v", err)
	}
	return recs
}

func verifyAudit(st store.Store, user string) error {
	return st.View(bg(), func(tx store.Tx) error { return tx.VerifyAudit(user) })
}

func init() { register("Audit", runAudit) }

func runAudit(t *testing.T, f Factory) {
	t.Run("first record starts at genesis and the chain links", func(t *testing.T) {
		st, _ := open(t, f)
		appendN(t, st, "u1", 3)
		recs := readAll(t, st, "u1")
		if len(recs) != 3 {
			t.Fatalf("got %d records, want 3", len(recs))
		}
		if recs[0].Seq != 1 || recs[0].PrevHash != audit.Genesis {
			t.Errorf("first record = seq %d prev %q", recs[0].Seq, recs[0].PrevHash)
		}
		for i := 1; i < 3; i++ {
			if recs[i].Seq != int64(i+1) || recs[i].PrevHash != recs[i-1].Hash {
				t.Errorf("record %d is not linked to its predecessor", i)
			}
		}
		if err := verifyAudit(st, "u1"); err != nil {
			t.Fatalf("VerifyAudit: %v", err)
		}
	})

	t.Run("chains are independent per user", func(t *testing.T) {
		st, _ := open(t, f)
		appendN(t, st, "u1", 3)
		appendN(t, st, "u2", 2)
		if got := len(readAll(t, st, "u1")); got != 3 {
			t.Errorf("u1 has %d records", got)
		}
		u2 := readAll(t, st, "u2")
		if len(u2) != 2 || u2[0].Seq != 1 || u2[0].PrevHash != audit.Genesis {
			t.Errorf("u2 chain = %+v", u2)
		}
		for _, u := range []string{"u1", "u2"} {
			if err := verifyAudit(st, u); err != nil {
				t.Errorf("VerifyAudit(%s): %v", u, err)
			}
		}
	})

	t.Run("an empty log verifies", func(t *testing.T) {
		st, _ := open(t, f)
		if err := verifyAudit(st, "nobody"); err != nil {
			t.Fatalf("VerifyAudit of an empty log: %v", err)
		}
	})

	t.Run("a zero timestamp becomes the store clock", func(t *testing.T) {
		st, clk := open(t, f)
		var rec audit.Record
		mustTx(t, st, func(tx store.Tx) error {
			var err error
			rec, err = tx.AppendAudit(ev("u1", "pause"))
			return err
		})
		if !rec.TS.Equal(clk.Now()) {
			t.Fatalf("TS = %v, want %v", rec.TS, clk.Now())
		}
	})

	t.Run("an explicit timestamp is kept at millisecond precision", func(t *testing.T) {
		st, _ := open(t, f)
		want := time.Date(2025, 5, 6, 7, 8, 9, 123456789, time.UTC)
		e := ev("u1", "pause")
		e.TS = want
		var rec audit.Record
		mustTx(t, st, func(tx store.Tx) error {
			var err error
			rec, err = tx.AppendAudit(e)
			return err
		})
		if !rec.TS.Equal(want.Truncate(time.Millisecond)) {
			t.Fatalf("TS = %v, want %v", rec.TS, want.Truncate(time.Millisecond))
		}
		if err := verifyAudit(st, "u1"); err != nil {
			t.Fatalf("a stored record must re-verify: %v", err)
		}
	})

	t.Run("events need a user and a kind", func(t *testing.T) {
		st, _ := open(t, f)
		for _, e := range []audit.Event{{Kind: "x"}, {UserID: "u1"}} {
			err := st.InTx(bg(), func(tx store.Tx) error {
				_, err := tx.AppendAudit(e)
				return err
			})
			if err == nil {
				t.Errorf("AppendAudit(%+v) succeeded", e)
			}
		}
	})

	t.Run("a rolled-back append leaves no gap", func(t *testing.T) {
		st, _ := open(t, f)
		boom := errors.New("boom")
		_ = st.InTx(bg(), func(tx store.Tx) error {
			if _, err := tx.AppendAudit(ev("u1", "a")); err != nil {
				return err
			}
			return boom
		})
		appendN(t, st, "u1", 1)
		recs := readAll(t, st, "u1")
		if len(recs) != 1 || recs[0].Seq != 1 {
			t.Fatalf("records after rollback = %+v", recs)
		}
	})

	t.Run("ReadAudit pages by sequence", func(t *testing.T) {
		st, _ := open(t, f)
		appendN(t, st, "u1", 5)
		var page []audit.Record
		err := st.View(bg(), func(tx store.Tx) error {
			var err error
			page, err = tx.ReadAudit("u1", 2, 2)
			return err
		})
		if err != nil || len(page) != 2 || page[0].Seq != 3 || page[1].Seq != 4 {
			t.Fatalf("page = %+v, %v", page, err)
		}
	})

	t.Run("ReadAuditBefore pages newest first", func(t *testing.T) {
		st, _ := open(t, f)
		appendN(t, st, "u1", 5)
		appendN(t, st, "u2", 9)
		read := func(user string, before int64, limit int) []int64 {
			t.Helper()
			var recs []audit.Record
			err := st.View(bg(), func(tx store.Tx) error {
				var err error
				recs, err = tx.ReadAuditBefore(user, before, limit)
				return err
			})
			if err != nil {
				t.Fatalf("ReadAuditBefore(%s, %d, %d): %v", user, before, limit, err)
			}
			var out []int64
			for _, r := range recs {
				if r.UserID != user {
					t.Fatalf("record of %s in %s's page", r.UserID, user)
				}
				out = append(out, r.Seq)
			}
			return out
		}
		eq := func(got []int64, want ...int64) bool {
			if len(got) != len(want) {
				return false
			}
			for i := range got {
				if got[i] != want[i] {
					return false
				}
			}
			return true
		}
		if got := read("u1", 0, 2); !eq(got, 5, 4) {
			t.Errorf("newest page = %v, want [5 4]", got)
		}
		if got := read("u1", 4, 2); !eq(got, 3, 2) {
			t.Errorf("page before 4 = %v, want [3 2]", got)
		}
		if got := read("u1", 2, 2); !eq(got, 1) {
			t.Errorf("page before 2 = %v, want [1]", got)
		}
		if got := read("u1", 1, 2); len(got) != 0 {
			t.Errorf("page before 1 = %v, want none", got)
		}
		if got := read("u1", 100, 0); !eq(got, 5, 4, 3, 2, 1) {
			t.Errorf("default limit from above the head = %v", got)
		}
		if got := read("u2", 0, 1000); len(got) != 9 || got[0] != 9 {
			t.Errorf("u2 = %v", got)
		}
		if got := read("nobody", 0, 10); len(got) != 0 {
			t.Errorf("unknown user = %v", got)
		}
		// a record read newest first is the same as read oldest first
		var asc, desc []audit.Record
		err := st.View(bg(), func(tx store.Tx) error {
			var err error
			if asc, err = tx.ReadAudit("u1", 2, 1); err != nil {
				return err
			}
			desc, err = tx.ReadAuditBefore("u1", 4, 1)
			return err
		})
		if err != nil || len(asc) != 1 || len(desc) != 1 || asc[0] != desc[0] {
			t.Errorf("record 3 differs between orders: %+v %+v %v", asc, desc, err)
		}
	})

	t.Run("concurrent appends keep one linear chain", func(t *testing.T) {
		st, _ := open(t, f)
		const workers, each = 10, 6
		var wg sync.WaitGroup
		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < each; i++ {
					err := st.InTx(bg(), func(tx store.Tx) error {
						_, err := tx.AppendAudit(ev("u1", "tool_call"))
						return err
					})
					if err != nil {
						t.Errorf("AppendAudit: %v", err)
						return
					}
				}
			}()
		}
		wg.Wait()
		recs := readAll(t, st, "u1")
		if len(recs) != workers*each {
			t.Fatalf("got %d records, want %d", len(recs), workers*each)
		}
		for i, r := range recs {
			if r.Seq != int64(i+1) {
				t.Fatalf("record %d has seq %d", i, r.Seq)
			}
		}
		if err := verifyAudit(st, "u1"); err != nil {
			t.Fatalf("VerifyAudit after concurrent appends: %v", err)
		}
	})
}
