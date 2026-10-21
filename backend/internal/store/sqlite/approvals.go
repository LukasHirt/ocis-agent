package sqlite

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/owncloud/ocis-agent/backend/internal/store"
)

const (
	defaultApprovalLimit = 50
	maxApprovalLimit     = 500

	approvalColumns = `id, user_id, job_id, call_id, tool, risk, args, args_hash, target_etag, rationale, preview,
	idem_key, state, requested_at, expires_at, decided_by, decided_at, executed_at, result, acknowledged_at, acknowledged_by`
)

func scanApproval(row scanner) (store.Approval, error) {
	var a store.Approval
	var args, preview, state, result string
	var requested, expires int64
	var decided, executed, acked sql.NullInt64
	if err := row.Scan(&a.ID, &a.UserID, &a.JobID, &a.CallID, &a.Tool, &a.Risk, &args, &a.ArgsHash, &a.TargetETag,
		&a.Rationale, &preview, &a.IdemKey, &state, &requested, &expires, &a.DecidedBy, &decided, &executed, &result,
		&acked, &a.AcknowledgedBy); err != nil {
		return store.Approval{}, err
	}
	a.Args = json.RawMessage(args)
	a.Preview = json.RawMessage(preview)
	a.State = store.ApprovalState(state)
	a.RequestedAt, a.ExpiresAt = fromMS(requested), fromMS(expires)
	if decided.Valid {
		d := fromMS(decided.Int64)
		a.DecidedAt = &d
	}
	if executed.Valid {
		e := fromMS(executed.Int64)
		a.ExecutedAt = &e
	}
	if result != "" {
		a.Result = json.RawMessage(result)
	}
	if acked.Valid {
		k := fromMS(acked.Int64)
		a.AcknowledgedAt = &k
	}
	return a, nil
}

