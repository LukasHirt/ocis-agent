package sqlite

import (
	"context"
	"testing"

	"github.com/owncloud/ocis-agent/backend/internal/store"
)

func TestCorruptModelsJSONReadsBackAsAnEmptyMap(t *testing.T) {
	s := openSQLite(t)
	ctx := context.Background()
	if err := s.InTx(ctx, func(tx store.Tx) error {
		return tx.PutUser(store.User{ID: "u1", Name: "n", PersonalDriveID: "d"})
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.w.ExecContext(ctx, `UPDATE policies SET models = 'not json' WHERE user_id = 'u1'`); err != nil {
		t.Fatal(err)
	}
	var p store.Policy
	if err := s.View(ctx, func(tx store.Tx) (err error) { p, err = tx.GetPolicy("u1"); return }); err != nil {
		t.Fatal(err)
	}
	if p.Models == nil || len(p.Models) != 0 {
		t.Errorf("Models = %#v, want empty and non-nil", p.Models)
	}
}
