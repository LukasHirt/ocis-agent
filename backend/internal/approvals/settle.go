package approvals

import (
	"context"

	"github.com/owncloud/ocis-agent/backend/internal/audit"
	"github.com/owncloud/ocis-agent/backend/internal/store"
)

// SettleEnded expires the pending and approved approvals of jobs that have
// ended (cancelled, failed or succeeded), at most limit of them (the store's
// default and maximum apply), and audits each as approval_expired, all in one
// transaction. It returns how many it settled; a second call settles nothing.
// Executing approvals are left alone: their outcome is open and
// ReconcileOrphans settles them. No job is resumed: an ended job never runs
// again, so the runner never sees these approvals (its "expired while
// pending" case cannot apply to them). The scheduler calls it periodically.
func (g *Gate) SettleEnded(ctx context.Context, limit int) (int, error) {
	n := 0
	err := g.st.InTx(ctx, func(tx store.Tx) error {
		refs, err := tx.ExpireEndedJobApprovals(limit)
		if err != nil {
			return err
		}
		for _, r := range refs {
			if err := g.event(tx, audit.Event{
				UserID: r.UserID, JobID: r.JobID, Kind: "approval_expired", Outcome: "expired",
				Detail: "approval=" + r.ID + "; job ended",
			}); err != nil {
				return err
			}
		}
		n = len(refs)
		return nil
	})
	if err != nil {
		return 0, err
	}
	return n, nil
}
