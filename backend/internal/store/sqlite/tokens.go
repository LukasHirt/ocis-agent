package sqlite

import "github.com/owncloud/ocis-agent/backend/internal/store"

// WipeAppToken implements store.Tx.
func (t *tx) WipeAppToken(userID string) error {
	res, err := t.q.ExecContext(t.ctx,
		`UPDATE app_tokens SET ciphertext = x'', nonce = x'', status = 'revoked' WHERE user_id = ?`, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.ErrNotFound
	}
	return nil
}
