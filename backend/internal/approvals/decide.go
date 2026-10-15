package approvals

import (
	"context"
	"errors"

	"github.com/owncloud/ocis-agent/backend/internal/audit"
	"github.com/owncloud/ocis-agent/backend/internal/store"
)

// ErrJobEnded means the approval's job is already finished (cancelled, failed
// or succeeded), so a decision would be meaningless.
var ErrJobEnded = errors.New("approvals: the job has ended; the approval can no longer be decided")

// DecisionItem is the user's decision on one approval. ArgsHash must be the
// hash the user was shown; a mismatch means the approval changed and the
// client must reload.
type DecisionItem struct {
	ApprovalID string
	Decision   store.ApprovalState // approved or rejected
	ArgsHash   string
}

// Decide records the user's decisions in one transaction: for every item it
// loads the approval, refuses if its job has ended, lets the store check
// ownership, state, expiry and the arguments hash, and audits the decision;
// afterwards each affected job is resumed if nothing is pending any more. If
// any item fails, none is applied. Only a request authenticated with the
// user's OIDC token may call this.
func (g *Gate) Decide(ctx context.Context, userID, decidedBy string, items []DecisionItem) ([]store.Approval, error) {
	if len(items) == 0 {
		return nil, store.Invalid("no decisions given")
	}
	if decidedBy == "" {
		return nil, store.Invalid("decidedBy is required")
	}
	for _, it := range items {
		if it.Decision != store.ApprovalApproved && it.Decision != store.ApprovalRejected {
			return nil, store.Invalid("a decision must be approved or rejected")
		}
	}
	var out []store.Approval
	err := g.st.InTx(ctx, func(tx store.Tx) error {
		out = out[:0]
		seen := map[string]bool{}
		var jobs []string
		for _, it := range items {
			cur, err := tx.GetApproval(userID, it.ApprovalID)
			if err != nil {
				return err
			}
			job, err := tx.GetJob(userID, cur.JobID)
			if err != nil {
				return err
			}
			if job.State.Terminal() {
				return ErrJobEnded
			}
			a, err := tx.DecideApproval(userID, it.ApprovalID, it.Decision, decidedBy, it.ArgsHash)
			if err != nil {
				return err
			}
			if err := g.event(tx, audit.Event{
				UserID: userID, JobID: a.JobID, Kind: "approval_decided", Tool: a.Tool, Risk: a.Risk,
				ArgsHash: a.ArgsHash, Cred: "oidc", Outcome: string(a.State), Detail: "approval=" + a.ID,
			}); err != nil {
				return err
			}
			out = append(out, a)
			if !seen[a.JobID] {
				seen[a.JobID] = true
				jobs = append(jobs, a.JobID)
			}
		}
		for _, j := range jobs {
			if _, err := tx.ResumeJobIfSettled(userID, j); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ExpireAndResume expires every pending approval past its deadline, audits
// each, and wakes the jobs that were waiting for them, all in one
// transaction (the refs are returned only once, so a split would strand the
// job). It returns how many approvals expired. The scheduler calls it
// periodically.
func (g *Gate) ExpireAndResume(ctx context.Context) (int, error) {
	n := 0
	err := g.st.InTx(ctx, func(tx store.Tx) error {
		refs, err := tx.ExpireApprovals()
		if err != nil {
			return err
		}
		n = len(refs)
		type key struct{ user, job string }
		seen := map[key]bool{}
		var order []key
		for _, r := range refs {
			if err := g.event(tx, audit.Event{
				UserID: r.UserID, JobID: r.JobID, Kind: "approval_expired", Outcome: "expired", Detail: "approval=" + r.ID,
			}); err != nil {
				return err
			}
			k := key{r.UserID, r.JobID}
			if !seen[k] {
				seen[k] = true
				order = append(order, k)
			}
		}
		for _, k := range order {
			if _, err := tx.ResumeJobIfSettled(k.user, k.job); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return n, nil
}
