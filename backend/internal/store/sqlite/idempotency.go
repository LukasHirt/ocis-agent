package sqlite

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/store"
)

const (
	defaultIdemLimit = 100
	maxIdemLimit     = 1000

	idemColumns = `idem_key, user_id, tool, state, started_at, finished_at, result`
)

func scanIdem(row scanner) (store.IdemRecord, error) {
	var r store.IdemRecord
	var state, result string
	var started int64
	var finished sql.NullInt64
	if err := row.Scan(&r.Key, &r.UserID, &r.Tool, &state, &started, &finished, &result); err != nil {
		return store.IdemRecord{}, err
	}
	r.State = store.IdemState(state)
	r.StartedAt = fromMS(started)
	if finished.Valid {
		f := fromMS(finished.Int64)
		r.FinishedAt = &f
	}
	if result != "" {
		r.Result = json.RawMessage(result)
	}
	return r, nil
}

// BeginIdempotent implements store.Tx.
func (t *tx) BeginIdempotent(rec store.IdemRecord) (*store.IdemRecord, error) {
	if rec.Key == "" || rec.UserID == "" || rec.Tool == "" {
		return nil, store.Invalid("an idempotency record needs a key, a user id and a tool")
	}
	res, err := t.q.ExecContext(t.ctx,
		`INSERT INTO idempotency(idem_key, user_id, tool, state, started_at, result)
		 VALUES(?, ?, ?, 'started', ?, '')
		 ON CONFLICT(idem_key) DO NOTHING`, rec.Key, rec.UserID, rec.Tool, ms(t.now()))
	if isForeignKey(err) {
		return nil, fmt.Errorf("%w: user", store.ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: begin idempotent call: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 1 {
		return nil, nil
	}
	existing, err := scanIdem(t.q.QueryRowContext(t.ctx,
		`SELECT `+idemColumns+` FROM idempotency WHERE idem_key = ?`, rec.Key))
	if err != nil {
		return nil, err
	}
	if existing.UserID != rec.UserID {
		return nil, fmt.Errorf("%w: the idempotency key belongs to another user", store.ErrConflict)
	}
	return &existing, nil
}

// FinishIdempotent implements store.Tx.
func (t *tx) FinishIdempotent(userID, key string, state store.IdemState, result json.RawMessage) error {
	if state != store.IdemDone && state != store.IdemFailed {
		return store.Invalid("an idempotency record finishes as done or failed, got %q", state)
	}
	if len(result) > 0 && !json.Valid(result) {
		return store.Invalid("the idempotency result must be valid JSON")
	}
	res, err := t.q.ExecContext(t.ctx,
		`UPDATE idempotency SET state = ?, finished_at = ?,
		     result = CASE WHEN ? = '' THEN result ELSE ? END
		 WHERE idem_key = ? AND user_id = ? AND state = 'started'`,
		string(state), ms(t.now()), string(result), string(result), key, userID)
	if err != nil {
		return fmt.Errorf("sqlite: finish idempotent call: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		cur, err := t.GetIdempotent(userID, key)
		if err != nil {
			return err
		}
		return fmt.Errorf("%w: the idempotency record is %s", store.ErrConflict, cur.State)
	}
	return nil
}

// GetIdempotent implements store.Tx.
func (t *tx) GetIdempotent(userID, key string) (store.IdemRecord, error) {
	r, err := scanIdem(t.q.QueryRowContext(t.ctx,
		`SELECT `+idemColumns+` FROM idempotency WHERE idem_key = ? AND user_id = ?`, key, userID))
	if errors.Is(err, sql.ErrNoRows) {
		return store.IdemRecord{}, store.ErrNotFound
	}
	return r, err
}

// ListStartedIdempotent implements store.Tx.
func (t *tx) ListStartedIdempotent(olderThan time.Time, limit int) ([]store.IdemRecord, error) {
	return t.listIdem(
		`SELECT `+idemColumns+` FROM idempotency WHERE state = 'started' AND started_at <= ?
		 ORDER BY started_at, idem_key LIMIT ?`, olderThan, limit)
}

// ListStartedIdempotentOfEndedJobs implements store.Tx. The job id is the
// key's prefix before the first ":" (the whole key if it has none), matched
// against the record user's jobs only.
func (t *tx) ListStartedIdempotentOfEndedJobs(olderThan time.Time, limit int) ([]store.IdemRecord, error) {
	return t.listIdem(
		`SELECT i.idem_key, i.user_id, i.tool, i.state, i.started_at, i.finished_at, i.result
		 FROM idempotency i
		 LEFT JOIN jobs j ON j.user_id = i.user_id AND j.id = CASE
		     WHEN instr(i.idem_key, ':') > 0 THEN substr(i.idem_key, 1, instr(i.idem_key, ':') - 1)
		     ELSE i.idem_key END
		 WHERE i.state = 'started' AND i.started_at <= ?
		   AND (j.id IS NULL OR j.state IN `+terminalJobStates+`)
		 ORDER BY i.started_at, i.idem_key LIMIT ?`, olderThan, limit)
}

func (t *tx) listIdem(query string, olderThan time.Time, limit int) ([]store.IdemRecord, error) {
	if limit <= 0 {
		limit = defaultIdemLimit
	}
	if limit > maxIdemLimit {
		limit = maxIdemLimit
	}
	rows, err := t.q.QueryContext(t.ctx, query, ms(olderThan), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.IdemRecord
	for rows.Next() {
		r, err := scanIdem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
