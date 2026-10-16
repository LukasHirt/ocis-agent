package sqlite

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/store"
)

// PutUser implements store.Tx.
func (t *tx) PutUser(u store.User) error {
	if u.ID == "" || u.Name == "" || u.PersonalDriveID == "" {
		return store.Invalid("a user needs an id, a name and a personal drive id")
	}
	now := t.now()
	if u.EnrolledAt.IsZero() {
		u.EnrolledAt = now
	}
	if u.LastSeenAt.IsZero() {
		u.LastSeenAt = now
	}
	if _, err := t.q.ExecContext(t.ctx,
		`INSERT INTO users(user_id, user_name, personal_drive_id, enrolled_at, last_seen_at)
		 VALUES(?,?,?,?,?)
		 ON CONFLICT(user_id) DO UPDATE SET
		     user_name = excluded.user_name, personal_drive_id = excluded.personal_drive_id`,
		u.ID, u.Name, u.PersonalDriveID, ms(u.EnrolledAt), ms(u.LastSeenAt)); err != nil {
		return fmt.Errorf("sqlite: put user: %w", err)
	}
	p := store.DefaultPolicy(u.ID, now)
	_, err := t.q.ExecContext(t.ctx,
		`INSERT INTO policies(user_id, paused, pause_reason, auto_approve_write, proactive_enabled, watch,
		                      sweep_interval_s, quiet_hours, daily_tokens, dormancy_days, updated_at)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?)
		 ON CONFLICT(user_id) DO NOTHING`,
		p.UserID, boolInt(p.Paused), p.PauseReason, boolInt(p.AutoApproveWrite), boolInt(p.ProactiveEnabled),
		"[]", int64(p.SweepInterval/time.Second), p.QuietHours, p.DailyTokens, p.DormancyDays, ms(p.UpdatedAt))
	if err != nil {
		return fmt.Errorf("sqlite: create default policy: %w", err)
	}
	return nil
}

// GetUser implements store.Tx.
func (t *tx) GetUser(id string) (store.User, error) {
	var u store.User
	var enrolled, seen int64
	err := t.q.QueryRowContext(t.ctx,
		`SELECT user_id, user_name, personal_drive_id, enrolled_at, last_seen_at FROM users WHERE user_id = ?`, id).
		Scan(&u.ID, &u.Name, &u.PersonalDriveID, &enrolled, &seen)
	if errors.Is(err, sql.ErrNoRows) {
		return store.User{}, store.ErrNotFound
	}
	if err != nil {
		return store.User{}, err
	}
	u.EnrolledAt, u.LastSeenAt = fromMS(enrolled), fromMS(seen)
	return u, nil
}

