package sqlite

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/owncloud/ocis-agent/backend/internal/store"
)

// PutDEK implements store.Tx.
func (t *tx) PutDEK(d store.DEK) error {
	if d.ID == "" || len(d.WrappedKey) == 0 || len(d.Nonce) == 0 {
		return store.Invalid("a DEK needs an id, a wrapped key and a nonce")
	}
	if d.CreatedAt.IsZero() {
		d.CreatedAt = t.now()
	}
	_, err := t.q.ExecContext(t.ctx,
		`INSERT INTO deks(dek_id, wrapped_key, nonce, created_at) VALUES(?,?,?,?)`,
		d.ID, d.WrappedKey, d.Nonce, ms(d.CreatedAt))
	if isUnique(err) {
		return fmt.Errorf("%w: DEK %s exists", store.ErrConflict, d.ID)
	}
	return err
}

// GetDEK implements store.Tx.
func (t *tx) GetDEK(id string) (store.DEK, error) {
	return t.scanDEK(t.q.QueryRowContext(t.ctx,
		`SELECT dek_id, wrapped_key, nonce, created_at, retired_at FROM deks WHERE dek_id = ?`, id))
}

// ActiveDEK implements store.Tx.
func (t *tx) ActiveDEK() (store.DEK, error) {
	return t.scanDEK(t.q.QueryRowContext(t.ctx,
		`SELECT dek_id, wrapped_key, nonce, created_at, retired_at FROM deks
		 WHERE retired_at IS NULL ORDER BY created_at DESC, dek_id DESC LIMIT 1`))
}

// RetireDEK implements store.Tx.
func (t *tx) RetireDEK(id string) error {
	res, err := t.q.ExecContext(t.ctx,
		`UPDATE deks SET retired_at = COALESCE(retired_at, ?) WHERE dek_id = ?`, ms(t.now()), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (t *tx) scanDEK(row *sql.Row) (store.DEK, error) {
	var d store.DEK
	var created int64
	var retired sql.NullInt64
	err := row.Scan(&d.ID, &d.WrappedKey, &d.Nonce, &created, &retired)
	if errors.Is(err, sql.ErrNoRows) {
		return store.DEK{}, store.ErrNotFound
	}
	if err != nil {
		return store.DEK{}, err
	}
	d.CreatedAt = fromMS(created)
	if retired.Valid {
		r := fromMS(retired.Int64)
		d.RetiredAt = &r
	}
	return d, nil
}
