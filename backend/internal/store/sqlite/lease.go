package sqlite

import (
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/store"
)

// AssertLease implements store.Tx.
func (t *tx) AssertLease(userID, jobID string, fence int64) error {
	var one int
	err := t.q.QueryRowContext(t.ctx,
		`SELECT 1 FROM jobs WHERE id = ? AND user_id = ? AND state = 'running' AND `+leaseHeld,
		jobID, userID, fence).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return t.diagnoseJobWrite(userID, jobID, true)
	}
	return err
}

// RequeueJob implements store.Tx.
func (t *tx) RequeueJob(c store.Claim, notBefore time.Time, checkpoint json.RawMessage) (bool, error) {
	applied, err := t.ReleaseJob(c, store.JobQueued, checkpoint, "")
	if err != nil || !applied {
		return applied, err
	}
	if notBefore.IsZero() {
		notBefore = t.now()
	}
	if _, err := t.q.ExecContext(t.ctx,
		`UPDATE jobs SET not_before = ? WHERE id = ? AND user_id = ?`, ms(notBefore), c.Job.ID, c.Job.UserID); err != nil {
		return false, err
	}
	return true, nil
}
