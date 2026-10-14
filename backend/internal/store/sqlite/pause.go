package sqlite

import "github.com/owncloud/ocis-agent/backend/internal/store"

// PausePolicy implements store.Tx.
func (t *tx) PausePolicy(userID, reason string) (bool, error) {
	if reason == "" || !store.ValidPauseReason(reason) {
		return false, store.Invalid("unknown pause reason %q", reason)
	}
	p, err := t.GetPolicy(userID)
	if err != nil {
		return false, err
	}
	current := ""
	if p.Paused {
		current = p.PauseReason
	}
	if !store.PauseSupersedes(current, reason) {
		return false, nil
	}
	if _, err := t.q.ExecContext(t.ctx,
		`UPDATE policies SET paused = 1, pause_reason = ?, updated_at = ? WHERE user_id = ?`,
		reason, ms(t.now()), userID); err != nil {
		return false, err
	}
	return true, nil
}

// ClearPause implements store.Tx.
func (t *tx) ClearPause(userID string, reasons ...string) (bool, error) {
	if len(reasons) == 0 {
		return false, store.Invalid("clearing a pause needs at least one reason")
	}
	for _, r := range reasons {
		if r == "" || !store.ValidPauseReason(r) {
			return false, store.Invalid("unknown pause reason %q", r)
		}
	}
	p, err := t.GetPolicy(userID)
	if err != nil {
		return false, err
	}
	if !p.Paused {
		return false, nil
	}
	owned := false
	for _, r := range reasons {
		if r == p.PauseReason {
			owned = true
		}
	}
	if !owned {
		return false, nil
	}
	if _, err := t.q.ExecContext(t.ctx,
		`UPDATE policies SET paused = 0, pause_reason = '', updated_at = ? WHERE user_id = ?`,
		ms(t.now()), userID); err != nil {
		return false, err
	}
	return true, nil
}
