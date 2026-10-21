package approvals

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/audit"
	"github.com/owncloud/ocis-agent/backend/internal/store"
)

func TestSettleEndedExpiresAndAuditsTheApprovalsOfAnEndedJob(t *testing.T) {
	f := newFixture(t)
	c, as := twoApprovals(f)
	if _, err := f.gate.Decide(bg, userID, userID, []DecisionItem{item(as[0], store.ApprovalApproved)}); err != nil {
		t.Fatal(err)
	}
	f.cancel(c.Job.ID)
	before := len(f.records())

	n, err := f.gate.SettleEnded(bg, 0)
	if err != nil || n != 2 {
		t.Fatalf("settled %d (%v), want 2", n, err)
	}
	for _, a := range as {
		if s := f.approvalState(a.ID); s != store.ApprovalExpired {
			t.Fatalf("approval %s = %s, want expired", a.ID, s)
		}
	}
	recs := f.records()
	if len(recs) != before+2 {
		t.Fatalf("audit grew by %d, want 2", len(recs)-before)
	}
	got := map[string]bool{}
	for _, r := range recs[before:] {
		if r.Kind != "approval_expired" || r.Outcome != "expired" || r.JobID != c.Job.ID || r.UserID != userID {
			t.Fatalf("record = %+v", r)
		}
		got[r.Detail] = true
	}
	for _, a := range as {
		if !got["approval="+a.ID+"; job ended"] {
			t.Fatalf("no audit record for %s: %v", a.ID, got)
		}
	}
	if err := f.st.View(bg, func(tx store.Tx) error { _, err := tx.VerifyAudit(userID); return err }); err != nil {
		t.Fatalf("audit chain: %v", err)
	}

	if n, err := f.gate.SettleEnded(bg, 0); err != nil || n != 0 {
		t.Fatalf("second call settled %d (%v), want 0", n, err)
	}
	if len(f.records()) != before+2 {
		t.Fatal("a second call must not audit again")
	}
}

func TestSettleEndedLeavesTheApprovalsOfALiveJobAlone(t *testing.T) {
	f := newFixture(t)
	c, as := twoApprovals(f)
	before := f.kinds()
	f.clk.Advance(time.Hour)
	if n, err := f.gate.SettleEnded(bg, 0); err != nil || n != 0 {
		t.Fatalf("settled %d (%v), want 0", n, err)
	}
	for _, a := range as {
		if s := f.approvalState(a.ID); s != store.ApprovalPending {
			t.Fatalf("approval %s = %s, want pending", a.ID, s)
		}
	}
	if f.jobState(c.Job.ID) != store.JobWaitingApproval || f.kinds() != before {
		t.Fatal("nothing may change for a live job")
	}
}

// TestASettledApprovalNeverReachesTheRunner records why the runner's special
// case for "expired while pending" (expired with no DecidedBy) cannot see an
// approval settled here: its job has ended, so it is never claimed or
// resumed again.
func TestASettledApprovalNeverReachesTheRunner(t *testing.T) {
	f := newFixture(t)
	c, as := f.requested(call("c1", "files.create_folder", `{"path":"/a"}`))
	f.cancel(c.Job.ID)
	if n, err := f.gate.SettleEnded(bg, 0); err != nil || n != 1 {
		t.Fatalf("settled %d (%v)", n, err)
	}
	err := f.st.InTx(bg, func(tx store.Tx) error {
		if resumed, err := tx.ResumeJobIfSettled(userID, c.Job.ID); err != nil || resumed {
			t.Errorf("resume = %v %v, an ended job must stay ended", resumed, err)
		}
		if _, ok, err := tx.ClaimNextJob("w2", time.Minute); err != nil || ok {
			t.Errorf("claim = %v %v, an ended job must not be claimable", ok, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if f.jobState(c.Job.ID) != store.JobCancelled || f.approvalState(as[0].ID) != store.ApprovalExpired {
		t.Fatal("job or approval changed")
	}
}

// failAuditStore fails every AppendAudit, to prove the state change and its
// audit are one transaction.
type failAuditStore struct{ store.Store }

var errAudit = errors.New("audit append failed")

func (s failAuditStore) InTx(ctx context.Context, fn func(tx store.Tx) error) error {
	return s.Store.InTx(ctx, func(tx store.Tx) error { return fn(failAuditTx{tx}) })
}

type failAuditTx struct{ store.Tx }

func (failAuditTx) AppendAudit(audit.Event) (audit.Record, error) { return audit.Record{}, errAudit }

func TestSettleEndedRollsBackWhenTheAuditFails(t *testing.T) {
	f := newFixture(t)
	c, as := twoApprovals(f)
	f.cancel(c.Job.ID)
	before := f.kinds()

	g, err := NewGate(failAuditStore{f.st}, f.reg, Config{Now: f.clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	if n, err := g.SettleEnded(bg, 0); !errors.Is(err, errAudit) || n != 0 {
		t.Fatalf("settled %d (%v), want 0 and the audit error", n, err)
	}
	for _, a := range as {
		if s := f.approvalState(a.ID); s != store.ApprovalPending {
			t.Fatalf("approval %s = %s, the failed call must change nothing", a.ID, s)
		}
	}
	if f.kinds() != before {
		t.Fatal("audit changed")
	}
	if n, err := f.gate.SettleEnded(bg, 0); err != nil || n != 2 {
		t.Fatalf("retry settled %d (%v), want 2", n, err)
	}
}
