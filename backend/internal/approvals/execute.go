package approvals

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/audit"
	"github.com/owncloud/ocis-agent/backend/internal/store"
	"github.com/owncloud/ocis-agent/backend/internal/tools"
)

// Errors for approvals that must not be executed.
var (
	// ErrWrongJob: the approval belongs to another job than the claim's.
	ErrWrongJob = errors.New("approvals: the approval belongs to another job")
	// ErrNotApproved: the approval is pending or rejected.
	ErrNotApproved = errors.New("approvals: the approval is not approved")
	// ErrTampered: the stored approval does not match its own hash,
	// idempotency key or risk, or names an unknown tool.
	ErrTampered = errors.New("approvals: the stored approval does not match its hash, key or risk")
)

// maxReason caps every model-visible reason, in bytes.
const maxReason = 500

const expiredReason = "the approval expired before the action could be executed"

// OutcomeState is how a call ended.
type OutcomeState int

// Outcome states.
const (
	OutcomeDone OutcomeState = iota
	OutcomeFailed
	OutcomeUnknown
	OutcomeExpired
)

func (s OutcomeState) String() string {
	switch s {
	case OutcomeDone:
		return "done"
	case OutcomeFailed:
		return "failed"
	case OutcomeUnknown:
		return "unknown"
	case OutcomeExpired:
		return "expired"
	}
	return "invalid"
}

// Outcome is what the runner gets back from the executor. Reason is safe to
// show to the model. Err is the underlying error for the runner only (it may
// match ocisclient.ErrUnauthorized); it is never shown to the model.
type Outcome struct {
	State      OutcomeState
	Result     tools.Result
	Reason     string
	Reconciled bool // from a stored record or a reconcile, not from a fresh call
	Err        error
}

// stored is the JSON kept in the idempotency record and the approval result
// (written here, read with store.DecodeOutcome).
type stored struct {
	Outcome string        `json:"outcome"`
	Result  *tools.Result `json:"result,omitempty"`
	Reason  string        `json:"reason,omitempty"`
}

func encode(o Outcome) json.RawMessage {
	s := stored{Outcome: o.State.String(), Reason: o.Reason}
	if o.State == OutcomeDone {
		r := o.Result
		s.Result = &r
	}
	b, _ := json.Marshal(s)
	return b
}

func decodeOutcome(raw json.RawMessage) Outcome {
	s, err := store.DecodeOutcome(raw) // the one decoder every reader of a stored outcome uses
	if err != nil {
		return Outcome{State: OutcomeUnknown, Reason: "the stored outcome is unreadable; please verify the action", Reconciled: true}
	}
	o := Outcome{Reason: s.Reason, Reconciled: true}
	if s.Result != nil {
		o.Result = tools.Result{Summary: s.Result.Summary, Diff: s.Result.Diff}
		if s.Result.Blocks != nil {
			o.Result.Blocks = make([]tools.Block, len(s.Result.Blocks))
			for i, b := range s.Result.Blocks {
				o.Result.Blocks[i] = tools.Block(b) // fails to compile if the shapes drift apart
			}
		}
	}
	switch s.Outcome {
	case "done":
		o.State = OutcomeDone
	case "failed":
		o.State = OutcomeFailed
	case "expired":
		o.State = OutcomeExpired
	default:
		o.State = OutcomeUnknown
	}
	return o
}

// detail is the trusted one-line text of an outcome for audit and steps.
func (o Outcome) detail() string {
	if o.State == OutcomeDone {
		return o.Result.Summary
	}
	return o.Reason
}

// Executor runs approved and auto-allowed calls under the two-transaction
// protocol.
type Executor struct {
	st   store.Store
	reg  *tools.Registry
	cfg  Config
	hook func(stage string) error // tests: an error simulates a crash at a boundary
}

// NewExecutor returns an executor over the store and the tool registry.
func NewExecutor(st store.Store, reg *tools.Registry, cfg Config) (*Executor, error) {
	if st == nil || reg == nil {
		return nil, errors.New("approvals: a store and a tool registry are required")
	}
	return &Executor{st: st, reg: reg, cfg: cfg.withDefaults()}, nil
}

