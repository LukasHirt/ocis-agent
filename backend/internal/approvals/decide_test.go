package approvals

import (
	"errors"
	"testing"
	"time"

	"github.com/owncloud/ocis-agent/backend/internal/store"
)

func item(a store.Approval, d store.ApprovalState) DecisionItem {
	return DecisionItem{ApprovalID: a.ID, Decision: d, ArgsHash: a.ArgsHash}
}

func (f *fixture) approvalState(id string) store.ApprovalState {
	f.t.Helper()
	var a store.Approval
	if err := f.st.View(bg, func(tx store.Tx) error {
		var err error
		a, err = tx.GetApproval(userID, id)
		return err
	}); err != nil {
		f.t.Fatal(err)
	}
	return a.State
}

func twoApprovals(f *fixture) (store.Claim, []store.Approval) {
	return f.requested(
		call("c1", "files.create_folder", `{"path":"/a"}`),
		call("c2", "files.create_folder", `{"path":"/b"}`),
	)
}

func TestDecideApprovesAndResumesTheJob(t *testing.T) {
	f := newFixture(t)
	c, as := twoApprovals(f)
	out, err := f.gate.Decide(bg, userID, userID, []DecisionItem{item(as[0], store.ApprovalApproved), item(as[1], store.ApprovalApproved)})
	if err != nil || len(out) != 2 {
		t.Fatalf("out = %+v %v", out, err)
	}
	for _, a := range out {
		if a.State != store.ApprovalApproved || a.DecidedBy != userID || a.DecidedAt == nil {
			t.Fatalf("approval = %+v", a)
		}
	}
	if got := f.jobState(c.Job.ID); got != store.JobQueued {
		t.Fatalf("job state = %s, want queued (resumed)", got)
	}
	if k := f.kinds(); k != "approval_requested,approval_requested,approval_decided,approval_decided" {
		t.Fatalf("audit = %s", k)
	}
	recs := f.records()
	last := recs[len(recs)-1]
	if last.Cred != "oidc" || last.Outcome != "approved" || last.Tool != "files.create_folder" || last.ArgsHash != as[1].ArgsHash {
		t.Fatalf("decision record = %+v", last)
	}
}

func TestRejectingAlsoResumesTheJob(t *testing.T) {
	f := newFixture(t)
	c, as := f.requested(call("c1", "files.create_folder", `{"path":"/a"}`))
	if _, err := f.gate.Decide(bg, userID, userID, []DecisionItem{item(as[0], store.ApprovalRejected)}); err != nil {
		t.Fatal(err)
	}
	if f.approvalState(as[0].ID) != store.ApprovalRejected || f.jobState(c.Job.ID) != store.JobQueued {
		t.Fatal("a rejection must be recorded and must wake the job")
	}
}

func TestTheJobWaitsUntilNothingIsPending(t *testing.T) {
	f := newFixture(t)
	c, as := twoApprovals(f)
	if _, err := f.gate.Decide(bg, userID, userID, []DecisionItem{item(as[0], store.ApprovalApproved)}); err != nil {
		t.Fatal(err)
	}
	if got := f.jobState(c.Job.ID); got != store.JobWaitingApproval {
		t.Fatalf("job state = %s, one approval is still pending", got)
	}
	if _, err := f.gate.Decide(bg, userID, userID, []DecisionItem{item(as[1], store.ApprovalRejected)}); err != nil {
		t.Fatal(err)
	}
	if got := f.jobState(c.Job.ID); got != store.JobQueued {
		t.Fatalf("job state = %s, want queued", got)
	}
}

func TestABulkDecisionIsAtomic(t *testing.T) {
	f := newFixture(t)
	c, as := twoApprovals(f)
	bad := item(as[1], store.ApprovalApproved)
	bad.ArgsHash = "sha256:" + "00"
	if _, err := f.gate.Decide(bg, userID, userID, []DecisionItem{item(as[0], store.ApprovalApproved), bad}); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid (hash mismatch)", err)
	}
	if f.approvalState(as[0].ID) != store.ApprovalPending || f.approvalState(as[1].ID) != store.ApprovalPending {
		t.Fatal("no item may be decided when one fails")
	}
	if f.jobState(c.Job.ID) != store.JobWaitingApproval || f.kinds() != "approval_requested,approval_requested" {
		t.Fatal("the job and the audit log must be unchanged")
	}
	// the same approval listed twice conflicts
	if _, err := f.gate.Decide(bg, userID, userID, []DecisionItem{item(as[0], store.ApprovalApproved), item(as[0], store.ApprovalApproved)}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("duplicate = %v, want ErrConflict", err)
	}
	if f.approvalState(as[0].ID) != store.ApprovalPending {
		t.Fatal("a failed bulk decision must leave everything pending")
	}
}

