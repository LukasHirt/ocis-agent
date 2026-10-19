package sqlite

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/store"
)

const (
	defaultJobLimit = 50
	maxJobLimit     = 500

	jobColumns = `id, user_id, trigger_kind, trigger_ref, goal, state, attempt, runs, not_before, checkpoint, error, created_at, updated_at`
)

// leaseHeld is the fence condition shared by the fenced writes: the user's
// lease row names this job, carries this fence and is not blank. The single
// placeholder is the fence; the job is matched through jobs.id.
const leaseHeld = `EXISTS (SELECT 1 FROM leases l WHERE l.user_id = jobs.user_id AND l.job_id = jobs.id AND l.fence = ? AND l.holder <> '')`

var errTTL = store.Invalid("the lease ttl must be at least one millisecond")

type scanner interface{ Scan(dest ...any) error }

func scanJob(row scanner) (store.Job, error) {
	var j store.Job
	var state, checkpoint string
	var notBefore, created, updated int64
	err := row.Scan(&j.ID, &j.UserID, &j.TriggerKind, &j.TriggerRef, &j.Goal, &state, &j.Attempt, &j.Runs,
		&notBefore, &checkpoint, &j.Error, &created, &updated)
	if err != nil {
		return store.Job{}, err
	}
	j.State = store.JobState(state)
	j.Checkpoint = json.RawMessage(checkpoint)
	j.NotBefore, j.CreatedAt, j.UpdatedAt = fromMS(notBefore), fromMS(created), fromMS(updated)
	return j, nil
}

// InsertJob implements store.Tx.
func (t *tx) InsertJob(j store.Job) (store.Job, error) {
	switch {
	case j.UserID == "" || j.TriggerKind == "" || j.Goal == "":
		return store.Job{}, store.Invalid("a job needs a user id, a trigger kind and a goal")
	case len(j.Checkpoint) > 0 && !json.Valid(j.Checkpoint):
		return store.Job{}, store.Invalid("the checkpoint is not valid JSON")
	}
	now := t.now()
	if j.ID == "" {
		j.ID = store.NewID()
	}
	j.State, j.Attempt, j.Runs, j.Error = store.JobQueued, 0, 0, ""
	if j.NotBefore.IsZero() {
		j.NotBefore = now
	}
	if len(j.Checkpoint) == 0 {
		j.Checkpoint = json.RawMessage("{}")
	}
	j.CreatedAt, j.UpdatedAt = now, now
	_, err := t.q.ExecContext(t.ctx,
		`INSERT INTO jobs(`+jobColumns+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		j.ID, j.UserID, j.TriggerKind, j.TriggerRef, j.Goal, string(j.State), j.Attempt, j.Runs,
		ms(j.NotBefore), string(j.Checkpoint), j.Error, ms(j.CreatedAt), ms(j.UpdatedAt))
	switch {
	case isUnique(err):
		return store.Job{}, fmt.Errorf("%w: job %s exists", store.ErrConflict, j.ID)
	case isForeignKey(err):
		return store.Job{}, fmt.Errorf("%w: user %s", store.ErrNotFound, j.UserID)
	case err != nil:
		return store.Job{}, fmt.Errorf("sqlite: insert job: %w", err)
	}
	return j, nil
}

// GetJob implements store.Tx.
func (t *tx) GetJob(userID, jobID string) (store.Job, error) {
	j, err := scanJob(t.q.QueryRowContext(t.ctx,
		`SELECT `+jobColumns+` FROM jobs WHERE id = ? AND user_id = ?`, jobID, userID))
	if errors.Is(err, sql.ErrNoRows) {
		return store.Job{}, store.ErrNotFound
	}
	return j, err
}

