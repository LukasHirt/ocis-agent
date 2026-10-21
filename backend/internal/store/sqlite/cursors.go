package sqlite

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/store"
)

// GetCursor implements store.Tx.
func (t *tx) GetCursor(userID string) (store.Cursor, error) {
	c := store.Cursor{UserID: userID, MemoryETags: map[string]string{}, WatchETags: map[string]string{}}
	var last, dirty, changed sql.NullInt64
	var etags, watch string
	err := t.q.QueryRowContext(t.ctx,
		`SELECT sse_last_event_id, last_sweep_at, memory_etags, watch_etags, watch_dirty_since, watch_changed_at
		 FROM cursors WHERE user_id = ?`, userID).
		Scan(&c.SSELastEventID, &last, &etags, &watch, &dirty, &changed)
	if errors.Is(err, sql.ErrNoRows) {
		return c, nil
	}
	if err != nil {
		return store.Cursor{}, err
	}
	c.LastSweepAt, c.WatchDirtySince, c.WatchChangedAt = optTime(last), optTime(dirty), optTime(changed)
	if err := json.Unmarshal([]byte(etags), &c.MemoryETags); err != nil {
		return store.Cursor{}, fmt.Errorf("sqlite: the stored memory etags of user %s are corrupt: %w", userID, err)
	}
	if c.MemoryETags == nil {
		c.MemoryETags = map[string]string{}
	}
	if err := json.Unmarshal([]byte(watch), &c.WatchETags); err != nil {
		return store.Cursor{}, fmt.Errorf("sqlite: the stored watch etags of user %s are corrupt: %w", userID, err)
	}
	if c.WatchETags == nil {
		c.WatchETags = map[string]string{}
	}
	return c, nil
}

func optTime(v sql.NullInt64) *time.Time {
	if !v.Valid {
		return nil
	}
	t := fromMS(v.Int64)
	return &t
}

func optMS(t *time.Time) any {
	if t == nil {
		return nil
	}
	return ms(*t)
}

func jsonMap(m map[string]string) (string, error) {
	if m == nil {
		m = map[string]string{}
	}
	raw, err := json.Marshal(m)
	return string(raw), err
}

// PutCursor implements store.Tx.
func (t *tx) PutCursor(c store.Cursor) error {
	if c.UserID == "" {
		return store.Invalid("a cursor needs a user id")
	}
	etags, err := jsonMap(c.MemoryETags)
	if err != nil {
		return err
	}
	watch, err := jsonMap(c.WatchETags)
	if err != nil {
		return err
	}
	_, err = t.q.ExecContext(t.ctx,
		`INSERT INTO cursors(user_id, sse_last_event_id, last_sweep_at, memory_etags, watch_etags, watch_dirty_since, watch_changed_at)
		 VALUES(?,?,?,?,?,?,?)
		 ON CONFLICT(user_id) DO UPDATE SET
		     sse_last_event_id = excluded.sse_last_event_id,
		     last_sweep_at = excluded.last_sweep_at,
		     memory_etags = excluded.memory_etags,
		     watch_etags = excluded.watch_etags,
		     watch_dirty_since = excluded.watch_dirty_since,
		     watch_changed_at = excluded.watch_changed_at`,
		c.UserID, c.SSELastEventID, optMS(c.LastSweepAt), etags, watch, optMS(c.WatchDirtySince), optMS(c.WatchChangedAt))
	if isForeignKey(err) {
		return fmt.Errorf("%w: user", store.ErrNotFound)
	}
	return err
}