// TouchUser implements store.Tx.
func (t *tx) TouchUser(id string, seenAt time.Time) error {
	res, err := t.q.ExecContext(t.ctx,
		`UPDATE users SET last_seen_at = MAX(last_seen_at, ?) WHERE user_id = ?`, ms(seenAt), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// DeleteUser implements store.Tx.
func (t *tx) DeleteUser(id string) error {
	res, err := t.q.ExecContext(t.ctx, `DELETE FROM users WHERE user_id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// GetPolicy implements store.Tx.
func (t *tx) GetPolicy(userID string) (store.Policy, error) {
	var p store.Policy
	var paused, auto, proactive int
	var watch, models string
	var sweepS, updated int64
	err := t.q.QueryRowContext(t.ctx,
		`SELECT user_id, paused, pause_reason, auto_approve_write, proactive_enabled, watch,
		        sweep_interval_s, quiet_hours, daily_tokens, dormancy_days, models, updated_at
		 FROM policies WHERE user_id = ?`, userID).
		Scan(&p.UserID, &paused, &p.PauseReason, &auto, &proactive, &watch,
			&sweepS, &p.QuietHours, &p.DailyTokens, &p.DormancyDays, &models, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return store.Policy{}, store.ErrNotFound
	}
	if err != nil {
		return store.Policy{}, err
	}
	p.Paused, p.AutoApproveWrite, p.ProactiveEnabled = paused == 1, auto == 1, proactive == 1
	p.SweepInterval = time.Duration(sweepS) * time.Second
	p.UpdatedAt = fromMS(updated)
	p.Watch = []string{}
	if err := json.Unmarshal([]byte(watch), &p.Watch); err != nil || p.Watch == nil {
		p.Watch = []string{}
	}
	p.Models = map[string]string{}
	if err := json.Unmarshal([]byte(models), &p.Models); err != nil || p.Models == nil {
		p.Models = map[string]string{}
	}
	return p, nil
}

// PutPolicy implements store.Tx.
func (t *tx) PutPolicy(p store.Policy) error {
	if err := p.Validate(); err != nil {
		return err
	}
	watch := p.Watch
	if watch == nil {
		watch = []string{}
	}
	raw, err := json.Marshal(watch)
	if err != nil {
		return err
	}
	models := p.Models
	if models == nil {
		models = map[string]string{}
	}
	rawModels, err := json.Marshal(models)
	if err != nil {
		return err
	}
	res, err := t.q.ExecContext(t.ctx,
		`UPDATE policies SET paused = ?, pause_reason = ?, auto_approve_write = ?, proactive_enabled = ?,
		        watch = ?, sweep_interval_s = ?, quiet_hours = ?, daily_tokens = ?, dormancy_days = ?, models = ?, updated_at = ?
		 WHERE user_id = ?`,
		boolInt(p.Paused), p.PauseReason, boolInt(p.AutoApproveWrite), boolInt(p.ProactiveEnabled),
		string(raw), int64(p.SweepInterval/time.Second), p.QuietHours, p.DailyTokens, p.DormancyDays, string(rawModels), ms(t.now()), p.UserID)
	if err != nil {
		return fmt.Errorf("sqlite: put policy: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// PutAppToken implements store.Tx.
func (t *tx) PutAppToken(a store.AppToken) error {
	switch {
	case a.UserID == "" || a.Label == "":
		return store.Invalid("an app token needs a user id and a label")
	case len(a.Sealed.Ciphertext) == 0 || len(a.Sealed.Nonce) == 0 || a.Sealed.DEKID == "":
		return store.Invalid("an app token needs sealed data")
	case !a.Status.Valid():
		return store.Invalid("unknown token status %q", a.Status)
	case a.ExpiresAt.IsZero():
		return store.Invalid("an app token needs an expiry")
	}
	if a.CreatedAt.IsZero() {
		a.CreatedAt = t.now()
	}
	_, err := t.q.ExecContext(t.ctx,
		`INSERT INTO app_tokens(user_id, ciphertext, nonce, dek_id, label, created_at, expires_at, status)
		 VALUES(?,?,?,?,?,?,?,?)
		 ON CONFLICT(user_id) DO UPDATE SET
		     ciphertext = excluded.ciphertext, nonce = excluded.nonce, dek_id = excluded.dek_id,
		     label = excluded.label, created_at = excluded.created_at, expires_at = excluded.expires_at,
		     status = excluded.status`,
		a.UserID, a.Sealed.Ciphertext, a.Sealed.Nonce, a.Sealed.DEKID, a.Label, ms(a.CreatedAt), ms(a.ExpiresAt), string(a.Status))
	if isForeignKey(err) {
		return fmt.Errorf("%w: user or key", store.ErrNotFound)
	}
	return err
}

// GetAppToken implements store.Tx.
func (t *tx) GetAppToken(userID string) (store.AppToken, error) {
	var a store.AppToken
	var created, expires int64
	var status string
	err := t.q.QueryRowContext(t.ctx,
		`SELECT user_id, ciphertext, nonce, dek_id, label, created_at, expires_at, status
		 FROM app_tokens WHERE user_id = ?`, userID).
		Scan(&a.UserID, &a.Sealed.Ciphertext, &a.Sealed.Nonce, &a.Sealed.DEKID, &a.Label, &created, &expires, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return store.AppToken{}, store.ErrNotFound
	}
	if err != nil {
		return store.AppToken{}, err
	}
	a.CreatedAt, a.ExpiresAt, a.Status = fromMS(created), fromMS(expires), store.TokenStatus(status)
	return a, nil
}

// SetAppTokenStatus implements store.Tx.
func (t *tx) SetAppTokenStatus(userID string, s store.TokenStatus) error {
	if !s.Valid() {
		return store.Invalid("unknown token status %q", s)
	}
	res, err := t.q.ExecContext(t.ctx, `UPDATE app_tokens SET status = ? WHERE user_id = ?`, string(s), userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// AddTokenLabel implements store.Tx.
func (t *tx) AddTokenLabel(userID, label string) error {
	if userID == "" || label == "" {
		return store.Invalid("a token label needs a user id and a label")
	}
	_, err := t.q.ExecContext(t.ctx,
		`INSERT INTO app_token_labels(user_id, label, created_at) VALUES(?,?,?)`, userID, label, ms(t.now()))
	switch {
	case isUnique(err):
		return fmt.Errorf("%w: label %s exists", store.ErrConflict, label)
	case isForeignKey(err):
		return fmt.Errorf("%w: user", store.ErrNotFound)
	}
	return err
}

// MarkTokenLabelDeleted implements store.Tx.
func (t *tx) MarkTokenLabelDeleted(userID, label string) error {
	res, err := t.q.ExecContext(t.ctx,
		`UPDATE app_token_labels SET deleted_at = COALESCE(deleted_at, ?) WHERE user_id = ? AND label = ?`,
		ms(t.now()), userID, label)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// ListTokenLabels implements store.Tx.
func (t *tx) ListTokenLabels(userID string) ([]store.TokenLabel, error) {
	rows, err := t.q.QueryContext(t.ctx,
		`SELECT user_id, label, created_at, deleted_at FROM app_token_labels
		 WHERE user_id = ? ORDER BY created_at, label`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.TokenLabel
	for rows.Next() {
		var l store.TokenLabel
		var created int64
		var deleted sql.NullInt64
		if err := rows.Scan(&l.UserID, &l.Label, &created, &deleted); err != nil {
			return nil, err
		}
		l.CreatedAt = fromMS(created)
		if deleted.Valid {
			d := fromMS(deleted.Int64)
			l.DeletedAt = &d
		}
		out = append(out, l)
	}
	return out, rows.Err()
}
