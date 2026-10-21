package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/approvals"
	"github.com/owncloud/ocis-agent/backend/internal/audit"
	"github.com/owncloud/ocis-agent/backend/internal/llm"
	"github.com/owncloud/ocis-agent/backend/internal/memory"
	"github.com/owncloud/ocis-agent/backend/internal/ocisclient"
	"github.com/owncloud/ocis-agent/backend/internal/store"
	"github.com/owncloud/ocis-agent/backend/internal/textcap"
	"github.com/owncloud/ocis-agent/backend/internal/tools"
)

// Fixed texts the model reads.
const (
	toolFailedText  = "The tool could not be run."
	actionLimitText = "Too many actions are waiting for approval in this job."
)

const (
	maxDeniedDetail = 200
	maxDiffDetail   = 2000
	loopLimit       = 3
	memoryRead      = "memory.read"
)

// processCalls resolves the unresolved calls of the last assistant message,
// one at a time and in order. Calls that need an approval are collected and
// requested together at the end, which releases the job. stop means the job
// was released or must not continue.
func (r *Runner) processCalls(ctx context.Context, claim store.Claim, env tools.Env, cp *Checkpoint) (bool, error) {
	byCall, err := r.approvalsByCall(ctx, claim)
	if err != nil {
		return true, err
	}
	var pending []approvals.Pending
	for _, c := range cp.Unresolved() {
		var stop bool
		if a, ok := byCall[c.ID]; ok {
			stop, err = r.resolveApproval(ctx, claim, env, cp, c, a)
		} else {
			stop, err = r.resolveNew(ctx, claim, env, cp, c, &pending)
		}
		if stop || err != nil {
			return true, err
		}
	}
	if len(pending) == 0 {
		return false, nil
	}
	return r.request(ctx, claim, cp, pending)
}

func (r *Runner) approvalsByCall(ctx context.Context, claim store.Claim) (map[string]store.Approval, error) {
	var list []store.Approval
	if err := r.d.Store.View(ctx, func(tx store.Tx) error {
		var err error
		list, err = tx.ListApprovals(claim.Job.UserID, store.ApprovalFilter{JobID: claim.Job.ID, Limit: 500})
		return err
	}); err != nil {
		return nil, fmt.Errorf("agent: list approvals: %w", err)
	}
	out := make(map[string]store.Approval, len(list))
	for _, a := range list {
		out[a.CallID] = a
	}
	return out, nil
}

// resolveNew classifies a call that has no approval yet and runs, denies or
// collects it.
func (r *Runner) resolveNew(ctx context.Context, claim store.Claim, env tools.Env, cp *Checkpoint, c llm.ToolCall, pending *[]approvals.Pending) (bool, error) {
	// Prepare may already call oCIS
	if stop, err := r.toolCheck(ctx, claim, &env); stop || err != nil {
		return true, err
	}
	call := approvals.Call{ID: c.ID, Tool: c.Name, Args: json.RawMessage(c.Arguments)}
	d, err := r.d.Gate.Classify(ctx, claim, env, call)
	if err != nil {
		return r.runError(ctx, claim, c.Name, err)
	}
	if d.Kind == approvals.KindDeny {
		return r.deny(ctx, claim, cp, c, d.Reason)
	}
	if cp.NoteCall(c.ID, c.Name+"\x00"+d.ArgsHash) >= loopLimit {
		return true, r.fail(ctx, claim, ReasonLoop)
	}
	switch {
	case d.Kind == approvals.KindApprove:
		*pending = append(*pending, approvals.Pending{Call: call, Decision: d})
		return false, nil
	case d.Risk == tools.RiskRead:
		return r.runRead(ctx, claim, env, cp, c, d, call)
	}
	out, err := r.d.Executor.RunAuto(ctx, claim, env, d, call)
	return r.applyOutcome(ctx, claim, env, cp, c.Name, c.ID, out, err, true)
}

// runRead runs a read. A failure the executor did not record gives the model
// a constant text, never the error.
func (r *Runner) runRead(ctx context.Context, claim store.Claim, env tools.Env, cp *Checkpoint, c llm.ToolCall, d approvals.Decision, call approvals.Call) (bool, error) {
	out, err := r.d.Executor.RunRead(ctx, claim, env, d, call)
	if err == nil {
		return r.applyOutcome(ctx, claim, env, cp, c.Name, c.ID, out, nil, false)
	}
	// the model gets no content: drop what the read may have cached
	if ferr := r.forgetRead(ctx, env, c.Name, c.Arguments); ferr != nil {
		return true, errors.Join(err, ferr)
	}
	switch {
	case isCtxErr(ctx, err), lost(err),
		errors.Is(err, approvals.ErrBadCall), errors.Is(err, approvals.ErrTampered):
		return r.runError(ctx, claim, c.Name, err)
	}
	if errors.Is(err, ocisclient.ErrUnauthorized) {
		return true, r.unauthorized(ctx, claim)
	}
	text, _ := RenderResult(c.Name, "failed", tools.Result{}, toolFailedText)
	step := store.JobStep{JobID: claim.Job.ID, Kind: "tool", Tool: c.Name, CallID: c.ID, Status: "failed", Summary: toolFailedText}
	return r.record(ctx, claim, cp, c.ID, text, &step, nil)
}

