package approvals

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/owncloud/ocis-agent/backend/internal/audit"
	"github.com/owncloud/ocis-agent/backend/internal/store"
	"github.com/owncloud/ocis-agent/backend/internal/tools"
)

// ErrNotApplied means the job was no longer running when the approvals were
// requested (for example cancelled); nothing was recorded.
var ErrNotApplied = errors.New("approvals: the job is no longer running")

// ErrActionLimit means the job would exceed its limit of actions that need
// approval; the whole batch is refused and nothing is written.
var ErrActionLimit = errors.New("approvals: the job would exceed its limit of actions that need approval")

// Pending is a call that needs an approval together with its decision.
type Pending struct {
	Call     Call
	Decision Decision
}

// event appends an audit event in tx.
func (g *Gate) event(tx store.Tx, ev audit.Event) error {
	ev.TS = g.cfg.Now()
	_, err := tx.AppendAudit(ev)
	return err
}

// cutRationale shortens s to at most max bytes without splitting a rune.
func cutRationale(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// Request records the approvals for the calls and puts the job to sleep, in
// one transaction: it inserts one pending approval per entry (fenced), appends
// an approval_requested audit event for each, and releases the job to
// waiting_approval with checkpoint. A job therefore never waits without its
// approvals and never has approvals while it still runs.
func (g *Gate) Request(ctx context.Context, claim store.Claim, pending []Pending, checkpoint json.RawMessage) ([]store.Approval, error) {
	if len(pending) == 0 {
		return nil, fmt.Errorf("%w: nothing to request", ErrBadCall)
	}
	seen := make(map[string]bool, len(pending))
	for _, p := range pending {
		d := p.Decision
		if d.Kind != KindApprove || d.Tool == nil {
			return nil, fmt.Errorf("%w: only calls that need an approval can be requested", ErrBadCall)
		}
		if seen[p.Call.ID] {
			return nil, fmt.Errorf("%w: the call id %q appears twice in one request", ErrBadCall, p.Call.ID)
		}
		seen[p.Call.ID] = true
		if p.Call.Tool != d.Tool.Name() {
			return nil, fmt.Errorf("%w: the decision is for another tool", ErrBadCall)
		}
		if h, err := tools.ArgsHash(d.Args); err != nil || h != d.ArgsHash {
			return nil, fmt.Errorf("%w: the arguments no longer match their hash", ErrBadCall)
		}
		if d.IdemKey != tools.IdemKey(claim.Job.ID, p.Call.ID, d.ArgsHash) {
			return nil, fmt.Errorf("%w: the decision was not made for this job and call", ErrBadCall)
		}
	}
	userID, jobID := claim.Job.UserID, claim.Job.ID
	expires := g.cfg.Now().Add(g.cfg.ApprovalTTL)
	var out []store.Approval
	err := g.st.InTx(ctx, func(tx store.Tx) error {
		out = out[:0]
		if err := tx.AssertLease(userID, jobID, claim.Fence); err != nil {
			return err
		}
		live, err := liveActions(tx, userID, jobID)
		if err != nil {
			return err
		}
		if live+len(pending) > g.cfg.MaxActionsPerJob {
			return ErrActionLimit
		}
		for _, p := range pending {
			d := p.Decision
			a, err := tx.InsertApproval(store.Approval{
				UserID: userID, JobID: jobID, CallID: p.Call.ID, Tool: d.Tool.Name(), Risk: d.Risk.String(),
				Args: d.Args, ArgsHash: d.ArgsHash, TargetETag: d.Prepared.TargetETag,
				Rationale: cutRationale(p.Call.Rationale, g.cfg.RationaleMax), Preview: d.Prepared.Preview,
				IdemKey: d.IdemKey, ExpiresAt: expires,
			}, claim.Fence)
			if err != nil {
				return err
			}
			if err := g.event(tx, audit.Event{
				UserID: userID, JobID: jobID, Kind: "approval_requested", Tool: a.Tool, Risk: a.Risk,
				ArgsHash: a.ArgsHash, Outcome: "pending", Detail: "approval=" + a.ID,
			}); err != nil {
				return err
			}
			out = append(out, a)
		}
		applied, err := tx.ReleaseJob(claim, store.JobWaitingApproval, checkpoint, "")
		if err != nil {
			return err
		}
		if !applied {
			return ErrNotApplied
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
