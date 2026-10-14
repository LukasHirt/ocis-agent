package sqlite

import (
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/store"
)

const (
	defaultListLimit = 100
	maxListLimit     = 1000

	msPerDay = 24 * 60 * 60 * 1000
)

func listLimit(n int) int {
	if n <= 0 {
		return defaultListLimit
	}
	if n > maxListLimit {
		return maxListLimit
	}
	return n
}

// ListUsers implements store.Tx.
func (t *tx) ListUsers(afterID string, limit int) ([]store.User, error) {
	rows, err := t.q.QueryContext(t.ctx,
		`SELECT user_id, user_name, personal_drive_id, enrolled_at, last_seen_at
		 FROM users WHERE user_id > ? ORDER BY user_id LIMIT ?`, afterID, listLimit(limit))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.User
	for rows.Next() {
		var u store.User
		var enrolled, seen int64
		if err := rows.Scan(&u.ID, &u.Name, &u.PersonalDriveID, &enrolled, &seen); err != nil {
			return nil, err
		}
		u.EnrolledAt, u.LastSeenAt = fromMS(enrolled), fromMS(seen)
		out = append(out, u)
	}
	return out, rows.Err()
}

// ListDormantUsers implements store.Tx.
func (t *tx) ListDormantUsers(now time.Time, limit int) ([]string, error) {
	rows, err := t.q.QueryContext(t.ctx,
		`SELECT u.user_id FROM users u JOIN policies p ON p.user_id = u.user_id
		 WHERE p.paused = 0 AND p.dormancy_days > 0 AND u.last_seen_at <= ? - p.dormancy_days * ?
		 ORDER BY u.last_seen_at, u.user_id LIMIT ?`, ms(now), msPerDay, listLimit(limit))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ListTokensDueForRenewal implements store.Tx.
func (t *tx) ListTokensDueForRenewal(now time.Time, limit int) ([]store.TokenDue, error) {
	rows, err := t.q.QueryContext(t.ctx,
		`SELECT user_id, label, created_at, expires_at FROM app_tokens
		 WHERE status = 'valid' AND ? >= created_at + (expires_at - created_at) / 2
		 ORDER BY expires_at, user_id LIMIT ?`, ms(now), listLimit(limit))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.TokenDue
	for rows.Next() {
		var d store.TokenDue
		var created, expires int64
		if err := rows.Scan(&d.UserID, &d.Label, &created, &expires); err != nil {
			return nil, err
		}
		d.CreatedAt, d.ExpiresAt = fromMS(created), fromMS(expires)
		out = append(out, d)
	}
	return out, rows.Err()
}
