package approvals

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/owncloud/ocis-agent/backend/internal/audit"
	"github.com/owncloud/ocis-agent/backend/internal/store"
)

// unknownOutcome approves the approval and settles it as unknown, like an
// executor that lost the outcome.
func (f *fixture) unknownOutcome(a store.Approval) {
	f.t.Helper()
	if _, err := f.gate.Decide(bg, userID, userID, []DecisionItem{item(a, store.ApprovalApproved)}); err != nil {
		f.t.Fatal(err)
	}
	if err := f.st.InTx(bg, func(tx store.Tx) error {
		if _, err := tx.TransitionApproval(userID, a.ID, store.ApprovalApproved, store.ApprovalExecuting, store.ApprovalPatch{}); err != nil {
			return err
		}
		_, err := tx.TransitionApproval(userID, a.ID, store.ApprovalExecuting, store.ApprovalUnknown,
			store.ApprovalPatch{Result: json.RawMessage(`{"outcome":"unknown"}`)})
		return err
	}); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) approval(id string) store.Approval {
	f.t.Helper()
	var a store.Approval
	if err := f.st.View(bg, func(tx store.Tx) error {
		var err error
		a, err = tx.GetApproval(userID, id)
		return err
	}); err != nil {
		f.t.Fatal(err)
	}
	return a
}

func TestAcknowledgeRecordsAndAuditsOnce(t *testing.T) {
	f := newFixture(t)
	_, as := f.requested(call("c1", "files.create_folder", `{"path":"/a"}`))
	f.unknownOutcome(as[0])
	before := len(f.records())

	got, err := f.gate.Acknowledge(bg, userID, as[0].ID, userID)
	if err != nil || got.AcknowledgedAt == nil || got.AcknowledgedBy != userID || got.State != store.ApprovalUnknown {
		t.Fatalf("acknowledge = %+v %v", got, err)
	}
	recs := f.records()
	if len(recs) != before+1 {
		t.Fatalf("audit grew by %d, want 1", len(recs)-before)
	}
	ev := recs[len(recs)-1]
	if ev.Kind != audit.KindApprovalAcknowledged || ev.Tool != "files.create_folder" || ev.Detail != "approval="+as[0].ID ||
		ev.Cred != "oidc" || ev.ArgsHash != "" || ev.Outcome != "" {
		t.Fatalf("event = %+v", ev)
	}
	again, err := f.gate.Acknowledge(bg, userID, as[0].ID, userID)
	if err != nil || !again.AcknowledgedAt.Equal(*got.AcknowledgedAt) {
		t.Fatalf("again = %+v %v", again, err)
	}
	if len(f.records()) != before+1 {
		t.Fatal("a second acknowledgement was audited")
	}
}

func TestAcknowledgeRefusesACertainOutcome(t *testing.T) {
	f := newFixture(t)
	_, as := f.requested(call("c1", "files.create_folder", `{"path":"/a"}`))
	before := f.kinds()
	if _, err := f.gate.Acknowledge(bg, userID, as[0].ID, userID); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("pending: %v", err)
	}
	if _, err := f.gate.Acknowledge(bg, userID, as[0].ID, ""); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("no acknowledger: %v", err)
	}
	if f.kinds() != before || f.approval(as[0].ID).AcknowledgedAt != nil {
		t.Fatal("a refused acknowledgement changed something")
	}
}

func TestAcknowledgeRollsBackWhenTheAuditFails(t *testing.T) {
	f := newFixture(t)
	_, as := f.requested(call("c1", "files.create_folder", `{"path":"/a"}`))
	f.unknownOutcome(as[0])
	before := f.kinds()
	g, err := NewGate(failAuditStore{f.st}, f.reg, Config{Now: f.clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.Acknowledge(bg, userID, as[0].ID, userID); !errors.Is(err, errAudit) {
		t.Fatalf("err = %v, want the audit error", err)
	}
	if a := f.approval(as[0].ID); a.AcknowledgedAt != nil || a.AcknowledgedBy != "" || !a.NeedsVerification() {
		t.Fatalf("approval = %+v, the failed call must change nothing", a)
	}
	if f.kinds() != before {
		t.Fatal("audit changed")
	}
}
