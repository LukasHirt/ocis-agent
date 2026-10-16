package sqlite

import (
	"fmt"
	"time"
)

// SumJobTokens implements store.Tx.
func (t *tx) SumJobTokens(userID string, since time.Time) (int64, int64, error) {
	var from int64
	if !since.IsZero() {
		from = ms(since)
	}
	var in, out int64
	err := t.q.QueryRowContext(t.ctx,
		`SELECT COALESCE(SUM(s.tokens_in), 0), COALESCE(SUM(s.tokens_out), 0)
		 FROM job_steps s JOIN jobs j ON j.id = s.job_id
		 WHERE j.user_id = ? AND s.created_at >= ?`, userID, from).Scan(&in, &out)
	if err != nil {
		return 0, 0, fmt.Errorf("sqlite: sum job tokens: %w", err)
	}
	return in, out, nil
}
