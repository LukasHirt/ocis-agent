package sqlite

import (
	"strings"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/store"
)

// CountJobsOfKinds implements store.Tx. With since it is a range on
// jobs_user (user_id, created_at).
func (t *tx) CountJobsOfKinds(userID string, kinds []string, states []store.JobState, since time.Time) (int, error) {
	if len(kinds) == 0 {
		return 0, nil
	}
	query := `SELECT COUNT(*) FROM jobs WHERE user_id = ?`
	args := []any{userID}
	if !since.IsZero() {
		query += ` AND created_at >= ?`
		args = append(args, ms(since))
	}
	marks := make([]string, len(kinds))
	for i, k := range kinds {
		marks[i] = "?"
		args = append(args, k)
	}
	query += ` AND trigger_kind IN (` + strings.Join(marks, ",") + `)`
	if len(states) > 0 {
		marks := make([]string, len(states))
		for i, s := range states {
			if !s.Valid() {
				return 0, store.Invalid("unknown job state")
			}
			marks[i] = "?"
			args = append(args, string(s))
		}
		query += ` AND state IN (` + strings.Join(marks, ",") + `)`
	}
	var n int
	err := t.q.QueryRowContext(t.ctx, query, args...).Scan(&n)
	return n, err
}