func (x *Executor) at(stage string) error {
	if x.hook == nil {
		return nil
	}
	return x.hook(stage)
}

func (x *Executor) event(tx store.Tx, ev audit.Event) error {
	ev.TS = x.cfg.Now()
	_, err := tx.AppendAudit(ev)
	return err
}

// job is one execution: everything the protocol needs, from the stored
// approval or from an auto-allowed decision.
type job struct {
	tool       tools.Tool
	callID     string
	args       json.RawMessage
	argsHash   string
	idemKey    string
	targetETag string
	risk       tools.Risk
	approval   *store.Approval
}

func (j job) info() tools.ExecInfo {
	return tools.ExecInfo{IdemKey: j.idemKey, TargetETag: j.targetETag}
}

// Run executes the approval with this id for the claimed job.
func (x *Executor) Run(ctx context.Context, claim store.Claim, env tools.Env, approvalID string) (Outcome, error) {
	var a store.Approval
	if err := x.st.View(ctx, func(tx store.Tx) error {
		var err error
		a, err = tx.GetApproval(claim.Job.UserID, approvalID)
		return err
	}); err != nil {
		return Outcome{}, err
	}
	if a.JobID != claim.Job.ID {
		return Outcome{}, ErrWrongJob
	}
	switch a.State {
	case store.ApprovalApproved, store.ApprovalExecuting:
	case store.ApprovalExecuted, store.ApprovalFailed, store.ApprovalUnknown:
		return decodeOutcome(a.Result), nil
	case store.ApprovalExpired:
		return Outcome{State: OutcomeExpired, Reason: expiredReason, Reconciled: true}, nil
	default:
		return Outcome{}, fmt.Errorf("%w: it is %s", ErrNotApproved, a.State)
	}
	tool, ok := x.reg.Get(a.Tool)
	if !ok {
		return Outcome{}, fmt.Errorf("%w: unknown tool", ErrTampered)
	}
	if h, err := tools.ArgsHash(a.Args); err != nil || h != a.ArgsHash {
		return Outcome{}, fmt.Errorf("%w: arguments hash", ErrTampered)
	}
	if tools.IdemKey(a.JobID, a.CallID, a.ArgsHash) != a.IdemKey {
		return Outcome{}, fmt.Errorf("%w: idempotency key", ErrTampered)
	}
	if tool.Risk(a.Args).String() != a.Risk {
		return Outcome{}, fmt.Errorf("%w: risk", ErrTampered)
	}
	if a.State == store.ApprovalApproved && !x.cfg.Now().Before(a.ExpiresAt) {
		return x.expire(ctx, claim, a)
	}
	return x.exec(ctx, claim, env, job{
		tool: tool, callID: a.CallID, args: a.Args, argsHash: a.ArgsHash, idemKey: a.IdemKey,
		targetETag: a.TargetETag, risk: tool.Risk(a.Args), approval: &a,
	})
}

// expire moves an approved but overdue approval to expired in one fenced
// transaction.
func (x *Executor) expire(ctx context.Context, claim store.Claim, a store.Approval) (Outcome, error) {
	out := Outcome{State: OutcomeExpired, Reason: expiredReason}
	err := x.st.InTx(ctx, func(tx store.Tx) error {
		if err := tx.AssertLease(a.UserID, a.JobID, claim.Fence); err != nil {
			return err
		}
		if _, err := tx.TransitionApproval(a.UserID, a.ID, store.ApprovalApproved, store.ApprovalExpired, store.ApprovalPatch{Result: encode(out)}); err != nil {
			return err
		}
		if err := x.event(tx, audit.Event{
			UserID: a.UserID, JobID: a.JobID, Kind: audit.KindApprovalExpired, Tool: a.Tool, Risk: a.Risk,
			ArgsHash: a.ArgsHash, Outcome: "expired", Detail: "approval=" + a.ID + " was not executed",
		}); err != nil {
			return err
		}
		_, err := tx.AppendStep(store.JobStep{
			JobID: a.JobID, Kind: "tool", Tool: a.Tool, CallID: a.CallID, Status: out.State.String(),
			Summary: out.Reason, ApprovalID: a.ID,
		}, claim.Fence)
		return err
	})
	if err != nil {
		return Outcome{}, err
	}
	return out, nil
}