// GetJobStatus implements store.Tx.
func (t *tx) GetJobStatus(userID, jobID string) (store.JobStatus, error) {
	var state string
	var updated int64
	err := t.q.QueryRowContext(t.ctx, `SELECT state, updated_at FROM jobs WHERE id = ? AND user_id = ?`, jobID, userID).
		Scan(&state, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return store.JobStatus{}, store.ErrNotFound
	}
	if err != nil {
		return store.JobStatus{}, err
	}
	return store.JobStatus{State: store.JobState(state), UpdatedAt: fromMS(updated)}, nil
}

// keysetBefore returns the condition and arguments of a PageKey over the
// given time column (rows strictly after the key in "col DESC, id DESC").
// It is written as "col <= ? AND (col < ? OR id < ?)" so that SQLite uses a
// range on the (user_id, col) index; the equivalent "col < ? OR (col = ? AND
// id < ?)" makes it scan the user's whole index from the start, which turns
// walking all pages quadratic.
func keysetBefore(col string, k *store.PageKey) (string, []any, error) {
	if k == nil {
		return "", nil, nil
	}
	if k.ID == "" || k.At.IsZero() {
		return "", nil, store.Invalid("a page key needs a time and an id")
	}
	at := ms(k.At)
	return ` AND ` + col + ` <= ? AND (` + col + ` < ? OR id < ?)`, []any{at, at, k.ID}, nil
}

// jobListQuery builds the WHERE ... ORDER BY ... LIMIT tail shared by
// ListJobs and ListJobSummaries.
func jobListQuery(userID string, f store.JobFilter) (string, []any, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = defaultJobLimit
	}
	if limit > maxJobLimit {
		limit = maxJobLimit
	}
	query := ` FROM jobs WHERE user_id = ?`
	args := []any{userID}
	if len(f.States) > 0 {
		marks := make([]string, len(f.States))
		for i, s := range f.States {
			marks[i] = "?"
			args = append(args, string(s))
		}
		query += ` AND state IN (` + strings.Join(marks, ",") + `)`
	}
	cond, cargs, err := keysetBefore("created_at", f.Before)
	if err != nil {
		return "", nil, err
	}
	query += cond + ` ORDER BY created_at DESC, id DESC LIMIT ?`
	args = append(append(args, cargs...), limit)
	return query, args, nil
}