// forgetRead drops the cached ETag of a memory.read whose content the model
// does not get.
func (r *Runner) forgetRead(ctx context.Context, env tools.Env, tool, arguments string) error {
	if tool != memoryRead {
		return nil
	}
	var a struct {
		Name string `json:"name"`
	}
	if json.Unmarshal([]byte(arguments), &a) != nil || a.Name == "" {
		return nil
	}
	return r.forget(ctx, env, a.Name)
}

// forget drops a cached memory ETag. It runs detached from ctx: it must also
// happen when the job's context has just ended.
func (r *Runner) forget(ctx context.Context, env tools.Env, name string) error {
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	err := env.Memory.Forget(fctx, session(env), name)
	if err != nil && !errors.Is(err, memory.ErrInvalidName) { // a bad name was never cached
		return fmt.Errorf("agent: forget memory read: %w", err)
	}
	return nil
}

// resolveApproval follows a call that already has an approval row.
func (r *Runner) resolveApproval(ctx context.Context, claim store.Claim, env tools.Env, cp *Checkpoint, c llm.ToolCall, a store.Approval) (bool, error) {
	for range 2 {
		switch a.State {
		case store.ApprovalRejected:
			text, _ := RenderResult(a.Tool, "rejected", tools.Result{}, ReasonRejectedByUser)
			step := store.JobStep{
				JobID: claim.Job.ID, Kind: "tool", Tool: a.Tool, CallID: c.ID, Status: "rejected",
				Summary: ReasonRejectedByUser, ApprovalID: a.ID,
			}
			return r.record(ctx, claim, cp, c.ID, text, &step, nil)
		case store.ApprovalPending:
			// a job that still runs must have no pending approval: never
			// execute, try again later
			return true, r.backoff(ctx, claim)
		}
		var afterWrite func(context.Context)
		if a.State == store.ApprovalApproved || a.State == store.ApprovalExecuting {
			// about to execute or reconcile
			if stop, err := r.toolCheck(ctx, claim, &env); stop || err != nil {
				return true, err
			}
			if a.State == store.ApprovalApproved && r.d.OwnWrites != nil {
				// never executed before: a Done outcome is this run's write
				afterWrite = r.d.OwnWrites.BeforeWrite(ctx, env, tools.TargetPaths(a.Args))
			}
		}
		out, err := r.d.Executor.Run(ctx, claim, env, a.ID)
		if afterWrite != nil && err == nil && out.State == approvals.OutcomeDone {
			afterWrite(ctx)
		}
		if err == nil && a.State == store.ApprovalExpired && a.DecidedBy == "" {
			// expired while pending (ExpireAndResume writes no step); an
			// approval that expired after the decision got its step from
			// the executor
			text, _ := RenderResult(a.Tool, out.State.String(), tools.Result{}, out.Reason)
			step := store.JobStep{
				JobID: claim.Job.ID, Kind: "tool", Tool: a.Tool, CallID: c.ID, Status: out.State.String(),
				Summary: out.Reason, ApprovalID: a.ID,
			}
			return r.record(ctx, claim, cp, c.ID, text, &step, nil)
		}
		if !errors.Is(err, approvals.ErrNotApproved) {
			return r.applyOutcome(ctx, claim, env, cp, a.Tool, c.ID, out, err, false)
		}
		if err := r.d.Store.View(ctx, func(tx store.Tx) error {
			var err error
			a, err = tx.GetApproval(claim.Job.UserID, a.ID)
			return err
		}); err != nil {
			return true, fmt.Errorf("agent: reload approval: %w", err)
		}
	}
	return true, errors.New("agent: the approval did not settle")
}

// applyOutcome renders an executor outcome as the call's tool message.
func (r *Runner) applyOutcome(ctx context.Context, claim store.Claim, env tools.Env, cp *Checkpoint, tool, callID string, out approvals.Outcome, err error, auditDiff bool) (bool, error) {
	if err != nil {
		if errors.Is(out.Err, ocisclient.ErrUnauthorized) {
			// the outcome was not recorded, but the 401 is still news
			r.reportUnauthorized(ctx, claim)
		}
		return r.runError(ctx, claim, tool, err)
	}
	text, truncated := RenderResult(tool, out.State.String(), out.Result, out.Reason)
	if tool == memoryRead {
		for _, key := range truncated {
			if name, ok := strings.CutPrefix(key, "memory:"); ok {
				if err := r.forget(ctx, env, name); err != nil {
					return true, err
				}
			}
		}
	}
	var events []audit.Event
	if auditDiff && out.Result.Diff != "" {
		events = append(events, audit.Event{
			Kind: audit.KindMemoryChanged, Tool: tool, Outcome: out.State.String(), Detail: textcap.Cut(out.Result.Diff, maxDiffDetail),
		})
	}
	if stop, err := r.record(ctx, claim, cp, callID, text, nil, events); stop || err != nil {
		if tool == memoryRead && out.State == approvals.OutcomeDone {
			// the result was not saved, so the model never sees it
			for _, b := range out.Result.Blocks {
				if ferr := r.forget(ctx, env, b.Path); ferr != nil {
					err = errors.Join(err, ferr)
				}
			}
		}
		return true, err
	}
	if errors.Is(out.Err, ocisclient.ErrUnauthorized) {
		return true, r.unauthorized(ctx, claim)
	}
	return false, nil
}

