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
		_, err := tx.AppendAudit(audit.Event{UserID: "u1", Kind: "x"})
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
				_, err := tx.AppendAudit(audit.Event{UserID: "u1", Kind: "x", Detail: strconv.Itoa(i)})
				return err
			}); err != nil {
				t.Fatal(err)
			}
		}
		return s
	}
	verify := func(s *Store) error {
		return s.View(context.Background(), func(tx store.Tx) error { return tx.VerifyAudit("u1") })
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
		if err := verify(s); !errors.Is(err, audit.ErrChainBroken) {
			t.Fatalf("err = %v, want ErrChainBroken", err)
		}
	})
	t.Run("deleted middle record", func(t *testing.T) {
		s := prepare(t)
		exec(t, s, `DROP TRIGGER audit_no_delete`)
		exec(t, s, `DELETE FROM audit WHERE user_id = 'u1' AND seq = 2`)
		if err := verify(s); !errors.Is(err, audit.ErrChainBroken) {
			t.Fatalf("err = %v, want ErrChainBroken", err)
		}
	})
	t.Run("deleted first record", func(t *testing.T) {
		s := prepare(t)
		exec(t, s, `DROP TRIGGER audit_no_delete`)
		exec(t, s, `DELETE FROM audit WHERE user_id = 'u1' AND seq = 1`)
		if err := verify(s); !errors.Is(err, audit.ErrChainBroken) {
			t.Fatalf("err = %v, want ErrChainBroken", err)
		}
	})
}
