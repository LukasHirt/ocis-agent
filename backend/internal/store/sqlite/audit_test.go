package sqlite

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/owncloud/ocis-agent/backend/internal/audit"
	"github.com/owncloud/ocis-agent/backend/internal/store"
)

func TestAuditTriggersRejectUpdateAndDelete(t *testing.T) {
	s := openSQLite(t)
	if err := s.InTx(context.Background(), func(tx store.Tx) error {
		_, err := tx.AppendAudit(audit.Event{UserID: "u1", Kind: audit.KindToolCall})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.w.Exec(`UPDATE audit SET detail = 'tampered'`); err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Errorf("UPDATE err = %v, want an append-only error", err)
	}
	if _, err := s.w.Exec(`DELETE FROM audit`); err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Errorf("DELETE err = %v, want an append-only error", err)
	}
}

func TestVerifyAuditDetectsDirectDatabaseTampering(t *testing.T) {
	prepare := func(t *testing.T) *Store {
		t.Helper()
		s := openSQLite(t)
		for i := 0; i < 4; i++ {
			if err := s.InTx(context.Background(), func(tx store.Tx) error {
				_, err := tx.AppendAudit(audit.Event{UserID: "u1", Kind: audit.KindToolCall, Detail: strconv.Itoa(i)})
				return err
			}); err != nil {
				t.Fatal(err)
			}
		}
		return s
	}
	// verify checks that the chain breaks at seq at after checked records
	verify := func(t *testing.T, s *Store, at, checked int64) {
		t.Helper()
		var n int64
		err := s.View(context.Background(), func(tx store.Tx) error {
			var err error
			n, err = tx.VerifyAudit("u1")
			return err
		})
		var ce *audit.ChainError
		if !errors.Is(err, audit.ErrChainBroken) || !errors.As(err, &ce) {
			t.Fatalf("err = %v, want a ChainError", err)
		}
		if ce.Seq != at || n != checked {
			t.Fatalf("broken at %d after %d checked, want %d after %d", ce.Seq, n, at, checked)
		}
	}
	exec := func(t *testing.T, s *Store, q string) {
		t.Helper()
		if _, err := s.w.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}

	t.Run("modified record", func(t *testing.T) {
		s := prepare(t)
		exec(t, s, `DROP TRIGGER audit_no_update`)
		exec(t, s, `UPDATE audit SET detail = 'tampered' WHERE user_id = 'u1' AND seq = 2`)
		verify(t, s, 2, 1)
	})
	t.Run("deleted middle record", func(t *testing.T) {
		s := prepare(t)
		exec(t, s, `DROP TRIGGER audit_no_delete`)
		exec(t, s, `DELETE FROM audit WHERE user_id = 'u1' AND seq = 2`)
		verify(t, s, 3, 1)
	})
	t.Run("deleted first record", func(t *testing.T) {
		s := prepare(t)
		exec(t, s, `DROP TRIGGER audit_no_delete`)
		exec(t, s, `DELETE FROM audit WHERE user_id = 'u1' AND seq = 1`)
		verify(t, s, 2, 0)
	})
}
