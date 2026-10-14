package sqlite

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/owncloud/ocis-agent/backend/internal/audit"
)

const (
	defaultAuditLimit = 100
	maxAuditLimit     = 1000
	verifyPageSize    = 500
)

// AppendAudit implements store.Tx.
func (t *tx) AppendAudit(ev audit.Event) (audit.Record, error) {
	if ev.UserID == "" || ev.Kind == "" {
		return audit.Record{}, errors.New("store: an audit event needs a user id and a kind")
	}
	if ev.TS.IsZero() {
		ev.TS = t.now()
	}
	ev.TS = fromMS(ms(ev.TS)) // millisecond precision, UTC: what is stored and hashed

	seq, prev := int64(1), audit.Genesis
	var lastSeq int64
	var lastHash string
	err := t.q.QueryRowContext(t.ctx,
		`SELECT seq, hash FROM audit WHERE user_id = ? ORDER BY seq DESC LIMIT 1`, ev.UserID).
		Scan(&lastSeq, &lastHash)
	switch {
	case err == nil:
		seq, prev = lastSeq+1, lastHash
	case errors.Is(err, sql.ErrNoRows):
	default:
		return audit.Record{}, fmt.Errorf("sqlite: read audit head: %w", err)
	}

	rec := audit.Record{Event: ev, Seq: seq, PrevHash: prev}
	rec.Hash = audit.Hash(prev, seq, ev)
	_, err = t.q.ExecContext(t.ctx,
		`INSERT INTO audit(user_id, seq, ts, job_id, kind, tool, risk, args_hash, cred, outcome, detail, prev_hash, hash)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		rec.UserID, rec.Seq, ms(rec.TS), rec.JobID, rec.Kind, rec.Tool, rec.Risk, rec.ArgsHash,
		rec.Cred, rec.Outcome, rec.Detail, rec.PrevHash, rec.Hash)
	if err != nil {
		return audit.Record{}, fmt.Errorf("sqlite: append audit: %w", err)
	}
	return rec, nil
}

// ReadAudit implements store.Tx.
func (t *tx) ReadAudit(userID string, afterSeq int64, limit int) ([]audit.Record, error) {
	if limit <= 0 {
		limit = defaultAuditLimit
	}
	if limit > maxAuditLimit {
		limit = maxAuditLimit
	}
	rows, err := t.q.QueryContext(t.ctx,
		`SELECT user_id, seq, ts, job_id, kind, tool, risk, args_hash, cred, outcome, detail, prev_hash, hash
		 FROM audit WHERE user_id = ? AND seq > ? ORDER BY seq LIMIT ?`, userID, afterSeq, limit)
	if err != nil {
		return nil, fmt.Errorf("sqlite: read audit: %w", err)
	}
	defer rows.Close()

	var out []audit.Record
	for rows.Next() {
		var r audit.Record
		var ts int64
		if err := rows.Scan(&r.UserID, &r.Seq, &ts, &r.JobID, &r.Kind, &r.Tool, &r.Risk, &r.ArgsHash,
			&r.Cred, &r.Outcome, &r.Detail, &r.PrevHash, &r.Hash); err != nil {
			return nil, err
		}
		r.TS = fromMS(ts)
		out = append(out, r)
	}
	return out, rows.Err()
}

// VerifyAudit implements store.Tx.
func (t *tx) VerifyAudit(userID string) error {
	v := audit.NewVerifier()
	var after int64
	for {
		recs, err := t.ReadAudit(userID, after, verifyPageSize)
		if err != nil {
			return err
		}
		if len(recs) == 0 {
			return nil
		}
		for _, r := range recs {
			if err := v.Add(r); err != nil {
				return err
			}
		}
		after = recs[len(recs)-1].Seq
	}
}