// RunAuto executes an auto-allowed write (a write-risk tool that reports
// AutoAllowed, that is a memory write) under the same protocol, without an
// approval. The decision must come from Classify for this claim and call.
func (x *Executor) RunAuto(ctx context.Context, claim store.Claim, env tools.Env, d Decision, call Call) (Outcome, error) {
	if d.Kind != KindAllow || d.Tool == nil || d.Risk != tools.RiskWrite || !d.Tool.AutoAllowed() {
		return Outcome{}, fmt.Errorf("%w: only auto-allowed writes run without an approval", ErrBadCall)
	}
	if d.IdemKey != tools.IdemKey(claim.Job.ID, call.ID, d.ArgsHash) {
		return Outcome{}, fmt.Errorf("%w: the decision was not made for this job and call", ErrBadCall)
	}
	if t, ok := x.reg.Get(call.Tool); !ok || t.Name() != d.Tool.Name() {
		return Outcome{}, fmt.Errorf("%w: the decision is for another tool than the call", ErrBadCall)
	}
	if h, err := tools.ArgsHash(d.Args); err != nil || h != d.ArgsHash {
		return Outcome{}, fmt.Errorf("%w: arguments hash", ErrTampered)
	}
	if d.Tool.Risk(d.Args) != tools.RiskWrite {
		return Outcome{}, fmt.Errorf("%w: the risk is not write", ErrBadCall)
	}
	return x.exec(ctx, claim, env, job{
		tool: d.Tool, callID: call.ID, args: d.Args, argsHash: d.ArgsHash, idemKey: d.IdemKey, risk: d.Risk,
	})
}

// RunRead executes a read: no protocol is needed, but the call is audited and
// recorded as a step. A refusal is a failed outcome the model reads; any
// other error is returned to the runner and nothing is recorded.
func (x *Executor) RunRead(ctx context.Context, claim store.Claim, env tools.Env, d Decision, call Call) (Outcome, error) {
	if d.Kind != KindAllow || d.Tool == nil || d.Risk != tools.RiskRead {
		return Outcome{}, fmt.Errorf("%w: RunRead only runs reads", ErrBadCall)
	}
	if d.IdemKey != tools.IdemKey(claim.Job.ID, call.ID, d.ArgsHash) {
		return Outcome{}, fmt.Errorf("%w: the decision was not made for this job and call", ErrBadCall)
	}
	if t, ok := x.reg.Get(call.Tool); !ok || t != d.Tool {
		return Outcome{}, fmt.Errorf("%w: the decision is for another tool than the call", ErrBadCall)
	}
	if h, err := tools.ArgsHash(d.Args); err != nil || h != d.ArgsHash {
		return Outcome{}, fmt.Errorf("%w: arguments hash", ErrTampered)
	}
	if d.Tool.Risk(d.Args) != tools.RiskRead {
		return Outcome{}, fmt.Errorf("%w: the risk is not read", ErrBadCall)
	}
	res, err := d.Tool.Execute(ctx, env, d.Args, tools.ExecInfo{IdemKey: d.IdemKey})
	var out Outcome
	switch {
	case err == nil:
		out = Outcome{State: OutcomeDone, Result: res}
	case isDefinitive(err):
		out = Outcome{State: OutcomeFailed, Reason: cutRationale(err.Error(), maxReason)}
	default:
		return Outcome{}, err
	}
	err = x.st.InTx(ctx, func(tx store.Tx) error {
		if _, err := tx.AppendStep(store.JobStep{
			JobID: claim.Job.ID, Kind: "tool", Tool: d.Tool.Name(), CallID: call.ID,
			Status: out.State.String(), Summary: out.detail(),
		}, claim.Fence); err != nil {
			return err
		}
		return x.event(tx, audit.Event{
			UserID: claim.Job.UserID, JobID: claim.Job.ID, Kind: audit.KindToolCall, Tool: d.Tool.Name(), Risk: d.Risk.String(),
			ArgsHash: d.ArgsHash, Cred: env.Creds.Kind(), Outcome: out.State.String(), Detail: out.detail(),
		})
	})
	if err != nil {
		return Outcome{}, err
	}
	return out, nil
}

