package approvals

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/audit"
	"github.com/owncloud/ocis-agent/backend/internal/ocisclient"
	"github.com/owncloud/ocis-agent/backend/internal/store"
	"github.com/owncloud/ocis-agent/backend/internal/tools"
)

// EnvFunc builds the tool environment for a user (credentials, clients). The
// scheduler supplies it; errors leave the user's records for the next pass.
type EnvFunc func(ctx context.Context, userID string) (tools.Env, error)

// ReconcileSummary counts what ReconcileOrphans did.
type ReconcileSummary struct {
	Resolved int
	Skipped  int
	Errors   []error
}

const notRetried = "the job ended before the action could be retried; it was not run"

// ReconcileOrphans resolves calls whose outcome was never recorded (an
// idempotency record still started) and whose job has ENDED (or no longer
// exists). While a job is still active it reconciles its own records when it
// runs again, so those are not listed at all (they cannot fill limit and
// starve the records of ended jobs behind them; Skipped counts only a record
// whose job turned out active anyway). An ended job is never resumed: a
// "retry" verdict is recorded as a failure, not executed.
//
// olderThan is clamped to Now()-RetrySettle: a call that started within the
// settle window may still be in flight (cancelling a job does not stop its
// worker), so a sweep never touches it. Pass a cut-off of at least the longest
// call duration plus RetrySettle.
func (x *Executor) ReconcileOrphans(ctx context.Context, envFor EnvFunc, olderThan time.Time, limit int) (ReconcileSummary, error) {
	if settled := x.cfg.Now().Add(-x.cfg.RetrySettle); olderThan.After(settled) {
		olderThan = settled
	}
	var recs []store.IdemRecord
	if err := x.st.View(ctx, func(tx store.Tx) error {
		var err error
		recs, err = tx.ListStartedIdempotentOfEndedJobs(olderThan, limit)
		return err
	}); err != nil {
		return ReconcileSummary{}, fmt.Errorf("approvals: list started records: %w", err)
	}
	var sum ReconcileSummary
	for _, rec := range recs {
		resolved, err := x.reconcileOne(ctx, envFor, rec)
		switch {
		case err != nil:
			sum.Errors = append(sum.Errors, err)
		case resolved:
			sum.Resolved++
		default:
			sum.Skipped++
		}
	}
	return sum, nil
}

// orphanMeta is what the audit event of a reconciled orphan records.
type orphanMeta struct{ tool, argsHash, risk, cred string }

func (x *Executor) reconcileOne(ctx context.Context, envFor EnvFunc, rec store.IdemRecord) (bool, error) {
	jobID, _, _ := strings.Cut(rec.Key, ":")
	var (
		job      store.Job
		jobFound bool
		approval *store.Approval
	)
	if err := x.st.View(ctx, func(tx store.Tx) error {
		j, err := tx.GetJob(rec.UserID, jobID)
		switch {
		case err == nil:
			job, jobFound = j, true
		case !errors.Is(err, store.ErrNotFound):
			return err
		}
		as, err := tx.ListApprovals(rec.UserID, store.ApprovalFilter{JobID: jobID, States: []store.ApprovalState{store.ApprovalExecuting}, Limit: 500})
		if err != nil {
			return err
		}
		for i := range as {
			if as[i].IdemKey == rec.Key {
				approval = &as[i]
			}
		}
		return nil
	}); err != nil {
		return false, err
	}
	if jobFound && !job.State.Terminal() {
		return false, nil // the active job reconciles its own record
	}

	meta := orphanMeta{tool: rec.Tool}
	out := Outcome{State: OutcomeUnknown, Reason: "the job ended before the outcome was recorded; please verify the action", Reconciled: true}
	if approval != nil {
		meta = orphanMeta{tool: approval.Tool, argsHash: approval.ArgsHash, risk: approval.Risk}
		if tool, ok := x.reg.Get(approval.Tool); ok {
			if rc, ok := tool.(tools.Reconciler); ok {
				env, err := envFor(ctx, rec.UserID)
				if err != nil {
					return false, fmt.Errorf("approvals: environment for user: %w", err)
				}
				meta.cred = env.Creds.Kind()
				r, err := rc.Reconcile(ctx, env, approval.Args, tools.ExecInfo{IdemKey: approval.IdemKey, TargetETag: approval.TargetETag})
				if err != nil && errors.Is(err, ocisclient.ErrUnauthorized) {
					return false, fmt.Errorf("approvals: reconcile: %w", err)
				}
				if err == nil {
					switch r.State {
					case tools.ReconDone:
						out = Outcome{State: OutcomeDone, Result: r.Result, Reconciled: true}
					case tools.ReconFailed:
						out = Outcome{State: OutcomeFailed, Reason: cutRationale(r.Reason, maxReason), Reconciled: true}
					case tools.ReconRetry:
						out = Outcome{State: OutcomeFailed, Reason: notRetried, Reconciled: true}
					}
				}
			}
		}
	}
	if err := x.finishOrphan(ctx, rec, approval, jobID, meta, out); err != nil {
		return false, err
	}
	return true, nil
}

// finishOrphan records the outcome without a lease and without a step: the
// job is over.
func (x *Executor) finishOrphan(ctx context.Context, rec store.IdemRecord, a *store.Approval, jobID string, meta orphanMeta, out Outcome) error {
	payload := encode(out)
	idemState, apState := statesOf(out)
	return x.st.InTx(ctx, func(tx store.Tx) error {
		if err := tx.FinishIdempotent(rec.UserID, rec.Key, idemState, payload); err != nil {
			return err
		}
		if a != nil {
			if _, err := tx.TransitionApproval(rec.UserID, a.ID, store.ApprovalExecuting, apState, store.ApprovalPatch{Result: payload}); err != nil {
				return err
			}
		}
		return x.event(tx, audit.Event{
			UserID: rec.UserID, JobID: jobID, Kind: "action_executed", Tool: meta.tool, Risk: meta.risk,
			ArgsHash: meta.argsHash, Cred: meta.cred, Outcome: out.State.String(), Detail: "reconciled: " + out.detail(),
		})
	})
}
