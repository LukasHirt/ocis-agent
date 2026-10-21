package sqlite

import (
	"fmt"
	"sort"

	"github.com/owncloud/ocis-agent/backend/internal/store"
)

const (
	defaultSettleLimit = 100
	maxSettleLimit     = 1000
)

// ExpireEndedJobApprovals implements store.Tx.
func (t *tx) ExpireEndedJobApprovals(limit int) ([]store.ApprovalRef, error) {
	if limit <= 0 {
		limit = defaultSettleLimit
	}
	if limit > maxSettleLimit {
		limit = maxSettleLimit
	}
	// The inner join drops approvals without a job row (there are none: the
	// foreign key cascades), so such a row can never make the query fail.
	// RETURNING has no defined order, so the rows are sorted afterwards.
	rows, err := t.q.QueryContext(t.ctx,
		`UPDATE approvals SET state = 'expired'
		 WHERE state IN ('pending', 'approved') AND id IN (
		     SELECT a.id FROM approvals a JOIN jobs j ON j.id = a.job_id AND j.user_id = a.user_id
		     WHERE a.state IN ('pending', 'approved') AND j.state IN `+terminalJobStates+`
		     ORDER BY a.requested_at, a.id LIMIT ?)
		 RETURNING id, user_id, job_id, requested_at`, limit)
	if err != nil {
		return nil, fmt.Errorf("sqlite: expire approvals of ended jobs: %w", err)
	}
	defer rows.Close()
	type row struct {
		ref       store.ApprovalRef
		requested int64
	}
	var got []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.ref.ID, &r.ref.UserID, &r.ref.JobID, &r.requested); err != nil {
			return nil, err
		}
		got = append(got, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(got, func(i, j int) bool {
		if got[i].requested != got[j].requested {
			return got[i].requested < got[j].requested
		}
		return got[i].ref.ID < got[j].ref.ID
	})
	out := make([]store.ApprovalRef, len(got))
	for i, r := range got {
		out[i] = r.ref
	}
	return out, nil
}
