package approvals

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/audit"
	"github.com/owncloud/ocis-agent/backend/internal/auth"
	"github.com/owncloud/ocis-agent/backend/internal/ocisclient"
	"github.com/owncloud/ocis-agent/backend/internal/store"
	"github.com/owncloud/ocis-agent/backend/internal/tools"
)

// EnvFunc builds the tool environment for a user (credentials, clients). The
// scheduler supplies it. An error matching auth.ErrCredentialInvalid or
// store.ErrNotFound settles the user's records as unknown; any other error
// leaves them for the next pass (until Config.OrphanGiveUp).
type EnvFunc func(ctx context.Context, userID string) (tools.Env, error)

// ReconcileSummary counts what ReconcileOrphans did.
type ReconcileSummary struct {
	Resolved int
	// Skipped counts listed records whose job was still active. The listing
	// leaves those out, so it is 0 in practice (a defence only).
	Skipped int
	Errors  []error
}

const (
	notRetried          = "the job ended before the action could be retried; it was not run"
	reasonNoCredentials = "the job ended before the outcome was recorded, and it could not be verified because the user's credentials were unavailable; please verify the action"
	reasonGaveUp        = "the job ended before the outcome was recorded, and it could not be verified in time; please verify the action"
)

// noCredentials reports an environment error that will not heal by
// retrying: the user has no usable app credential or no longer exists.
func noCredentials(err error) bool {
	return errors.Is(err, auth.ErrCredentialInvalid) || errors.Is(err, store.ErrNotFound)
}

// givenUp reports whether a record is older than Config.OrphanGiveUp: then
// a check that keeps failing settles it as unknown instead of leaving it open.
func (x *Executor) givenUp(rec store.IdemRecord) bool {
	return x.cfg.Now().Sub(rec.StartedAt) >= x.cfg.OrphanGiveUp
}

// ReconcileOrphans resolves calls whose outcome was never recorded (an
// idempotency record still started) and whose job has ENDED (or no longer
// exists). While a job is still active it reconciles its own records when it
// runs again, so those are not listed at all (they cannot fill limit and
// starve the records of ended jobs behind them). An ended job is never
// resumed: a "retry" verdict is recorded as a failure, not executed. A user
// without usable credentials, or a check still failing after OrphanGiveUp,
// settles the record as unknown, so no record stays open forever.
//
// olderThan is clamped to Now()-RetrySettle: a call that started within the
// settle window may still be in flight (cancelling a job does not stop its
// worker), so a sweep never touches it. Pass a cut-off of at least the longest
// call duration plus RetrySettle. Once ctx ends it stops (returning the
// context error with what it did so far) and a check the context cut short
// leaves its record open.
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
		if err := ctx.Err(); err != nil {
			// a deadline of the caller: the rest waits for the next pass
			return sum, fmt.Errorf("approvals: reconcile orphans: %w", err)
		}
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
	if err := ctx.Err(); err != nil {
		return sum, fmt.Errorf("approvals: reconcile orphans: %w", err)
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
				checked, cred, err := x.checkOrphan(ctx, envFor, rc, rec, approval)
				if err != nil {
					return false, err
				}
				meta.cred = cred
				if checked != nil {
					out = *checked
				}
			}
		}
	}
	if err := x.finishOrphan(ctx, rec, approval, jobID, meta, out); err != nil {
		return false, err
	}
	return true, nil
}

// checkOrphan asks the tool what became of the call. It returns the outcome
// to record (nil: the default unknown) and the credential kind used, or an
// error to leave the record open for the next pass. A user without usable
// credentials, or a check that still fails once the record is older than
// OrphanGiveUp, yields unknown with a fixed reason; nothing is executed.
func (x *Executor) checkOrphan(ctx context.Context, envFor EnvFunc, rc tools.Reconciler, rec store.IdemRecord, a *store.Approval) (*Outcome, string, error) {
	unknown := func(reason string) *Outcome {
		return &Outcome{State: OutcomeUnknown, Reason: reason, Reconciled: true}
	}
	env, err := envFor(ctx, rec.UserID)
	switch {
	case err != nil && ctx.Err() != nil:
		return nil, "", fmt.Errorf("approvals: environment for user: %w", err)
	case err != nil && noCredentials(err):
		return unknown(reasonNoCredentials), "", nil
	case err != nil && x.givenUp(rec):
		return unknown(reasonGaveUp), "", nil
	case err != nil:
		return nil, "", fmt.Errorf("approvals: environment for user: %w", err)
	}
	cred := env.Creds.Kind()
	r, err := rc.Reconcile(ctx, env, a.Args, tools.ExecInfo{IdemKey: a.IdemKey, TargetETag: a.TargetETag})
	switch {
	case err != nil && ctx.Err() != nil:
		// cut short by the caller's deadline: the tool could not tell
		return nil, cred, fmt.Errorf("approvals: reconcile: %w", err)
	case err != nil && errors.Is(err, ocisclient.ErrUnauthorized) && x.givenUp(rec):
		return unknown(reasonGaveUp), cred, nil
	case err != nil && errors.Is(err, ocisclient.ErrUnauthorized):
		return nil, cred, fmt.Errorf("approvals: reconcile: %w", err)
	case err != nil:
		return nil, cred, nil
	}
	switch r.State {
	case tools.ReconDone:
		return &Outcome{State: OutcomeDone, Result: r.Result, Reconciled: true}, cred, nil
	case tools.ReconFailed:
		return &Outcome{State: OutcomeFailed, Reason: cutRationale(r.Reason, maxReason), Reconciled: true}, cred, nil
	case tools.ReconRetry:
		return &Outcome{State: OutcomeFailed, Reason: notRetried, Reconciled: true}, cred, nil
	}
	return nil, cred, nil
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
			UserID: rec.UserID, JobID: jobID, Kind: audit.KindActionExecuted, Tool: meta.tool, Risk: meta.risk,
			ArgsHash: meta.argsHash, Cred: meta.cred, Outcome: out.State.String(), Detail: "reconciled: " + out.detail(),
		})
	})
}
