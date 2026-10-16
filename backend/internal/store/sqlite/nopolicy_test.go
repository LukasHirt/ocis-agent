package sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/store"
)

// A user whose policy row is missing (not reachable through the Store API:
// PutUser always creates one) is not paused, so its jobs are claimable.
func TestAUserWithoutAPolicyRowIsClaimable(t *testing.T) {
	s := openSQLite(t)
	ctx := context.Background()
	if err := s.InTx(ctx, func(tx store.Tx) error {
		if err := tx.PutUser(store.User{ID: "u1", Name: "n", PersonalDriveID: "d"}); err != nil {
			return err
		}
		_, err := tx.InsertJob(store.Job{UserID: "u1", TriggerKind: "chat", Goal: "g"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.w.ExecContext(ctx, `DELETE FROM policies WHERE user_id = 'u1'`); err != nil {
		t.Fatal(err)
	}
	var ok bool
	if err := s.InTx(ctx, func(tx store.Tx) (err error) {
		_, ok, err = tx.ClaimNextJob("w1", time.Minute)
		return
	}); err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("the job of a user without a policy row must be claimable")
	}
}