// runError maps an error of the gate or the executor. Every path stops.
func (r *Runner) runError(ctx context.Context, claim store.Claim, tool string, err error) (bool, error) {
	switch {
	case isCtxErr(ctx, err):
		return true, err
	case errors.Is(err, approvals.ErrTampered):
		return true, r.end(ctx, claim, ending{
			state: store.JobFailed, reason: ReasonInternal,
			events: []audit.Event{{Kind: audit.KindTampered, Tool: tool, Outcome: "refused"}},
		})
	case errors.Is(err, approvals.ErrWrongJob), errors.Is(err, approvals.ErrBadCall), errors.Is(err, store.ErrNotFound):
		return true, r.fail(ctx, claim, ReasonInternal)
	case errors.Is(err, ocisclient.ErrUnauthorized):
		return true, r.unauthorized(ctx, claim)
	}
	return true, fmt.Errorf("agent: run tool: %w", err)
}

// deny answers a refused call and audits the refusal.
func (r *Runner) deny(ctx context.Context, claim store.Claim, cp *Checkpoint, c llm.ToolCall, reason string) (bool, error) {
	text, _ := RenderResult(c.Name, "denied", tools.Result{}, reason)
	detail := textcap.Cut(reason, maxDeniedDetail)
	step := store.JobStep{JobID: claim.Job.ID, Kind: "tool", Tool: c.Name, CallID: c.ID, Status: "denied", Summary: detail}
	ev := audit.Event{Kind: audit.KindPolicyDenied, Tool: c.Name, Outcome: "denied", Detail: detail}
	return r.record(ctx, claim, cp, c.ID, text, &step, []audit.Event{ev})
}

// record appends the tool message for callID and saves the checkpoint, with
// an optional step and audit events, in one transaction that starts with
// AssertLease.
func (r *Runner) record(ctx context.Context, claim store.Claim, cp *Checkpoint, callID, text string, step *store.JobStep, events []audit.Event) (bool, error) {
	cp.Messages = append(cp.Messages, llm.Message{Role: llm.RoleTool, ToolCallID: callID, Content: text})
	raw, err := cp.Marshal()
	if err != nil {
		return true, r.fail(ctx, claim, ReasonTooLarge)
	}
	j := claim.Job
	if err := r.d.Store.InTx(ctx, func(tx store.Tx) error {
		if err := tx.AssertLease(j.UserID, j.ID, claim.Fence); err != nil {
			return err
		}
		for _, ev := range events {
			ev.UserID, ev.JobID, ev.TS = j.UserID, j.ID, r.cfg.Now()
			if _, err := tx.AppendAudit(ev); err != nil {
				return err
			}
		}
		if step != nil {
			if _, err := tx.AppendStep(*step, claim.Fence); err != nil {
				return err
			}
		}
		return tx.SaveCheckpoint(j.UserID, j.ID, raw, claim.Fence)
	}); err != nil {
		return true, fmt.Errorf("agent: record tool result: %w", err)
	}
	return false, nil
}

// request asks for the collected approvals, which releases the job. Over the
// job's action limit, every collected call is denied instead.
func (r *Runner) request(ctx context.Context, claim store.Claim, cp *Checkpoint, pending []approvals.Pending) (bool, error) {
	raw, err := cp.Marshal()
	if err != nil {
		return true, r.fail(ctx, claim, ReasonTooLarge)
	}
	_, err = r.d.Gate.Request(ctx, claim, pending, raw)
	switch {
	case err == nil:
		r.d.Queue.Wake()
		return true, nil
	case errors.Is(err, approvals.ErrActionLimit):
		for _, p := range pending {
			c := llm.ToolCall{ID: p.Call.ID, Name: p.Call.Tool}
			if stop, err := r.deny(ctx, claim, cp, c, actionLimitText); stop || err != nil {
				return true, err
			}
		}
		return false, nil
	case errors.Is(err, approvals.ErrBadCall):
		return true, r.fail(ctx, claim, ReasonInternal)
	case isCtxErr(ctx, err):
		return true, err
	}
	return true, fmt.Errorf("agent: request approvals: %w", err)
}