// InsertApproval implements store.Tx.
func (t *tx) InsertApproval(a store.Approval, fence int64) (store.Approval, error) {
	switch {
	case a.UserID == "" || a.JobID == "" || a.CallID == "" || a.Tool == "":
		return store.Approval{}, store.Invalid("an approval needs a user id, a job id, a call id and a tool")
	case a.Risk != store.RiskWrite && a.Risk != store.RiskDestructive && a.Risk != store.RiskOutward:
		return store.Approval{}, store.Invalid("an approval needs a risk of write, destructive or outward, got %q", a.Risk)
	case len(a.Args) == 0 || !json.Valid(a.Args):
		return store.Approval{}, store.Invalid("the approval arguments must be valid JSON")
	case a.ArgsHash == "" || a.IdemKey == "":
		return store.Approval{}, store.Invalid("an approval needs an arguments hash and an idempotency key")
	case len(a.Preview) > 0 && !json.Valid(a.Preview):
		return store.Approval{}, store.Invalid("the approval preview must be valid JSON")
	}
	now := t.now()
	if !a.ExpiresAt.After(now) {
		return store.Approval{}, store.Invalid("an approval must expire in the future")
	}
	if err := t.AssertLease(a.UserID, a.JobID, fence); err != nil {
		return store.Approval{}, err
	}

	if a.ID == "" {
		a.ID = store.NewID()
	}
	if len(a.Preview) == 0 {
		a.Preview = json.RawMessage("{}")
	}
	a.State, a.RequestedAt = store.ApprovalPending, fromMS(ms(now))
	a.ExpiresAt = fromMS(ms(a.ExpiresAt))
	a.DecidedBy, a.DecidedAt, a.ExecutedAt, a.Result = "", nil, nil, nil
	a.AcknowledgedAt, a.AcknowledgedBy = nil, ""
	_, err := t.q.ExecContext(t.ctx,
		`INSERT INTO approvals(`+approvalColumns+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,NULL,NULL,'',NULL,'')`,
		a.ID, a.UserID, a.JobID, a.CallID, a.Tool, a.Risk, string(a.Args), a.ArgsHash, a.TargetETag, a.Rationale,
		string(a.Preview), a.IdemKey, string(a.State), ms(a.RequestedAt), ms(a.ExpiresAt), a.DecidedBy)
	switch {
	case isUnique(err):
		return store.Approval{}, fmt.Errorf("%w: approval id or idempotency key exists", store.ErrConflict)
	case isForeignKey(err):
		return store.Approval{}, fmt.Errorf("%w: user or job", store.ErrNotFound)
	case err != nil:
		return store.Approval{}, fmt.Errorf("sqlite: insert approval: %w", err)
	}
	return a, nil
}

// GetApproval implements store.Tx.
func (t *tx) GetApproval(userID, id string) (store.Approval, error) {
	a, err := scanApproval(t.q.QueryRowContext(t.ctx,
		`SELECT `+approvalColumns+` FROM approvals WHERE id = ? AND user_id = ?`, id, userID))
	if errors.Is(err, sql.ErrNoRows) {
		return store.Approval{}, store.ErrNotFound
	}
	return a, err
}

// GetApprovalKey implements store.Tx.
func (t *tx) GetApprovalKey(userID, id string) (store.PageKey, error) {
	var at int64
	err := t.q.QueryRowContext(t.ctx, `SELECT requested_at FROM approvals WHERE id = ? AND user_id = ?`, id, userID).Scan(&at)
	if errors.Is(err, sql.ErrNoRows) {
		return store.PageKey{}, store.ErrNotFound
	}
	if err != nil {
		return store.PageKey{}, err
	}
	return store.PageKey{At: fromMS(at), ID: id}, nil
}

// CountPendingOfLiveJobs implements store.Tx.
func (t *tx) CountPendingOfLiveJobs(userID string) (int, error) {
	var n int
	err := t.q.QueryRowContext(t.ctx,
		`SELECT COUNT(*) FROM approvals a JOIN jobs j ON j.id = a.job_id AND j.user_id = a.user_id
		 WHERE a.user_id = ? AND a.state = 'pending' AND j.state NOT IN `+terminalJobStates,
		userID).Scan(&n)
	return n, err
}

// approvalListQuery is the FROM ... ORDER BY tail of a list query and its
// arguments, with the limit clamped.
func approvalListQuery(userID string, f store.ApprovalFilter, extra string) (string, []any, int, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = defaultApprovalLimit
	}
	if limit > maxApprovalLimit {
		limit = maxApprovalLimit
	}
	if f.MaxBytes < 0 {
		return "", nil, 0, store.Invalid("the byte budget must not be negative")
	}
	query := ` FROM approvals WHERE user_id = ?`
	args := []any{userID}
	if f.JobID != "" {
		query += ` AND job_id = ?`
		args = append(args, f.JobID)
	}
	if len(f.States) > 0 {
		marks := make([]string, len(f.States))
		for i, s := range f.States {
			marks[i] = "?"
			args = append(args, string(s))
		}
		query += ` AND state IN (` + strings.Join(marks, ",") + `)`
	}
	query += extra
	cond, cargs, err := keysetBefore("requested_at", f.Before)
	if err != nil {
		return "", nil, 0, err
	}
	query += cond + ` ORDER BY requested_at DESC, id DESC`
	return query, append(args, cargs...), limit, nil
}

// ListApprovals implements store.Tx.
func (t *tx) ListApprovals(userID string, f store.ApprovalFilter) ([]store.Approval, error) {
	query, args, limit, err := approvalListQuery(userID, f, "")
	if err != nil {
		return nil, err
	}
	query += ` LIMIT ?`
	args = append(args, limit)
	if f.MaxBytes > 0 {
		// first the sizes, then only the rows within the budget
		ids, err := t.approvalsWithin(query, args, f.MaxBytes)
		if err != nil {
			return nil, err
		}
		return t.approvalsByID(userID, ids)
	}
	return t.queryApprovals(query, args)
}

// ListApprovalsToVerify implements store.Tx.
func (t *tx) ListApprovalsToVerify(userID string, f store.ApprovalFilter, maxScan int) ([]store.Approval, *store.PageKey, error) {
	if maxScan <= 0 || maxScan > store.MaxVerifyScan {
		maxScan = store.MaxVerifyScan
	}
	// the candidates; Approval.NeedsVerification decides on each
	query, args, limit, err := approvalListQuery(userID, f,
		` AND acknowledged_at IS NULL AND (state = 'unknown' OR result != '')`)
	if err != nil {
		return nil, nil, err
	}
	rows, err := t.q.QueryContext(t.ctx,
		`SELECT id, requested_at, ifnull(length(CAST(args AS BLOB)), 0) + ifnull(length(CAST(preview AS BLOB)), 0), state, result`+
			query+` LIMIT ?`, append(args, maxScan)...)
	if err != nil {
		return nil, nil, err
	}
	var ids []any
	var total int64
	var last store.PageKey
	scanned, stopped := 0, false
	for rows.Next() {
		var id, state, result string
		var at, n int64
		if err := rows.Scan(&id, &at, &n, &state, &result); err != nil {
			_ = rows.Close()
			return nil, nil, err
		}
		scanned++
		last = store.PageKey{At: fromMS(at), ID: id}
		a := store.Approval{State: store.ApprovalState(state)}
		if result != "" {
			a.Result = json.RawMessage(result)
		}
		if !a.NeedsVerification() {
			continue
		}
		ids = append(ids, id)
		if total += n; len(ids) >= limit || (f.MaxBytes > 0 && total >= f.MaxBytes) {
			stopped = true
			break
		}
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return nil, nil, err
	}
	var to *store.PageKey
	if !stopped && scanned == maxScan {
		to = &last
	}
	list, err := t.approvalsByID(userID, ids)
	if err != nil {
		return nil, nil, err
	}
	return list, to, nil
}

// approvalsByID loads the user's approvals with these ids in list order.
func (t *tx) approvalsByID(userID string, ids []any) ([]store.Approval, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	marks := strings.Repeat(",?", len(ids))[1:]
	return t.queryApprovals(` FROM approvals WHERE user_id = ? AND id IN (`+marks+`) ORDER BY requested_at DESC, id DESC`,
		append([]any{userID}, ids...))
}

func (t *tx) queryApprovals(tail string, args []any) ([]store.Approval, error) {
	rows, err := t.q.QueryContext(t.ctx, `SELECT `+approvalColumns+tail, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.Approval
	for rows.Next() {
		a, err := scanApproval(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// approvalsWithin returns the ids of the rows of the list query (FROM ...
// LIMIT tail) until their args and preview reach budget bytes.
func (t *tx) approvalsWithin(tail string, args []any, budget int64) ([]any, error) {
	rows, err := t.q.QueryContext(t.ctx,
		`SELECT id, ifnull(length(CAST(args AS BLOB)), 0) + ifnull(length(CAST(preview AS BLOB)), 0)`+tail, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []any
	var total int64
	for rows.Next() {
		var id string
		var n int64
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		ids = append(ids, id)
		if total += n; total >= budget {
			break
		}
	}
	return ids, rows.Err()
}

// AcknowledgeApproval implements store.Tx.
func (t *tx) AcknowledgeApproval(userID, id, by string) (store.Approval, bool, error) {
	if by == "" {
		return store.Approval{}, false, store.Invalid("an acknowledgement needs to record who acknowledged")
	}
	a, err := t.GetApproval(userID, id)
	if err != nil {
		return store.Approval{}, false, err
	}
	if a.AcknowledgedAt != nil {
		return a, false, nil
	}
	if !a.State.Terminal() || !a.OutcomeUncertain() {
		return store.Approval{}, false, fmt.Errorf("%w: the outcome of the approval is not uncertain", store.ErrConflict)
	}
	now := fromMS(ms(t.now()))
	res, err := t.q.ExecContext(t.ctx,
		`UPDATE approvals SET acknowledged_at = ?, acknowledged_by = ?
		 WHERE id = ? AND user_id = ? AND acknowledged_at IS NULL AND state = ? AND result = ?`,
		ms(now), by, id, userID, string(a.State), string(a.Result))
	if err != nil {
		return store.Approval{}, false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.Approval{}, false, fmt.Errorf("%w: the approval changed", store.ErrConflict)
	}
	a.AcknowledgedAt, a.AcknowledgedBy = &now, by
	return a, true, nil
}

// DecideApproval implements store.Tx.
func (t *tx) DecideApproval(userID, id string, decision store.ApprovalState, decidedBy, argsHash string) (store.Approval, error) {
	if decision != store.ApprovalApproved && decision != store.ApprovalRejected {
		return store.Approval{}, store.Invalid("a decision must be approved or rejected, got %q", decision)
	}
	if decidedBy == "" {
		return store.Approval{}, store.Invalid("a decision needs to record who decided")
	}
	a, err := t.GetApproval(userID, id)
	if err != nil {
		return store.Approval{}, err
	}
	if a.State != store.ApprovalPending {
		return store.Approval{}, fmt.Errorf("%w: approval is %s", store.ErrConflict, a.State)
	}
	now := t.now()
	if !now.Before(a.ExpiresAt) {
		return store.Approval{}, store.ErrExpired
	}
	if argsHash != a.ArgsHash {
		return store.Approval{}, store.Invalid("the arguments hash does not match the approval")
	}
	res, err := t.q.ExecContext(t.ctx,
		`UPDATE approvals SET state = ?, decided_by = ?, decided_at = ?
		 WHERE id = ? AND user_id = ? AND state = 'pending'`,
		string(decision), decidedBy, ms(now), id, userID)
	if err != nil {
		return store.Approval{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.Approval{}, fmt.Errorf("%w: approval is no longer pending", store.ErrConflict)
	}
	a.State, a.DecidedBy = decision, decidedBy
	decidedAt := fromMS(ms(now))
	a.DecidedAt = &decidedAt
	return a, nil
}

// TransitionApproval implements store.Tx.
func (t *tx) TransitionApproval(userID, id string, from, to store.ApprovalState, patch store.ApprovalPatch) (store.Approval, error) {
	if !store.ApprovalTransitionAllowed(from, to) {
		return store.Approval{}, store.Invalid("approval transition %s -> %s is not allowed", from, to)
	}
	if len(patch.Result) > 0 && !json.Valid(patch.Result) {
		return store.Approval{}, store.Invalid("the approval result must be valid JSON")
	}
	res, err := t.q.ExecContext(t.ctx,
		`UPDATE approvals SET state = ?,
		     result = CASE WHEN ? = '' THEN result ELSE ? END,
		     executed_at = CASE WHEN ? = 'executed' THEN ? ELSE executed_at END
		 WHERE id = ? AND user_id = ? AND state = ?`,
		string(to), string(patch.Result), string(patch.Result), string(to), ms(t.now()), id, userID, string(from))
	if err != nil {
		return store.Approval{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		cur, err := t.GetApproval(userID, id)
		if err != nil {
			return store.Approval{}, err
		}
		return store.Approval{}, fmt.Errorf("%w: approval is %s, expected %s", store.ErrConflict, cur.State, from)
	}
	return t.GetApproval(userID, id)
}

// ExpireApprovals implements store.Tx.
func (t *tx) ExpireApprovals() ([]store.ApprovalRef, error) {
	rows, err := t.q.QueryContext(t.ctx,
		`UPDATE approvals SET state = 'expired' WHERE state = 'pending' AND expires_at <= ?
		 RETURNING id, user_id, job_id`, ms(t.now()))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.ApprovalRef
	for rows.Next() {
		var r store.ApprovalRef
		if err := rows.Scan(&r.ID, &r.UserID, &r.JobID); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ResumeJobIfSettled implements store.Tx.
func (t *tx) ResumeJobIfSettled(userID, jobID string) (bool, error) {
	j, err := t.GetJob(userID, jobID)
	if err != nil {
		return false, err
	}
	if j.State != store.JobWaitingApproval {
		return false, nil
	}
	var pending int
	if err := t.q.QueryRowContext(t.ctx,
		`SELECT COUNT(*) FROM approvals WHERE job_id = ? AND user_id = ? AND state = 'pending'`,
		jobID, userID).Scan(&pending); err != nil {
		return false, err
	}
	if pending > 0 {
		return false, nil
	}
	now := t.now()
	res, err := t.q.ExecContext(t.ctx,
		`UPDATE jobs SET state = 'queued', not_before = ?, updated_at = ?
		 WHERE id = ? AND user_id = ? AND state = 'waiting_approval'`, ms(now), ms(now), jobID, userID)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, nil
	}
	return true, t.dropPartial(jobID)
}