// ListJobs implements store.Tx.
func (t *tx) ListJobs(userID string, f store.JobFilter) ([]store.Job, error) {
	tail, args, err := jobListQuery(userID, f)
	if err != nil {
		return nil, err
	}
	rows, err := t.q.QueryContext(t.ctx, `SELECT `+jobColumns+tail, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// jobSummaryColumns are jobColumns without the checkpoint, plus the step
// count (a correlated count over the job_steps primary key). Not selecting
// the checkpoint keeps it out of Go memory, but SQLite still reads its
// overflow pages from disk for the columns stored after it in the row.
const jobSummaryColumns = `id, user_id, trigger_kind, trigger_ref, goal, state, attempt, runs, not_before, error, created_at, updated_at,
	(SELECT COUNT(*) FROM job_steps s WHERE s.job_id = jobs.id)`

// ListJobSummaries implements store.Tx.
func (t *tx) ListJobSummaries(userID string, f store.JobFilter) ([]store.JobSummary, error) {
	tail, args, err := jobListQuery(userID, f)
	if err != nil {
		return nil, err
	}
	rows, err := t.q.QueryContext(t.ctx, `SELECT `+jobSummaryColumns+tail, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.JobSummary
	for rows.Next() {
		var j store.JobSummary
		var state string
		var notBefore, created, updated int64
		if err := rows.Scan(&j.ID, &j.UserID, &j.TriggerKind, &j.TriggerRef, &j.Goal, &state, &j.Attempt, &j.Runs,
			&notBefore, &j.Error, &created, &updated, &j.StepCount); err != nil {
			return nil, err
		}
		j.State = store.JobState(state)
		j.NotBefore, j.CreatedAt, j.UpdatedAt = fromMS(notBefore), fromMS(created), fromMS(updated)
		out = append(out, j)
	}
	return out, rows.Err()
}

// CancelJob implements store.Tx.
func (t *tx) CancelJob(userID, jobID string) error {
	res, err := t.q.ExecContext(t.ctx,
		`UPDATE jobs SET state = 'cancelled', updated_at = ?
		 WHERE id = ? AND user_id = ? AND state NOT IN ('succeeded', 'failed', 'cancelled')`,
		ms(t.now()), jobID, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return t.diagnoseJobWrite(userID, jobID, false)
	}
	return nil
}

// diagnoseJobWrite explains why a conditional job update changed nothing:
// the job does not exist, it is terminal, or (for fenced writes) the lease is
// no longer ours.
func (t *tx) diagnoseJobWrite(userID, jobID string, fenced bool) error {
	j, err := t.GetJob(userID, jobID)
	if err != nil {
		return err
	}
	if j.State.Terminal() {
		return fmt.Errorf("%w: job is %s", store.ErrConflict, j.State)
	}
	if fenced {
		return store.ErrFenceLost
	}
	return store.ErrConflict
}

// SaveCheckpoint implements store.Tx.
func (t *tx) SaveCheckpoint(userID, jobID string, checkpoint json.RawMessage, fence int64) error {
	if len(checkpoint) == 0 || !json.Valid(checkpoint) {
		return store.Invalid("the checkpoint must be valid JSON")
	}
	res, err := t.q.ExecContext(t.ctx,
		`UPDATE jobs SET checkpoint = ?, updated_at = ?
		 WHERE id = ? AND user_id = ? AND state = 'running' AND `+leaseHeld,
		string(checkpoint), ms(t.now()), jobID, userID, fence)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return t.diagnoseJobWrite(userID, jobID, true)
	}
	return nil
}

// AppendStep implements store.Tx.
func (t *tx) AppendStep(step store.JobStep, fence int64) (store.JobStep, error) {
	if step.Kind != "llm" && step.Kind != "tool" {
		return store.JobStep{}, store.Invalid("unknown step kind %q", step.Kind)
	}
	if step.JobID == "" || step.Status == "" {
		return store.JobStep{}, store.Invalid("a step needs a job id and a status")
	}
	var userID string
	err := t.q.QueryRowContext(t.ctx, `SELECT user_id FROM jobs WHERE id = ?`, step.JobID).Scan(&userID)
	if errors.Is(err, sql.ErrNoRows) {
		return store.JobStep{}, store.ErrNotFound
	}
	if err != nil {
		return store.JobStep{}, err
	}

	now := t.now()
	res, err := t.q.ExecContext(t.ctx,
		`INSERT INTO job_steps(job_id, n, kind, tool, call_id, status, tokens_in, tokens_out, summary, approval_id, created_at)
		 SELECT jobs.id, COALESCE((SELECT MAX(n) FROM job_steps WHERE job_id = jobs.id), 0) + 1, ?, ?, ?, ?, ?, ?, ?, ?, ?
		 FROM jobs WHERE jobs.id = ? AND jobs.state = 'running' AND `+leaseHeld,
		step.Kind, step.Tool, step.CallID, step.Status, step.TokensIn, step.TokensOut, step.Summary, step.ApprovalID,
		ms(now), step.JobID, fence)
	if err != nil {
		return store.JobStep{}, fmt.Errorf("sqlite: append step: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.JobStep{}, t.diagnoseJobWrite(userID, step.JobID, true)
	}
	if err := t.q.QueryRowContext(t.ctx, `SELECT MAX(n) FROM job_steps WHERE job_id = ?`, step.JobID).Scan(&step.N); err != nil {
		return store.JobStep{}, err
	}
	step.CreatedAt = now
	return step, nil
}

// ListSteps implements store.Tx.
func (t *tx) ListSteps(userID, jobID string) ([]store.JobStep, error) {
	rows, err := t.q.QueryContext(t.ctx,
		`SELECT s.job_id, s.n, s.kind, s.tool, s.call_id, s.status, s.tokens_in, s.tokens_out, s.summary, s.approval_id, s.created_at
		 FROM job_steps s JOIN jobs j ON j.id = s.job_id
		 WHERE s.job_id = ? AND j.user_id = ? ORDER BY s.n`, jobID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.JobStep
	for rows.Next() {
		var s store.JobStep
		var created int64
		if err := rows.Scan(&s.JobID, &s.N, &s.Kind, &s.Tool, &s.CallID, &s.Status, &s.TokensIn, &s.TokensOut,
			&s.Summary, &s.ApprovalID, &created); err != nil {
			return nil, err
		}
		s.CreatedAt = fromMS(created)
		out = append(out, s)
	}
	return out, rows.Err()
}

// ClaimNextJob implements store.Tx.
func (t *tx) ClaimNextJob(worker string, ttl time.Duration) (store.Claim, bool, error) {
	if worker == "" {
		return store.Claim{}, false, store.Invalid("claiming needs a worker id")
	}
	if ttl < time.Millisecond {
		return store.Claim{}, false, errTTL
	}
	now := t.now()
	var id, userID string
	err := t.q.QueryRowContext(t.ctx,
		`SELECT j.id, j.user_id FROM jobs j LEFT JOIN leases l ON l.user_id = j.user_id
		 WHERE j.state = 'queued' AND j.not_before <= ?
		   AND (l.user_id IS NULL OR l.holder = '' OR l.expires_at <= ?)
		   AND NOT EXISTS (SELECT 1 FROM policies p WHERE p.user_id = j.user_id AND p.paused = 1)
		   AND NOT EXISTS (SELECT 1 FROM system s WHERE s.key = ? AND s.value = ?)
		 ORDER BY j.created_at, j.id LIMIT 1`, ms(now), ms(now), store.KillSwitchKey, store.KillSwitchOnValue).Scan(&id, &userID)
	if errors.Is(err, sql.ErrNoRows) {
		return store.Claim{}, false, nil
	}
	if err != nil {
		return store.Claim{}, false, fmt.Errorf("sqlite: find a runnable job: %w", err)
	}

	expires := now.Add(ttl)
	if _, err := t.q.ExecContext(t.ctx,
		`INSERT INTO leases(user_id, holder, job_id, fence, expires_at) VALUES(?,?,?,1,?)
		 ON CONFLICT(user_id) DO UPDATE SET
		     holder = excluded.holder, job_id = excluded.job_id,
		     fence = leases.fence + 1, expires_at = excluded.expires_at`,
		userID, worker, id, ms(expires)); err != nil {
		return store.Claim{}, false, fmt.Errorf("sqlite: take the lease: %w", err)
	}
	var fence int64
	if err := t.q.QueryRowContext(t.ctx, `SELECT fence FROM leases WHERE user_id = ?`, userID).Scan(&fence); err != nil {
		return store.Claim{}, false, err
	}
	if _, err := t.q.ExecContext(t.ctx,
		`UPDATE jobs SET state = 'running', attempt = attempt + 1, runs = runs + 1, updated_at = ? WHERE id = ?`, ms(now), id); err != nil {
		return store.Claim{}, false, err
	}
	job, err := t.GetJob(userID, id)
	if err != nil {
		return store.Claim{}, false, err
	}
	return store.Claim{Job: job, Worker: worker, Fence: fence, ExpiresAt: expires}, true, nil
}

// RenewLease implements store.Tx.
func (t *tx) RenewLease(c store.Claim, ttl time.Duration) (store.Claim, error) {
	if ttl < time.Millisecond {
		return store.Claim{}, errTTL
	}
	expires := t.now().Add(ttl)
	res, err := t.q.ExecContext(t.ctx,
		`UPDATE leases SET expires_at = ? WHERE user_id = ? AND holder = ? AND job_id = ? AND fence = ? AND holder <> ''
		 AND EXISTS (SELECT 1 FROM jobs WHERE jobs.id = leases.job_id AND jobs.state = 'running')`,
		ms(expires), c.Job.UserID, c.Worker, c.Job.ID, c.Fence)
	if err != nil {
		return store.Claim{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.Claim{}, t.diagnoseJobWrite(c.Job.UserID, c.Job.ID, true)
	}
	c.ExpiresAt = expires
	return c, nil
}

// ReleaseJob implements store.Tx.
func (t *tx) ReleaseJob(c store.Claim, next store.JobState, checkpoint json.RawMessage, errMsg string) (bool, error) {
	if !next.Valid() || next == store.JobRunning {
		return false, store.Invalid("cannot release a job into state %q", next)
	}
	if len(checkpoint) > 0 && !json.Valid(checkpoint) {
		return false, store.Invalid("the checkpoint must be valid JSON")
	}
	res, err := t.q.ExecContext(t.ctx,
		`UPDATE leases SET holder = '', job_id = '', expires_at = 0
		 WHERE user_id = ? AND holder = ? AND job_id = ? AND fence = ? AND holder <> ''`,
		c.Job.UserID, c.Worker, c.Job.ID, c.Fence)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, t.diagnoseJobWrite(c.Job.UserID, c.Job.ID, true)
	}
	res, err = t.q.ExecContext(t.ctx,
		`UPDATE jobs SET state = ?, checkpoint = CASE WHEN ? = '' THEN checkpoint ELSE ? END, error = ?,
		     attempt = CASE WHEN ? IN ('queued','waiting_approval') AND attempt > 0 THEN attempt - 1 ELSE attempt END,
		     updated_at = ?
		 WHERE id = ? AND user_id = ? AND state = 'running'`,
		string(next), string(checkpoint), string(checkpoint), errMsg, string(next), ms(t.now()), c.Job.ID, c.Job.UserID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ReapExpired implements store.Tx.
func (t *tx) ReapExpired(maxAttempts int, backoff func(attempt int) time.Duration) (store.ReapResult, error) {
	if maxAttempts < 1 || backoff == nil {
		return store.ReapResult{}, store.Invalid("reaping needs maxAttempts >= 1 and a backoff function")
	}
	now := t.now()

	type stale struct {
		id, userID string
		attempt    int
	}
	rows, err := t.q.QueryContext(t.ctx,
		`SELECT j.id, j.user_id, j.attempt FROM jobs j LEFT JOIN leases l ON l.user_id = j.user_id
		 WHERE j.state = 'running' AND (l.user_id IS NULL OR l.job_id <> j.id OR l.expires_at <= ?)`, ms(now))
	if err != nil {
		return store.ReapResult{}, err
	}
	var jobs []stale
	for rows.Next() {
		var s stale
		if err := rows.Scan(&s.id, &s.userID, &s.attempt); err != nil {
			_ = rows.Close()
			return store.ReapResult{}, err
		}
		jobs = append(jobs, s)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return store.ReapResult{}, err
	}
	_ = rows.Close()

	var res store.ReapResult
	for _, s := range jobs {
		if s.attempt >= maxAttempts {
			_, err = t.q.ExecContext(t.ctx,
				`UPDATE jobs SET state = 'failed', error = 'max attempts exceeded', updated_at = ? WHERE id = ?`, ms(now), s.id)
			res.Failed++
		} else {
			_, err = t.q.ExecContext(t.ctx,
				`UPDATE jobs SET state = 'queued', not_before = ?, updated_at = ? WHERE id = ?`,
				ms(now.Add(backoff(s.attempt))), ms(now), s.id)
			res.Requeued++
		}
		if err != nil {
			return store.ReapResult{}, err
		}
		// blank the lease only if it still belongs to this job; keep the fence
		if _, err = t.q.ExecContext(t.ctx,
			`UPDATE leases SET holder = '', job_id = '', expires_at = 0 WHERE user_id = ? AND job_id = ?`,
			s.userID, s.id); err != nil {
			return store.ReapResult{}, err
		}
	}
	return res, nil
}