// statesOf maps an outcome to the idempotency state and the approval state it
// is recorded as. The idempotency store has no unknown state: an unknown
// outcome is finished as failed with {"outcome":"unknown"} in its result.
func statesOf(o Outcome) (store.IdemState, store.ApprovalState) {
	switch o.State {
	case OutcomeDone:
		return store.IdemDone, store.ApprovalExecuted
	case OutcomeUnknown:
		return store.IdemFailed, store.ApprovalUnknown
	}
	return store.IdemFailed, store.ApprovalFailed
}

// isDefinitive reports an error the tool raised on purpose (a refusal, bad
// arguments, a changed target): the call certainly did not do its work.
func isDefinitive(err error) bool {
	return errors.Is(err, tools.ErrRefused) || errors.Is(err, tools.ErrInvalidArgs) || errors.Is(err, tools.ErrTargetChanged)
}

var errUnfinished = errors.New("approvals: a previous attempt did not finish")

// exec runs transaction A, the external call and transaction B. If B fails
// for a reason other than a lost lease, the computed Outcome is returned
// together with the error so that the runner still sees Err (for example a
// 401). An interrupted call (the context ended) returns an error only and
// records nothing: the record stays started for the next Run to reconcile.
func (x *Executor) exec(ctx context.Context, claim store.Claim, env tools.Env, j job) (Outcome, error) {
	existing, err := x.begin(ctx, claim, j)
	if err != nil {
		return Outcome{}, err
	}
	if err := x.at("afterA"); err != nil {
		return Outcome{}, err
	}
	var out Outcome
	switch {
	case existing != nil && existing.State != store.IdemStarted:
		// a finished record: B already committed (the approval is final too)
		return decodeOutcome(existing.Result), nil
	case existing != nil:
		retried := x.cfg.Now().Sub(existing.StartedAt) < x.cfg.RetrySettle
		out, err = x.resolve(ctx, env, j, errUnfinished, retried)
	default:
		out, err = x.attempt(ctx, env, j, false)
	}
	if err != nil {
		return Outcome{}, err
	}
	if err := x.finish(ctx, claim, env, j, out); err != nil {
		return out, err
	}
	return out, nil
}

// begin is transaction A.
func (x *Executor) begin(ctx context.Context, claim store.Claim, j job) (*store.IdemRecord, error) {
	user, jobID := claim.Job.UserID, claim.Job.ID
	var existing *store.IdemRecord
	err := x.st.InTx(ctx, func(tx store.Tx) error {
		existing = nil
		if err := tx.AssertLease(user, jobID, claim.Fence); err != nil {
			return err
		}
		// mustExist: the approval was already executing, so a previous attempt
		// wrote the idempotency record in its transaction A.
		mustExist := false
		if j.approval != nil {
			if j.approval.State == store.ApprovalApproved {
				_, err := tx.TransitionApproval(user, j.approval.ID, store.ApprovalApproved, store.ApprovalExecuting, store.ApprovalPatch{})
				// ErrConflict: a previous attempt already moved it; recover below
				if err != nil {
					if !errors.Is(err, store.ErrConflict) {
						return err
					}
					mustExist = true
				}
			} else {
				mustExist = true
			}
			cur, err := tx.GetApproval(user, j.approval.ID)
			if err != nil {
				return err
			}
			if cur.State != store.ApprovalExecuting {
				return fmt.Errorf("%w: it is %s", ErrNotApproved, cur.State)
			}
		}
		rec, err := tx.BeginIdempotent(store.IdemRecord{Key: j.idemKey, UserID: user, Tool: j.tool.Name()})
		if err != nil {
			return err
		}
		if mustExist && rec == nil {
			return fmt.Errorf("%w: an executing approval has no idempotency record", ErrTampered)
		}
		if rec != nil && (rec.Tool != j.tool.Name() || (j.approval != nil && !mustExist)) {
			return fmt.Errorf("%w: the idempotency record does not belong to this call", ErrTampered)
		}
		existing = rec
		return nil
	})
	if err != nil {
		return nil, err
	}
	return existing, nil
}

