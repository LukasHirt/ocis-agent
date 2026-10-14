package sqlite

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/owncloud/ocis-agent/backend/internal/store"
)

// GetCursor implements store.Tx.
func (t *tx) GetCursor(userID string) (store.Cursor, error) {
	c := store.Cursor{UserID: userID, MemoryETags: map[string]string{}}
	var last sql.NullInt64
	var etags string
	err := t.q.QueryRowContext(t.ctx,
		`SELECT sse_last_event_id, last_sweep_at, memory_etags FROM cursors WHERE user_id = ?`, userID).
		Scan(&c.SSELastEventID, &last, &etags)
	if errors.Is(err, sql.ErrNoRows) {
		return c, nil
	}
	if err != nil {
		return store.Cursor{}, err
	}
	if last.Valid {
		v := fromMS(last.Int64)
		c.LastSweepAt = &v
	}
	if err := json.Unmarshal([]byte(etags), &c.MemoryETags); err != nil {
		return store.Cursor{}, fmt.Errorf("sqlite: the stored memory etags of user %s are corrupt: %w", userID, err)
	}
	if c.MemoryETags == nil {
		c.MemoryETags = map[string]string{}
	}
	return c, nil
}

// PutCursor implements store.Tx.
func (t *tx) PutCursor(c store.Cursor) error {
	if c.UserID == "" {
		return store.Invalid("a cursor needs a user id")
	}
	etags := c.MemoryETags
	if etags == nil {
		etags = map[string]string{}
	}
	raw, err := json.Marshal(etags)
	if err != nil {
		return err
	}
	var last any
	if c.LastSweepAt != nil {
		last = ms(*c.LastSweepAt)
	}
	_, err = t.q.ExecContext(t.ctx,
		`INSERT INTO cursors(user_id, sse_last_event_id, last_sweep_at, memory_etags) VALUES(?,?,?,?)
		 ON CONFLICT(user_id) DO UPDATE SET
		     sse_last_event_id = excluded.sse_last_event_id,
		     last_sweep_at = excluded.last_sweep_at,
		     memory_etags = excluded.memory_etags`,
		c.UserID, c.SSELastEventID, last, string(raw))
	if isForeignKey(err) {
		return fmt.Errorf("%w: user", store.ErrNotFound)
	}
	return err
}
