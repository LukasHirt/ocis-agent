package sqlite

import (
	"database/sql"
	"errors"

	"github.com/owncloud/ocis-agent/backend/internal/store"
)

// GetSystem implements store.Tx.
func (t *tx) GetSystem(key string) (string, error) {
	var v string
	err := t.q.QueryRowContext(t.ctx, `SELECT value FROM system WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", store.ErrNotFound
	}
	return v, err
}

// SetSystem implements store.Tx.
func (t *tx) SetSystem(key, value string) error {
	_, err := t.q.ExecContext(t.ctx,
		`INSERT INTO system(key, value) VALUES(?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}