func TestDecideRefusals(t *testing.T) {
	f := newFixture(t)
	_, as := f.requested(call("c1", "files.create_folder", `{"path":"/a"}`))
	a := as[0]
	if _, err := f.gate.Decide(bg, userID, userID, nil); !errors.Is(err, store.ErrInvalid) {
		t.Errorf("empty list = %v", err)
	}
	if _, err := f.gate.Decide(bg, userID, "", []DecisionItem{item(a, store.ApprovalApproved)}); !errors.Is(err, store.ErrInvalid) {
		t.Errorf("empty decidedBy = %v", err)
	}
	if _, err := f.gate.Decide(bg, userID, userID, []DecisionItem{item(a, store.ApprovalExecuted)}); !errors.Is(err, store.ErrInvalid) {
		t.Errorf("decision 'executed' = %v", err)
	}
	if _, err := f.gate.Decide(bg, "u2", "u2", []DecisionItem{item(a, store.ApprovalApproved)}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("another user's approval = %v, want ErrNotFound", err)
	}
	if _, err := f.gate.Decide(bg, userID, userID, []DecisionItem{{ApprovalID: "nope", Decision: store.ApprovalApproved, ArgsHash: a.ArgsHash}}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown approval = %v", err)
	}
	if f.approvalState(a.ID) != store.ApprovalPending {
		t.Fatal("refusals must leave the approval pending")
	}
	// decided once, never again
	if _, err := f.gate.Decide(bg, userID, userID, []DecisionItem{item(a, store.ApprovalApproved)}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.gate.Decide(bg, userID, userID, []DecisionItem{item(a, store.ApprovalRejected)}); !errors.Is(err, store.ErrConflict) {
		t.Errorf("deciding twice = %v, want ErrConflict", err)
	}
}

func TestDecideAfterExpiryIsRefusedAndAfterTheJobEnded(t *testing.T) {
	f := newFixture(t)
	c, as := f.requested(call("c1", "files.create_folder", `{"path":"/a"}`))
	f.clk.Advance(169 * time.Hour)
	if _, err := f.gate.Decide(bg, userID, userID, []DecisionItem{item(as[0], store.ApprovalApproved)}); !errors.Is(err, store.ErrExpired) {
		t.Fatalf("late decision = %v, want ErrExpired", err)
	}
	if f.approvalState(as[0].ID) != store.ApprovalPending {
		t.Fatal("state must be unchanged after ErrExpired")
	}

	f2 := newFixture(t)
	c2, as2 := f2.requested(call("c1", "files.create_folder", `{"path":"/a"}`))
	if err := f2.st.InTx(bg, func(tx store.Tx) error { return tx.CancelJob(userID, c2.Job.ID) }); err != nil {
		t.Fatal(err)
	}
	if _, err := f2.gate.Decide(bg, userID, userID, []DecisionItem{item(as2[0], store.ApprovalApproved)}); !errors.Is(err, ErrJobEnded) {
		t.Fatalf("decision on a cancelled job = %v, want ErrJobEnded", err)
	}
	if f2.approvalState(as2[0].ID) != store.ApprovalPending {
		t.Fatal("an ended job's approval must stay untouched")
	}
	_ = c
}

func TestExpireAndResume(t *testing.T) {
	f := newFixture(t)
	c, as := twoApprovals(f)
	if n, err := f.gate.ExpireAndResume(bg); err != nil || n != 0 {
		t.Fatalf("nothing is due yet: %d %v", n, err)
	}
	// approve one (it stays approved: expiry only touches pending ones), let the other expire
	if _, err := f.gate.Decide(bg, userID, userID, []DecisionItem{item(as[0], store.ApprovalApproved)}); err != nil {
		t.Fatal(err)
	}
	f.clk.Advance(169 * time.Hour)
	n, err := f.gate.ExpireAndResume(bg)
	if err != nil || n != 1 {
		t.Fatalf("expired %d (%v), want 1", n, err)
	}
	if f.approvalState(as[0].ID) != store.ApprovalApproved || f.approvalState(as[1].ID) != store.ApprovalExpired {
		t.Fatal("only the pending approval may expire")
	}
	if got := f.jobState(c.Job.ID); got != store.JobQueued {
		t.Fatalf("job state = %s, expiry must wake the job", got)
	}
	recs := f.records()
	last := recs[len(recs)-1]
	if last.Kind != "approval_expired" || last.Detail != "approval="+as[1].ID {
		t.Fatalf("last record = %+v", last)
	}
	if n, err := f.gate.ExpireAndResume(bg); err != nil || n != 0 {
		t.Fatalf("second pass: %d %v", n, err)
	}
}

func TestExpireResumesEachJobOnlyWhenSettled(t *testing.T) {
	f := newFixture(t)
	c, as := twoApprovals(f)
	f.clk.Advance(169 * time.Hour)
	if n, err := f.gate.ExpireAndResume(bg); err != nil || n != 2 {
		t.Fatalf("expired %d (%v), want both", n, err)
	}
	if got := f.jobState(c.Job.ID); got != store.JobQueued {
		t.Fatalf("job state = %s", got)
	}
	for _, a := range as {
		if f.approvalState(a.ID) != store.ApprovalExpired {
			t.Fatalf("approval %s not expired", a.ID)
		}
	}
}