// attempt performs the external call and maps its error.
func (x *Executor) attempt(ctx context.Context, env tools.Env, j job, retried bool) (Outcome, error) {
	res, err := j.tool.Execute(ctx, env, j.args, j.info())
	if herr := x.at("afterExecute"); herr != nil {
		return Outcome{}, herr
	}
	switch {
	case err == nil:
		return Outcome{State: OutcomeDone, Result: res}, nil
	case isDefinitive(err):
		return Outcome{State: OutcomeFailed, Reason: cutRationale(err.Error(), maxReason)}, nil
	}
	if ctx.Err() != nil {
		return Outcome{}, fmt.Errorf("approvals: interrupted: %w", ctx.Err())
	}
	return x.resolve(ctx, env, j, err, retried)
}

// resolve settles an outcome nobody can be sure about by asking the tool
// what happened. At most one retry is made, and only when the tool says the
// call certainly did not happen.
func (x *Executor) resolve(ctx context.Context, env tools.Env, j job, cause error, retried bool) (Outcome, error) {
	if ctx.Err() != nil {
		return Outcome{}, fmt.Errorf("approvals: interrupted: %w", ctx.Err())
	}
	unknown := Outcome{State: OutcomeUnknown, Reason: "the outcome of the action is unknown; please verify it", Err: cause}
	rc, ok := j.tool.(tools.Reconciler)
	if !ok {
		return unknown, nil
	}
	r, err := rc.Reconcile(ctx, env, j.args, j.info())
	if err != nil {
		if ctx.Err() != nil {
			return Outcome{}, fmt.Errorf("approvals: interrupted: %w", ctx.Err())
		}
		unknown.Err = errors.Join(cause, err)
		return unknown, nil
	}
	switch r.State {
	case tools.ReconDone:
		return Outcome{State: OutcomeDone, Result: r.Result, Reconciled: true}, nil
	case tools.ReconFailed:
		return Outcome{State: OutcomeFailed, Reason: cutRationale(r.Reason, maxReason), Reconciled: true}, nil
	case tools.ReconRetry:
		if retried {
			return unknown, nil
		}
		return x.attempt(ctx, env, j, true)
	}
	return unknown, nil
}

// finish is transaction B, with an unfenced fallback when the lease is gone.
func (x *Executor) finish(ctx context.Context, claim store.Claim, env tools.Env, j job, out Outcome) error {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	payload := encode(out)
	idemState, apState := statesOf(out)
	user, jobID := claim.Job.UserID, claim.Job.ID
	record := func(tx store.Tx, fenced bool) error {
		if fenced {
			if err := tx.AssertLease(user, jobID, claim.Fence); err != nil {
				return err
			}
		}
		if err := tx.FinishIdempotent(user, j.idemKey, idemState, payload); err != nil {
			return err
		}
		approvalID := ""
		if j.approval != nil {
			approvalID = j.approval.ID
			if _, err := tx.TransitionApproval(user, approvalID, store.ApprovalExecuting, apState, store.ApprovalPatch{Result: payload}); err != nil {
				return err
			}
		}
		if err := x.event(tx, audit.Event{
			UserID: user, JobID: jobID, Kind: audit.KindActionExecuted, Tool: j.tool.Name(), Risk: j.risk.String(),
			ArgsHash: j.argsHash, Cred: env.Creds.Kind(), Outcome: out.State.String(), Detail: out.detail(),
		}); err != nil {
			return err
		}
		if !fenced {
			return nil
		}
		_, err := tx.AppendStep(store.JobStep{
			JobID: jobID, Kind: "tool", Tool: j.tool.Name(), CallID: j.callID, Status: out.State.String(),
			Summary: out.detail(), ApprovalID: approvalID,
		}, claim.Fence)
		return err
	}
	err := x.st.InTx(cctx, func(tx store.Tx) error { return record(tx, true) })
	if errors.Is(err, store.ErrFenceLost) || errors.Is(err, store.ErrConflict) {
		// The job was cancelled or another worker took over while the call
		// ran. The outcome is known, so record it, but leave the job's
		// timeline to its owner.
		return x.st.InTx(cctx, func(tx store.Tx) error { return record(tx, false) })
	}
	return err
}
